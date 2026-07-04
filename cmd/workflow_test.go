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
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
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
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"triggered"}`)
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
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"triggered"}`)
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

func TestWorkflowHTTPAPI(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"api"}`)
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
	handler := newWorkflowHTTPHandler(dbPath)

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

func TestWorkflowGateApproveAndResume(t *testing.T) {
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
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("gate");
gate("approve", "approve before worker");
return agent("after gate", { label: "after" });`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runWorkflow(&out, []string{"run", "--id", "wf-gate-cli", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "gate"}, false)
	if err == nil || !strings.Contains(err.Error(), "workflow paused") {
		t.Fatalf("expected paused run, got %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"gate", "list", "wf-gate-cli", "--db", dbPath}, true); err != nil {
		t.Fatalf("gate list failed: %v", err)
	}
	var gates []map[string]any
	if err := json.Unmarshal(out.Bytes(), &gates); err != nil {
		t.Fatalf("decode gates: %v\n%s", err, out.String())
	}
	if len(gates) != 1 || gates[0]["status"] != "open" {
		t.Fatalf("expected open gate, got %#v", gates)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"gate", "approve", "wf-gate-cli", "approve", "--db", dbPath}, true); err != nil {
		t.Fatalf("gate approve failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"resume", "wf-gate-cli", "--db", dbPath}, true); err != nil {
		t.Fatalf("resume after gate failed: %v", err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode resumed snapshot: %v\n%s", err, out.String())
	}
	run := snapshot["run"].(map[string]any)
	if run["status"] != "completed" {
		t.Fatalf("expected completed run after gate approval, got %#v", snapshot)
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
