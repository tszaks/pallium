package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveProvider picks the worker provider for an agent call. Precedence,
// highest first: an explicit per-agent provider (the script's already-
// resolved agent.Provider), the call's own opts.Provider, a global env
// override, the detected steering agent, and finally "codex" as the
// long-standing default. Both inputs treat "" and "default" as unset so
// callers can pass either raw script options or already-stored Agent
// records through the same precedence chain.
func ResolveProvider(agentProvider, optsProvider string) string {
	if p := normalizeProvider(agentProvider); p != "" {
		return p
	}
	if p := normalizeProvider(optsProvider); p != "" {
		return p
	}
	if p := normalizeProvider(os.Getenv("PALLIUM_WORKFLOW_PROVIDER")); p != "" {
		return p
	}
	if p := DetectSteeringProvider(); p != "" {
		return p
	}
	return "codex"
}

// ProviderForDisplay resolves an agent's provider for reporting/analytics of
// an already-executed run. Unlike ResolveProvider it never re-runs live
// steering-agent detection: a historical run must report the provider it
// actually used, not whatever agent happens to be inspecting it later. The
// resolved provider is persisted on each agent row, so an empty value only
// occurs for pre-adoption records, which default to the historical "codex".
func ProviderForDisplay(agentProvider string) string {
	if p := normalizeProvider(agentProvider); p != "" {
		return p
	}
	return "codex"
}

func normalizeProvider(provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" || provider == "default" {
		return ""
	}
	return provider
}

// DetectSteeringProvider looks for environment signatures left by whichever
// agent is currently driving Pallium, so workflow workers adopt the same
// model with zero configuration. Returns "" when no known steering agent is
// detected. Structured as its own function so more signatures (codex,
// cursor, gemini, ...) can be added later without touching ResolveProvider.
func DetectSteeringProvider() string {
	if os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" {
		return "claude"
	}
	return ""
}

// runProviderCommand is the single dispatch point for a raw provider
// invocation. provider is already resolved (via ResolveProvider) by the
// caller; this only decides which of the three raw-invocation
// implementations actually runs a process: codex (codex_provider.go), an
// explicitly configured wrapper command (runConfiguredProviderCommand), or
// the built-in claude CLI (claude_provider.go). An explicitly configured
// wrapper always wins over the codex/claude built-ins for any provider name
// other than "codex" itself, so operators can override "claude" with a
// hardened wrapper (see providers/claude.sh) without a code change. Any
// other provider with no configured wrapper is a clear configuration error
// naming the env var it expects.
//
// Both a live agent call (runAgentCommand) and a one-off text call
// (RunProviderText, used by workflow generation) route through this same
// function, so no caller special-cases codex or any other provider.
func (r *Runner) runProviderCommand(ctx context.Context, provider, tmpDir, outFile, usageFile, cwd, prompt string, agent *Agent, opts AgentOptions, networkAllowed bool) (string, error) {
	if provider == "codex" {
		return r.runCodexCommand(ctx, tmpDir, outFile, cwd, prompt, agent, opts, networkAllowed)
	}
	if command := strings.TrimSpace(os.Getenv(providerCommandEnvName(provider))); command != "" {
		return r.runConfiguredProviderCommand(ctx, command, tmpDir, outFile, usageFile, cwd, prompt, agent, opts, networkAllowed)
	}
	if provider == "claude" {
		// The built-in claude provider has no network tool, so a double-consented
		// networked agent still runs without egress here. Say so rather than
		// silently degrade — resolveAgentNetwork already logged "network enabled".
		if networkAllowed {
			fmt.Fprintf(os.Stderr, "[workflow] agent %s requested network but the built-in claude provider has no network tool; running without egress (configure a claude wrapper via %s for networked claude)\n", firstNonEmpty(agent.Label, agent.ID), providerCommandEnvName(provider))
		}
		return r.runBuiltinClaudeCommand(ctx, usageFile, cwd, prompt, agent, opts)
	}
	return "", fmt.Errorf("workflow agent provider %q is not configured; set %s", provider, providerCommandEnvName(provider))
}

// RunProviderText resolves a provider via ResolveProvider and runs a single
// read-only, schema-less text-out call — no worktree, no retry, no usage
// accounting, just "run this prompt and give me the text back". It exists so
// callers outside a live workflow run (currently `pallium workflow generate
// --llm`) dispatch through the exact same codex/wrapper/claude resolution a
// running agent uses, instead of hardcoding a provider.
func (r *Runner) RunProviderText(ctx context.Context, prompt string) (string, error) {
	if r.CodexBinary == "" {
		r.CodexBinary = "codex"
	}
	cwd := strings.TrimSpace(r.Run.CWD)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	tmpDir, err := os.MkdirTemp("", "pallium-workflow-text-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	outFile := filepath.Join(tmpDir, "last-message.txt")
	usageFile := filepath.Join(tmpDir, "usage.json")
	provider := ResolveProvider("", "")
	agent := &Agent{Mode: "read-only", Prompt: prompt, Provider: provider}
	return r.runProviderCommand(ctx, provider, tmpDir, outFile, usageFile, cwd, prompt, agent, AgentOptions{}, false)
}
