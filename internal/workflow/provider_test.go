package workflow

import "testing"

// clearProviderEnv resets every env var ResolveProvider/DetectSteeringProvider
// consult, so a test's expectations don't depend on whatever the ambient
// shell happens to have set (a real concern: a dev running these tests from
// inside Claude Code has CLAUDECODE set for real).
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PALLIUM_WORKFLOW_PROVIDER",
		"CLAUDECODE",
		"CLAUDE_CODE_ENTRYPOINT",
	} {
		t.Setenv(name, "")
	}
}

func TestResolveProviderPrecedence(t *testing.T) {
	cases := []struct {
		name          string
		agentProvider string
		optsProvider  string
		envProvider   string
		claudeCode    string
		want          string
	}{
		{name: "agent provider wins over everything", agentProvider: "codex", optsProvider: "claude", envProvider: "gemini", claudeCode: "1", want: "codex"},
		{name: "opts provider wins over env and detection", optsProvider: "gemini", envProvider: "claude", claudeCode: "1", want: "gemini"},
		{name: "env override wins over detection", envProvider: "gemini", claudeCode: "1", want: "gemini"},
		{name: "detected steering agent used when nothing else set", claudeCode: "1", want: "claude"},
		{name: "falls back to codex when nothing detected", want: "codex"},
		{name: "agent provider default treated as unset", agentProvider: "default", optsProvider: "gemini", want: "gemini"},
		{name: "opts provider default treated as unset", optsProvider: "default", claudeCode: "1", want: "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearProviderEnv(t)
			if tc.envProvider != "" {
				t.Setenv("PALLIUM_WORKFLOW_PROVIDER", tc.envProvider)
			}
			if tc.claudeCode != "" {
				t.Setenv("CLAUDECODE", tc.claudeCode)
			}
			if got := ResolveProvider(tc.agentProvider, tc.optsProvider); got != tc.want {
				t.Fatalf("ResolveProvider(%q, %q) = %q, want %q", tc.agentProvider, tc.optsProvider, got, tc.want)
			}
		})
	}
}

func TestDetectSteeringProvider(t *testing.T) {
	t.Run("CLAUDECODE set", func(t *testing.T) {
		clearProviderEnv(t)
		t.Setenv("CLAUDECODE", "1")
		if got := DetectSteeringProvider(); got != "claude" {
			t.Fatalf("DetectSteeringProvider() = %q, want claude", got)
		}
	})
	t.Run("CLAUDE_CODE_ENTRYPOINT set", func(t *testing.T) {
		clearProviderEnv(t)
		t.Setenv("CLAUDE_CODE_ENTRYPOINT", "sdk-cli")
		if got := DetectSteeringProvider(); got != "claude" {
			t.Fatalf("DetectSteeringProvider() = %q, want claude", got)
		}
	})
	t.Run("nothing set", func(t *testing.T) {
		clearProviderEnv(t)
		if got := DetectSteeringProvider(); got != "" {
			t.Fatalf("DetectSteeringProvider() = %q, want empty", got)
		}
	})
}
