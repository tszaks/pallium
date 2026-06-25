package cmd

import (
	"bytes"
	"os"
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

func TestConsoleRunReadAndOwnedList(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	logPath := filepath.Join(tmp, "owned.log")
	var out bytes.Buffer
	err := runConsole(&out, []string{
		"run",
		"--id", "owned-test",
		"--db", dbPath,
		"--cwd", tmp,
		"--log", logPath,
		"--",
		"/bin/sh", "-c", "printf hello-owned",
	}, false)
	if err != nil {
		t.Fatalf("console run failed: %v", err)
	}
	if !strings.Contains(out.String(), "hello-owned") {
		t.Fatalf("expected command output, got %q", out.String())
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "hello-owned") {
		t.Fatalf("expected log output, got %q", string(raw))
	}

	out.Reset()
	if err := runConsole(&out, []string{"read", "owned-test", "--db", dbPath}, false); err != nil {
		t.Fatalf("console read failed: %v", err)
	}
	if !strings.Contains(out.String(), "hello-owned") {
		t.Fatalf("expected read output, got %q", out.String())
	}

	out.Reset()
	if err := runConsole(&out, []string{"owned", "list", "--db", dbPath}, false); err != nil {
		t.Fatalf("owned list failed: %v", err)
	}
	if !strings.Contains(out.String(), "owned-test exited") {
		t.Fatalf("expected owned list output, got %q", out.String())
	}
}
