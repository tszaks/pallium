package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestRunnerSupportsCoordinatorReplan(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_SEQUENCE", `[
		"{\"summary\":\"initial\"}",
		"{\"decision\":\"continue\",\"next_steps\":[\"inspect deeper\"]}"
	]`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"fallback"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("inspect");
await agent("initial inspection", { label: "initial" });
const plan = await coordinator.replan("adjust the remaining plan", { label: "coordinator" });
return { decision: plan.decision, next_steps: plan.next_steps };`
	scriptPath, err := WriteRunScript("wf-coordinator", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-coordinator", Task: "coordinator", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"decision": "continue"`) || !strings.Contains(result, "inspect deeper") {
		t.Fatalf("unexpected coordinator result: %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[1].Label != "coordinator" {
		t.Fatalf("expected initial and coordinator agents, got %+v", agents)
	}
	if !strings.Contains(agents[1].Prompt, "initial inspection") {
		t.Fatalf("expected coordinator prompt to include snapshot with initial work, got %s", agents[1].Prompt)
	}
}

func TestRunnerSupportsPalliumPrimitives(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PALLIUM_STUB", `{"ok":true,"args":"{{ARGS}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pallium");
const verify = await pallium.verify("fast");
const explain = await pallium.explain("README.md");
const preflight = await pallium.preflight("tighten auth", "src/auth");
const task = await pallium.task.start("tighten auth", "src/auth");
return { verify: verify.args, explain: explain.args, preflight: preflight.args, task: task.args };`
	scriptPath, err := WriteRunScript("wf-pallium", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pallium", Task: "pallium", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"verify fast", "explain README.md", "workflow preflight tighten auth --scope src/auth", "task start tighten auth src/auth"} {
		if !strings.Contains(result, want) {
			t.Fatalf("expected %q in result: %s", want, result)
		}
	}
}

func TestRunnerRecordsAgentProvider(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("providers");
await agent("codex provider", { label: "codex-worker", provider: "codex" });
return { ok: true };`
	scriptPath, err := WriteRunScript("wf-provider", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-provider", Task: "provider", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Provider != "codex" {
		t.Fatalf("expected codex provider, got %+v", agents)
	}
}

func TestRunnerUsesConfiguredProviderCommand(t *testing.T) {
	tmp := t.TempDir()
	providerScript := filepath.Join(tmp, "fake-provider.sh")
	if err := os.WriteFile(providerScript, []byte(`#!/bin/sh
if [ ! -s "$PALLIUM_WORKFLOW_PROMPT_FILE" ]; then
  echo "missing prompt file" >&2
  exit 1
fi
printf '{"ok":true,"provider":"%s","label":"%s","mode":"%s"}' "$PALLIUM_WORKFLOW_PROVIDER" "$PALLIUM_WORKFLOW_LABEL" "$PALLIUM_WORKFLOW_MODE" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAKE_COMMAND", providerScript)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `const result = await agent("fake provider", { label: "fake-worker", provider: "fake", mode: "read-only" }); return result;`
	scriptPath, err := WriteRunScript("wf-provider-fail", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-provider-fail", Task: "provider", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"provider": "fake"`) || !strings.Contains(result, `"label": "fake-worker"`) {
		t.Fatalf("unexpected provider result: %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Provider != "fake" {
		t.Fatalf("expected fake provider agent, got %+v", agents)
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

func TestRunnerEnforcesBudget(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_COST_USD", "0.01")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("budget"); agent("one"); agent("two"); return "done";`
	scriptPath, err := WriteRunScript("wf-budget", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-budget", Task: "budget", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "0.015"}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected budget exhausted error, got %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected one agent before budget stop, got %+v", agents)
	}
	if agents[0].EstimatedCostUSD != 0.01 {
		t.Fatalf("expected persisted estimated cost, got %+v", agents[0])
	}
}

func TestRunnerComposesSavedWorkflow(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".pallium", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".pallium", "workflows", "inner.js"), []byte(`phase("inner");
const result = agent("inner " + args.topic, { label: "inner" });
return { inner: result.prompt };`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("outer");
const result = workflow("inner", { topic: "compose" });
return { composed: result.inner };`
	scriptPath, err := WriteRunScript("wf-compose", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-compose", Task: "compose", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "inner compose") {
		t.Fatalf("expected nested workflow result, got %s", result)
	}
}

func TestRunnerRejectsDeepNestedWorkflow(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".pallium", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".pallium", "workflows", "inner.js"), []byte(`return workflow("inner", {});`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return workflow("inner", {});`
	scriptPath, err := WriteRunScript("wf-compose-depth", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-compose-depth", Task: "compose", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "nested workflow depth exceeded") {
		t.Fatalf("expected depth error, got %v", err)
	}
}

func TestRunnerVerifyUntilGreenFixLoop(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_SEQUENCE", `[
		"{\"ok\":false,\"command\":\"go test ./...\",\"summary\":\"failed\",\"output_tail\":\"boom\",\"failures\":[{\"name\":\"TestOne\",\"message\":\"boom\"}]}",
		"{\"summary\":\"fixed\",\"files_changed\":[\"main.go\"],\"confidence\":\"high\"}",
		"{\"ok\":true,\"command\":\"go test ./...\",\"summary\":\"passed\",\"output_tail\":\"ok\",\"failures\":[]}"
	]`)
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("verify");
const result = verify.untilGreen("go test ./...", { label: "green", maxRounds: 2 });
return result;`
	scriptPath, err := WriteRunScript("wf-until-green", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-until-green", Task: "until green", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) || !strings.Contains(result, `"fixed": true`) {
		t.Fatalf("expected successful fix loop, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 || agents[1].Mode != "edit" {
		t.Fatalf("expected check/fix/check agents, got %+v", agents)
	}
}

func TestRunnerRecordsAndSearchesDecisions(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	firstScript := `phase("decide");
const decision = pallium.decisions.record("Use workflow decisions", "Carry durable choices across runs.", "workflow", "memory");
return decision;`
	firstPath, err := WriteRunScript("wf-decision-one", tmp, firstScript)
	if err != nil {
		t.Fatal(err)
	}
	firstRun, err := store.CreateRun(Run{ID: "wf-decision-one", Task: "decision", CWD: tmp, ScriptPath: firstPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: firstRun, MaxAgents: 10}).Execute(context.Background(), firstScript, nil); err != nil {
		t.Fatal(err)
	}
	secondScript := `phase("recall");
const decisions = pallium.decisions.search("durable", 5);
return { count: decisions.length, title: decisions[0].title };`
	secondPath, err := WriteRunScript("wf-decision-two", tmp, secondScript)
	if err != nil {
		t.Fatal(err)
	}
	secondRun, err := store.CreateRun(Run{ID: "wf-decision-two", Task: "decision", CWD: tmp, ScriptPath: secondPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: secondRun, MaxAgents: 10}).Execute(context.Background(), secondScript, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Use workflow decisions") {
		t.Fatalf("expected prior decision in result, got %s", result)
	}
}

func TestRunnerGatePausesUntilApproved(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("gate");
gate("approve-patches", "review before continuing");
const result = agent("after gate", { label: "after" });
return result;`
	scriptPath, err := WriteRunScript("wf-gate", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-gate", Task: "gate", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if !errors.Is(err, ErrWorkflowPaused) {
		t.Fatalf("expected workflow paused, got %v", err)
	}
	gates, err := store.ListGates(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].Status != "open" {
		t.Fatalf("expected open gate, got %+v", gates)
	}
	if _, err := store.ApproveGate(run.ID, "approve-patches"); err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) {
		t.Fatalf("expected run after approved gate, got %s", result)
	}
}

func TestRunnerAppliesPatchToAgentRepo(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"changed other repo"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_FILE", "target.txt")
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_CONTENT", "changed\n")
	rootRepo := t.TempDir()
	otherRepo := t.TempDir()
	for _, repo := range []string{rootRepo, otherRepo} {
		runGit(t, repo, "init")
		runGit(t, repo, "config", "user.email", "test@example.com")
		runGit(t, repo, "config", "user.name", "Test User")
		if err := os.WriteFile(filepath.Join(repo, "target.txt"), []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", "target.txt")
		runGit(t, repo, "commit", "-m", "initial")
	}
	store, err := Open(filepath.Join(rootRepo, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("multi-repo");
agent("change other repo", { label: "other", mode: "edit", isolation: "worktree", repo: args.otherRepo });
return { ok: true };`
	scriptPath, err := WriteRunScript("wf-multi-repo", rootRepo, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-multi-repo", Task: "multi repo", CWD: rootRepo, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, map[string]any{"otherRepo": otherRepo}); err != nil {
		t.Fatal(err)
	}
	rootRaw, err := os.ReadFile(filepath.Join(rootRepo, "target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	otherRaw, err := os.ReadFile(filepath.Join(otherRepo, "target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rootRaw) != "original\n" || string(otherRaw) != "changed\n" {
		t.Fatalf("patch applied to wrong repo: root=%q other=%q", string(rootRaw), string(otherRaw))
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reverted, err := RevertPatches(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(reverted) != 1 {
		t.Fatalf("expected one reverted patch, got %#v", reverted)
	}
	otherRaw, err = os.ReadFile(filepath.Join(otherRepo, "target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(otherRaw) != "original\n" {
		t.Fatalf("expected reverted other repo, got %q", string(otherRaw))
	}
}

func TestRunnerPatchIncludesNewFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"new file"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_FILE", "created.txt")
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_CONTENT", "created by workflow\n")
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("edit"); return agent("create file", { label: "creator", mode: "edit", isolation: "worktree" });`
	scriptPath, err := WriteRunScript("wf-new-file", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-new-file", Task: "new file", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "created.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "created by workflow\n" {
		t.Fatalf("unexpected new file content: %q", string(raw))
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

func TestParallelSupportsItemCallback(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "250")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["a", "b", "c", "d"], (item, index) =>
  agent("inspect " + item + " #" + index, { label: item })
);
return { count: results.length, prompts: results.map(result => result.prompt) };`
	scriptPath, err := WriteRunScript("wf-parallel-callback", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel-callback", Task: "parallel callback", CWD: tmp, ScriptPath: scriptPath})
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
		t.Fatalf("parallel callback agents appear sequential, elapsed=%s result=%s", elapsed, result)
	}
	if !strings.Contains(result, `"count": 4`) || !strings.Contains(result, "inspect d #3") {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestPhaseCallbackKeepsAwaitedAgentsScoped(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `const inside = await phase("inside", async () => {
  const result = await agent("inside awaited", { label: "inside-agent" });
  return result;
});
phase("outside");
const outside = await agent("outside awaited", { label: "outside-agent" });
return { inside: inside.prompt, outside: outside.prompt };`
	scriptPath, err := WriteRunScript("wf-phase-async", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-phase-async", Task: "phase async", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "inside awaited") || !strings.Contains(result, "outside awaited") {
		t.Fatalf("unexpected result: %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	byLabel := map[string]string{}
	for _, agent := range agents {
		byLabel[agent.Label] = agent.Phase
	}
	if byLabel["inside-agent"] != "inside" {
		t.Fatalf("expected inside-agent in inside phase, got phases %#v", byLabel)
	}
	if byLabel["outside-agent"] != "outside" {
		t.Fatalf("expected outside-agent in outside phase, got phases %#v", byLabel)
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

func TestPipelineStreamsItemsAcrossStages(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "pipeline.log")
	providerPath := filepath.Join(tmp, "provider.py")
	if err := os.WriteFile(providerPath, []byte(`#!/usr/bin/env python3
import json
import os
import time

prompt = open(os.environ["PALLIUM_WORKFLOW_PROMPT_FILE"]).read().strip()
log = os.environ["PIPELINE_LOG"]
with open(log, "a") as f:
    f.write(f"start|{prompt}|{time.time()}\n")
if prompt == "stage1 slow":
    time.sleep(0.45)
elif prompt == "stage1 fast":
    time.sleep(0.05)
else:
    time.sleep(0.10)
with open(log, "a") as f:
    f.write(f"end|{prompt}|{time.time()}\n")
parts = prompt.split()
with open(os.environ["PALLIUM_WORKFLOW_OUTPUT_FILE"], "w") as f:
    json.dump({"stage": parts[0], "item": parts[1], "prompt": prompt}, f)
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_PROBE_COMMAND", providerPath)
	t.Setenv("PIPELINE_LOG", logPath)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["slow", "fast"],
  item => agent("stage1 " + item, { label: "stage1-" + item, provider: "probe" }),
  result => agent("stage2 " + result.item, { label: "stage2-" + result.item, provider: "probe" })
);
return { prompts: results.map(result => result.prompt) };`
	scriptPath, err := WriteRunScript("wf-pipeline-streaming", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pipeline-streaming", Task: "pipeline streaming", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 4}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "stage2 slow") || !strings.Contains(result, "stage2 fast") {
		t.Fatalf("unexpected result: %s", result)
	}
	times := readPipelineTimes(t, logPath)
	if times["start|stage2 fast"] >= times["end|stage1 slow"] {
		t.Fatalf("pipeline has a stage barrier: stage2 fast started at %f after stage1 slow ended at %f\nlog=%#v", times["start|stage2 fast"], times["end|stage1 slow"], times)
	}
}

func TestPipelineStageReceivesPreviousOriginalAndIndex(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["alpha", "beta"],
  (item, original, index) => ({ current: item.toUpperCase(), original, index }),
  (prev, original, index) => agent("verify " + prev.current + " from " + original + " #" + index, { label: "verify-" + index })
);
return { prompts: results.map(result => result.prompt) };`
	scriptPath, err := WriteRunScript("wf-pipeline-args", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pipeline-args", Task: "pipeline args", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "verify ALPHA from alpha #0") || !strings.Contains(result, "verify BETA from beta #1") {
		t.Fatalf("pipeline did not pass previous result, original item, and index: %s", result)
	}
}

func TestPipelineStageThrowDropsOnlyThatItem(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["keep", "drop", "also-keep"],
  item => {
    if (item === "drop") {
      throw new Error("drop this item");
    }
    return item.toUpperCase();
  }
);
return { results };`
	scriptPath, err := WriteRunScript("wf-pipeline-throw", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pipeline-throw", Task: "pipeline throw", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"KEEP"`) || !strings.Contains(result, "null") || !strings.Contains(result, `"ALSO-KEEP"`) {
		t.Fatalf("pipeline throw did not preserve item order with null drop: %s", result)
	}
}

func TestWorkflowCollectionItemLimit(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `const items = Array.from({ length: 4097 }, (_, index) => index);
return parallel(items, item => item);`
	scriptPath, err := WriteRunScript("wf-item-cap", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-item-cap", Task: "item cap", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "parallel item limit exceeded") {
		t.Fatalf("expected parallel item limit error, got %v", err)
	}

	script = `const items = Array.from({ length: 4097 }, (_, index) => index);
return pipeline(items, item => item);`
	scriptPath, err = WriteRunScript("wf-pipeline-item-cap", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.CreateRun(Run{ID: "wf-pipeline-item-cap", Task: "pipeline item cap", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "pipeline item limit exceeded") {
		t.Fatalf("expected pipeline item limit error, got %v", err)
	}
}

func TestWorkflowDeterministicGuards(t *testing.T) {
	for name, script := range map[string]string{
		"math-random": `return Math.random();`,
		"date-now":    `return Date.now();`,
		"new-date":    `return new Date();`,
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			scriptPath, err := WriteRunScript("wf-"+name, tmp, script)
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.CreateRun(Run{ID: "wf-" + name, Task: name, CWD: tmp, ScriptPath: scriptPath})
			if err != nil {
				t.Fatal(err)
			}
			_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
			if err == nil || !strings.Contains(err.Error(), "disabled in Pallium workflow scripts") {
				t.Fatalf("expected deterministic guard error, got %v", err)
			}
		})
	}
}

func TestWorkflowBudgetObjectShape(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_COST_USD", "0.25")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("budget");
const before = { total: budget.total, spent: budget.spent(), remaining: budget.remaining() };
await agent("one", { label: "one" });
const after = { total: budget.total, spent: budget.spent(), remaining: budget.remaining() };
return { before, after };`
	scriptPath, err := WriteRunScript("wf-budget-object", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-budget-object", Task: "budget object", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "1.00"}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"total": 1`, `"spent": 0`, `"remaining": 1`, `"spent": 0.25`, `"remaining": 0.75`} {
		if !strings.Contains(result, want) {
			t.Fatalf("budget object result missing %s: %s", want, result)
		}
	}
}

func TestWorkflowAllowsDeterministicDateConstruction(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return new Date(0).toISOString();`
	scriptPath, err := WriteRunScript("wf-date-explicit", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-date-explicit", Task: "date explicit", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "1970-01-01T00:00:00.000Z") {
		t.Fatalf("unexpected explicit date result: %s", result)
	}
}

func readPipelineTimes(t *testing.T, path string) map[string]float64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	times := map[string]float64{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			t.Fatalf("unexpected pipeline log line %q", line)
		}
		value, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			t.Fatalf("parse pipeline timestamp %q: %v", line, err)
		}
		times[parts[0]+"|"+parts[1]] = value
	}
	return times
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

func TestRunnerStopsWhenRunIsMarkedStopped(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "2000")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["a", "b", "c", "d"], item =>
  agent("inspect " + item, { label: item })
);
return { count: results.length };`
	scriptPath, err := WriteRunScript("wf-stop-cooperative", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-stop-cooperative", Task: "stop cooperative", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 4}).Execute(context.Background(), script, nil)
		errCh <- err
	}()

	deadline := time.After(2 * time.Second)
	for {
		agents, err := store.ListAgents(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(agents) == 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for running agents, got %d", len(agents))
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := store.SetRunStatus(run.ID, "stopped", "", "test stop"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrWorkflowStopped) {
			t.Fatalf("expected ErrWorkflowStopped, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop after run status changed to stopped")
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "stopped" {
		t.Fatalf("expected stopped run, got %+v", snapshot.Run)
	}
	stoppedAgents := 0
	for _, agent := range snapshot.Agents {
		if agent.Status == "stopped" {
			stoppedAgents++
		}
	}
	if stoppedAgents == 0 {
		t.Fatalf("expected at least one stopped agent, got %+v", snapshot.Agents)
	}
}

func TestRunnerPausesWhenRunIsMarkedPaused(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "2000")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["a", "b", "c", "d"], item =>
  agent("inspect " + item, { label: item })
);
return { count: results.length };`
	scriptPath, err := WriteRunScript("wf-pause-cooperative", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pause-cooperative", Task: "pause cooperative", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 4}).Execute(context.Background(), script, nil)
		errCh <- err
	}()

	deadline := time.After(2 * time.Second)
	for {
		agents, err := store.ListAgents(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(agents) == 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for running agents, got %d", len(agents))
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := store.SetRunStatus(run.ID, "paused", "", "test pause"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrWorkflowPaused) {
			t.Fatalf("expected ErrWorkflowPaused, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not pause after run status changed to paused")
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "paused" {
		t.Fatalf("expected paused run, got %+v", snapshot.Run)
	}
	pausedAgents := 0
	for _, agent := range snapshot.Agents {
		if agent.Status == "paused" {
			pausedAgents++
		}
	}
	if pausedAgents == 0 {
		t.Fatalf("expected at least one paused agent, got %+v", snapshot.Agents)
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

func TestRunnerInvalidatesCompletedAgentCacheOnFingerprintChange(t *testing.T) {
	cases := []struct {
		name       string
		first      string
		second     string
		firstArgs  any
		secondArgs any
	}{
		{
			name: "script",
			first: `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return result;`,
			second: `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return { source: result.source, changed_script: true };`,
		},
		{
			name: "args",
			first: `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return { source: result.source, topic: args.topic };`,
			second: `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return { source: result.source, topic: args.topic };`,
			firstArgs:  map[string]any{"topic": "one"},
			secondArgs: map[string]any{"topic": "two"},
		},
		{
			name: "schema",
			first: `phase("scan");
const result = await agent("stable prompt", {
  label: "stable",
  schema: { type: "object", properties: { source: { type: "string" } }, required: ["source"] }
});
return result;`,
			second: `phase("scan");
const result = await agent("stable prompt", {
  label: "stable",
  schema: { type: "object", properties: { source: { type: "string" }, changed: { type: "boolean" } }, required: ["source", "changed"] }
});
return result;`,
		},
		{
			name: "model",
			first: `phase("scan");
const result = await agent("stable prompt", { label: "stable", model: "model-a" });
return result;`,
			second: `phase("scan");
const result = await agent("stable prompt", { label: "stable", model: "model-b" });
return result;`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			scriptPath, err := WriteRunScript("wf-cache-"+tc.name, tmp, tc.first)
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.CreateRun(Run{ID: "wf-cache-" + tc.name, Task: "cache invalidation", CWD: tmp, ScriptPath: scriptPath})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first","prompt":"{{PROMPT}}","changed":false}`)
			first, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), tc.first, tc.firstArgs)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(first, `"source": "first"`) {
				t.Fatalf("unexpected first result: %s", first)
			}
			t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second","prompt":"{{PROMPT}}","changed":true}`)
			second, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), tc.second, tc.secondArgs)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(second, `"source": "second"`) {
				t.Fatalf("expected cache miss after %s fingerprint change, got %s", tc.name, second)
			}
			agents, err := store.ListAgents(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(agents) != 2 {
				t.Fatalf("expected two agent records after cache miss, got %+v", agents)
			}
		})
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

func TestScanPatchPolicyFindsAddedSecrets(t *testing.T) {
	patch := []byte(`diff --git a/config.env b/config.env
index 1111111..2222222 100644
--- a/config.env
+++ b/config.env
@@ -1 +1,2 @@
 SAFE=value
+OPENAI_API_KEY=sk-1234567890abcdefghijklmnop
`)
	findings := ScanPatchPolicy(patch)
	if len(findings) == 0 || findings[0].Kind != "openai-key" {
		t.Fatalf("expected openai-key finding, got %+v", findings)
	}
}

func TestApplyPatchBlocksSecretAdditions(t *testing.T) {
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "config.env"), []byte("SAFE=value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "config.env")
	runGit(t, tmp, "commit", "-m", "initial")
	patchPath := filepath.Join(tmp, "secret.patch")
	patch := `diff --git a/config.env b/config.env
index 00ba9f1..9a46df0 100644
--- a/config.env
+++ b/config.env
@@ -1 +1,2 @@
 SAFE=value
+OPENAI_API_KEY=sk-1234567890abcdefghijklmnop
`
	if err := os.WriteFile(patchPath, []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	applied, err := applyPatch(context.Background(), tmp, patchPath)
	if err == nil || !strings.Contains(err.Error(), "workflow patch policy blocked") {
		t.Fatalf("expected policy block, applied=%t err=%v", applied, err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "OPENAI_API_KEY") {
		t.Fatalf("secret patch should not have been applied: %s", string(raw))
	}
}

func TestApplyPatchSecretBypassIsExplicit(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_ALLOW_SECRET_PATCH", "1")
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "config.env"), []byte("SAFE=value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "config.env")
	runGit(t, tmp, "commit", "-m", "initial")
	patchPath := filepath.Join(tmp, "secret.patch")
	patch := `diff --git a/config.env b/config.env
index 00ba9f1..9a46df0 100644
--- a/config.env
+++ b/config.env
@@ -1 +1,2 @@
 SAFE=value
+OPENAI_API_KEY=sk-1234567890abcdefghijklmnop
`
	if err := os.WriteFile(patchPath, []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	applied, err := applyPatch(context.Background(), tmp, patchPath)
	if err != nil || !applied {
		t.Fatalf("expected explicit bypass to apply, applied=%t err=%v", applied, err)
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

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
