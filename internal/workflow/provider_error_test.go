package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeaningfulProviderErrorLine(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   string
		found  bool
	}{
		{
			name:   "usage limit buried above a generic kill signal",
			output: "some startup noise\nERROR: You've hit your usage limit, try again at Aug 7th, 2026\nsignal: killed: Reading additional input from stdin...",
			want:   "ERROR: You've hit your usage limit, try again at Aug 7th, 2026",
			found:  true,
		},
		{
			name:   "no meaningful line present",
			output: "signal: killed: Reading additional input from stdin...",
			found:  false,
		},
		{
			name:   "matches are case-insensitive",
			output: "Rate Limit exceeded, slow down",
			want:   "Rate Limit exceeded, slow down",
			found:  true,
		},
		{
			name:   "empty output",
			output: "",
			found:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := meaningfulProviderErrorLine(tc.output)
			if ok != tc.found {
				t.Fatalf("meaningfulProviderErrorLine() ok = %v, want %v", ok, tc.found)
			}
			if got != tc.want {
				t.Fatalf("meaningfulProviderErrorLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunConfiguredProviderCommandSurfacesMeaningfulErrorLine proves a
// configured provider wrapper that dies with a generic kill signal after
// printing its real failure reason (a quota wall) surfaces that reason as
// the leading error message instead of just the signal.
func TestRunConfiguredProviderCommandSurfacesMeaningfulErrorLine(t *testing.T) {
	clearProviderEnv(t)
	tmp := t.TempDir()
	wrapperScript := filepath.Join(tmp, "quota-wall.sh")
	if err := os.WriteFile(wrapperScript, []byte(`#!/bin/sh
echo "ERROR: You've hit your usage limit. try again at Aug 7th, 2026"
echo "signal: killed: Reading additional input from stdin..." >&2
exit 1
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_GROK_COMMAND", wrapperScript)

	agent := &Agent{Mode: "read-only", Prompt: "hello", Provider: "grok"}
	r := &Runner{}
	callTmp := t.TempDir()
	_, err := r.runProviderCommand(context.Background(), "grok", callTmp, filepath.Join(callTmp, "out.txt"), filepath.Join(callTmp, "usage.json"), callTmp, agent.Prompt, agent, AgentOptions{}, false)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("expected surfaced error to contain the usage-limit line, got: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "ERROR: You've hit your usage limit") {
		t.Fatalf("expected the meaningful line to lead the error message, got: %v", err)
	}
}
