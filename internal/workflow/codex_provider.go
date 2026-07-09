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
		baseErr := fmt.Errorf("codex agent failed: %w: %s", err, strings.TrimSpace(stderr.String()))
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
