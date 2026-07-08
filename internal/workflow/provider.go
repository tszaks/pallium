package workflow

import (
	"os"
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
