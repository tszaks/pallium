package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleHelpIsRouted(t *testing.T) {
	var out bytes.Buffer
	app := NewApp(&out, &out)
	if err := app.Run([]string{"console", "--help"}); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium console") {
		t.Fatalf("expected console help, got %q", out.String())
	}
}

func TestConsoleManifestSetAndShow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.sqlite")
	var out bytes.Buffer
	err := runConsole(&out, []string{
		"manifest", "set",
		"--db", dbPath,
		"--session", "session-1",
		"--provider", "codex",
		"--machine", "test-host",
		"--goal", "Fix production issue",
		"--step", "Inspect signal",
		"--file", "cmd/app.go",
		"--next", "run tests",
		"--risk", "production",
		"--stop", "before deploy",
	}, false)
	if err != nil {
		t.Fatalf("manifest set failed: %v", err)
	}
	if !strings.Contains(out.String(), "Manifest saved") {
		t.Fatalf("expected save output, got %q", out.String())
	}

	out.Reset()
	err = runConsole(&out, []string{
		"manifest", "show",
		"--db", dbPath,
		"--session", "session-1",
		"--provider", "codex",
		"--machine", "test-host",
	}, false)
	if err != nil {
		t.Fatalf("manifest show failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Fix production issue", "cmd/app.go", "before deploy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in output %q", want, got)
		}
	}
}

func TestConsoleAuthorityDecisionRejectsConflictingFlags(t *testing.T) {
	var out bytes.Buffer
	err := runConsole(&out, []string{"authority", "decide", "event-1", "--approve", "--deny", "--db", filepath.Join(t.TempDir(), "sessions.sqlite")}, false)
	if err == nil {
		t.Fatal("expected conflicting flag error")
	}
	if !strings.Contains(err.Error(), "choose only one") {
		t.Fatalf("unexpected error: %v", err)
	}
}
