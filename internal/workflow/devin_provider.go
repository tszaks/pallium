package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// devinCLIName is a var (not a const) so tests can point it at a fake
// binary without touching PATH.
var devinCLIName = "devin"

// devinSessionDBEnv is set by the Devin CLI for its own subprocesses and
// points at the CLI's sessions database. DetectSteeringProvider uses its
// presence to recognize a Devin-driven Pallium invocation, the same way
// CLAUDECODE recognizes Claude Code.
const devinSessionDBEnv = "CHISEL_SESSION_DB"

// devinExport mirrors the parts of Devin's ATIF export (--export) that
// Pallium consumes: the provider-assigned session id (the resume token for
// team turns) and the run's token usage. Devin bills in ACU, not USD, so
// there is no cost_usd to report — token counts are recorded for
// diagnostics while the flat per-agent estimate still drives budgets (same
// honesty split codex occupies, except devin reports real token counts).
type devinExport struct {
	SessionID    string `json:"session_id"`
	FinalMetrics struct {
		PromptTokens     int64 `json:"total_prompt_tokens"`
		CompletionTokens int64 `json:"total_completion_tokens"`
	} `json:"final_metrics"`
	Steps []struct {
		Source  string `json:"source"`
		Message string `json:"message"`
	} `json:"steps"`
}

// buildDevinArgs builds the `devin` CLI argv for one headless invocation.
// The prompt goes through --prompt-file (never argv/stdin): a large prompt
// as an inline -p value can exceed the OS argument-list limit before the
// CLI even starts, the same E2BIG concern documented on the claude/codex
// paths.
//
// Permission-mode mapping, verified against devin 3000.10.x in -p mode:
//   - "auto" auto-approves only read-only tools; a write/exec/web call in
//     non-interactive mode is rejected outright ("rejected a tool call that
//     requires confirmation"), so it fails closed — exactly the read-only
//     contract, including no network egress.
//   - edit/test/check need "dangerous": accept-edits auto-approves file
//     writes but still prompts on shell commands, and autonomous requires
//     --sandbox, which prompts on file writes too — both fail closed in
//     -p mode, so they cannot run an edit/test/check worker at all.
//   - KNOWN ASYMMETRY: "dangerous" also auto-approves network egress, so
//     unlike codex's pinned network_access=false or claude's curl/gh
//     disallowlist, a devin edit/test/check worker's network is NOT gated
//     by PALLIUM_WORKFLOW_NETWORK — Pallium's worktree isolation bounds the
//     filesystem blast radius, not egress. Documented rather than silently
//     smoothed over, matching providers/README.md's candor conventions.
//
// --respect-workspace-trust=false is required unconditionally: print mode
// cannot show the workspace-trust prompt and fails in an untrusted
// directory (Pallium worktrees are never pre-trusted).
//
// --export writes an ATIF transcript after each turn carrying session_id
// and final_metrics — Pallium's channel for team session capture and token
// usage, since `devin -p` itself reports neither.
func buildDevinArgs(mode, model, promptFile, exportFile, sessionToken string) []string {
	args := []string{"-p", "--respect-workspace-trust", "false", "--prompt-file", promptFile, "--export", exportFile}
	if sessionToken != "" {
		args = append(args, "-r", sessionToken)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	switch mode {
	case "edit", "test", "check":
		args = append(args, "--permission-mode", "dangerous")
	default:
		args = append(args, "--permission-mode", "auto")
	}
	return args
}

// extractDevinOutput returns the answer text for one headless run. The ATIF
// export's last agent step is authoritative — `devin -p` prints EVERY agent
// message to stdout concatenated without separators, so stdout alone can't
// distinguish the final answer from mid-turn chatter. stdout is the
// fallback for a missing/corrupt export, then the same schema-mode fence
// stripping/balanced-JSON recovery the claude path applies.
func extractDevinOutput(rawStdout string, export *devinExport, hasSchema bool) (string, error) {
	text := ""
	if export != nil {
		for i := len(export.Steps) - 1; i >= 0; i-- {
			if export.Steps[i].Source == "agent" && strings.TrimSpace(export.Steps[i].Message) != "" {
				text = export.Steps[i].Message
				break
			}
		}
	}
	if text == "" {
		text = strings.TrimSpace(rawStdout)
	}
	if hasSchema {
		text = stripJSONFence(text)
		if json.Unmarshal([]byte(text), new(any)) != nil {
			if candidate, ok := extractBalancedJSON(text); ok {
				text = candidate
			}
		}
	}
	if text == "" {
		return "", fmt.Errorf("empty output")
	}
	return text, nil
}

// readDevinExport parses the ATIF export file if it exists; a missing or
// malformed file yields nil so callers degrade to stdout-only output and an
// empty session token rather than failing an otherwise successful turn.
func readDevinExport(path string) *devinExport {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var export devinExport
	if json.Unmarshal(raw, &export) != nil {
		return nil
	}
	return &export
}

// writeDevinUsage records token usage in Pallium's usage-file shape
// ({"input_tokens","output_tokens","cost_usd"}) when the export carried
// metrics. cost_usd is deliberately absent — see devinExport's comment.
func writeDevinUsage(usageFile string, export *devinExport) {
	if export == nil {
		return
	}
	usage := map[string]any{}
	if export.FinalMetrics.PromptTokens > 0 {
		usage["input_tokens"] = export.FinalMetrics.PromptTokens
	}
	if export.FinalMetrics.CompletionTokens > 0 {
		usage["output_tokens"] = export.FinalMetrics.CompletionTokens
	}
	if len(usage) == 0 {
		return
	}
	if raw, err := json.Marshal(usage); err == nil {
		_ = os.WriteFile(usageFile, raw, 0o600)
	}
}

// runBuiltinDevinCommand invokes the `devin` CLI directly, used when the
// resolved provider is "devin" and no PALLIUM_WORKFLOW_PROVIDER_DEVIN_COMMAND
// wrapper is configured — provider adoption works with just the CLI on
// PATH, same as built-in claude.
func (r *Runner) runBuiltinDevinCommand(ctx context.Context, tmpDir, usageFile, cwd, prompt string, agent *Agent, opts AgentOptions) (string, error) {
	fullPrompt, err := buildSchemaPrompt(prompt, opts.Schema)
	if err != nil {
		return "", fmt.Errorf("workflow provider \"devin\": %w", err)
	}
	promptFile := filepath.Join(tmpDir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte(fullPrompt), 0o600); err != nil {
		return "", err
	}
	exportFile := filepath.Join(tmpDir, "export.json")
	args := buildDevinArgs(agent.Mode, opts.Model, promptFile, exportFile, "")
	cmd := exec.CommandContext(ctx, devinCLIName, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	export := readDevinExport(exportFile)
	writeDevinUsage(usageFile, export)
	if runErr != nil {
		baseErr := formatProviderFailure("workflow provider \"devin\"", runErr, truncateForError(strings.TrimSpace(stderr.String())))
		return truncateForError(strings.TrimSpace(stdout.String())), wrapProviderCommandError(baseErr, stdout.String()+stderr.String())
	}
	text, parseErr := extractDevinOutput(stdout.String(), export, len(opts.Schema) > 0)
	if parseErr != nil {
		return "", fmt.Errorf("workflow provider \"devin\" produced no output: %w", parseErr)
	}
	return text, nil
}

// runDevinTeamTurn runs one turn of a devin teammate's native conversation.
// Devin mints its own session ids (like codex's thread.started — the caller
// cannot supply one), so the token is captured from the ATIF export AFTER
// the run rather than streamed mid-run: a first turn killed before its
// export is written leaves an orphaned devin-side session row and retries
// as a fresh session — harmless, unlike a captured-then-clobbered token.
// The token is returned even on a failed turn when the export exists, so a
// retry resumes whatever partial conversation devin already recorded.
// member.SessionToken != "" is the resume check (not SessionEstablished):
// a failed first turn can still have minted a session worth resuming.
func (r *Runner) runDevinTeamTurn(ctx context.Context, mode, model, sessionToken, cwd, prompt string, schema map[string]any) (output, capturedToken string, usage map[string]any, err error) {
	fullPrompt, err := buildSchemaPrompt(prompt, schema)
	if err != nil {
		return "", "", nil, fmt.Errorf("team turn (devin): %w", err)
	}
	tmpDir, terr := os.MkdirTemp("", "pallium-team-turn-devin-*")
	if terr != nil {
		return "", "", nil, terr
	}
	defer os.RemoveAll(tmpDir)
	promptFile := filepath.Join(tmpDir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte(fullPrompt), 0o600); err != nil {
		return "", "", nil, err
	}
	exportFile := filepath.Join(tmpDir, "export.json")
	args := buildDevinArgs(mode, model, promptFile, exportFile, sessionToken)
	cmd := exec.CommandContext(ctx, devinCLIName, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	export := readDevinExport(exportFile)
	captured := sessionToken
	if export != nil && export.SessionID != "" {
		captured = export.SessionID
	}
	if export != nil && (export.FinalMetrics.PromptTokens > 0 || export.FinalMetrics.CompletionTokens > 0) {
		usage = map[string]any{
			"input_tokens":  export.FinalMetrics.PromptTokens,
			"output_tokens": export.FinalMetrics.CompletionTokens,
		}
	}
	if runErr != nil {
		baseErr := formatProviderFailure("team turn (devin)", runErr, truncateForError(strings.TrimSpace(stderr.String())))
		return "", captured, usage, wrapProviderCommandError(baseErr, stdout.String()+stderr.String())
	}
	text, parseErr := extractDevinOutput(stdout.String(), export, len(schema) > 0)
	if parseErr != nil {
		return "", captured, usage, fmt.Errorf("team turn (devin) produced no output: %w", parseErr)
	}
	return text, captured, usage, nil
}
