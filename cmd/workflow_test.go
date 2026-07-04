package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowRunShowReadAndSave(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"status":"ok","prompt":"{{PROMPT}}"}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`export const meta = { name: "test", phases: ["scan"] };
phase("scan");
const result = agent("scan repo", { label: "scanner" });
return { result };`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-cli",
		"--db", dbPath,
		"--cwd", tmp,
		"--script", scriptPath,
		"test task",
	}, false)
	if err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	if !strings.Contains(out.String(), "Workflow wf-cli: completed") {
		t.Fatalf("unexpected run output: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(tmp, ".pallium", "workflow-runs")); !os.IsNotExist(err) {
		t.Fatalf("workflow run should not write repo-local run artifacts, stat err=%v", err)
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"show", "wf-cli", "--db", dbPath}, true); err != nil {
		t.Fatalf("workflow show failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode workflow show json: %v\n%s", err, out.String())
	}
	run := payload["run"].(map[string]any)
	if run["status"] != "completed" {
		t.Fatalf("expected completed status, got %#v", run["status"])
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"read", "wf-cli", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow read failed: %v", err)
	}
	if !strings.Contains(out.String(), "scan repo") {
		t.Fatalf("expected result output, got %s", out.String())
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"save", "wf-cli", "--db", dbPath, "--name", "saved-test"}, false); err != nil {
		t.Fatalf("workflow save failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".pallium", "workflows", "saved-test.js")); err != nil {
		t.Fatalf("expected saved workflow: %v", err)
	}
}

func TestWorkflowHelpIsRouted(t *testing.T) {
	var out bytes.Buffer
	app := NewApp(&out, &out)
	if err := app.Run([]string{"workflow", "--help"}); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium workflow") {
		t.Fatalf("expected workflow help, got %q", out.String())
	}
}

func TestWorkflowStopMarksForegroundRunStopped(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"status":"ok"}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("one"); return "done";`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-stop", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "stop test"}, false); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"stop", "wf-stop", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow stop failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"show", "wf-stop", "--db", dbPath}, true); err != nil {
		t.Fatalf("workflow show failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	run := payload["run"].(map[string]any)
	if run["status"] != "stopped" {
		t.Fatalf("expected stopped status, got %#v", run["status"])
	}
}

func TestWorkflowEditAgentAutoAppliesPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"status":"edited"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_FILE", "note.txt")
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_CONTENT", "changed by workflow\n")
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "note.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "note.txt")
	runGit(t, tmp, "commit", "-m", "initial")

	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("edit");
const result = agent("edit note", { label: "editor", mode: "edit", isolation: "worktree" });
return result;`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-edit",
		"--db", dbPath,
		"--cwd", tmp,
		"--script", scriptPath,
		"edit note",
	}, false)
	if err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	if got := readFile(t, filepath.Join(tmp, "note.txt")); got != "changed by workflow\n" {
		t.Fatalf("expected workflow run to auto-apply patch, got %q", got)
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"apply", "wf-edit", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow apply should be idempotent after auto-apply: %v", err)
	}
	if !strings.Contains(out.String(), "No workflow patches to apply.") {
		t.Fatalf("expected idempotent apply message, got %s", out.String())
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
