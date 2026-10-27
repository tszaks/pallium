package workflow

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cursorCLIName is a var (not a const) so tests can point it at a fake
// binary without touching PATH.
var cursorCLIName = "cursor-agent"

// cursorAgentEnv is set to "1" by cursor-agent on every shell command it
// runs (verified on cursor-agent 2026.10.22; CURSOR_CONVERSATION_ID is set
// beside it). DetectSteeringProvider uses it to recognize a Cursor-driven
// Pallium invocation. It is checked BEFORE CLAUDECODE because those child
// shells inherit the parent environment: a Cursor agent started from a
// Claude Code terminal still carries CLAUDECODE, and the innermost driver
// is the one that matters.
const cursorAgentEnv = "CURSOR_AGENT"

// cursorStream is what Pallium keeps from `cursor-agent -p --output-format
// stream-json`. Events are one JSON object per line; the types seen on
// 2026.10.22 are system(init), user, thinking, assistant, tool_call
// (started/completed) and result. Unknown types and malformed lines are
// skipped, never fatal — Cursor's formats are undocumented.
type cursorStream struct {
	SessionID string
	// Text is the final answer: the last non-empty assistant message, else
	// the result event's text. The result event concatenates EVERY
	// assistant message of the turn (mid-turn chatter plus the answer), so
	// it is only the fallback.
	Text       string
	ResultText string
	IsError    bool
	Usage      cursorUsage
}

// cursorUsage mirrors the result event's usage block. Cursor reports token
// counts but never USD, so cost stays untracked (same split as devin).
type cursorUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

// buildCursorArgs builds the `cursor-agent` argv for one headless run. The
// prompt is NOT in argv: it is piped on stdin (verified to work with -p), so
// a large prompt cannot hit the OS argument-list limit (E2BIG), the same
// concern documented on the claude/codex/devin paths.
//
// Permission mapping, verified against cursor-agent 2026.10.22 in -p mode:
//   - read-only maps to --mode ask. Ask mode REMOVES the edit and shell
//     tools from the model: a spike asked for a file append plus a shell
//     command and neither ran (file unchanged, no file created), while read
//     and glob tools still worked. That is fail-closed enough for the
//     read-only contract. Note plain `-p` is NOT read-only: its own help
//     says it "has access to all tools, including write and shell".
//   - edit/test/check map to --force. Without it, -p applies file edits but
//     REJECTS shell commands ("rejected" tool result), so a test/check
//     worker could not run anything. --force allows commands unless
//     explicitly denied in ~/.cursor/cli-config.json.
//   - KNOWN ASYMMETRY: --force is also unconditional network egress, so an
//     edit-capable cursor worker's network is NOT gated by
//     PALLIUM_WORKFLOW_NETWORK. Pallium's worktree isolation bounds the
//     filesystem blast radius, not egress (same candor as buildDevinArgs).
//
// --trust is required unconditionally: print mode cannot show the
// workspace-trust prompt and Pallium worktrees are never pre-trusted.
// sessionToken is a chat id from an earlier turn's session_id; --resume
// continues the same chat (verified: same session_id, prior context kept).
func buildCursorArgs(mode, model, sessionToken string) []string {
	args := []string{"-p", "--output-format", "stream-json", "--trust"}
	if sessionToken != "" {
		args = append(args, "--resume", sessionToken)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	switch mode {
	case "edit", "test", "check":
		args = append(args, "--force")
	default:
		args = append(args, "--mode", "ask")
	}
	return args
}

// parseCursorStream reads the stream-json output, tolerating partial runs:
// whatever session id and text arrived before a crash are still returned.
func parseCursorStream(stdout string) cursorStream {
	var out cursorStream
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Result    string `json:"result"`
			IsError   bool   `json:"is_error"`
			Message   struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Usage cursorUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.SessionID != "" {
			out.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "assistant":
			var text strings.Builder
			for _, part := range ev.Message.Content {
				if part.Type == "text" {
					text.WriteString(part.Text)
				}
			}
			if strings.TrimSpace(text.String()) != "" {
				out.Text = strings.TrimSpace(text.String())
			}
		case "result":
			out.ResultText = strings.TrimSpace(ev.Result)
			out.IsError = ev.IsError
			out.Usage = ev.Usage
		}
	}
	if out.Text == "" {
		out.Text = out.ResultText
	}
	return out
}

// extractCursorOutput applies the schema-mode fence stripping and
// balanced-JSON recovery the claude and devin paths use.
func extractCursorOutput(stream cursorStream, hasSchema bool) (string, error) {
	text := stream.Text
	if stream.IsError && text == "" {
		return "", fmt.Errorf("cursor-agent reported an error with no output")
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

// cursorUsageMap returns the usage in Pallium's usage-file shape, or nil
// when the stream carried no counts. cost_usd is deliberately absent.
func cursorUsageMap(u cursorUsage) map[string]any {
	usage := map[string]any{}
	if u.InputTokens > 0 {
		usage["input_tokens"] = u.InputTokens
	}
	if u.OutputTokens > 0 {
		usage["output_tokens"] = u.OutputTokens
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

func writeCursorUsage(usageFile string, u cursorUsage) {
	usage := cursorUsageMap(u)
	if usage == nil {
		return
	}
	if raw, err := json.Marshal(usage); err == nil {
		_ = os.WriteFile(usageFile, raw, 0o600)
	}
}

// runCursorProcess runs one cursor-agent turn with the prompt on stdin.
func runCursorProcess(ctx context.Context, args []string, cwd, prompt string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, cursorCLIName, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(prompt)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

// runBuiltinCursorCommand invokes `cursor-agent` directly, used when the
// resolved provider is "cursor" and no PALLIUM_WORKFLOW_PROVIDER_CURSOR_COMMAND
// wrapper is configured.
func (r *Runner) runBuiltinCursorCommand(ctx context.Context, usageFile, cwd, prompt string, agent *Agent, opts AgentOptions) (string, error) {
	fullPrompt, err := buildSchemaPrompt(prompt, opts.Schema)
	if err != nil {
		return "", fmt.Errorf("workflow provider \"cursor\": %w", err)
	}
	args := buildCursorArgs(agent.Mode, opts.Model, "")
	stdout, stderr, runErr := runCursorProcess(ctx, args, cwd, fullPrompt)
	stream := parseCursorStream(stdout)
	writeCursorUsage(usageFile, stream.Usage)
	if runErr != nil {
		baseErr := formatProviderFailure("workflow provider \"cursor\"", runErr, truncateForError(strings.TrimSpace(stderr)))
		return truncateForError(strings.TrimSpace(stdout)), wrapProviderCommandError(baseErr, stdout+stderr)
	}
	text, parseErr := extractCursorOutput(stream, len(opts.Schema) > 0)
	if parseErr != nil {
		return "", fmt.Errorf("workflow provider \"cursor\" produced no output: %w", parseErr)
	}
	return text, nil
}

// runCursorTeamTurn runs one turn of a cursor teammate's native chat.
// Cursor mints its own chat id (reported as session_id on every event,
// including the first), so the token is captured from the stream and
// returned even on a failed turn: a retry resumes whatever partial chat
// cursor already recorded.
func (r *Runner) runCursorTeamTurn(ctx context.Context, mode, model, sessionToken, cwd, prompt string, schema map[string]any) (output, capturedToken string, usage map[string]any, err error) {
	fullPrompt, err := buildSchemaPrompt(prompt, schema)
	if err != nil {
		return "", "", nil, fmt.Errorf("team turn (cursor): %w", err)
	}
	args := buildCursorArgs(mode, model, sessionToken)
	stdout, stderr, runErr := runCursorProcess(ctx, args, cwd, fullPrompt)
	stream := parseCursorStream(stdout)
	captured := sessionToken
	if stream.SessionID != "" {
		captured = stream.SessionID
	}
	usage = cursorUsageMap(stream.Usage)
	if runErr != nil {
		baseErr := formatProviderFailure("team turn (cursor)", runErr, truncateForError(strings.TrimSpace(stderr)))
		return "", captured, usage, wrapProviderCommandError(baseErr, stdout+stderr)
	}
	text, parseErr := extractCursorOutput(stream, len(schema) > 0)
	if parseErr != nil {
		return "", captured, usage, fmt.Errorf("team turn (cursor) produced no output: %w", parseErr)
	}
	return text, captured, usage, nil
}
