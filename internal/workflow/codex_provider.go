package workflow

import (
	"bufio"
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

// runCodexCommand invokes the `codex exec` CLI directly, used when the
// resolved provider is "codex" (the long-standing default; see
// ResolveProvider). This is the ONLY place that spawns a codex process —
// every other codex-specific concern (sandbox flags, schema file, model
// flag) lives here too, mirroring runBuiltinClaudeCommand's role as the sole
// claude invocation site.
func (r *Runner) runCodexCommand(ctx context.Context, tmpDir, outFile, cwd, prompt string, agent *Agent, opts AgentOptions, networkAllowed bool) (string, error) {
	cmdArgs := []string{"exec", "--cd", cwd, "--output-last-message", outFile}
	cmdArgs = append(cmdArgs, codexSandboxArgs(agent.Mode, networkAllowed)...)
	if opts.Model != "" {
		cmdArgs = append(cmdArgs, "--model", opts.Model)
	}
	if len(opts.Schema) > 0 {
		schemaPath := filepath.Join(tmpDir, "schema.json")
		normalizedSchema := normalizeSchema(opts.Schema)
		raw, err := json.MarshalIndent(normalizedSchema, "", "  ")
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(schemaPath, raw, 0o644); err != nil {
			return "", err
		}
		cmdArgs = append(cmdArgs, "--output-schema", schemaPath)
	}
	cmdArgs = append(cmdArgs, prompt)
	cmd := exec.CommandContext(ctx, r.CodexBinary, cmdArgs...)
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		baseErr := formatProviderFailure("codex agent", err, truncateForError(strings.TrimSpace(stderr.String())))
		return "", wrapProviderCommandError(baseErr, stdout.String()+stderr.String())
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// codexThreadStartedEvent is the one `codex exec --json` event this file
// cares about parsing: {"type":"thread.started","thread_id":"<uuid>"}, codex's
// own session identifier, emitted once at the start of a fresh (non-resumed)
// thread. Unlike claude, codex assigns this id itself — it cannot be minted
// ahead of time — so it must be captured from the live event stream.
type codexThreadStartedEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
}

// runCodexTeamTurn runs one turn of a codex teammate's native conversation.
// On the first turn (sessionToken empty), it runs a fresh `codex exec --json`
// and captures the assigned thread_id from the `thread.started` event AS
// SOON as that line streams in — via onSessionCaptured, called at most once
// — rather than waiting for the whole turn to finish, so a process killed
// moments later still leaves the session resumable. Later turns resume that
// thread directly (`codex exec resume <thread_id>`); onSessionCaptured is not
// called again since the id is already known. Output is still written to
// outFile via --output-last-message, exactly like the regular worker path;
// --json is layered on top purely to observe thread.started, not to parse
// the final answer.
func (r *Runner) runCodexTeamTurn(ctx context.Context, tmpDir, outFile, cwd, model, sessionToken string, mode string, networkAllowed bool, prompt string, schema map[string]any, onSessionCaptured func(threadID string)) (string, error) {
	var cmdArgs []string
	if sessionToken == "" {
		cmdArgs = []string{"exec", "--cd", cwd, "--json", "--output-last-message", outFile}
	} else {
		cmdArgs = []string{"exec", "resume", sessionToken, "--cd", cwd, "--json", "--output-last-message", outFile}
	}
	cmdArgs = append(cmdArgs, codexSandboxArgs(mode, networkAllowed)...)
	if model != "" {
		cmdArgs = append(cmdArgs, "--model", model)
	}
	if len(schema) > 0 {
		schemaPath := filepath.Join(tmpDir, "team-decision-schema.json")
		raw, err := json.MarshalIndent(normalizeSchema(schema), "", "  ")
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(schemaPath, raw, 0o644); err != nil {
			return "", err
		}
		cmdArgs = append(cmdArgs, "--output-schema", schemaPath)
	}
	cmdArgs = append(cmdArgs, prompt)
	cmd := exec.CommandContext(ctx, r.CodexBinary, cmdArgs...)
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}

	// The pipe MUST be fully drained before Wait — calling Wait while the
	// pipe still has unread data races the pipe's own close (a documented
	// os/exec pitfall). The scan loop below always runs to EOF (or the
	// process's own exit closes the write end) before we call cmd.Wait().
	var stdout bytes.Buffer
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	captured := false
	for scanner.Scan() {
		line := scanner.Bytes()
		stdout.Write(line)
		stdout.WriteByte('\n')
		if sessionToken == "" && !captured {
			var event codexThreadStartedEvent
			if json.Unmarshal(line, &event) == nil && event.Type == "thread.started" && event.ThreadID != "" {
				captured = true
				if onSessionCaptured != nil {
					onSessionCaptured(event.ThreadID)
				}
			}
		}
	}
	waitErr := cmd.Wait()
	// A scan error (a line over the 4MB cap, or the pipe itself erroring)
	// stops the loop the same way a clean EOF does — Scan() just returns
	// false either way — so without this check a truncated/corrupted
	// stream with waitErr==nil would silently fall through to "success"
	// on whatever partial last-message file happened to exist.
	if scanErr := scanner.Err(); scanErr != nil {
		baseErr := fmt.Errorf("team turn (codex) failed reading output: %w", scanErr)
		return "", wrapProviderCommandError(baseErr, stdout.String()+stderr.String())
	}
	if waitErr != nil {
		baseErr := formatProviderFailure("team turn (codex)", waitErr, truncateForError(strings.TrimSpace(stderr.String())))
		return "", wrapProviderCommandError(baseErr, stdout.String()+stderr.String())
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// codexSandboxArgs returns the `codex exec` sandbox flags for an agent.
//
// Default (no network): edit/test/check get workspace-write, everything else
// read-only. Neither of these grants network egress, which is the safe
// baseline — no worker can reach the network unless the operator explicitly
// opted the run in.
//
// When network is allowed we want the most-scoped sandbox that still grants
// egress. Codex v0.142.5 exposes a granular per-mode toggle: with
// `--sandbox workspace-write` the filesystem stays scoped to the workspace,
// and `-c sandbox_workspace_write.network_access=true` enables the network
// without falling back to the far broader `--sandbox danger-full-access`
// (which would also allow writes anywhere on the host). Evidence, from the
// bundled codex binary's own help/config text:
//
//	"In `workspace-write`, network access still depends on your Codex
//	 configuration (for example `[sandbox_workspace_write] network_access = true`)."
//
// So a networked worker is still confined to the workspace for filesystem
// writes. A read-only agent that opts into network is therefore upgraded to
// workspace-write (read-only has no per-mode network toggle) — the minimal
// scope that can carry egress.
//
// The non-network workspace-write path (edit/test/check) must ALSO pin the
// toggle, explicitly to false. Codex resolves `network_access` from the
// operator's ambient ~/.codex/config.toml when the flag is absent, so a bare
// `--sandbox workspace-write` would silently inherit
// `[sandbox_workspace_write] network_access = true` and hand a worker egress
// with no --allow-network and no network:true — defeating the safe default.
// Forcing `-c sandbox_workspace_write.network_access=false` makes Pallium's
// lockdown authoritative regardless of ambient config. Verified against codex
// v0.142.5: the flag is accepted, and with an ambient config setting the key
// true the explicit `=false` override resolves to network-disabled (a
// sandboxed connect attempt is refused). read-only has no network regardless,
// so it needs no pin.
func codexSandboxArgs(mode string, networkAllowed bool) []string {
	if networkAllowed {
		return []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true"}
	}
	if mode == "edit" || mode == "test" || mode == "check" {
		return []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=false"}
	}
	return []string{"--sandbox", "read-only"}
}
