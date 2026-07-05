package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdversarialUnconfiguredProviderFailsFast(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("provider");
return agent("ghost worker", { label: "ghost", provider: "ghost" });`
	scriptPath, err := WriteRunScript("wf-ghost-provider", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-ghost-provider", Task: "ghost", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil {
		t.Fatal("expected unconfigured provider error")
	}
	if !strings.Contains(err.Error(), "PALLIUM_WORKFLOW_PROVIDER_GHOST_COMMAND") {
		t.Fatalf("expected provider env hint, got %v", err)
	}
}

func TestAdversarialProviderCommandFailureSurfaces(t *testing.T) {
	tmp := t.TempDir()
	scriptPath := filepath.Join(tmp, "fail.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAIL_COMMAND", scriptPath)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("provider");
return agent("failing provider", { label: "fail", provider: "fail" });`
	runScriptPath, err := WriteRunScript("wf-fail-provider", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-fail-provider", Task: "fail", CWD: tmp, ScriptPath: runScriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), `workflow provider "fail" failed`) {
		t.Fatalf("expected provider failure, got %v", err)
	}
}

func TestAdversarialGateApproveWrongNameDoesNotMutateAgentGate(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"approved":true,"reason":"ok"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("gate");
gate("approve", "verify before worker");
return agent("after gate", { label: "after" });`
	scriptPath, err := WriteRunScript("wf-wrong-gate", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-wrong-gate", Task: "gate", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatalf("expected agent gate to approve, got %v", err)
	}
	if _, err := store.ApproveGate(run.ID, "wrong-gate"); err == nil {
		t.Fatal("expected wrong gate approval to fail")
	} else if !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("expected not-found gate error, got %v", err)
	}
	gates, err := store.ListGates(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].Name != "approve" || gates[0].Status != "approved" {
		t.Fatalf("expected original gate to stay approved, got %+v", gates)
	}
}

func TestAdversarialBudgetAllowsExactBoundarySpend(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_COST_USD", "0.01")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("budget");
agent("one", { label: "one" });
agent("two", { label: "two" });
return "done";`
	scriptPath, err := WriteRunScript("wf-budget-boundary", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-budget-boundary", Task: "budget", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "0.02"}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatalf("expected two agents within exact budget, got %v", err)
	}
	if result != "done" && result != `"done"` {
		t.Fatalf("unexpected result: %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected exactly two agents at budget boundary, got %+v", agents)
	}
}

func TestAdversarialParallelConcurrentUnderRace(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "200")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["a", "b", "c", "d"], item =>
  agent("inspect " + item, { label: item, mode: "read-only" })
);
return { count: results.length };`
	scriptPath, err := WriteRunScript("wf-race-parallel", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-race-parallel", Task: "parallel", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 4}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) >= 700*time.Millisecond {
		t.Fatalf("parallel agents appear sequential under race build, elapsed=%s", time.Since(start))
	}
	if !strings.Contains(result, `"count": 4`) {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestAdversarialCountActiveRunsIgnoresTerminalStatuses(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	statuses := []struct {
		id     string
		status string
		active bool
	}{
		{"wf-completed", "completed", false},
		{"wf-failed", "failed", false},
		{"wf-stopped", "stopped", false},
		{"wf-running", "running", true},
		{"wf-paused", "paused", true},
	}
	for _, item := range statuses {
		if _, err := store.CreateRun(Run{ID: item.id, Task: item.id, CWD: tmp, ScriptPath: "workflow.js", Status: item.status}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := store.CountActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected two active runs, got %d", count)
	}
	if err := CheckActiveRunCapacity(store, 2); err == nil {
		t.Fatal("expected fleet limit at capacity")
	}
	if err := CheckActiveRunCapacity(store, 3); err != nil {
		t.Fatalf("expected room at limit 3, got %v", err)
	}
}

func TestAdversarialValidateRejectsEmptyScript(t *testing.T) {
	result := ValidateScript("   \n  ")
	if result.Valid {
		t.Fatal("expected empty script to be invalid")
	}
	if !strings.Contains(result.Error, "empty") {
		t.Fatalf("expected empty-script error, got %#v", result)
	}
}
