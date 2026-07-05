package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	consoleStore "github.com/tszaks/pallium/internal/console"
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
		"/bin/sh", "-c", "printf 'hello-owned\n'",
	}, false)
	if err != nil {
		t.Fatalf("console run failed: %v", err)
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

	out.Reset()
	if err := runConsole(&out, []string{"owned", "show", "owned-test", "--db", dbPath}, true); err != nil {
		t.Fatalf("owned show json failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode owned show json: %v\n%s", err, out.String())
	}
	if payload["exit_code"] != float64(0) {
		t.Fatalf("expected explicit zero exit_code, got %#v in %s", payload["exit_code"], out.String())
	}
}

func TestReadOwnedLogTailIgnoresTrailingNewline(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "owned.log")
	if err := os.WriteFile(logPath, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readOwnedLog(logPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != "beta\ngamma\n" {
		t.Fatalf("expected last two lines, got %q", got)
	}
}

func TestConsoleRunRejectsUnsafeOwnedID(t *testing.T) {
	tmp := t.TempDir()
	var out bytes.Buffer
	err := runConsole(&out, []string{
		"run",
		"--id", "../escape",
		"--db", filepath.Join(tmp, "sessions.sqlite"),
		"--cwd", tmp,
		"--",
		"/bin/echo", "hello",
	}, false)
	if err == nil {
		t.Fatal("expected unsafe owned session id to fail")
	}
	if strings.Contains(out.String(), "hello") {
		t.Fatalf("command should not have run, got output %q", out.String())
	}
}

func TestInterruptProcessStopsOwnedProcessGroup(t *testing.T) {
	tmp := t.TempDir()
	store, err := consoleStore.Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session, err := store.CreateOwnedSession(consoleStore.OwnedSession{
		ID:      "interrupt-test",
		Command: []string{"/bin/sh", "-c", "printf 'before-sleep\n'; sleep 10; printf 'after-sleep\n'"},
		CWD:     tmp,
		LogPath: filepath.Join(tmp, "owned.log"),
		Status:  "starting",
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		exitCode, _ := runOwnedProcess(store, session, io.Discard, false)
		done <- exitCode
	}()

	var childPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.OwnedSession(session.ID)
		if err == nil && current.ChildPID > 0 {
			childPID = current.ChildPID
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("owned session never reported child pid")
	}
	if err := interruptProcess(childPID); err != nil {
		t.Fatalf("interrupt failed: %v", err)
	}

	select {
	case exitCode := <-done:
		if exitCode == 0 {
			t.Fatal("expected interrupted process to exit non-zero")
		}
	case <-time.After(3 * time.Second):
		_ = interruptProcess(childPID)
		t.Fatal("owned process did not stop after interrupt")
	}

	raw, err := os.ReadFile(session.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "after-sleep") {
		t.Fatalf("expected process to stop before after-sleep, got %q", string(raw))
	}
}
