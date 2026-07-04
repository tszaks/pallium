package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
