package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tszaks/pallium/internal/workflow"
)

func TestWorkflowRunShowReadAndSave(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"status":"ok","prompt":"{{PROMPT}}"}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
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

func TestWorkflowRunSavedWorkflowByName(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"status":"ok","prompt":"{{PROMPT}}"}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	workflowDir := filepath.Join(tmp, ".pallium", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "saved-review.js"), []byte(`phase("scan");
const result = await agent("saved workflow " + args.topic, { label: "saved" });
return result;`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-saved-name",
		"--db", dbPath,
		"--cwd", tmp,
		"--workflow", "saved-review",
		"--args", `{"topic":"auth"}`,
		"saved task",
	}, false); err != nil {
		t.Fatalf("workflow run by name failed: %v", err)
	}
	if !strings.Contains(out.String(), "saved workflow auth") {
		t.Fatalf("expected saved workflow output, got %s", out.String())
	}

	out.Reset()
	if err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-saved-slash",
		"--db", dbPath,
		"--cwd", tmp,
		"--args", `{"topic":"billing"}`,
		"/saved-review",
		"slash task",
	}, false); err != nil {
		t.Fatalf("workflow run by slash name failed: %v", err)
	}
	if !strings.Contains(out.String(), "saved workflow billing") {
		t.Fatalf("expected slash workflow output, got %s", out.String())
	}
}

func TestWorkflowGenerateStatusAndInspect(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"command":"go test ./...","summary":"passed","output_tail":"","failures":[]}`)
	t.Setenv("PALLIUM_WORKFLOW_PALLIUM_STUB", `{"ok":true,"args":"{{ARGS}}"}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	generatedPath := filepath.Join(tmp, "generated.js")

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{
		"generate",
		"--style", "test-fix",
		"--test-command", "go test ./...",
		"--max-rounds", "2",
		"--output", generatedPath,
		"fix tests until green",
	}, false); err != nil {
		t.Fatalf("workflow generate failed: %v", err)
	}
	raw, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("expected generated workflow: %v", err)
	}
	if !strings.Contains(string(raw), "verify.untilGreen") || !strings.Contains(string(raw), "maxRounds") {
		t.Fatalf("generated workflow missing test-fix loop:\n%s", string(raw))
	}

	out.Reset()
	if err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-generated",
		"--db", dbPath,
		"--cwd", tmp,
		"--script", generatedPath,
		"generated run",
	}, false); err != nil {
		t.Fatalf("generated workflow run failed: %v", err)
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"status", "wf-generated", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow status failed: %v", err)
	}
	if !strings.Contains(out.String(), "Workflow wf-generated: completed") || !strings.Contains(out.String(), "Agents:") {
		t.Fatalf("unexpected status output: %s", out.String())
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"inspect", "wf-generated", "--db", dbPath}, true); err != nil {
		t.Fatalf("workflow inspect failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode inspect json: %v\n%s", err, out.String())
	}
	if payload["script_path"] == "" || payload["status"] == nil {
		t.Fatalf("inspect payload missing fields: %#v", payload)
	}
}

func TestWorkflowGenerateSaveByName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	t.Chdir(tmp)
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{
		"generate",
		"--style", "review",
		"--save", "review-generated",
		"review this branch",
	}, false); err != nil {
		t.Fatalf("workflow generate save failed: %v", err)
	}
	path := filepath.Join(".pallium", "workflows", "review-generated.js")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected saved generated workflow at %s: %v", path, err)
	}
}

func TestWorkflowGenerateLLMValidatesScript(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_GENERATE_STUB", "phase(\"llm\");\nreturn { ok: true };")
	tmp := t.TempDir()
	outputPath := filepath.Join(tmp, "llm.workflow.js")
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"generate", "--llm", "--output", outputPath, "write custom workflow"}, true); err != nil {
		t.Fatalf("llm generate failed: %v", err)
	}
	raw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `phase("llm")`) {
		t.Fatalf("expected stubbed llm script, got %s", string(raw))
	}
}

func TestWorkflowGenerateLLMReturnsStyleError(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_GENERATE_STUB", "phase(\"llm\");\nreturn { ok: true };")
	var out bytes.Buffer
	err := runWorkflow(&out, []string{"generate", "--llm", "--style", "bogus", "write custom workflow"}, true)
	if err == nil {
		t.Fatal("expected llm generate to reject bogus style")
	}
	if !strings.Contains(err.Error(), `unknown workflow style "bogus"`) {
		t.Fatalf("expected unknown style error, got %v", err)
	}
}

// TestWorkflowGenerateLLMIsProviderAgnostic proves `workflow generate --llm`
// dispatches through the same provider resolution as a live agent call
// instead of hardcoding codex: forcing PALLIUM_WORKFLOW_PROVIDER at a
// configured wrapper makes generation return the wrapper's output, with no
// codex binary involved at all.
func TestWorkflowGenerateLLMIsProviderAgnostic(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_GENERATE_STUB", "")
	tmp := t.TempDir()
	wrapperScript := filepath.Join(tmp, "fake-wrapper.sh")
	if err := os.WriteFile(wrapperScript, []byte("#!/bin/sh\nprintf 'phase(\"llm-wrapper\");\\nreturn { ok: true };' > \"$PALLIUM_WORKFLOW_OUTPUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "grok")
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_GROK_COMMAND", wrapperScript)

	outputPath := filepath.Join(tmp, "llm.workflow.js")
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"generate", "--llm", "--output", outputPath, "write custom workflow"}, true); err != nil {
		t.Fatalf("llm generate failed: %v", err)
	}
	raw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `phase("llm-wrapper")`) {
		t.Fatalf("expected wrapper-generated script, got %s", string(raw))
	}
}

func TestWorkflowValidateScript(t *testing.T) {
	tmp := t.TempDir()
	validPath := filepath.Join(tmp, "valid.js")
	if err := os.WriteFile(validPath, []byte(`phase("scan"); return { ok: true };`), 0o644); err != nil {
		t.Fatal(err)
	}
	invalidPath := filepath.Join(tmp, "invalid.js")
	if err := os.WriteFile(invalidPath, []byte(`phase("scan"; return { ok: true };`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"validate", validPath}, false); err != nil {
		t.Fatalf("workflow validate failed: %v", err)
	}
	if !strings.Contains(out.String(), "Workflow script valid") {
		t.Fatalf("unexpected valid output: %s", out.String())
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"validate", invalidPath}, true); err != nil {
		t.Fatalf("workflow validate json should not error on invalid script: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode validation json: %v\n%s", err, out.String())
	}
	if payload["valid"] != false || payload["error"] == "" {
		t.Fatalf("expected invalid validation payload, got %#v", payload)
	}
}

func TestWorkflowPreflightCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "main.go")
	runGit(t, tmp, "commit", "-m", "initial")

	var out bytes.Buffer
	if err := runIndex(&out, []string{tmp}, false); err != nil {
		t.Fatalf("index failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"preflight", "tighten workflow", "--cwd", tmp, "--scope", "main.go"}, true); err != nil {
		t.Fatalf("workflow preflight failed: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode preflight: %v\n%s", err, out.String())
	}
	if report["task"] != "tighten workflow" {
		t.Fatalf("expected task in preflight, got %#v", report)
	}
	scope := report["scope_paths"].([]any)
	if len(scope) == 0 || scope[0] != "main.go" {
		t.Fatalf("expected main.go scope, got %#v", report)
	}
	if len(report["agent_instructions"].([]any)) == 0 {
		t.Fatalf("expected agent instructions, got %#v", report)
	}
}

func TestWorkflowTriggerAddShowRun(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_SEQUENCE", `[
		"{\"summary\":\"triggered\",\"steps\":[],\"risks\":[]}",
		"{\"verdict\":\"pass\",\"notes\":[]}"
	]`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"fallback","steps":[],"risks":[]}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	dbPath := filepath.Join(tmp, "sessions.sqlite")

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"trigger", "add", "daily-review", "review repo", "--db", dbPath, "--cwd", tmp}, true); err != nil {
		t.Fatalf("trigger add failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "run", "daily-review", "--id", "wf-triggered", "--db", dbPath}, true); err != nil {
		t.Fatalf("trigger run failed: %v", err)
	}
	var status map[string]any
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatalf("decode trigger run status: %v\n%s", err, out.String())
	}
	run := status["run"].(map[string]any)
	if run["id"] != "wf-triggered" || run["status"] != "completed" {
		t.Fatalf("expected completed triggered run, got %#v", status)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "show", "daily-review", "--db", dbPath}, true); err != nil {
		t.Fatalf("trigger show failed: %v", err)
	}
	var trigger map[string]any
	if err := json.Unmarshal(out.Bytes(), &trigger); err != nil {
		t.Fatalf("decode trigger: %v\n%s", err, out.String())
	}
	if trigger["last_run_id"] != "wf-triggered" {
		t.Fatalf("expected trigger last_run_id, got %#v", trigger)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"fleet", "status", "--db", dbPath}, true); err != nil {
		t.Fatalf("fleet status failed: %v", err)
	}
	var fleet map[string]any
	if err := json.Unmarshal(out.Bytes(), &fleet); err != nil {
		t.Fatalf("decode fleet: %v\n%s", err, out.String())
	}
	if fleet["runs_total"].(float64) == 0 || fleet["triggers_total"].(float64) != 1 {
		t.Fatalf("expected fleet run and trigger counts, got %#v", fleet)
	}
}

func TestWorkflowTriggerOnChangedSkipsUnchangedRepo(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_SEQUENCE", `[
		"{\"summary\":\"triggered\",\"steps\":[],\"risks\":[]}",
		"{\"verdict\":\"pass\",\"notes\":[]}"
	]`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"fallback","steps":[],"risks":[]}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"trigger", "add", "changed-review", "review changes", "--kind", "on-changed", "--db", dbPath, "--cwd", tmp}, true); err != nil {
		t.Fatalf("trigger add failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "run", "changed-review", "--id", "wf-changed-1", "--db", dbPath}, true); err != nil {
		t.Fatalf("first trigger run failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "run", "changed-review", "--db", dbPath}, true); err != nil {
		t.Fatalf("unchanged trigger run failed: %v", err)
	}
	var skipped map[string]any
	if err := json.Unmarshal(out.Bytes(), &skipped); err != nil {
		t.Fatalf("decode skipped trigger: %v\n%s", err, out.String())
	}
	if skipped["skipped"] != true || skipped["reason"] != "unchanged" {
		t.Fatalf("expected unchanged skip, got %#v", skipped)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "watch", "--once", "--db", dbPath}, true); err != nil {
		t.Fatalf("trigger watch failed: %v", err)
	}
	var watched map[string]any
	if err := json.Unmarshal(out.Bytes(), &watched); err != nil {
		t.Fatalf("decode trigger watch: %v\n%s", err, out.String())
	}
	if watched["checked"].(float64) != 1 || watched["skipped"].(float64) != 1 {
		t.Fatalf("expected watch to check and skip unchanged trigger, got %#v", watched)
	}
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "run", "changed-review", "--id", "wf-changed-2", "--db", dbPath}, true); err != nil {
		t.Fatalf("changed trigger run failed: %v", err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode changed trigger run: %v\n%s", err, out.String())
	}
	run := snapshot["run"].(map[string]any)
	if run["id"] != "wf-changed-2" {
		t.Fatalf("expected second run after repo changed, got %#v", snapshot)
	}
}

func TestWorkflowTriggerOnChangedFailedRunKeepsFingerprint(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"status":"ok","prompt":"{{PROMPT}}"}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(t.TempDir(), "trigger-workflow.js")

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"trigger", "add", "broken-review", "review changes", "--kind", "on-changed", "--script", scriptPath, "--db", dbPath, "--cwd", tmp}, true); err != nil {
		t.Fatalf("trigger add failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "run", "broken-review", "--id", "wf-broken-1", "--db", dbPath}, true); err == nil {
		t.Fatalf("expected trigger run with missing script to fail, got %s", out.String())
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "show", "broken-review", "--db", dbPath}, true); err != nil {
		t.Fatalf("trigger show failed: %v", err)
	}
	var trigger map[string]any
	if err := json.Unmarshal(out.Bytes(), &trigger); err != nil {
		t.Fatalf("decode trigger: %v\n%s", err, out.String())
	}
	if fingerprint := trigger["last_fingerprint"]; fingerprint != nil && fingerprint != "" {
		t.Fatalf("expected fingerprint unset after failed run, got %#v", trigger)
	}
	if err := os.WriteFile(scriptPath, []byte(`phase("scan");
const result = agent("scan repo", { label: "scanner" });
return { result };`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "run", "broken-review", "--id", "wf-broken-2", "--db", dbPath}, true); err != nil {
		t.Fatalf("retry trigger run failed: %v", err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode retry run: %v\n%s", err, out.String())
	}
	if snapshot["skipped"] == true {
		t.Fatalf("expected retry to fire instead of skipping, got %#v", snapshot)
	}
	run := snapshot["run"].(map[string]any)
	if run["id"] != "wf-broken-2" {
		t.Fatalf("expected retry run after failed launch, got %#v", snapshot)
	}
}

func TestWorkflowTriggerMidRunFailureConsumesFingerprint(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"status":"ok","prompt":"{{PROMPT}}"}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(t.TempDir(), "trigger-workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`throw new Error("mid-run failure");`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"trigger", "add", "midrun-review", "review changes", "--kind", "on-changed", "--script", scriptPath, "--db", dbPath, "--cwd", tmp}, true); err != nil {
		t.Fatalf("trigger add failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "run", "midrun-review", "--id", "wf-midrun-1", "--db", dbPath}, true); err == nil {
		t.Fatalf("expected trigger run with throwing script to fail, got %s", out.String())
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "run", "midrun-review", "--id", "wf-midrun-2", "--db", dbPath}, true); err != nil {
		t.Fatalf("second trigger run failed: %v\n%s", err, out.String())
	}
	var snapshot map[string]any
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode second run: %v\n%s", err, out.String())
	}
	if snapshot["skipped"] != true {
		t.Fatalf("expected second cycle to skip as unchanged after mid-run failure, got %#v", snapshot)
	}
}

func TestWorkflowHelpTopicShowsHelp(t *testing.T) {
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"help", "generate"}, false); err != nil {
		t.Fatalf("workflow help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium workflow") {
		t.Fatalf("expected workflow help output, got %q", out.String())
	}
}

func TestWorkflowHTTPAPI(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_SEQUENCE", `[
		"{\"summary\":\"api\",\"steps\":[],\"risks\":[]}",
		"{\"verdict\":\"pass\",\"notes\":[]}"
	]`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"fallback","steps":[],"risks":[]}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	handler := newWorkflowHTTPHandler(dbPath, "")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health failed: %d %s", rec.Code, rec.Body.String())
	}

	rawBody, err := json.Marshal(map[string]string{"id": "wf-api", "task": "api workflow", "cwd": tmp})
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewReader(rawBody)
	req = httptest.NewRequest(http.MethodPost, "/workflows/run", body)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run failed: %d %s", rec.Code, rec.Body.String())
	}
	var snapshot map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode run response: %v\n%s", err, rec.Body.String())
	}
	run := snapshot["run"].(map[string]any)
	if run["id"] != "wf-api" || run["status"] != "completed" {
		t.Fatalf("expected completed api run, got %#v", snapshot)
	}

	req = httptest.NewRequest(http.MethodGet, "/workflows/runs/wf-api", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot failed: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/workflows/fleet", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet failed: %d %s", rec.Code, rec.Body.String())
	}
	var fleet map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fleet); err != nil {
		t.Fatalf("decode fleet response: %v\n%s", err, rec.Body.String())
	}
	if fleet["runs_total"].(float64) == 0 {
		t.Fatalf("expected fleet to include api run, got %#v", fleet)
	}
	req = httptest.NewRequest(http.MethodGet, "/workflows/analytics", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("analytics failed: %d %s", rec.Code, rec.Body.String())
	}
	var analytics map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &analytics); err != nil {
		t.Fatalf("decode analytics response: %v\n%s", err, rec.Body.String())
	}
	if analytics["runs_total"].(float64) == 0 {
		t.Fatalf("expected analytics to include api run, got %#v", analytics)
	}
	req = httptest.NewRequest(http.MethodGet, "/workflows/library", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "security-audit") {
		t.Fatalf("library failed: %d %s", rec.Code, rec.Body.String())
	}
	rawBody, err = json.Marshal(map[string]any{"pack": "security-audit", "cwd": tmp, "name": "api-security"})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/workflows/library/install", bytes.NewReader(rawBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("library install failed: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(tmp, ".pallium", "workflows", "api-security.js")); err != nil {
		t.Fatalf("expected api-installed workflow: %v", err)
	}
}

// TestWorkflowRunRequestArgsAllowNetwork covers workflowRunRequestArgs in
// isolation: allow_network:true in the HTTP request body must translate to
// --allow-network on the underlying `workflow run` args, and omitting it must
// not add the flag (safe default).
func TestWorkflowRunRequestArgsAllowNetwork(t *testing.T) {
	args, err := workflowRunRequestArgs("db.sqlite", workflowRunRequest{Task: "t", AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArg(args, "--allow-network") {
		t.Fatalf("expected --allow-network in args, got %v", args)
	}

	args, err = workflowRunRequestArgs("db.sqlite", workflowRunRequest{Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if containsArg(args, "--allow-network") {
		t.Fatalf("expected no --allow-network by default, got %v", args)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestWorkflowHTTPRunAllowNetwork verifies the POST /workflows/run HTTP path
// exposes the operator's --allow-network ceiling: previously only the CLI
// run/resume/trigger parsers could set it, leaving this entry point unable to
// grant network access at all. allow_network:true in the JSON body must
// thread through to the same --allow-network flag the CLI path uses, and
// omitting it must keep the safe default (no egress).
func TestWorkflowHTTPRunAllowNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	// A granted-network agent is forced into an isolated worktree, so the run
	// cwd must be a git repo.
	initGitRepo(t, tmp)
	setupNetworkProvider(t, tmp)
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := newWorkflowHTTPHandler(dbPath, "")

	rawBody, err := json.Marshal(map[string]any{
		"id": "wf-http-net", "task": "net test", "cwd": tmp, "script_path": scriptPath, "allow_network": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/workflows/run", bytes.NewReader(rawBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run with allow_network failed: %d %s", rec.Code, rec.Body.String())
	}
	assertNetworkRun(t, dbPath, "wf-http-net", true, "1")

	rawBody, err = json.Marshal(map[string]any{
		"id": "wf-http-net-off", "task": "net test", "cwd": tmp, "script_path": scriptPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/workflows/run", bytes.NewReader(rawBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run without allow_network failed: %d %s", rec.Code, rec.Body.String())
	}
	assertNetworkRun(t, dbPath, "wf-http-net-off", false, "0")
}

func TestWorkflowHTTPAPIRequiresTokenWhenConfigured(t *testing.T) {
	tmp := t.TempDir()
	handler := newWorkflowHTTPHandler(filepath.Join(tmp, "sessions.sqlite"), "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health should not require token, got %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/workflows/fleet", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized without token, got %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/workflows/fleet", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected authorized request, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowServeRejectsWhitespaceTokenForNonLocalBind(t *testing.T) {
	var out bytes.Buffer
	err := runWorkflowServe(&out, []string{"--addr", "0.0.0.0:0", "--token", "   ", "--db", filepath.Join(t.TempDir(), "sessions.sqlite")}, false)
	if err == nil || !strings.Contains(err.Error(), "requires --token") {
		t.Fatalf("expected whitespace-only token to be rejected, got %v", err)
	}
}

func TestWorkflowResumeUsesStoredCaps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first"}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	firstScript := `phase("one");
return agent("first", { label: "first" });`
	if err := os.WriteFile(scriptPath, []byte(firstScript), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-resume-stored-cap",
		"--db", dbPath,
		"--cwd", tmp,
		"--script", scriptPath,
		"--max-agents", "1",
		"stored cap",
	}, false); err != nil {
		t.Fatalf("initial workflow run failed: %v", err)
	}
	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Run("wf-resume-stored-cap")
	if err != nil {
		t.Fatal(err)
	}
	if run.MaxAgents != 1 {
		t.Fatalf("expected stored max agents, got %+v", run)
	}
	secondScript := `phase("one");
agent("first", { label: "first" });
return agent("second", { label: "second" });`
	if err := os.WriteFile(run.ScriptPath, []byte(secondScript), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second"}`)
	out.Reset()
	err = runWorkflow(&out, []string{"resume", "wf-resume-stored-cap", "--db", dbPath}, false)
	if err == nil || !strings.Contains(err.Error(), "workflow exceeded max agents: 1") {
		t.Fatalf("expected resume to preserve stored max-agent cap, got %v", err)
	}
}

func TestWorkflowResumePersistsAgentTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "1500")
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("one");
return agent("slow", { label: "slow" });`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-resume-timeout",
		"--db", dbPath,
		"--cwd", tmp,
		"--script", scriptPath,
		"--agent-timeout", "1",
		"timeout test",
	}, false)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("expected initial run to hit the 1s agent timeout, got %v", err)
	}
	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Run("wf-resume-timeout")
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if run.AgentTimeout != 1 {
		t.Fatalf("expected agent timeout stored on the run, got %+v", run)
	}
	// Resume without the flag must reuse the stored 1s timeout, not the 600s
	// flag default.
	out.Reset()
	err = runWorkflow(&out, []string{"resume", "wf-resume-timeout", "--db", dbPath}, false)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("expected resume to reuse the stored agent timeout, got %v", err)
	}
	// An explicit flag on resume overrides the stored value.
	out.Reset()
	if err := runWorkflow(&out, []string{"resume", "wf-resume-timeout", "--db", dbPath, "--agent-timeout", "30"}, false); err != nil {
		t.Fatalf("expected explicit --agent-timeout to override stored value, got %v", err)
	}
}

// TestWorkflowResumePreservesDisabledAgentTimeout guards against a
// regression where 0 (the documented value for "no agent timeout") could
// not survive a plain resume: a naive `stored value > 0` check cannot tell
// "never configured" apart from "explicitly disabled", so resume without
// --agent-timeout silently fell back to the nested run's 600s flag default
// instead of keeping timeouts off.
func TestWorkflowResumePreservesDisabledAgentTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("one");
return agent("fast", { label: "fast" });`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-resume-timeout-disabled",
		"--db", dbPath,
		"--cwd", tmp,
		"--script", scriptPath,
		"--agent-timeout", "0",
		"timeout disabled test",
	}, false); err != nil {
		t.Fatalf("expected initial run with --agent-timeout 0 to succeed, got %v", err)
	}
	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Run("wf-resume-timeout-disabled")
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if run.AgentTimeout != 0 || !run.AgentTimeoutExplicit {
		t.Fatalf("expected disabled timeout stored explicitly on the run, got %+v", run)
	}
	// Resume without the flag must keep the timeout disabled, not fall back
	// to the 600s flag default.
	out.Reset()
	if err := runWorkflow(&out, []string{"resume", "wf-resume-timeout-disabled", "--db", dbPath}, false); err != nil {
		t.Fatalf("expected resume to succeed with timeouts still disabled, got %v", err)
	}
	store, err = workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.Run("wf-resume-timeout-disabled")
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if run.AgentTimeout != 0 || !run.AgentTimeoutExplicit {
		t.Fatalf("expected resume to keep the disabled timeout explicit, got %+v", run)
	}
}

func TestWorkflowReportSummarizesAgentOutputs(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"reviewed auth","observations":["auth flow is covered"],"risks":["missing edge test"],"next_steps":["add edge test"]}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("review");
const result = await agent("review auth", { label: "auth-review" });
return result;`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-report", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "report test"}, false); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"report", "wf-report", "--db", dbPath}, true); err != nil {
		t.Fatalf("workflow report failed: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report json: %v\n%s", err, out.String())
	}
	if report["status"] != "completed" {
		t.Fatalf("expected completed report, got %#v", report)
	}
	findings := report["findings"].([]any)
	risks := report["risks"].([]any)
	nextSteps := report["next_steps"].([]any)
	if findings[0] != "auth flow is covered" || risks[0] != "missing edge test" || nextSteps[0] != "add edge test" {
		t.Fatalf("unexpected report extraction: %#v", report)
	}
}

func TestWorkflowToolsAndTemplateCatalog(t *testing.T) {
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"tools", "list", "--kind", "verification"}, true); err != nil {
		t.Fatalf("workflow tools list failed: %v", err)
	}
	var tools []map[string]any
	if err := json.Unmarshal(out.Bytes(), &tools); err != nil {
		t.Fatalf("decode tools json: %v\n%s", err, out.String())
	}
	if len(tools) != 2 || tools[0]["name"] != "check" || tools[1]["name"] != "verify.untilGreen" {
		t.Fatalf("expected verification catalog to contain check and verify.untilGreen, got %#v", tools)
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"template", "list"}, false); err != nil {
		t.Fatalf("workflow template list failed: %v", err)
	}
	if !strings.Contains(out.String(), "test-fix") || !strings.Contains(out.String(), "Requires --test-command") {
		t.Fatalf("unexpected template list output: %s", out.String())
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"template", "show", "fix-until-green"}, true); err != nil {
		t.Fatalf("workflow template show alias failed: %v", err)
	}
	var tmpl map[string]any
	if err := json.Unmarshal(out.Bytes(), &tmpl); err != nil {
		t.Fatalf("decode template json: %v\n%s", err, out.String())
	}
	if tmpl["name"] != "test-fix" || tmpl["requires_test_command"] != true {
		t.Fatalf("unexpected template payload: %#v", tmpl)
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

func TestWorkflowPauseAndResumeReusesCompletedAgents(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first","prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return result;`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-pause-resume", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "pause resume test"}, false); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"pause", "wf-pause-resume", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow pause failed: %v", err)
	}
	if !strings.Contains(out.String(), "paused") {
		t.Fatalf("expected pause output, got %s", out.String())
	}

	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second","prompt":"{{PROMPT}}"}`)
	out.Reset()
	if err := runWorkflow(&out, []string{"resume", "wf-pause-resume", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow resume failed: %v", err)
	}
	if !strings.Contains(out.String(), `"source": "first"`) {
		t.Fatalf("expected resume to reuse completed agent output, got %s", out.String())
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"status", "wf-pause-resume", "--db", dbPath}, true); err != nil {
		t.Fatalf("workflow status failed: %v", err)
	}
	var status map[string]any
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatalf("decode status json: %v\n%s", err, out.String())
	}
	if status["status"] != "completed" {
		t.Fatalf("expected completed status after resume, got %#v", status)
	}
	if got := int(status["agents_completed"].(float64)); got != 1 {
		t.Fatalf("expected one completed agent after cached resume, got %#v", status)
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
	out.Reset()
	if err := runWorkflow(&out, []string{"revert", "wf-edit", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow revert failed: %v", err)
	}
	if got := readFile(t, filepath.Join(tmp, "note.txt")); got != "original\n" {
		t.Fatalf("expected workflow revert to restore original file, got %q", got)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"revert", "wf-edit", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow revert should be idempotent: %v", err)
	}
	if !strings.Contains(out.String(), "No workflow patches to revert.") {
		t.Fatalf("expected idempotent revert message, got %s", out.String())
	}
}

func TestWorkflowAgentGateApprovesDuringRun(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"approved":true,"reason":"ok"}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("gate");
const verdict = gate("approve", "verify before worker");
return { verdict };`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-gate-cli", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "gate"}, true); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode gate snapshot: %v\n%s", err, out.String())
	}
	run := snapshot["run"].(map[string]any)
	if run["status"] != "completed" {
		t.Fatalf("expected completed run after agent gate approval, got %#v", snapshot)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"gate", "list", "wf-gate-cli", "--db", dbPath}, true); err != nil {
		t.Fatalf("gate list failed: %v", err)
	}
	var gates []map[string]any
	if err := json.Unmarshal(out.Bytes(), &gates); err != nil {
		t.Fatalf("decode gates: %v\n%s", err, out.String())
	}
	if len(gates) != 1 || gates[0]["status"] != "approved" {
		t.Fatalf("expected approved gate, got %#v", gates)
	}
}

func TestWorkflowLibraryInstallAndAnalytics(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	dbPath := filepath.Join(tmp, "sessions.sqlite")

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"library", "list"}, true); err != nil {
		t.Fatalf("library list failed: %v", err)
	}
	if !strings.Contains(out.String(), `"name": "security-audit"`) {
		t.Fatalf("expected security-audit pack, got %s", out.String())
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"library", "install", "test-gap-finder", "--cwd", tmp}, true); err != nil {
		t.Fatalf("library install failed: %v", err)
	}
	installedPath := filepath.Join(tmp, ".pallium", "workflows", "test-gap-finder.js")
	if _, err := os.Stat(installedPath); err != nil {
		t.Fatalf("expected installed workflow: %v", err)
	}

	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("inspect");
await agent("inspect", { label: "inspect", mode: "read-only", provider: "codex" });
return { ok: true };`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"run", "--id", "wf-analytics", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "analytics"}, true); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"trigger", "add", "analytics-trigger", "analytics task", "--db", dbPath, "--cwd", tmp}, true); err != nil {
		t.Fatalf("trigger add failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"analytics", "--db", dbPath}, true); err != nil {
		t.Fatalf("analytics failed: %v", err)
	}
	var analytics map[string]any
	if err := json.Unmarshal(out.Bytes(), &analytics); err != nil {
		t.Fatalf("decode analytics: %v\n%s", err, out.String())
	}
	if analytics["runs_total"].(float64) != 1 {
		t.Fatalf("expected one run, got %#v", analytics)
	}
	providers := analytics["agents_by_provider"].(map[string]any)
	if providers["codex"].(float64) != 1 {
		t.Fatalf("expected codex provider count, got %#v", analytics)
	}
	if analytics["triggers_total"].(float64) != 1 {
		t.Fatalf("expected one trigger, got %#v", analytics)
	}
}

func TestWorkflowAuditListsRequirements(t *testing.T) {
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"audit"}, true); err != nil {
		t.Fatalf("workflow audit failed: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode audit json: %v\n%s", err, out.String())
	}
	if result["complete"] != true {
		t.Fatalf("expected complete audit, got %#v", result)
	}
	versions := result["versions"].([]any)
	if len(versions) != 7 {
		t.Fatalf("expected seven versions, got %#v", versions)
	}
}

func TestWorkflowFleetLimitBlocksSecondActiveRun(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "500")
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("scan");
await agent("slow", { label: "slow", mode: "read-only" });
return { ok: true };`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(workflow.Run{ID: "wf-active-blocker", Task: "block", CWD: tmp, ScriptPath: scriptPath, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	var out bytes.Buffer
	err = runWorkflow(&out, []string{"run", "--id", "wf-fleet-limit", "--db", dbPath, "--cwd", tmp, "--max-active-runs", "1", "--script", scriptPath, "fleet limit"}, false)
	if err == nil {
		t.Fatalf("expected fleet limit error, got output: %s", out.String())
	}
	if !strings.Contains(err.Error(), "workflow fleet limit reached") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWorkflowReadAndInspectSurfaceFailuresAndScriptChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_TEST_COMMAND", `PALLIUM_WORKFLOW_PROMPT="$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")"; if printf '%s' "$PALLIUM_WORKFLOW_PROMPT" | grep -q bad; then echo "failed intentionally" >&2; exit 7; fi; printf '{"prompt":"%s"}' "$PALLIUM_WORKFLOW_PROMPT" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	script := `phase("parallel");
const results = await parallel(["good", "bad"], item =>
  agent("worker " + item, { label: item, provider: "test" })
);
return { results };`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-failures", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "failure surfacing"}, false); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"read", "wf-failures", "--db", dbPath}, true); err != nil {
		t.Fatalf("workflow read failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode read json: %v\n%s", err, out.String())
	}
	failures, ok := payload["failures"].([]any)
	if !ok || len(failures) != 1 {
		t.Fatalf("expected one failure in read payload, got %#v", payload)
	}
	failure := failures[0].(map[string]any)
	if failure["label"] != "bad" || !strings.Contains(failure["error"].(string), "failed intentionally") {
		t.Fatalf("unexpected failure entry: %#v", failure)
	}

	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Run("wf-failures")
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	changedScript := script + "\n// tail edit"
	if err := os.WriteFile(run.ScriptPath, []byte(changedScript), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"resume", "wf-failures", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow resume failed: %v", err)
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"inspect", "wf-failures", "--db", dbPath}, true); err != nil {
		t.Fatalf("workflow inspect failed: %v", err)
	}
	var inspection map[string]any
	if err := json.Unmarshal(out.Bytes(), &inspection); err != nil {
		t.Fatalf("decode inspect json: %v\n%s", err, out.String())
	}
	status := inspection["status"].(map[string]any)
	if status["script_changed"] != true {
		t.Fatalf("expected script_changed=true in inspect status, got %#v", status)
	}
	if failures, ok := inspection["failures"].([]any); !ok || len(failures) != 1 {
		t.Fatalf("expected inspect failures list, got %#v", inspection["failures"])
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"report", "wf-failures", "--db", dbPath}, true); err != nil {
		t.Fatalf("workflow report failed: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report json: %v\n%s", err, out.String())
	}
	if failures, ok := report["failures"].([]any); !ok || len(failures) != 1 {
		t.Fatalf("expected report failures list, got %#v", report["failures"])
	}
}

func TestWorkflowGCRemovesOldTerminalRunArtifacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	recent := time.Now().UTC().Format(time.RFC3339Nano)
	runs := []workflow.Run{
		{ID: "wf-gc-old-done", Status: "completed", CreatedAt: old, UpdatedAt: old, CompletedAt: old},
		{ID: "wf-gc-new-done", Status: "completed", CreatedAt: recent, UpdatedAt: recent, CompletedAt: recent},
		{ID: "wf-gc-old-running", Status: "running", CreatedAt: old, UpdatedAt: old},
		{ID: "wf-gc-old-failed", Status: "failed", CreatedAt: old, UpdatedAt: old, CompletedAt: old},
	}
	for _, run := range runs {
		run.Task = "gc test"
		run.CWD = tmp
		run.ScriptPath = filepath.Join(tmp, "workflow.js")
		if _, err := store.CreateRun(run); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(home, ".pallium", "workflow-runs", run.ID, "patches")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.patch"), []byte("data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"gc", "--db", dbPath, "--dry-run"}, true); err != nil {
		t.Fatalf("workflow gc --dry-run failed: %v", err)
	}
	var dry map[string]any
	if err := json.Unmarshal(out.Bytes(), &dry); err != nil {
		t.Fatalf("decode gc dry-run json: %v\n%s", err, out.String())
	}
	if dry["count"] != float64(1) || dry["dry_run"] != true || dry["bytes_freed"] != float64(5) {
		t.Fatalf("expected dry run to report one old terminal run, got %#v", dry)
	}
	for _, run := range runs {
		if _, err := os.Stat(filepath.Join(home, ".pallium", "workflow-runs", run.ID)); err != nil {
			t.Fatalf("dry run must not remove artifacts for %s: %v", run.ID, err)
		}
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"gc", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow gc failed: %v", err)
	}
	if !strings.Contains(out.String(), "removed 1 run directories (5 bytes freed)") || !strings.Contains(out.String(), "wf-gc-old-done") {
		t.Fatalf("unexpected gc output: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".pallium", "workflow-runs", "wf-gc-old-done")); !os.IsNotExist(err) {
		t.Fatalf("expected old terminal run artifacts removed, stat err=%v", err)
	}
	// Failed runs are resumable, so their artifacts stay unless
	// --include-failed opts in.
	for _, keep := range []string{"wf-gc-new-done", "wf-gc-old-running", "wf-gc-old-failed"} {
		if _, err := os.Stat(filepath.Join(home, ".pallium", "workflow-runs", keep)); err != nil {
			t.Fatalf("expected %s artifacts kept: %v", keep, err)
		}
	}

	out.Reset()
	if err := runWorkflow(&out, []string{"gc", "--db", dbPath, "--include-failed"}, false); err != nil {
		t.Fatalf("workflow gc --include-failed failed: %v", err)
	}
	if !strings.Contains(out.String(), "wf-gc-old-failed") {
		t.Fatalf("expected --include-failed to collect the old failed run: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".pallium", "workflow-runs", "wf-gc-old-failed")); !os.IsNotExist(err) {
		t.Fatalf("expected old failed run artifacts removed with --include-failed, stat err=%v", err)
	}
	for _, keep := range []string{"wf-gc-new-done", "wf-gc-old-running"} {
		if _, err := os.Stat(filepath.Join(home, ".pallium", "workflow-runs", keep)); err != nil {
			t.Fatalf("expected %s artifacts kept: %v", keep, err)
		}
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

// initGitRepo makes dir a committable git repo so worktree-forcing paths (e.g.
// a granted-network agent) can branch from it.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// networkProviderScript writes PALLIUM_WORKFLOW_NETWORK into its output so a
// test can assert exactly what env the configured provider saw.
const networkProviderScript = `#!/bin/sh
printf '{"ok":true,"network":"%s"}' "$PALLIUM_WORKFLOW_NETWORK" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
`

func setupNetworkProvider(t *testing.T, tmp string) {
	t.Helper()
	provider := filepath.Join(tmp, "net-provider.sh")
	if err := os.WriteFile(provider, []byte(networkProviderScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_NET_COMMAND", provider)
}

func assertNetworkRun(t *testing.T, dbPath, id string, wantAllow bool, wantNetworkVal string) {
	t.Helper()
	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.Run(id)
	if err != nil {
		t.Fatal(err)
	}
	if run.AllowNetwork != wantAllow {
		t.Fatalf("run %s AllowNetwork = %v, want %v", id, run.AllowNetwork, wantAllow)
	}
	agents, err := store.ListAgents(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	want := `"network":"` + wantNetworkVal + `"`
	if !strings.Contains(agents[0].Output, want) {
		t.Fatalf("expected provider to see %s, got agent output %q", want, agents[0].Output)
	}
}

// TestWorkflowRunAllowNetworkFlag verifies the operator ceiling wires end to
// end: --allow-network on `workflow run` lets a network: true agent through
// (provider sees PALLIUM_WORKFLOW_NETWORK=1), the ceiling is stored on the
// run, and a plain resume preserves it (persists like AgentTimeout).
func TestWorkflowRunAllowNetworkFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	// A granted-network agent is forced into an isolated worktree (FIX 2), so
	// the run cwd must be a git repo.
	initGitRepo(t, tmp)
	setupNetworkProvider(t, tmp)
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{
		"run", "--id", "wf-net-run", "--db", dbPath, "--cwd", tmp,
		"--script", scriptPath, "--allow-network", "net test",
	}, false); err != nil {
		t.Fatalf("run with --allow-network failed: %v", err)
	}
	assertNetworkRun(t, dbPath, "wf-net-run", true, "1")

	// Resume without the flag must keep the stored ceiling, not silently drop
	// it back to sandboxed.
	out.Reset()
	if err := runWorkflow(&out, []string{"resume", "wf-net-run", "--db", dbPath}, false); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	assertNetworkRun(t, dbPath, "wf-net-run", true, "1")
}

// TestWorkflowRunNetworkDefaultOff guards the safe default: without
// --allow-network, a network: true agent still runs sandboxed
// (PALLIUM_WORKFLOW_NETWORK=0) and the run records no ceiling.
func TestWorkflowRunNetworkDefaultOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	setupNetworkProvider(t, tmp)
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{
		"run", "--id", "wf-net-off", "--db", dbPath, "--cwd", tmp,
		"--script", scriptPath, "net test",
	}, false); err != nil {
		t.Fatalf("run without --allow-network failed: %v", err)
	}
	assertNetworkRun(t, dbPath, "wf-net-off", false, "0")
}

// TestWorkflowRunAllowNetworkNotLatchedOnReuse covers FIX 4: reusing a run-id
// that once had --allow-network must NOT keep egress on when the new `run`
// invocation omits the flag. A fresh run reflects only this invocation's flag.
func TestWorkflowRunAllowNetworkNotLatchedOnReuse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	// A granted-network agent is forced into an isolated worktree (FIX 2), so
	// the run cwd must be a git repo.
	initGitRepo(t, tmp)
	setupNetworkProvider(t, tmp)
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// First run grants the ceiling and completes.
	if err := runWorkflow(&out, []string{
		"run", "--id", "wf-net-reuse", "--db", dbPath, "--cwd", tmp,
		"--script", scriptPath, "--allow-network", "net test",
	}, false); err != nil {
		t.Fatalf("run with --allow-network failed: %v", err)
	}
	assertNetworkRun(t, dbPath, "wf-net-reuse", true, "1")

	// Reusing the same id WITHOUT --allow-network must turn the stored ceiling
	// back off (no OR-fold latch). Assert the stored AllowNetwork directly:
	// per-agent output is cached from the first run and is not the ceiling.
	out.Reset()
	if err := runWorkflow(&out, []string{
		"run", "--id", "wf-net-reuse", "--db", dbPath, "--cwd", tmp,
		"--script", scriptPath, "net test",
	}, false); err != nil {
		t.Fatalf("reuse run without --allow-network failed: %v", err)
	}
	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.Run("wf-net-reuse")
	if err != nil {
		t.Fatal(err)
	}
	if run.AllowNetwork {
		t.Fatalf("reused run kept AllowNetwork=true; expected false when the new run omitted --allow-network")
	}
}

// TestWorkflowTriggerRunAllowNetwork covers threading the operator ceiling
// through the trigger path: `trigger run --allow-network` forwards the flag to
// the launched run (so a triggered workflow can use the network path), and
// omitting it keeps the safe default of no egress.
func TestWorkflowTriggerRunAllowNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	// A granted-network agent is forced into an isolated worktree, so the run
	// cwd must be a git repo.
	initGitRepo(t, tmp)
	setupNetworkProvider(t, tmp)
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runWorkflow(&out, []string{
		"trigger", "add", "nettrig", "net task",
		"--script", scriptPath, "--cwd", tmp, "--db", dbPath,
	}, false); err != nil {
		t.Fatalf("trigger add failed: %v", err)
	}

	// With --allow-network the launched run carries the ceiling and the agent
	// runs networked.
	out.Reset()
	if err := runWorkflow(&out, []string{
		"trigger", "run", "nettrig", "--id", "wf-trig-net", "--db", dbPath, "--allow-network",
	}, false); err != nil {
		t.Fatalf("trigger run --allow-network failed: %v", err)
	}
	assertNetworkRun(t, dbPath, "wf-trig-net", true, "1")

	// Without the flag the triggered run stays sandboxed (safe default).
	out.Reset()
	if err := runWorkflow(&out, []string{
		"trigger", "run", "nettrig", "--id", "wf-trig-off", "--db", dbPath,
	}, false); err != nil {
		t.Fatalf("trigger run without --allow-network failed: %v", err)
	}
	assertNetworkRun(t, dbPath, "wf-trig-off", false, "0")
}

// TestRenderWorkflowResultSurfacesDroppedItems makes sure the immediate
// `workflow run` output shows dropped items, so a run that nulled every
// pipeline item is never presented as a clean success with no visible reason.
func TestRenderWorkflowResultSurfacesDroppedItems(t *testing.T) {
	snapshot := workflow.Snapshot{
		Run: workflow.Run{
			ID:     "wf-render-drops",
			Task:   "render drops",
			Status: "completed",
			Failures: []workflow.RunFailure{
				{Label: "pipeline stage 1 item 0", Phase: "judge", Error: "TypeError: Object has no member 'then' (did you chain .then()/.catch() on agent() or check()?)"},
			},
		},
	}
	out := renderWorkflowResult(snapshot, `{"results":[null]}`)
	if !strings.Contains(out, "Dropped items:") {
		t.Fatalf("expected dropped items section in run output, got:\n%s", out)
	}
	if !strings.Contains(out, "pipeline stage 1 item 0") || !strings.Contains(out, "did you chain .then()") {
		t.Fatalf("expected dropped item label and reason in run output, got:\n%s", out)
	}
}
