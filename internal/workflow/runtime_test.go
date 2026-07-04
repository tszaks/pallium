package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerExecutesScriptAndRecordsAgents(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scriptPath, err := WriteRunScript("wf-test", tmp, `export const meta = { name: "test", phases: ["scan"] };
phase("scan");
const results = pipeline(["a", "b"], item => agent("inspect " + item, { label: item, mode: "read-only" }));
return { count: results.length, first: results[0].prompt };`)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-test", Task: "test workflow", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: store, Run: run, MaxAgents: 10}
	result, err := runner.Execute(context.Background(), `export const meta = { name: "test", phases: ["scan"] };
phase("scan");
const results = pipeline(["a", "b"], item => agent("inspect " + item, { label: item, mode: "read-only" }));
return { count: results.length, first: results[0].prompt };`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"count": 2`) || !strings.Contains(result, "inspect a") {
		t.Fatalf("unexpected result: %s", result)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "completed" {
		t.Fatalf("expected completed run, got %+v", snapshot.Run)
	}
	if len(snapshot.Phases) != 1 || snapshot.Phases[0].AgentCount != 2 {
		t.Fatalf("expected one phase with two agents, got %+v", snapshot.Phases)
	}
	if len(snapshot.Agents) != 2 {
		t.Fatalf("expected two agents, got %+v", snapshot.Agents)
	}
}

func TestRunnerSupportsTopLevelAwait(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("scan");
const result = await agent("inspect awaited", { label: "awaited" });
return { prompt: result.prompt };`
	scriptPath, err := WriteRunScript("wf-await", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-await", Task: "await", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "inspect awaited") {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestRunnerSupportsCheckHelper(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"command":"go test ./...","summary":"passed","output_tail":"","failures":[]}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("verify");
const result = await check("go test ./...", { label: "unit-tests" });
return { ok: result.ok, command: result.command };`
	scriptPath, err := WriteRunScript("wf-check", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-check", Task: "check", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) || !strings.Contains(result, "go test ./...") {
		t.Fatalf("unexpected result: %s", result)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Agents) != 1 || snapshot.Agents[0].Mode != "test" {
		t.Fatalf("expected one test agent, got %+v", snapshot.Agents)
	}
}

func TestRunnerStopsAtMaxAgents(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("scan"); agent("one"); agent("two"); return "done";`
	scriptPath, err := WriteRunScript("wf-max", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-max", Task: "max", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 1}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded max agents") {
		t.Fatalf("expected max agents error, got %v", err)
	}
}

func TestParallelRunsAgentsConcurrently(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "250")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = parallel([
  () => agent("inspect a", { label: "a" }),
  () => agent("inspect b", { label: "b" }),
  () => agent("inspect c", { label: "c" }),
  () => agent("inspect d", { label: "d" })
]);
return { count: results.length, prompts: results.map(result => result.prompt) };`
	scriptPath, err := WriteRunScript("wf-parallel", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel", Task: "parallel", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed >= 800*time.Millisecond {
		t.Fatalf("parallel agents appear sequential, elapsed=%s result=%s", elapsed, result)
	}
	if !strings.Contains(result, `"count": 4`) || !strings.Contains(result, "inspect d") {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestParallelRunsChecksConcurrently(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"command":"stub","summary":"passed","output_tail":"","failures":[]}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "250")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("checks");
const results = parallel([
  () => check("npm test", { label: "npm" }),
  () => check("go test ./...", { label: "go" }),
  () => check("pytest", { label: "pytest" })
]);
return { count: results.length, ok: results.every(result => result.ok) };`
	scriptPath, err := WriteRunScript("wf-parallel-checks", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel-checks", Task: "parallel checks", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed >= 800*time.Millisecond {
		t.Fatalf("parallel checks appear sequential, elapsed=%s result=%s", elapsed, result)
	}
	if !strings.Contains(result, `"count": 3`) || !strings.Contains(result, `"ok": true`) {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestPipelineRunsStageAgentsConcurrently(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "250")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["a", "b", "c", "d"], item =>
  agent("inspect " + item, { label: item })
);
return { count: results.length, prompts: results.map(result => result.prompt) };`
	scriptPath, err := WriteRunScript("wf-pipeline", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pipeline", Task: "pipeline", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed >= 800*time.Millisecond {
		t.Fatalf("pipeline agents appear sequential, elapsed=%s result=%s", elapsed, result)
	}
	if !strings.Contains(result, `"count": 4`) || !strings.Contains(result, "inspect d") {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestParallelHonorsConcurrentAgentCap(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "200")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("cap");
const results = parallel([
  () => agent("one", { label: "one" }),
  () => agent("two", { label: "two" }),
  () => agent("three", { label: "three" }),
  () => agent("four", { label: "four" }),
  () => agent("five", { label: "five" }),
  () => agent("six", { label: "six" })
]);
return { count: results.length };`
	scriptPath, err := WriteRunScript("wf-cap", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-cap", Task: "cap", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 2}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 550*time.Millisecond || elapsed >= 1100*time.Millisecond {
		t.Fatalf("expected capped execution near three waves, elapsed=%s result=%s", elapsed, result)
	}
}

func TestRunnerReusesCompletedAgentsOnRerun(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first","prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return result;`
	scriptPath, err := WriteRunScript("wf-resume", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-resume", Task: "resume", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	first, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, `"source": "first"`) {
		t.Fatalf("unexpected first result: %s", first)
	}
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second","prompt":"{{PROMPT}}"}`)
	second, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, `"source": "first"`) || strings.Contains(second, `"source": "second"`) {
		t.Fatalf("expected cached first result on rerun, got %s", second)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected one live agent record reused from cache, got %+v", agents)
	}
	phases, err := store.ListPhases(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].AgentCount != 1 {
		t.Fatalf("expected one reused phase with one cached agent, got %+v", phases)
	}
}

func TestNormalizeSchemaAddsAdditionalPropertiesFalse(t *testing.T) {
	raw := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	normalized := normalizeSchema(raw).(map[string]any)
	if normalized["additionalProperties"] != false {
		t.Fatalf("root object missing additionalProperties=false: %#v", normalized)
	}
	props := normalized["properties"].(map[string]any)
	items := props["items"].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Fatalf("nested object missing additionalProperties=false: %#v", items)
	}
}

func TestRunArtifactDirUsesHomePalliumDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RunArtifactDir("wf-artifact", "patches")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".pallium", "workflow-runs", "wf-artifact", "patches")
	if path != want {
		t.Fatalf("expected %s, got %s", want, path)
	}
	if _, err := RunArtifactDir("../bad", "patches"); err == nil {
		t.Fatal("expected unsafe run id to fail")
	}
	if _, err := os.Stat(filepath.Join(home, ".pallium")); !os.IsNotExist(err) {
		t.Fatalf("RunArtifactDir should not create dirs, stat err=%v", err)
	}
}
