package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/tszaks/pallium/internal/gitlog"
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

// TestRunnerFailsRunWhenProviderReportedCostExceedsBudget guards against a
// regression where a configured provider's reported cost_usd could push
// spend past --max-budget-usd after the agent already ran. The preflight
// check only guards the flat per-agent estimate, so a single agent call with
// no subsequent agent() call to trigger another preflight check must still
// fail the run instead of completing successfully while over budget.
func TestRunnerFailsRunWhenProviderReportedCostExceedsBudget(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_METERED_COMMAND", `printf '{"input_tokens":100,"output_tokens":50,"cost_usd":5.0}' > "$PALLIUM_WORKFLOW_USAGE_FILE"; printf '{"ok":true}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("budget");
return await agent("one metered call", { label: "one", provider: "metered" });`
	scriptPath, err := WriteRunScript("wf-budget-provider-cost", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-budget-provider-cost", Task: "budget provider cost", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "1.00"}).Execute(context.Background(), script, nil)
	if err == nil {
		t.Fatal("expected the run to fail once the reported cost exceeds the budget")
	}
	// The error crosses the goja VM boundary as plain text (see
	// classifyRunError), so identity checks like errors.Is don't survive a
	// top-level script return; match the message the way other budget/max-
	// agents tests in this file do.
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected a budget exhausted error, got %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Status != "failed" {
		t.Fatalf("expected one failed agent, got %+v", agents)
	}
	if agents[0].EstimatedCostUSD != 5.0 {
		t.Fatalf("expected the provider-reported cost persisted, got %+v", agents[0])
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "failed" {
		t.Fatalf("expected the run itself to be marked failed, got %+v", snapshot.Run)
	}
}

// TestRunnerRunStatusCompletedWithFailuresWhenSomeAgentsSucceed proves a run
// whose script rejects after some agents already completed gets an honest
// "completed_with_failures" status instead of a flat "failed" that would
// contradict a `workflow report` still built from those completed agents.
func TestRunnerRunStatusCompletedWithFailuresWhenSomeAgentsSucceed(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_OKPROV_COMMAND", `printf '{"ok":true}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAILPROV_COMMAND", `exit 1`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `
const okResults = await pipeline(["a", "b", "c"], item => agent("ok " + item, { label: item, provider: "okprov", mode: "read-only" }));
await agent("boom", { label: "boom", provider: "failprov", mode: "read-only" });
return { okResults };`
	scriptPath, err := WriteRunScript("wf-partial-fail", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-partial-fail", Task: "partial failure", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil {
		t.Fatal("expected the run to return an error from the unwrapped failing agent call")
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, a := range agents {
		if a.Status == "completed" {
			completed++
		}
	}
	if completed != 3 {
		t.Fatalf("expected 3 completed agents ahead of the failure, got %d (%+v)", completed, agents)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "completed_with_failures" {
		t.Fatalf("expected completed_with_failures status since 3 agents already succeeded, got %q (error=%q)", snapshot.Run.Status, snapshot.Run.Error)
	}
	if snapshot.Run.Error == "" {
		t.Fatal("expected a non-empty run error even though the run partially succeeded")
	}
}

// TestSetRunStatusNeverPersistsEmptyErrorForFailure proves the store-level
// backstop: whatever upstream code called SetRunStatus with a failing status
// and a blank errorText (e.g. an error whose own .Error() rendered empty),
// the persisted run always ends up with a non-empty, actionable message.
func TestSetRunStatusNeverPersistsEmptyErrorForFailure(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.CreateRun(Run{ID: "wf-empty-error", Task: "empty error backstop", CWD: tmp, ScriptPath: "unused.js"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"failed", "completed_with_failures"} {
		if err := store.SetRunStatus(run.ID, status, "", ""); err != nil {
			t.Fatal(err)
		}
		got, err := store.Run(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(got.Error) == "" {
			t.Fatalf("expected a non-empty error for status %q, got %+v", status, got)
		}
		if got.CompletedAt == "" {
			t.Fatalf("expected %q to be treated as terminal (completed_at set), got %+v", status, got)
		}
	}
}

// TestStoreAcquireRepoLockExclusiveAndIdempotent covers bug #34's core
// contract at the Store level: a second run cannot acquire a repo lock
// already held by a different run, the original holder can re-enter
// idempotently (multiple edit-intent agents in the same run), and releasing
// only removes the row when the caller is still the holder.
// writeEditThenVerifyProvider writes a fake provider script that actually
// inspects the filesystem it runs in: in "edit" mode it writes target.txt,
// otherwise it reports success only if target.txt already reads "edited".
// This is what proves bug #30's fix rather than a canned stub: a verifier
// that runs against the pristine checkout (the bug) sees "original" and
// reports failure, while one that runs against a worktree seeded from the
// run's staging state (the fix) sees the edit.
func writeEditThenVerifyProvider(t *testing.T, dir, successJSON, failureJSON string) string {
	t.Helper()
	path := filepath.Join(dir, "edit-then-verify-provider.sh")
	script := `#!/bin/sh
if [ "$PALLIUM_WORKFLOW_MODE" = "edit" ]; then
  printf 'edited\n' > target.txt
  printf '{"summary":"edited target"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
elif [ -f target.txt ] && [ "$(cat target.txt)" = "edited" ]; then
  printf '` + successJSON + `' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
else
  printf '` + failureJSON + `' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
fi
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func initEditTargetRepo(t *testing.T, tmp string) {
	t.Helper()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "target.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "target.txt")
	runGit(t, tmp, "commit", "-m", "initial")
}

// TestCheckSeesPriorEditAgentChanges is the bug #30 acceptance test: a
// standalone check() after an edit agent must see that agent's edit, not the
// pristine repo. Before the fix, check() ran against the untouched
// r.Run.CWD, so the fake verifier below would see "original" and report
// ok=false; with the run-scoped staging worktree, it sees "edited".
func TestCheckSeesPriorEditAgentChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	initEditTargetRepo(t, tmp)
	provider := writeEditThenVerifyProvider(t, t.TempDir(),
		`{"ok":true,"command":"verify target","summary":"target edited","output_tail":"","failures":[]}`,
		`{"ok":false,"command":"verify target","summary":"target not edited","output_tail":"missing edit","failures":[{"name":"target","message":"not edited"}]}`)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAKE_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("edit");
agent("edit target file", { label: "editor", mode: "edit", isolation: "worktree", provider: "fake" });
phase("verify");
const result = check("verify target file", { provider: "fake" });
return result;`
	scriptPath, err := WriteRunScript("wf-check-sees-edit", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-check-sees-edit", Task: "check sees edit", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) {
		t.Fatalf("expected check() to see the edit agent's change, got %s", result)
	}
	if raw, err := os.ReadFile(filepath.Join(tmp, "target.txt")); err != nil || string(raw) != "edited\n" {
		t.Fatalf("expected the edit applied to the real repo on completion, got %q err=%v", string(raw), err)
	}
	list := exec.Command("git", "worktree", "list", "--porcelain")
	list.Dir = tmp
	raw, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "worktree "); got != 1 {
		t.Fatalf("expected only the main worktree registered after completion (staging worktree cleaned up), got %d:\n%s", got, string(raw))
	}
}

// TestGateSeesPriorEditAgentChanges is bug #30's acceptance test for the
// agent-based gate() verifier instead of check().
func TestGateSeesPriorEditAgentChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	initEditTargetRepo(t, tmp)
	provider := writeEditThenVerifyProvider(t, t.TempDir(),
		`{"approved":true,"reason":"target edited","evidence":["target contains the edit"]}`,
		`{"approved":false,"reason":"target not edited","evidence":[]}`)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAKE_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("edit");
agent("edit target file", { label: "editor", mode: "edit", isolation: "worktree", provider: "fake" });
phase("verify");
const result = gate("target-edited", "verify target file", { provider: "fake" });
return result;`
	scriptPath, err := WriteRunScript("wf-gate-sees-edit", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-gate-sees-edit", Task: "gate sees edit", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"approved": true`) {
		t.Fatalf("expected gate() to see the edit agent's change, got %s", result)
	}
	if raw, err := os.ReadFile(filepath.Join(tmp, "target.txt")); err != nil || string(raw) != "edited\n" {
		t.Fatalf("expected the edit applied to the real repo on completion, got %q err=%v", string(raw), err)
	}
}

// TestMultipleEditAgentsComposeInStaging proves edits accumulate: a second
// edit agent branches from the first agent's change (not the pristine
// checkout), and a trailing check() sees both edits.
func TestMultipleEditAgentsComposeInStaging(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	initEditTargetRepo(t, tmp)
	if err := os.WriteFile(filepath.Join(tmp, "second.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "second.txt")
	runGit(t, tmp, "commit", "-m", "second file")

	providerDir := t.TempDir()
	firstEdit := filepath.Join(providerDir, "first-edit.sh")
	if err := os.WriteFile(firstEdit, []byte("#!/bin/sh\nprintf 'edited\\n' > target.txt\nprintf '{\"summary\":\"edited target\"}' > \"$PALLIUM_WORKFLOW_OUTPUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	secondEdit := filepath.Join(providerDir, "second-edit.sh")
	if err := os.WriteFile(secondEdit, []byte("#!/bin/sh\nprintf 'edited\\n' > second.txt\nprintf '{\"summary\":\"edited second\"}' > \"$PALLIUM_WORKFLOW_OUTPUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	verify := filepath.Join(providerDir, "verify.sh")
	if err := os.WriteFile(verify, []byte(`#!/bin/sh
if [ "$(cat target.txt)" = "edited" ] && [ "$(cat second.txt)" = "edited" ]; then
  printf '{"ok":true,"command":"verify both","summary":"both edited","output_tail":"","failures":[]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
else
  printf '{"ok":false,"command":"verify both","summary":"not both edited","output_tail":"","failures":[{"name":"both","message":"missing edit"}]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FIRST_COMMAND", firstEdit)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_SECOND_COMMAND", secondEdit)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_VERIFY_COMMAND", verify)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `agent("edit target", { label: "first", mode: "edit", isolation: "worktree", provider: "first" });
agent("edit second", { label: "second", mode: "edit", isolation: "worktree", provider: "second" });
const result = check("verify both files", { provider: "verify" });
return result;`
	scriptPath, err := WriteRunScript("wf-compose-staging", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-compose-staging", Task: "compose staging", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) {
		t.Fatalf("expected check() to see both edit agents' changes, got %s", result)
	}
	for _, name := range []string{"target.txt", "second.txt"} {
		if raw, err := os.ReadFile(filepath.Join(tmp, name)); err != nil || string(raw) != "edited\n" {
			t.Fatalf("expected %s applied to the real repo on completion, got %q err=%v", name, string(raw), err)
		}
	}
}

// TestUntilGreenSeesPriorEditAgentChanges proves verify.untilGreen's own base
// redirects to the run's staging worktree too: its very first check (before
// it creates its own persistent loop worktree) must see an earlier standalone
// edit agent's change instead of the pristine checkout.
func TestUntilGreenSeesPriorEditAgentChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	initEditTargetRepo(t, tmp)
	provider := writeEditThenVerifyProvider(t, t.TempDir(),
		`{"ok":true,"command":"verify target","summary":"target edited","output_tail":"","failures":[]}`,
		`{"ok":false,"command":"verify target","summary":"target not edited","output_tail":"missing edit","failures":[{"name":"target","message":"not edited"}]}`)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAKE_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `agent("edit target file", { label: "editor", mode: "edit", isolation: "worktree", provider: "fake" });
const result = verify.untilGreen("verify target file", { label: "green", maxRounds: 1, provider: "fake" });
return result;`
	scriptPath, err := WriteRunScript("wf-untilgreen-sees-edit", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-untilgreen-sees-edit", Task: "untilgreen sees edit", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) || strings.Contains(result, `"fixed": true`) {
		t.Fatalf("expected untilGreen to see the prior edit and converge WITHOUT needing its own fix round, got %s", result)
	}
}

// TestCheckSeesEditAgentChangesAfterResume is bug #30's resume test: the
// first execution runs the edit agent then hits a budget wall before check()
// runs; the second execution (resume) replays the edit agent from cache and
// must still rebuild the staging worktree from its durable patch so check()
// sees the edit.
func TestCheckSeesEditAgentChangesAfterResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	initEditTargetRepo(t, tmp)
	provider := writeEditThenVerifyProvider(t, t.TempDir(),
		`{"ok":true,"command":"verify target","summary":"target edited","output_tail":"","failures":[]}`,
		`{"ok":false,"command":"verify target","summary":"target not edited","output_tail":"missing edit","failures":[{"name":"target","message":"not edited"}]}`)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAKE_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("edit");
agent("edit target file", { label: "editor", mode: "edit", isolation: "worktree", provider: "fake" });
phase("verify");
const result = check("verify target file", { provider: "fake" });
return result;`
	scriptPath, err := WriteRunScript("wf-resume-check-sees-edit", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-resume-check-sees-edit", Task: "resume check sees edit", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	// First execution: the budget covers the edit agent only, so check() aborts
	// the run mid-flight before it ever runs.
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "0.01"}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected mid-run budget exhaustion before check(), got %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(tmp, "target.txt")); err != nil || string(raw) != "original\n" {
		t.Fatalf("expected the failed run to leave the real repo untouched, got %q err=%v", string(raw), err)
	}
	// Second execution (resume): the edit agent replays from cache; check()
	// must still see the edit via the freshly rebuilt staging worktree.
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) {
		t.Fatalf("expected resumed check() to see the edit agent's change, got %s", result)
	}
	if raw, err := os.ReadFile(filepath.Join(tmp, "target.txt")); err != nil || string(raw) != "edited\n" {
		t.Fatalf("expected the edit applied to the real repo on completion, got %q err=%v", string(raw), err)
	}
}

// TestConcurrentNonEditReadsAgainstStagingGetOwnEphemeralWorktrees is the
// regression test for the P0 and concurrency-race findings left open by the
// prior attempt at bug #30: a naive fix pointed a non-edit step's cwd
// directly at the shared staging worktree, so concurrent non-edit steps
// raced raw git plumbing against the SAME directory, and a side-effecting
// "read-only" step could leave stray files in the staging state that would
// eventually leak into the real repo. Each concurrent step here performs
// the exact git plumbing seedFromStaging/mergeIntoStaging themselves use
// (`git add -N` + `git diff`) against its OWN cwd and writes a stray,
// call-specific file — under the fix, that is always a throwaway worktree,
// so all N calls succeed cleanly with no index.lock contention, and none of
// the stray files ever reach the real repo.
func TestConcurrentNonEditReadsAgainstStagingGetOwnEphemeralWorktrees(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	initEditTargetRepo(t, tmp)

	editProvider := filepath.Join(t.TempDir(), "edit.sh")
	if err := os.WriteFile(editProvider, []byte("#!/bin/sh\nprintf 'edited\\n' > target.txt\nprintf '{\"summary\":\"edited\"}' > \"$PALLIUM_WORKFLOW_OUTPUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Mirrors seedFromStaging/worktreeDiff's own git plumbing so a real race
	// on a shared directory would surface as an index.lock failure here too.
	readerProvider := filepath.Join(t.TempDir(), "reader.sh")
	if err := os.WriteFile(readerProvider, []byte(`#!/bin/sh
git add -N -- . || exit 1
git diff --binary > /dev/null || exit 1
echo "leftover from $PALLIUM_WORKFLOW_AGENT_ID" > "leftover-$PALLIUM_WORKFLOW_AGENT_ID.txt"
printf '{"ok":true}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_EDITOR_COMMAND", editProvider)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_READER_COMMAND", readerProvider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// parallel() drops a failed item to null and keeps the run "completed"
	// rather than erroring Execute() itself, so the assertion below must
	// count nulls explicitly — a bare results.length would stay 5 even if
	// every single read failed and was dropped.
	script := `agent("edit target", { label: "editor", mode: "edit", isolation: "worktree", provider: "editor" });
const results = parallel(["a", "b", "c", "d", "e"], item => agent("read " + item, { label: "reader-" + item, mode: "test", provider: "reader" }));
return { count: results.length, failed: results.filter(r => r === null).length };`
	scriptPath, err := WriteRunScript("wf-concurrent-staging-reads", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-concurrent-staging-reads", Task: "concurrent staging reads", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 5}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatalf("expected all concurrent staging reads to succeed with no race, got: %v", err)
	}
	if !strings.Contains(result, `"failed": 0`) {
		t.Fatalf("expected all 5 concurrent reads to succeed with none dropped to null, got %s", result)
	}
	if raw, err := os.ReadFile(filepath.Join(tmp, "target.txt")); err != nil || string(raw) != "edited\n" {
		t.Fatalf("expected the edit applied to the real repo on completion, got %q err=%v", string(raw), err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "leftover-") {
			t.Fatalf("expected no leftover file from a discarded non-edit worktree to reach the real repo, found %s", entry.Name())
		}
	}
}

// TestNonEditAgentSeesStagingViaSubdirTranslation is the regression test for
// the subdir-translation finding: `git worktree add` always checks out the
// WHOLE repo, so a naive fix that pointed a non-edit step directly at the
// staging path (or at an ephemeral worktree's ROOT) would silently relocate
// a monorepo-subdir-scoped agent out of its intended subdirectory. The fix
// must route the same worktreeSubdirCWD translation already used for
// edit/networked worktrees.
func TestNonEditAgentSeesStagingViaSubdirTranslation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	sub := filepath.Join(tmp, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "target.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "sub/target.txt")
	runGit(t, tmp, "commit", "-m", "initial")

	provider := writeEditThenVerifyProvider(t, t.TempDir(),
		`{"ok":true,"command":"verify target","summary":"target edited","output_tail":"","failures":[]}`,
		`{"ok":false,"command":"verify target","summary":"target not edited","output_tail":"missing edit","failures":[{"name":"target","message":"not edited"}]}`)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAKE_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// check()/gate() have no repo option of their own (CheckOptions/
	// GateOptions carry no Repo field) — they always run against r.Run.CWD.
	// Scoping the RUN itself to the subdirectory, matching how a real
	// monorepo-subdir invocation is actually launched (e.g. `--cwd sub`),
	// exercises the same worktreeSubdirCWD translation without depending on
	// a per-call repo override neither builtin verifier supports.
	script := `phase("edit");
agent("edit target file", { label: "editor", mode: "edit", isolation: "worktree", provider: "fake" });
phase("verify");
const result = check("verify target file", { provider: "fake" });
return result;`
	scriptPath, err := WriteRunScript("wf-subdir-staging", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-subdir-staging", Task: "subdir staging", CWD: sub, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) {
		t.Fatalf("expected check() to see the edit via subdir-translated staging, got %s", result)
	}
	if raw, err := os.ReadFile(filepath.Join(sub, "target.txt")); err != nil || string(raw) != "edited\n" {
		t.Fatalf("expected the edit applied to sub/target.txt on completion, got %q err=%v", string(raw), err)
	}
}

func TestStoreAcquireRepoLockExclusiveAndIdempotent(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	holder, ok, err := store.AcquireRepoLock("/repo", "run-a", time.Hour)
	if err != nil || !ok || holder != "run-a" {
		t.Fatalf("expected run-a to acquire the fresh lock, got holder=%q ok=%v err=%v", holder, ok, err)
	}
	// Idempotent re-entry: the same run acquiring again succeeds.
	holder, ok, err = store.AcquireRepoLock("/repo", "run-a", time.Hour)
	if err != nil || !ok || holder != "run-a" {
		t.Fatalf("expected run-a to re-enter its own lock, got holder=%q ok=%v err=%v", holder, ok, err)
	}
	// A different run is refused while the lock is fresh.
	holder, ok, err = store.AcquireRepoLock("/repo", "run-b", time.Hour)
	if err != nil || ok || holder != "run-a" {
		t.Fatalf("expected run-b to be refused with holder=run-a, got holder=%q ok=%v err=%v", holder, ok, err)
	}
	// A release by the wrong run must not remove run-a's row.
	if err := store.ReleaseRepoLock("/repo", "run-b"); err != nil {
		t.Fatal(err)
	}
	holder, ok, err = store.AcquireRepoLock("/repo", "run-b", time.Hour)
	if err != nil || ok || holder != "run-a" {
		t.Fatalf("expected run-a's lock to survive a release attempt by a non-holder, got holder=%q ok=%v err=%v", holder, ok, err)
	}
	// The real holder releasing frees the repo for another run.
	if err := store.ReleaseRepoLock("/repo", "run-a"); err != nil {
		t.Fatal(err)
	}
	holder, ok, err = store.AcquireRepoLock("/repo", "run-b", time.Hour)
	if err != nil || !ok || holder != "run-b" {
		t.Fatalf("expected run-b to acquire the released lock, got holder=%q ok=%v err=%v", holder, ok, err)
	}
}

// TestStoreRepoLockStaleTakeover covers the crash-safety half of bug #34: a
// lock that hasn't been refreshed in longer than staleAfter is reclaimable
// by another run, so a killed workflow process can never permanently block a
// repo.
func TestStoreRepoLockStaleTakeover(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, ok, err := store.AcquireRepoLock("/repo", "run-a", time.Hour); err != nil || !ok {
		t.Fatalf("expected run-a to acquire the fresh lock, ok=%v err=%v", ok, err)
	}
	// A generous staleAfter refuses the second run: run-a's lock is not stale.
	if holder, ok, err := store.AcquireRepoLock("/repo", "run-b", time.Hour); err != nil || ok || holder != "run-a" {
		t.Fatalf("expected run-b refused while the lock is fresh, got holder=%q ok=%v err=%v", holder, ok, err)
	}
	time.Sleep(5 * time.Millisecond)
	// A tiny staleAfter (simulating a crashed run-a with no heartbeat since)
	// lets run-b reclaim the lock.
	holder, ok, err := store.AcquireRepoLock("/repo", "run-b", time.Millisecond)
	if err != nil || !ok || holder != "run-b" {
		t.Fatalf("expected run-b to reclaim the stale lock, got holder=%q ok=%v err=%v", holder, ok, err)
	}
	// run-a can no longer treat itself as the holder once reclaimed.
	if holder, ok, err := store.AcquireRepoLock("/repo", "run-a", time.Hour); err != nil || ok || holder != "run-b" {
		t.Fatalf("expected run-a to be refused after losing the lock, got holder=%q ok=%v err=%v", holder, ok, err)
	}
}

// TestConcurrentStaleTakeoverOnlyOneWins races several independent Store
// handles (separate *sql.DB, i.e. separate processes) to take over the SAME
// stale lock. The compare-and-swap in acquireRepoLockOnce must let exactly one
// win: without it, every racer that read the same stale row would UPDATE and
// believe it holds the lock (bug #34 double-grant).
func TestConcurrentStaleTakeoverOnlyOneWins(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "sessions.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := seed.AcquireRepoLock("/repo", "run-old", time.Hour); err != nil || !ok {
		t.Fatalf("seed acquire: ok=%v err=%v", ok, err)
	}
	// Backdate the seed so it is unambiguously stale under a 1h window, while a
	// fresh takeover (updated_at=now) stays non-stale through the whole race —
	// so the outcome tests the CAS, not staleness timing.
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := seed.db.Exec(`UPDATE workflow_repo_locks SET updated_at=? WHERE repo_root=?`, old, "/repo"); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	const n = 6
	stores := make([]*Store, n)
	for i := range stores {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
		defer s.Close()
	}
	results := make([]bool, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, ok, _ := stores[i].AcquireRepoLock("/repo", fmt.Sprintf("run-%d", i), time.Hour)
			results[i] = ok
		}(i)
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly one stale-takeover winner, got %d", wins)
	}
}

// writeConflictingPatch creates a scratch worktree of repoRoot checked out
// from HEAD, writes newContent to filename, and returns a patch file
// capturing that change. Independent of any Runner/staging state so two
// calls with different newContent for the same filename both generate their
// diff against the SAME original HEAD — a genuine 3-way conflict when both
// are applied in sequence onto one worktree, not just two edits that happen
// to touch the same line via different intermediate states.
func writeConflictingPatch(t *testing.T, dir, repoRoot, filename, newContent string) string {
	t.Helper()
	scratch := filepath.Join(dir, "scratch-"+strconv.Itoa(len(newContent))+"-"+filename)
	runGit(t, repoRoot, "worktree", "add", "--detach", scratch, "HEAD")
	defer func() {
		rm := exec.Command("git", "worktree", "remove", "--force", scratch)
		rm.Dir = repoRoot
		_ = rm.Run()
	}()
	if err := os.WriteFile(filepath.Join(scratch, filename), []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}
	add := exec.Command("git", "add", "-N", "--", ".")
	add.Dir = scratch
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add -N: %v: %s", err, out)
	}
	diffCmd := exec.Command("git", "diff", "--binary")
	diffCmd.Dir = scratch
	raw, err := diffCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(dir, filename+"-"+strconv.Itoa(len(newContent))+".patch")
	if err := os.WriteFile(patchPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return patchPath
}

// TestMergeIntoStagingComposesTwoNonConflictingEditsToTheSameFile is the
// regression test for a bug discovered while writing the conflict-restore
// test below: the ORIGINAL applyWorktreeDiff ran `git reset -q` after each
// successful --3way apply to unstage it (so a later git-diff-based capture
// would still see it as an uncommitted change). That left the index clean
// (matching HEAD) while the working tree carried the accumulated edit — and
// `git apply --3way`'s fallback path refuses with "does not match index"
// against ANY file with pre-existing unstaged changes, even when the new
// patch's own target lines don't overlap with the first edit at all. That
// made mergeIntoStaging fail outright for two agents editing the same file
// in ANY way, not just a genuine conflict — a much more basic break than
// the conflict-handling finding. The fix commits each merge instead of
// leaving it uncommitted, so the index is always settled before the next
// apply.
func TestMergeIntoStagingComposesTwoNonConflictingEditsToTheSameFile(t *testing.T) {
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "shared.txt"), []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "shared.txt")
	runGit(t, tmp, "commit", "-m", "initial")

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.CreateRun(Run{ID: "wf-same-file-compose", Task: "same file compose", CWD: tmp, ScriptPath: "unused.js"})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: store, Run: run}

	patchDir := t.TempDir()
	patchA := writeConflictingPatch(t, patchDir, tmp, "shared.txt", "line1\nCHANGED-A\nline3\nline4\nline5\n")
	if err := r.mergeIntoStaging(tmp, patchA); err != nil {
		t.Fatalf("first merge should succeed: %v", err)
	}
	// Non-overlapping change: touches line4, not line2.
	patchC := writeConflictingPatch(t, patchDir, tmp, "shared.txt", "line1\nline2\nline3\nCHANGED-C\nline5\n")
	if err := r.mergeIntoStaging(tmp, patchC); err != nil {
		t.Fatalf("expected a second, non-overlapping edit to the same file to compose cleanly, got: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(r.stagingPathFor(tmp), "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "line1\nCHANGED-A\nline3\nCHANGED-C\nline5\n"
	if string(raw) != want {
		t.Fatalf("expected both edits composed, got %q want %q", string(raw), want)
	}
}

// TestMergeIntoStagingRestoresSnapshotOnConflict is the regression test for
// the adversarial-review finding that a failed `git apply --3way` (a real
// conflict between two edit-intent agents touching the same lines) left
// staging's shared, long-lived worktree with literal conflict markers and a
// dirty index — silently corrupting every later mergeIntoStaging/
// seedFromStaging read for the rest of the run. The fix snapshots staging
// before each merge attempt and restores it on failure.
func TestMergeIntoStagingRestoresSnapshotOnConflict(t *testing.T) {
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "shared.txt"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "shared.txt")
	runGit(t, tmp, "commit", "-m", "initial")

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.CreateRun(Run{ID: "wf-merge-conflict", Task: "merge conflict", CWD: tmp, ScriptPath: "unused.js"})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: store, Run: run}

	patchDir := t.TempDir()
	patchA := writeConflictingPatch(t, patchDir, tmp, "shared.txt", "line1\nCHANGED-A\nline3\n")
	if err := r.mergeIntoStaging(tmp, patchA); err != nil {
		t.Fatalf("first merge should succeed cleanly: %v", err)
	}
	stagingPath := r.stagingPathFor(tmp)
	stagingBase := r.stagingEntry(tmp).base
	before, err := diffAgainstBase(stagingPath, stagingBase)
	if err != nil {
		t.Fatal(err)
	}

	patchB := writeConflictingPatch(t, patchDir, tmp, "shared.txt", "line1\nCHANGED-B\nline3\n")
	if err := r.mergeIntoStaging(tmp, patchB); err == nil {
		t.Fatal("expected the conflicting second merge to fail")
	}

	after, err := diffAgainstBase(stagingPath, stagingBase)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("expected staging restored to its pre-conflict snapshot after the failed merge, got a different diff:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	raw, err := os.ReadFile(filepath.Join(stagingPath, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "<<<<<<<") {
		t.Fatalf("expected no conflict markers left in staging after the restore, got:\n%s", raw)
	}
}

// TestNonEditWorktreeDiscardedWhenSeedFromStagingFails is the regression
// test for the adversarial-review finding that the real dispatch path's
// containment-worktree discard defer was registered AFTER the
// seedFromStaging call, so a seed failure returned before the defer ever
// existed and leaked the freshly created worktree on disk permanently.
func TestNonEditWorktreeDiscardedWhenSeedFromStagingFails(t *testing.T) {
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "file.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "file.txt")
	runGit(t, tmp, "commit", "-m", "initial")

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.CreateRun(Run{ID: "wf-seed-fail-leak", Task: "seed fail leak", CWD: tmp, ScriptPath: "unused.js"})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: store, Run: run}
	// A broken staging entry (pointing at a path that is not a valid git
	// worktree at all) makes seedFromStaging fail deterministically without
	// needing to construct a genuine merge conflict — the specific trigger
	// doesn't matter for this test, only that ANY seedFromStaging failure
	// must not leak the fresh containment worktree runAgentCommand created
	// immediately before calling it.
	r.staging = map[string]*stagingWorktree{
		tmp: {path: filepath.Join(tmp, "not-a-real-worktree")},
	}

	agent := &Agent{ID: "check-agent-1", RunID: run.ID, Repo: tmp, Mode: "test"}
	_, _, worktree, err := r.runAgentCommand(context.Background(), agent, AgentOptions{})
	if err == nil {
		t.Fatal("expected seedFromStaging's failure to propagate")
	}
	if worktree == "" {
		t.Fatal("expected runAgentCommand to report the worktree path it had created before the seed failure")
	}
	if _, statErr := os.Stat(worktree); !os.IsNotExist(statErr) {
		t.Fatalf("expected the containment worktree removed from disk after the seed failure, stat err=%v", statErr)
	}
	list := exec.Command("git", "worktree", "list", "--porcelain")
	list.Dir = tmp
	raw, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "worktree "); got != 1 {
		t.Fatalf("expected only the main worktree registered after the leak fix, got %d:\n%s", got, string(raw))
	}
}

// TestStubPathNonEditWorktreeDiscardedWhenSeedFromStagingFails is the same
// regression as above for the PALLIUM_WORKFLOW_AGENT_STUB test-stub
// dispatch path, which had no discard mechanism at all for this case.
func TestStubPathNonEditWorktreeDiscardedWhenSeedFromStagingFails(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "file.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "file.txt")
	runGit(t, tmp, "commit", "-m", "initial")

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.CreateRun(Run{ID: "wf-seed-fail-leak-stub", Task: "seed fail leak stub", CWD: tmp, ScriptPath: "unused.js"})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: store, Run: run}
	r.staging = map[string]*stagingWorktree{
		tmp: {path: filepath.Join(tmp, "not-a-real-worktree")},
	}

	agent := &Agent{ID: "check-agent-2", RunID: run.ID, Repo: tmp, Mode: "test"}
	_, _, worktree, err := r.runAgentCommand(context.Background(), agent, AgentOptions{})
	if err == nil {
		t.Fatal("expected seedFromStaging's failure to propagate")
	}
	if worktree == "" {
		t.Fatal("expected runAgentCommand to report the worktree path it had created before the seed failure")
	}
	if _, statErr := os.Stat(worktree); !os.IsNotExist(statErr) {
		t.Fatalf("expected the containment worktree removed from disk after the seed failure, stat err=%v", statErr)
	}
	list := exec.Command("git", "worktree", "list", "--porcelain")
	list.Dir = tmp
	raw, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "worktree "); got != 1 {
		t.Fatalf("expected only the main worktree registered after the leak fix, got %d:\n%s", got, string(raw))
	}
}

// TestRepoLockContentionInsideParallelHaltsRunInsteadOfBeingDropped is the
// regression test for the adversarial-review finding that acquireRepoLock's
// contention error had no distinguishable sentinel, so parallel()/pipeline()
// silently dropped every contended edit-intent agent call to null via the
// same path as an ordinary per-item provider failure — masking cross-run
// lock contention as N unrelated failures instead of halting the run with
// one clear cause.
func TestRepoLockContentionInsideParallelHaltsRunInsteadOfBeingDropped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"edited"}`)
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "note.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "note.txt")
	runGit(t, tmp, "commit", "-m", "initial")

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `const results = parallel(["a", "b"], item => agent("edit " + item, { label: item, mode: "edit", isolation: "worktree" }));
return { count: results.length };`
	scriptPath, err := WriteRunScript("wf-lock-parallel", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-lock-parallel", Task: "lock contention in parallel", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}

	canonical, err := gitlog.CanonicalRepoRoot(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.AcquireRepoLock(canonical, "other-run", time.Hour); err != nil || !ok {
		t.Fatalf("seed contending lock: ok=%v err=%v", ok, err)
	}

	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil {
		t.Fatal("expected the run to fail outright on lock contention, not silently drop both items to null")
	}
	if !strings.Contains(err.Error(), "workflow repo edit lock contended") {
		t.Fatalf("expected the lock-contention sentinel to surface as the run's error, got: %v", err)
	}
}

// TestConcurrentEditRunsOnSameRepoOneFailsFast is bug #34's non-negotiable
// race test: two concurrent edit runs against the same repo, using two
// separate Store handles onto the same sqlite file (mirroring two separate
// `pallium workflow run` processes sharing one DB). Exactly one must proceed
// and apply its edit; the other must fail fast with the lock error before
// ever creating a worktree.
func TestConcurrentEditRunsOnSameRepoOneFailsFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"edited"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_FILE", "note.txt")
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_CONTENT", "changed\n")
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "50")
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
	store1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store1.Close()
	store2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	script := `agent("edit note", { label: "editor", mode: "edit", isolation: "worktree" }); return { ok: true };`
	scriptPath, err := WriteRunScript("wf-lock-a", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	runA, err := store1.CreateRun(Run{ID: "wf-lock-a", Task: "lock a", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	runB, err := store1.CreateRun(Run{ID: "wf-lock-b", Task: "lock b", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = (&Runner{Store: store1, Run: runA, MaxAgents: 10}).Execute(context.Background(), script, nil)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = (&Runner{Store: store2, Run: runB, MaxAgents: 10}).Execute(context.Background(), script, nil)
	}()
	wg.Wait()

	succeeded, lockFailed := 0, 0
	for _, callErr := range errs {
		switch {
		case callErr == nil:
			succeeded++
		case strings.Contains(callErr.Error(), "another edit run holds"):
			lockFailed++
		default:
			t.Fatalf("unexpected error: %v", callErr)
		}
	}
	if succeeded != 1 || lockFailed != 1 {
		t.Fatalf("expected exactly one success and one lock failure, got succeeded=%d lockFailed=%d errs=%v", succeeded, lockFailed, errs)
	}
	if raw, err := os.ReadFile(filepath.Join(tmp, "note.txt")); err != nil || string(raw) != "changed\n" {
		t.Fatalf("expected the winning run's edit applied to the repo, got %q err=%v", string(raw), err)
	}
	// The repo lock must not outlive either Execute call. Look it up under
	// the same canonical key acquireRepoLock uses, not the raw tmp path.
	canonical, err := gitlog.CanonicalRepoRoot(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if holder, ok, err := store1.AcquireRepoLock(canonical, "run-c", time.Hour); err != nil || !ok || holder != "run-c" {
		t.Fatalf("expected the repo lock released after both runs finished, got holder=%q ok=%v err=%v", holder, ok, err)
	}
	if err := store1.ReleaseRepoLock(canonical, "run-c"); err != nil {
		t.Fatal(err)
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
	t.Setenv("HOME", t.TempDir())
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
	if len(agents) != 4 || agents[1].Mode != "edit" {
		t.Fatalf("expected check/fix/check/patch agents, got %+v", agents)
	}
	patchAgent := agents[3]
	if patchAgent.Label != "green-patch" || patchAgent.Provider != "internal" || patchAgent.Status != "completed" || patchAgent.PatchPath == "" {
		t.Fatalf("expected completed internal patch agent, got %+v", patchAgent)
	}
	if _, err := os.Stat(patchAgent.Worktree); !os.IsNotExist(err) {
		t.Fatalf("expected loop worktree removed after patch capture, stat err=%v", err)
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

func TestRunnerAgentGateApprovesAndContinues(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"approved":true,"reason":"checks passed","evidence":[]}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("gate");
const verdict = gate("approve-patches", "verify before continuing");
const result = agent("after gate", { label: "after" });
return { verdict, result };`
	scriptPath, err := WriteRunScript("wf-gate", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-gate", Task: "gate", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	gates, err := store.ListGates(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].Status != "approved" {
		t.Fatalf("expected approved gate, got %+v", gates)
	}
	if !strings.Contains(result, `"approved": true`) || !strings.Contains(result, "checks passed") {
		t.Fatalf("expected agent-approved gate result, got %s", result)
	}
}

func TestRunnerAgentGateRejectsByDefault(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"approved":false,"reason":"tests are failing","evidence":[]}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("gate");
gate("approve-patches", "verify before continuing");
return agent("after gate", { label: "after" });`
	scriptPath, err := WriteRunScript("wf-gate-reject", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-gate-reject", Task: "gate reject", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), `workflow gate "approve-patches" rejected by agent`) {
		t.Fatalf("expected agent gate rejection, got %v", err)
	}
	gates, err := store.ListGates(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].Status != "rejected" {
		t.Fatalf("expected rejected gate, got %+v", gates)
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

func TestRunnerRemovesWorktreeAfterPatchCapture(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"edited"}`)
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
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("edit"); return agent("edit note", { label: "editor", mode: "edit", isolation: "worktree" });`
	scriptPath, err := WriteRunScript("wf-worktree-gc", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-worktree-gc", Task: "worktree cleanup", CWD: tmp, ScriptPath: scriptPath})
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
	if len(agents) != 1 || agents[0].PatchPath == "" || agents[0].Worktree == "" {
		t.Fatalf("expected one edit agent with patch and worktree, got %+v", agents)
	}
	if _, err := os.Stat(agents[0].PatchPath); err != nil {
		t.Fatalf("expected durable patch file: %v", err)
	}
	if _, err := os.Stat(agents[0].Worktree); !os.IsNotExist(err) {
		t.Fatalf("expected worktree removed after patch capture, stat err=%v", err)
	}
	list := exec.Command("git", "worktree", "list", "--porcelain")
	list.Dir = tmp
	raw, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "worktree "); got != 1 {
		t.Fatalf("expected only the main worktree registered, got %d:\n%s", got, string(raw))
	}
	if content, err := os.ReadFile(filepath.Join(tmp, "note.txt")); err != nil || string(content) != "changed by workflow\n" {
		t.Fatalf("expected patch applied on completion, got %q err=%v", string(content), err)
	}
}

func TestRunnerUntilGreenConvergesInLoopWorktreeAndAppliesPatch(t *testing.T) {
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
	providerScript := filepath.Join(t.TempDir(), "loop-provider.sh")
	if err := os.WriteFile(providerScript, []byte(`#!/bin/sh
if [ "$PALLIUM_WORKFLOW_MODE" = "edit" ]; then
  printf 'fixed\n' > marker.txt
  printf '{"summary":"created marker"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
elif [ -f marker.txt ]; then
  printf '{"ok":true,"command":"check marker","summary":"marker present","output_tail":"","failures":[]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
else
  printf '{"ok":false,"command":"check marker","summary":"marker missing","output_tail":"no marker","failures":[{"name":"marker","message":"missing"}]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_LOOP_COMMAND", providerScript)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("verify");
const result = verify.untilGreen("check marker", { label: "green", maxRounds: 2, provider: "loop" });
return result;`
	scriptPath, err := WriteRunScript("wf-until-green-worktree", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-until-green-worktree", Task: "until green worktree", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) || !strings.Contains(result, `"fixed": true`) {
		t.Fatalf("expected converged fix loop, got %s", result)
	}
	if content, err := os.ReadFile(filepath.Join(tmp, "marker.txt")); err != nil || string(content) != "fixed\n" {
		t.Fatalf("expected combined patch applied to repo on completion, got %q err=%v", string(content), err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	patchAgent := agents[len(agents)-1]
	if patchAgent.Label != "green-patch" || patchAgent.Status != "completed" || patchAgent.PatchPath == "" {
		t.Fatalf("expected registered untilGreen patch agent, got %+v", patchAgent)
	}
	if raw, err := os.ReadFile(patchAgent.PatchPath); err != nil || !strings.Contains(string(raw), "marker.txt") {
		t.Fatalf("expected combined patch to include marker.txt, err=%v patch=%s", err, string(raw))
	}
	if _, err := os.Stat(patchAgent.Worktree); !os.IsNotExist(err) {
		t.Fatalf("expected loop worktree removed after patch capture, stat err=%v", err)
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

func TestPipelineSupportsAsyncStages(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["alpha"],
  async item => item.toUpperCase(),
  async prev => prev + " DONE",
  prev => agent("inspect " + prev, { label: "inspect" })
);
return { result: results[0].prompt };`
	scriptPath, err := WriteRunScript("wf-pipeline-async", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pipeline-async", Task: "pipeline async", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "inspect ALPHA DONE") {
		t.Fatalf("async pipeline stage did not resolve before next stage: %s", result)
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

func TestParallelMapperThrowDropsOnlyThatItem(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["keep", "drop", "also-keep"], item => {
  if (item === "drop") {
    throw new Error("drop this item");
  }
  return agent("inspect " + item, { label: item });
});
return { results };`
	scriptPath, err := WriteRunScript("wf-parallel-throw", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel-throw", Task: "parallel throw", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "inspect keep") || !strings.Contains(result, "null") || !strings.Contains(result, "inspect also-keep") {
		t.Fatalf("parallel mapper throw did not preserve item order with null drop: %s", result)
	}
}

func TestParallelMapperFatalErrorAbortsRun(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// "drop" throws an ordinary error and must still be nulled out, but "fatal"
	// synchronously calls verify.untilGreen(), which runs an agent past the
	// MaxAgents cap (already exhausted by the earlier warmup agent() call).
	// That fatal error must propagate and fail the whole run instead of being
	// swallowed into a null result.
	script := `phase("parallel-fatal");
agent("warmup");
const results = await parallel(["drop", "fatal"], item => {
  if (item === "drop") {
    throw new Error("drop this item");
  }
  return verify.untilGreen("stub-check", { maxRounds: 0 });
});
return { results };`
	scriptPath, err := WriteRunScript("wf-parallel-fatal", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel-fatal", Task: "parallel fatal", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 1}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded max agents") {
		t.Fatalf("expected fatal mapper error to abort the run, got %v", err)
	}
}

// TestParallelThunksFormThrowDropsOnlyThatItem is the crash-isolation-parity
// regression test for parallel(arrayOfThunks) — the no-mapper shape (each
// item is itself a zero-arg function) that Ultracode-native agent reflexes
// reach for by default. Before this fix, a thunk that threw (synchronously,
// e.g. from touching an agent()'s return value directly instead of via the
// deferred parallel()-capture/replace mechanism — a natural mistake for code
// that doesn't know about that indirection) did a bare `panic(err)` and
// killed the whole run, unlike the documented parallel(items, fn) mapper
// form (see TestParallelMapperThrowDropsOnlyThatItem), which already
// isolated an ordinary throw to a null result. The thunks form must match.
func TestParallelThunksFormThrowDropsOnlyThatItem(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel-thunks");
const results = await parallel([
  () => agent("inspect keep1", { label: "keep1" }),
  () => { throw new Error("drop this item"); },
  () => agent("inspect keep2", { label: "keep2" })
]);
return { results };`
	scriptPath, err := WriteRunScript("wf-parallel-thunks-throw", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel-thunks-throw", Task: "parallel thunks throw", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatalf("expected the run to complete with the failing thunk dropped to null, got: %v", err)
	}
	if !strings.Contains(result, "inspect keep1") || !strings.Contains(result, "null") || !strings.Contains(result, "inspect keep2") {
		t.Fatalf("parallel thunks-form throw did not preserve item order with null drop: %s", result)
	}
}

// TestParallelThunksFormFatalErrorAbortsRun mirrors
// TestParallelMapperFatalErrorAbortsRun for the thunks form: a genuinely
// fatal error (budget/max-agents/interrupt) synchronously raised inside a
// thunk must still abort the whole run rather than being swallowed into a
// null result — crash-isolation parity means matching the mapper form's
// fatal-vs-transient classification exactly, not just "never fail".
func TestParallelThunksFormFatalErrorAbortsRun(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel-thunks-fatal");
agent("warmup");
const results = await parallel([
  () => { throw new Error("drop this item"); },
  () => verify.untilGreen("stub-check", { maxRounds: 0 })
]);
return { results };`
	scriptPath, err := WriteRunScript("wf-parallel-thunks-fatal", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel-thunks-fatal", Task: "parallel thunks fatal", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 1}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded max agents") {
		t.Fatalf("expected fatal thunk error to abort the run, got %v", err)
	}
}

// TestParallelMapperInterruptPreservesPausedStatus covers pausing a run
// while it's synchronously inside a parallel() mapper (e.g. via
// verify.untilGreen()): the run must end up "paused", not "failed".
//
// jsParallel's fatalCause()-based rethrow uses r.throwable(fatal) rather
// than fatal.Error() alone specifically so the interrupt sentinel token
// survives if this rethrow ever needs to cross another JS call frame that
// does get stack-decorated by goja (the way an inner verify.untilGreen()
// call already does — see unwrapMapperThrow and
// TestUnwrapMapperThrowRecoversInterruptedSentinel for that decoration).
// This specific test's exact crossing isn't decorated in practice, so
// isWorkflowPausedError's own text-equality fallback already recognizes it
// even without throwable — confirmed by reverting to fatal.Error() locally
// and re-running, which still passes. The throwable() call is kept anyway
// as the same defensive, consistent pattern used at every other point in
// this file where a Go error is thrown into goja (see jsVerify), rather
// than relying on a second, independent text-matching path.
func TestParallelMapperInterruptPreservesPausedStatus(t *testing.T) {
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
	// Each check round sleeps briefly and always reports not-green, giving
	// the test time to mark the run paused between rounds so the next
	// round's check hits ensureNotStopped synchronously inside the
	// parallel() mapper (verify.untilGreen never actually converges here;
	// the pause always wins the race first).
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_LOOP_COMMAND", `sleep 0.2; printf '{"ok":false,"command":"check marker","summary":"missing","output_tail":"","failures":[]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel-pause");
const results = parallel(["only"], item =>
  verify.untilGreen("check marker", { label: "green", maxRounds: 5, provider: "loop" })
);
return { results };`
	scriptPath, err := WriteRunScript("wf-parallel-pause", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel-pause", Task: "parallel pause", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, execErr := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
		errCh <- execErr
	}()
	deadline := time.After(2 * time.Second)
	for {
		agents, listErr := store.ListAgents(run.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(agents) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the first check round to start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := store.SetRunStatus(run.ID, "paused", "", "test pause"); err != nil {
		t.Fatal(err)
	}
	select {
	case execErr := <-errCh:
		if !errors.Is(execErr, ErrWorkflowPaused) {
			t.Fatalf("expected ErrWorkflowPaused, got %v", execErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not pause after run status changed to paused")
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "paused" {
		t.Fatalf("expected paused run status (not failed), got %+v", snapshot.Run)
	}
}

// TestUnwrapMapperThrowRecoversInterruptedSentinel guards against a subtler
// variant of the same swallowing bug: when a parallel() mapper throws
// synchronously because the workflow was stopped/paused (not just a budget
// or max-agents cap), goja wraps the thrown value in a *goja.Exception whose
// Error() appends call-site stack info (e.g. " at ...(native)"). That extra
// text breaks the exact-string comparison isWorkflowStoppedError/
// isWorkflowPausedError use for ErrWorkflowStopped/ErrWorkflowPaused, so
// isWorkflowFatalAgentError alone misses it. unwrapMapperThrow must recover
// the original message so the fatal check still matches.
func TestUnwrapMapperThrowRecoversInterruptedSentinel(t *testing.T) {
	vm := goja.New()
	if err := vm.Set("boom", func(call goja.FunctionCall) goja.Value {
		// Mirrors how jsVerify's untilGreen (and other JS bindings) surface a
		// Go error to a script: panic with the error text as a plain JS value.
		panic(vm.ToValue(ErrWorkflowStopped.Error()))
	}); err != nil {
		t.Fatal(err)
	}
	// jsParallel never calls a native-bound function directly as the
	// "mapper" - it calls a real JS function, which in turn calls a native
	// binding like verify.untilGreen(). That extra JS call frame is what
	// makes goja attach stack info to the exception's Error() text, so
	// reproduce it here instead of calling "boom" directly.
	if _, err := vm.RunString("var mapperLike = function() { return boom(); };"); err != nil {
		t.Fatal(err)
	}
	fn, ok := goja.AssertFunction(vm.Get("mapperLike"))
	if !ok {
		t.Fatal("expected mapperLike to be callable")
	}
	_, err := fn(goja.Undefined())
	if err == nil {
		t.Fatal("expected boom() to return an error")
	}
	if isWorkflowFatalAgentError(err) {
		t.Fatalf("expected the raw goja exception to not classify as fatal without unwrapping (documents why unwrapMapperThrow is needed), got %v", err)
	}
	cause := unwrapMapperThrow(err)
	if !isWorkflowFatalAgentError(cause) {
		t.Fatalf("expected unwrapMapperThrow(%v) to recover a fatal interrupted error, got %v", err, cause)
	}
	if cause.Error() != ErrWorkflowStopped.Error() {
		t.Fatalf("expected unwrapped cause to carry the clean stopped sentinel message, got %q", cause.Error())
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

func TestWorkflowBudgetSpentUpdatesWithoutLimit(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_COST_USD", "0.25")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("budget");
const before = { total: budget.total, spent: budget.spent() };
await agent("one", { label: "one" });
const after = { total: budget.total, spent: budget.spent() };
return { before, after };`
	scriptPath, err := WriteRunScript("wf-budget-no-limit", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-budget-no-limit", Task: "budget no limit", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"total": null`, `"spent": 0`, `"spent": 0.25`} {
		if !strings.Contains(result, want) {
			t.Fatalf("uncapped budget result missing %s: %s", want, result)
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
  schema: { type: "object", properties: { source: { type: "string" }, changed: { type: "boolean" } }, required: ["source"] }
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
			t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first","changed":false}`)
			first, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), tc.first, tc.firstArgs)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(first, `"source": "first"`) {
				t.Fatalf("unexpected first result: %s", first)
			}
			t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second","changed":true}`)
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

func TestRunnerReusesCompletedAgentPrefixAfterScriptTailChange(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	firstScript := `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return result;`
	secondScript := `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return { source: result.source, changed_script: true };`
	scriptPath, err := WriteRunScript("wf-cache-script-prefix", tmp, firstScript)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-cache-script-prefix", Task: "cache prefix", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first"}`)
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), firstScript, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second"}`)
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), secondScript, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"source": "first"`) || !strings.Contains(result, `"changed_script": true`) {
		t.Fatalf("expected cached prefix with edited tail, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected one cached prefix agent, got %+v", agents)
	}
}

func TestRunnerDoesNotCacheIdenticalAgentCallsWithinSameExecution(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_SEQUENCE", `[
		"{\"source\":\"first\"}",
		"{\"source\":\"second\"}"
	]`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"fallback"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("loop");
const first = await agent("same prompt", { label: "same" });
const second = await agent("same prompt", { label: "same" });
return { first: first.source, second: second.source };`
	scriptPath, err := WriteRunScript("wf-identical-calls", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-identical-calls", Task: "identical calls", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"first": "first"`) || !strings.Contains(result, `"second": "second"`) {
		t.Fatalf("expected identical calls to execute independently, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0].CallIndex != 1 || agents[1].CallIndex != 2 {
		t.Fatalf("expected two positional agent calls, got %+v", agents)
	}
}

func TestRunnerResumePreservesMaxAgentCap(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	firstScript := `phase("scan");
const first = await agent("first", { label: "first" });
return first;`
	secondScript := `phase("scan");
const first = await agent("first", { label: "first" });
const second = await agent("second", { label: "second" });
return { first, second };`
	scriptPath, err := WriteRunScript("wf-agent-cap-resume", tmp, firstScript)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-agent-cap-resume", Task: "agent cap", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 1}).Execute(context.Background(), firstScript, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second"}`)
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 1}).Execute(context.Background(), secondScript, nil)
	if err == nil || !strings.Contains(err.Error(), "workflow exceeded max agents") {
		t.Fatalf("expected resumed run to preserve max agent cap, got %v", err)
	}
}

func TestRunnerResumePreservesBudgetCap(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_COST_USD", "0.01")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	firstScript := `phase("budget");
const first = await agent("first", { label: "first" });
return first;`
	secondScript := `phase("budget");
const first = await agent("first", { label: "first" });
const second = await agent("second", { label: "second" });
return { first, second };`
	scriptPath, err := WriteRunScript("wf-budget-resume", tmp, firstScript)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-budget-resume", Task: "budget resume", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "0.01"}).Execute(context.Background(), firstScript, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second"}`)
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "0.01"}).Execute(context.Background(), secondScript, nil)
	if err == nil || !strings.Contains(err.Error(), "workflow budget exhausted") {
		t.Fatalf("expected resumed run to preserve budget cap, got %v", err)
	}
}

func TestRunnerDoesNotDuplicateDecisionOnResume(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("decide");
return pallium.decisions.record("Stable decision", "Do not duplicate this.", "resume");`
	scriptPath, err := WriteRunScript("wf-decision-idempotent", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-decision-idempotent", Task: "decision idempotent", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
			t.Fatal(err)
		}
	}
	decisions, err := store.SearchDecisions("Stable decision", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected one idempotent decision, got %+v", decisions)
	}
}

func TestParallelAgentFailureReturnsNullWithoutCancellingSiblings(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_TEST_COMMAND", `PALLIUM_WORKFLOW_PROMPT="$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")"; if printf '%s' "$PALLIUM_WORKFLOW_PROMPT" | grep -q bad; then echo "failed intentionally" >&2; exit 7; fi; printf '{"prompt":"%s"}' "$PALLIUM_WORKFLOW_PROMPT" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["good", "bad", "also-good"], item =>
  agent("worker " + item, { label: item, provider: "test" })
);
return { results };`
	scriptPath, err := WriteRunScript("wf-parallel-null", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-parallel-null", Task: "parallel null", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 3}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "worker good") || !strings.Contains(result, "null") || !strings.Contains(result, "worker also-good") {
		t.Fatalf("expected failed parallel agent to become null while siblings complete, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 {
		t.Fatalf("expected three agent records, got %+v", agents)
	}
	failed := 0
	for _, agent := range agents {
		if agent.Status == "failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("expected one failed agent record, got %+v", agents)
	}
}

func TestRunnerValidatesStructuredOutputFromCustomProvider(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_TEST_COMMAND", `printf '{"summary":123}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("schema");
return await agent("structured", {
  label: "structured",
  provider: "test",
  schema: {
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"]
  }
});`
	scriptPath, err := WriteRunScript("wf-provider-schema", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-provider-schema", Task: "provider schema", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "agent output does not match schema") {
		t.Fatalf("expected local schema validation failure, got %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Status != "failed" {
		t.Fatalf("expected failed schema agent, got %+v", agents)
	}
}

func TestSchemaFailedEditAgentPreservesAndAppliesPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"status":"edited"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_FILE", "note.txt")
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_CONTENT", "changed\n")
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "note.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "note.txt")
	runGit(t, tmp, "commit", "-m", "initial")
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("edit");
const results = await parallel([
  () => agent("edit invalid", {
    label: "editor",
    mode: "edit",
    isolation: "worktree",
    schema: { type: "object", properties: { status: { type: "integer" } }, required: ["status"] }
  })
]);
return results;`
	scriptPath, err := WriteRunScript("wf-schema-patch", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-schema-patch", Task: "schema patch", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatalf("parallel schema failure should be nonfatal, got %v", err)
	}
	_ = result
	// The structured output failed schema validation, but the edit is completed
	// WORK: the patch must LAND (the data-loss bug is fixed) and the schema
	// failure must be reported in the run failures, not silently discarded.
	raw, err := os.ReadFile(filepath.Join(tmp, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "changed\n" {
		t.Fatalf("schema-failed edit patch was discarded; note.txt=%q (want %q)", string(raw), "changed\n")
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Status == "failed" || agents[0].PatchPath == "" {
		t.Fatalf("expected a completed edit agent with a preserved patch, got %+v", agents)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Run.Failures) != 1 {
		t.Fatalf("expected the schema failure to be reported in run failures, got %+v", snapshot.Run.Failures)
	}
}

func TestRunnerValidatesNullableObjectSchemasRecursively(t *testing.T) {
	schema := map[string]any{
		"type": []any{"object", "null"},
		"properties": map[string]any{
			"summary": map[string]any{"type": "string"},
		},
		"required":             []any{"summary"},
		"additionalProperties": false,
	}
	if _, err := parseAgentOutputWithSchema(`{}`, schema); err == nil || !strings.Contains(err.Error(), "missing required property") {
		t.Fatalf("expected nullable object schema to enforce required fields, got %v", err)
	}
	if _, err := parseAgentOutputWithSchema(`{"summary":"ok"}`, schema); err != nil {
		t.Fatalf("expected object with required field to pass: %v", err)
	}
	if _, err := parseAgentOutputWithSchema(`null`, schema); err != nil {
		t.Fatalf("expected null branch to pass: %v", err)
	}
}

// TestValidateSchemaValueEnforcesEnum is the regression test for a real
// gap an adversarial review found in #52: teamDecisionSchema's own status
// field has always declared enum:["active","idle","blocked"], but nothing
// in validateSchemaValue ever read the "enum" keyword at all — an out-of-
// enum value like "done" passed validation and would have persisted a
// status the rest of the codebase never branches on. Covers exactly what
// was asked: the real teamDecisionSchema shape, all three legal values,
// and an enum on an unrelated field to prove it's a general schema
// capability, not a status-only special case.
func TestValidateSchemaValueEnforcesEnum(t *testing.T) {
	statusSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []any{"active", "idle", "blocked"}},
		},
		"required": []any{"status"},
	}
	if _, err := parseAgentOutputWithSchema(`{"status":"done"}`, statusSchema); err == nil {
		t.Fatal("expected an out-of-enum status value to be rejected")
	} else if !strings.Contains(err.Error(), "must be one of") {
		t.Fatalf("expected an enum violation message, got %v", err)
	}
	for _, legal := range []string{"active", "idle", "blocked"} {
		if _, err := parseAgentOutputWithSchema(`{"status":"`+legal+`"}`, statusSchema); err != nil {
			t.Fatalf("expected legal enum value %q to pass, got %v", legal, err)
		}
	}

	genericSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"priority": map[string]any{"type": "string", "enum": []any{"low", "medium", "high"}},
		},
	}
	if _, err := parseAgentOutputWithSchema(`{"priority":"urgent"}`, genericSchema); err == nil {
		t.Fatal("expected enum enforcement on a non-status field, not just status")
	}
	if _, err := parseAgentOutputWithSchema(`{"priority":"high"}`, genericSchema); err != nil {
		t.Fatalf("expected a legal value on a non-status field to pass, got %v", err)
	}
}

func TestPipelineCallIndexesAreDeterministicByItemAndStage(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first","prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["slow", "fast"],
  item => agent("stage1 " + item, { label: "stage1-" + item }),
  result => agent("stage2 " + result.prompt, { label: "stage2-" + result.prompt })
);
return results;`
	scriptPath, err := WriteRunScript("wf-pipeline-indexes", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pipeline-indexes", Task: "pipeline indexes", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 4}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, agent := range agents {
		got[agent.Label] = agent.CallIndex
	}
	expected := map[string]int{
		"stage1-slow":        pipelineCallIndex(1, 0, 0, 0),
		"stage1-fast":        pipelineCallIndex(1, 0, 1, 0),
		"stage2-stage1 slow": pipelineCallIndex(1, 1, 0, 0),
		"stage2-stage1 fast": pipelineCallIndex(1, 1, 1, 0),
	}
	for label, want := range expected {
		if got[label] != want {
			t.Fatalf("call index for %s = %d, want %d; all=%+v", label, got[label], want, agents)
		}
	}
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second","prompt":"{{PROMPT}}"}`)
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 4}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "second") {
		t.Fatalf("expected deterministic pipeline cache reuse, got %s", result)
	}
}

func TestStoreBackfillsLegacyAgentCallIndexes(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-legacy-index", Task: "legacy", CWD: tmp, ScriptPath: "workflow.js"})
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"first", "second"} {
		if _, err := store.db.Exec(`INSERT INTO workflow_agents(id,run_id,call_index,prompt,mode,status,output,created_at,updated_at,completed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			NewID("agent"), run.ID, 0, prompt, "read-only", "completed", `{"ok":true}`, nowString(), nowString(), nowString()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0].CallIndex != 1 || agents[1].CallIndex != 2 {
		t.Fatalf("expected legacy call indexes to be backfilled, got %+v", agents)
	}
}

// TestStoreBackfillsLegacyAgentTimeoutExplicit guards against a regression
// where a pre-upgrade run with a positive stored agent_timeout_seconds, but
// no agent_timeout_explicit column yet, would silently stop honoring its
// custom timeout on resume: resume only forwards the stored value when
// AgentTimeoutExplicit is true, and that column's own DEFAULT is 0 for rows
// written before it existed.
func TestStoreBackfillsLegacyAgentTimeoutExplicit(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-legacy-timeout", Task: "legacy timeout", CWD: tmp, ScriptPath: "workflow.js"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a row written before agent_timeout_explicit existed: a
	// positive stored timeout with the column's own bare default (0).
	if _, err := store.db.Exec(`UPDATE workflow_runs SET agent_timeout_seconds=45, agent_timeout_explicit=0 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reopened, err := store.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.AgentTimeout != 45 || !reopened.AgentTimeoutExplicit {
		t.Fatalf("expected legacy positive timeout to be backfilled as explicit, got %+v", reopened)
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

func TestAgentTimeoutInParallelBecomesNull(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_SLOWPOKE_COMMAND", `PALLIUM_WORKFLOW_PROMPT="$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")"; if printf '%s' "$PALLIUM_WORKFLOW_PROMPT" | grep -q slow; then exec sleep 5; fi; printf '{"prompt":"%s"}' "$PALLIUM_WORKFLOW_PROMPT" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["slow", "fast"], item =>
  agent("worker " + item, { label: item, provider: "slowpoke" })
);
return { results };`
	scriptPath, err := WriteRunScript("wf-agent-timeout", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-agent-timeout", Task: "agent timeout", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 2, AgentTimeoutSeconds: 1}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "worker fast") || !strings.Contains(result, "null") {
		t.Fatalf("expected timed-out agent to become null while sibling completes, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected two agent records, got %+v", agents)
	}
	timedOut := 0
	for _, agent := range agents {
		if agent.Status == "timed_out" && strings.Contains(agent.Error, "Pallium enforced the configured agent timeout after 1s") && strings.Contains(agent.Error, "--agent-timeout SECONDS") {
			timedOut++
		}
	}
	if timedOut != 1 {
		t.Fatalf("expected one timed-out agent record, got %+v", agents)
	}
}

func TestAgentTimeoutOptionFailsDirectCall(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_SLOWPOKE_COMMAND", `exec sleep 5`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return await agent("slow direct", { label: "slow", provider: "slowpoke", timeout_seconds: 1 });`
	scriptPath, err := WriteRunScript("wf-agent-timeout-direct", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-agent-timeout-direct", Task: "agent timeout direct", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "Pallium enforced the configured agent timeout after 1s") || !strings.Contains(err.Error(), "--agent-timeout SECONDS") {
		t.Fatalf("expected agent timeout error, got %v", err)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "failed" {
		t.Fatalf("expected failed run, got %+v", snapshot.Run)
	}
	if len(snapshot.Agents) != 1 || snapshot.Agents[0].Status != "timed_out" {
		t.Fatalf("expected one timed-out agent, got %+v", snapshot.Agents)
	}
}

func TestWorkflowEditTimeoutPreservesRecoveryWithoutApplying(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_SLOWEDIT_COMMAND", `echo partial > partial.txt; sleep 5`)
	store, err := Open(filepath.Join(repo, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("edit", {label:"editor", provider:"slowedit", mode:"edit", isolation:"worktree"});`
	scriptPath, _ := WriteRunScript("wf-timeout-recovery", repo, script)
	run, _ := store.CreateRun(Run{ID: "wf-timeout-recovery", Task: "edit", CWD: repo, ScriptPath: scriptPath})
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 2, AgentTimeoutSeconds: 1}).Execute(context.Background(), script, nil)
	if err == nil {
		t.Fatal("expected timeout")
	}
	agents, _ := store.ListAgents(run.ID)
	if len(agents) != 1 || agents[0].Status != "timed_out" || agents[0].PatchPath == "" || agents[0].Worktree == "" || !strings.Contains(agents[0].Error, "pallium workflow resume wf-timeout-recovery --agent-timeout 2") {
		t.Fatalf("expected durable workflow timeout recovery, got %+v", agents)
	}
	for _, detail := range []string{"changed files:\nA partial.txt", "branch: (detached HEAD)", "commits after worker base:\n(none)", "recovery patch (not applied):"} {
		if !strings.Contains(agents[0].Error, detail) {
			t.Fatalf("expected recovery detail %q in %q", detail, agents[0].Error)
		}
	}
	if raw, err := os.ReadFile(agents[0].PatchPath); err != nil || !strings.Contains(string(raw), "partial.txt") {
		t.Fatalf("expected readable non-applied recovery patch, err=%v patch=%q", err, raw)
	}
	if _, err := os.Stat(agents[0].Worktree); err != nil {
		t.Fatalf("expected timeout worktree preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "partial.txt")); !os.IsNotExist(err) {
		t.Fatalf("partial edit must not be applied, stat=%v", err)
	}
}

func TestAgentCommandErrorPreservesProviderDetailAndGivesRerunGuidance(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := agentCommandError(ctx, time.Second, errors.New("provider quota exhausted"))
	for _, want := range []string{"Pallium enforced the configured agent timeout after 1s", "--agent-timeout SECONDS", "provider quota exhausted"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("timeout diagnostic missing %q: %v", want, err)
		}
	}
}

func TestDefaultGateSchemaRequiresEveryProperty(t *testing.T) {
	schema := defaultGateSchema()
	properties := schema["properties"].(map[string]any)
	required := schema["required"].([]any)
	if len(properties) != 3 || len(required) != 3 {
		t.Fatalf("expected three properties and all three required, got properties=%v required=%v", properties, required)
	}
	want := []any{"approved", "reason", "evidence"}
	if !reflect.DeepEqual(required, want) {
		t.Fatalf("expected exact required contract %v, got %v", want, required)
	}
	for _, name := range want {
		if _, ok := properties[name.(string)]; !ok {
			t.Fatalf("required property %q missing from properties: %v", name, properties)
		}
	}
}

func TestEditWorkerPromptExplainsDetachedHeadPatchContract(t *testing.T) {
	got := editWorkerPrompt("fix it")
	for _, want := range []string{"detached HEAD is intentional", "captures your edits as a patch", "Do not create or switch branches"} {
		if !strings.Contains(got, want) {
			t.Fatalf("edit-worker prompt missing %q: %s", want, got)
		}
	}
}

func TestConfiguredProviderSchemaRetryCorrectsOutput(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SCHEMA_MARKER", filepath.Join(tmp, "marker"))
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FLAKY_COMMAND", `PALLIUM_WORKFLOW_PROMPT="$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")"; if [ -f "$SCHEMA_MARKER" ]; then if printf '%s' "$PALLIUM_WORKFLOW_PROMPT" | grep -q "schema validation"; then printf '{"summary":"corrected"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"; else printf '{"summary":"missing-correction"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"; fi; else touch "$SCHEMA_MARKER"; printf 'sure, here is the JSON you asked for' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"; fi`)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("schema");
return await agent("structured", {
  label: "structured",
  provider: "flaky",
  schema: {
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"]
  }
});`
	scriptPath, err := WriteRunScript("wf-schema-retry", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-schema-retry", Task: "schema retry", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"summary": "corrected"`) {
		t.Fatalf("expected corrective retry output, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Status != "completed" {
		t.Fatalf("expected one completed agent, got %+v", agents)
	}
}

func TestConfiguredProviderSchemaRetryStillFailingIsNonFatalInParallel(t *testing.T) {
	tmp := t.TempDir()
	callLog := filepath.Join(tmp, "calls.log")
	t.Setenv("SCHEMA_CALL_LOG", callLog)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_PROSE_COMMAND", `PALLIUM_WORKFLOW_PROMPT="$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")"; echo call >> "$SCHEMA_CALL_LOG"; if printf '%s' "$PALLIUM_WORKFLOW_PROMPT" | grep -q bad; then printf 'not json at all' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"; else printf '{"summary":"good"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"; fi`)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["good", "bad"], item =>
  agent("worker " + item, {
    label: item,
    provider: "prose",
    schema: {
      type: "object",
      properties: { summary: { type: "string" } },
      required: ["summary"]
    }
  })
);
return { results };`
	scriptPath, err := WriteRunScript("wf-schema-retry-fail", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-schema-retry-fail", Task: "schema retry fail", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 2}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"summary": "good"`) || !strings.Contains(result, "null") {
		t.Fatalf("expected schema-failed agent to become null while sibling survives, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected two agent records, got %+v", agents)
	}
	failed := 0
	for _, agent := range agents {
		if agent.Status == "failed" && strings.Contains(agent.Error, "does not match schema") {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("expected one schema-failed agent record, got %+v", agents)
	}
	raw, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Count(string(raw), "call"); calls != 3 {
		t.Fatalf("expected exactly one corrective retry (3 provider calls total), got %d", calls)
	}
}

// TestConfiguredProviderSchemaRetryPropagatesRetryFailure guards against a
// regression where a corrective schema retry's own error (nonzero exit,
// timeout, etc.) was silently discarded, letting the agent fall through to
// schema-validating the stale first-attempt output. The retry's real failure
// must be the one that fails the agent, not a generic schema mismatch on
// data that was already known to be invalid.
func TestConfiguredProviderSchemaRetryPropagatesRetryFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SCHEMA_MARKER", filepath.Join(tmp, "marker"))
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FLAKY_COMMAND", `if [ -f "$SCHEMA_MARKER" ]; then echo "retry boom" >&2; exit 3; else touch "$SCHEMA_MARKER"; printf 'sure, here is the JSON you asked for' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"; fi`)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("schema");
return await agent("structured", {
  label: "structured",
  provider: "flaky",
  schema: {
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"]
  }
});`
	scriptPath, err := WriteRunScript("wf-schema-retry-error", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-schema-retry-error", Task: "schema retry error", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil {
		t.Fatal("expected the retry's failure to fail the agent")
	}
	if !strings.Contains(err.Error(), "workflow provider") || !strings.Contains(err.Error(), "retry boom") {
		t.Fatalf("expected the retry's actual provider error to surface, got %v", err)
	}
	if strings.Contains(err.Error(), "does not match schema") {
		t.Fatalf("retry failure must not be masked as a generic schema mismatch, got %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Status != "failed" {
		t.Fatalf("expected one failed agent, got %+v", agents)
	}
	if !strings.Contains(agents[0].Error, "workflow provider") {
		t.Fatalf("expected persisted agent error to reflect the retry failure, got %q", agents[0].Error)
	}
}

func TestConfiguredProviderUsageFileOverridesCostEstimate(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_METERED_COMMAND", `printf '{"input_tokens":100,"output_tokens":50,"cost_usd":0.25}' > "$PALLIUM_WORKFLOW_USAGE_FILE"; printf '{"ok":true}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("usage");
await agent("first metered", { label: "first", provider: "metered" });
await agent("second metered", { label: "second", provider: "metered" });
return "done";`
	scriptPath, err := WriteRunScript("wf-usage", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-usage", Task: "usage", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "0.20"}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected reported cost to exhaust budget, got %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected one agent record, got %+v", agents)
	}
	if agents[0].EstimatedCostUSD != 0.25 {
		t.Fatalf("expected reported cost 0.25, got %+v", agents[0])
	}
	if agents[0].UsageJSON != `{"input_tokens":100,"output_tokens":50,"cost_usd":0.25}` {
		t.Fatalf("expected raw usage json persisted, got %q", agents[0].UsageJSON)
	}
}

// TestInternalProviderNameIsReservedForBookkeeping guards against a
// regression where Store.AgentUsage's exclusion of provider="internal" rows
// from the --max-agents count (added to stop registerUntilGreenPatch's
// bookkeeping row from inflating the cap) could also silently hide a real
// user agent that happens to choose the same provider name, letting a
// resumed run exceed its configured agent cap.
func TestInternalProviderNameIsReservedForBookkeeping(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("reserved");
return await agent("use reserved provider", { label: "one", provider: "internal" });`
	scriptPath, err := WriteRunScript("wf-reserved-provider", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-reserved-provider", Task: "reserved provider", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected provider \"internal\" to be rejected as reserved, got %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("expected no agent row created for a rejected provider name, got %+v", agents)
	}
}

// TestSchemaRetrySkippedWhenFirstAttemptAlreadyOverBudget guards against a
// regression where the corrective schema retry could fire a second paid
// provider call even though the first attempt's own reported cost already
// exhausted the budget: the retry starts before runAgentAtCallIndex ever
// gets a chance to apply that cost and check it, so skipping the retry once
// the first attempt is already over budget is the only place that can catch
// it before a second bill.
func TestSchemaRetrySkippedWhenFirstAttemptAlreadyOverBudget(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("SCHEMA_CALL_LOG", callLog)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_PRICEY_COMMAND", `echo call >> "$SCHEMA_CALL_LOG"; printf '{"cost_usd":5.0}' > "$PALLIUM_WORKFLOW_USAGE_FILE"; printf 'not json at all' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("schema-budget");
return await agent("structured", {
  label: "structured",
  provider: "pricey",
  schema: {
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"]
  }
});`
	scriptPath, err := WriteRunScript("wf-schema-over-budget", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-schema-over-budget", Task: "schema over budget", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "1.00"}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected the over-budget first attempt to fail the run as budget exhausted, got %v", err)
	}
	raw, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Count(string(raw), "call"); calls != 1 {
		t.Fatalf("expected the corrective retry to be skipped once already over budget (1 provider call), got %d", calls)
	}
}

func TestParallelAgentFailureIsTrackedInRunFailures(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_TEST_COMMAND", `PALLIUM_WORKFLOW_PROMPT="$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")"; if printf '%s' "$PALLIUM_WORKFLOW_PROMPT" | grep -q bad; then echo "failed intentionally" >&2; exit 7; fi; printf '{"prompt":"%s"}' "$PALLIUM_WORKFLOW_PROMPT" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["good", "bad"], item =>
  agent("worker " + item, { label: item, provider: "test" })
);
return { results };`
	scriptPath, err := WriteRunScript("wf-failure-list", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-failure-list", Task: "failure list", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 2}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "worker good") || !strings.Contains(result, "null") {
		t.Fatalf("expected dropped agent to stay null in results, got %s", result)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "completed" {
		t.Fatalf("expected completed run, got %+v", snapshot.Run)
	}
	if len(snapshot.Run.Failures) != 1 {
		t.Fatalf("expected one run failure, got %+v", snapshot.Run.Failures)
	}
	failure := snapshot.Run.Failures[0]
	if failure.Label != "bad" || failure.Phase != "parallel" || !strings.Contains(failure.Error, "failed intentionally") {
		t.Fatalf("unexpected run failure entry: %+v", failure)
	}
}

func TestPipelineStageThrowIsTrackedInRunFailures(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["keep", "drop"],
  item => {
    if (item === "drop") {
      throw new Error("stage rejected this item");
    }
    return item.toUpperCase();
  }
);
return { results };`
	scriptPath, err := WriteRunScript("wf-pipeline-failure-list", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pipeline-failure-list", Task: "pipeline failure list", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Run.Failures) != 1 {
		t.Fatalf("expected one run failure, got %+v", snapshot.Run.Failures)
	}
	failure := snapshot.Run.Failures[0]
	if failure.Label != "pipeline stage 0 item 1" || !strings.Contains(failure.Error, "stage rejected this item") {
		t.Fatalf("unexpected pipeline failure entry: %+v", failure)
	}
}

// TestPipelineThenChainDropIsTrackedWithHint reproduces the silent-failure
// finding: a pipeline stage that chains .then() onto agent(). During the
// capture pass agent() returns a placeholder marker (not a real promise), so
// .then() throws and the item drops to null. The drop must be tracked in the
// run failures list with an actionable reason, never silently absent.
func TestPipelineThenChainDropIsTrackedWithHint(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("judge");
const results = await pipeline(["alpha", "beta"],
  item => item.toUpperCase(),
  prev => agent("judge " + prev, { label: "judge" }).then(x => x)
);
return { results };`
	scriptPath, err := WriteRunScript("wf-then-chain", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-then-chain", Task: "then chain", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "null") {
		t.Fatalf("expected .then-chained items to drop to null, got %s", result)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Run.Failures) != 2 {
		t.Fatalf("expected both .then-chained items tracked as failures, got %+v", snapshot.Run.Failures)
	}
	for _, failure := range snapshot.Run.Failures {
		if failure.Phase != "judge" {
			t.Fatalf("expected drop tagged with its phase, got %+v", failure)
		}
		if !strings.Contains(failure.Error, "return the agent()/check() call directly") {
			t.Fatalf("expected actionable .then() hint in drop reason, got %q", failure.Error)
		}
	}
}

// TestPipelinePendingPromiseDropIsTrackedWithHint covers the async variant of
// the same bug: a stage that returns a promise which never settles. The drop
// must be tracked and its reason must name the likely cause and the fix.
func TestPipelinePendingPromiseDropIsTrackedWithHint(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["alpha"],
  item => item,
  prev => new Promise(() => {})
);
return { results };`
	scriptPath, err := WriteRunScript("wf-pending-promise", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-pending-promise", Task: "pending promise", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Run.Failures) != 1 {
		t.Fatalf("expected the pending-promise item tracked as a failure, got %+v", snapshot.Run.Failures)
	}
	failure := snapshot.Run.Failures[0]
	if !strings.Contains(failure.Error, "unresolved (pending) promise") {
		t.Fatalf("expected pending-promise reason, got %q", failure.Error)
	}
	if !strings.Contains(failure.Error, "return the agent()/check() call directly") {
		t.Fatalf("expected actionable hint in pending-promise reason, got %q", failure.Error)
	}
}

// TestPipelineStageThrowReasonIsNotRewritten guards the boundary of the hint
// rewrite: an ordinary stage throw is already tracked (see
// TestPipelineStageThrowIsTrackedInRunFailures), and its message must pass
// through verbatim rather than being decorated with the .then() hint.
func TestPipelineStageThrowReasonIsNotRewritten(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["drop"],
  item => { throw new Error("custom stage boom"); }
);
return { results };`
	scriptPath, err := WriteRunScript("wf-throw-verbatim", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-throw-verbatim", Task: "throw verbatim", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Run.Failures) != 1 {
		t.Fatalf("expected one tracked drop, got %+v", snapshot.Run.Failures)
	}
	failure := snapshot.Run.Failures[0]
	if !strings.Contains(failure.Error, "custom stage boom") {
		t.Fatalf("expected the thrown message preserved, got %q", failure.Error)
	}
	if strings.Contains(failure.Error, "return the agent()/check() call directly") {
		t.Fatalf("ordinary stage throw must not get the .then() hint appended, got %q", failure.Error)
	}
}

// TestPipelineDirectAgentStagesHaveNoSpuriousFailures is the regression guard:
// a normal two-stage pipeline whose stages return agent() directly still works
// and records no failures.
func TestPipelineDirectAgentStagesHaveNoSpuriousFailures(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("pipeline");
const results = await pipeline(["alpha"],
  item => item.toUpperCase(),
  prev => agent("inspect " + prev, { label: "inspect" })
);
return { result: results[0].prompt };`
	scriptPath, err := WriteRunScript("wf-direct-agent", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-direct-agent", Task: "direct agent", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "inspect ALPHA") {
		t.Fatalf("expected direct agent() stage to resolve, got %s", result)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "completed" {
		t.Fatalf("expected completed run, got %+v", snapshot.Run)
	}
	if len(snapshot.Run.Failures) != 0 {
		t.Fatalf("normal pipeline must not record spurious failures, got %+v", snapshot.Run.Failures)
	}
}

func TestIsEmptyResult(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty string", "", true},
		{"bare null", "null", true},
		{"empty object", "{}", true},
		{"empty array", "[]", true},
		{"empty json string", `""`, true},
		{"all-null pipeline result", `{"results":[null,null]}`, true},
		{"nested all-null", `{"a":{"b":null},"c":[null]}`, true},
		{"partial content", `{"results":["ok",null]}`, false},
		{"scalar string", `"done"`, false},
		{"non-json text", "done", false},
	}
	for _, tc := range cases {
		if got := isEmptyResult(tc.input); got != tc.want {
			t.Errorf("%s: isEmptyResult(%q) = %v, want %v", tc.name, tc.input, got, tc.want)
		}
	}
}

func TestParallelAgentErrorContainingBudgetPhraseIsNonFatal(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_TEST_COMMAND", `PALLIUM_WORKFLOW_PROMPT="$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")"; if printf '%s' "$PALLIUM_WORKFLOW_PROMPT" | grep -q bad; then echo "workflow budget exhausted" >&2; exit 7; fi; printf '{"prompt":"%s"}' "$PALLIUM_WORKFLOW_PROMPT" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("parallel");
const results = await parallel(["good", "bad"], item =>
  agent("worker " + item, { label: item, provider: "test" })
);
return { results };`
	scriptPath, err := WriteRunScript("wf-budget-phrase", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-budget-phrase", Task: "budget phrase", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, MaxConcurrentAgents: 2}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatalf("agent error text containing the budget phrase must not be fatal, got %v", err)
	}
	if !strings.Contains(result, "worker good") || !strings.Contains(result, "null") {
		t.Fatalf("expected phrase-matching failure to become null while sibling survives, got %s", result)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "completed" || len(snapshot.Run.Failures) != 1 {
		t.Fatalf("expected completed run with one tracked drop, got %+v", snapshot.Run)
	}
}

func TestScriptWrappedBudgetErrorFailsAsScriptError(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_TEST_COMMAND", `echo "provider boom" >&2; exit 3`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("wrap");
try {
  await agent("boom", { label: "boom", provider: "test" });
} catch (e) {
  throw new Error("workflow budget exhausted: " + e);
}
return "unreachable";`
	scriptPath, err := WriteRunScript("wf-wrapped-budget", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-wrapped-budget", Task: "wrapped budget", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "5.00"}).Execute(context.Background(), script, nil)
	if err == nil {
		t.Fatal("expected wrapped script error")
	}
	if errors.Is(err, ErrWorkflowBudgetExhausted) {
		t.Fatalf("script-wrapped budget phrase must not classify as budget exhaustion: %v", err)
	}
	if !strings.Contains(err.Error(), "workflow budget exhausted") {
		t.Fatalf("expected the wrapped script error to surface unchanged, got %v", err)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "failed" {
		t.Fatalf("expected normal failed run, got %+v", snapshot.Run)
	}
}

func TestPhaseInsidePipelineStagesUnderRace(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"prompt":"{{PROMPT}}"}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "5")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `const items = ["a", "b", "c", "d", "e", "f", "g", "h"];
phase("outer");
const results = await pipeline(items,
  (item, original, index) => phase("find-" + index, () => agent("inspect " + item, { label: item })),
  (prev, original, index) => phase("verify-" + index, () => agent("verify " + prev.prompt, { label: "verify-" + original }))
);
return { count: results.length };`
	scriptPath, err := WriteRunScript("wf-phase-pipeline-race", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-phase-pipeline-race", Task: "phase pipeline race", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 40, MaxConcurrentAgents: 8}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"count": 8`) {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestStripMetaHandlesNestedMetaObjects(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	script := `export const meta = { name: "nested", limits: { agents: 3 }, phases: ["scan"] };
phase("scan");
const result = await agent("inspect", { label: "inspect" });
return { ok: result.ok };`
	if validation := ValidateScript(script); !validation.Valid {
		t.Fatalf("expected nested meta script to validate, got %s", validation.Error)
	}
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scriptPath, err := WriteRunScript("wf-nested-meta", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-nested-meta", Task: "nested meta", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestResumeWithChangedScriptWarnsAndFlagsRun(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	firstScript := `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return result;`
	secondScript := `phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return { source: result.source, changed_script: true };`
	scriptPath, err := WriteRunScript("wf-script-changed", tmp, firstScript)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-script-changed", Task: "script changed", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), firstScript, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.ScriptHash == "" || snapshot.Run.ScriptChanged {
		t.Fatalf("expected stored hash without change flag after first run, got %+v", snapshot.Run)
	}
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second"}`)
	stderr, result, err := captureStderr(t, func() (string, error) {
		return (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), secondScript, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "script changed since original run; unchanged prefix will replay from cache") {
		t.Fatalf("expected script-change warning on stderr, got %q", stderr)
	}
	if !strings.Contains(result, `"source": "first"`) {
		t.Fatalf("expected unchanged prefix to replay from cache, got %s", result)
	}
	snapshot, err = store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Run.ScriptChanged {
		t.Fatalf("expected script_changed flag after resume with edited script, got %+v", snapshot.Run)
	}
}

func captureStderr(t *testing.T, fn func() (string, error)) (string, string, error) {
	t.Helper()
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	result, fnErr := fn()
	os.Stderr = original
	_ = writer.Close()
	captured, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(captured), result, fnErr
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

func TestUntilGreenResumeRestoresFixEditsAfterMidLoopError(t *testing.T) {
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
	providerScript := filepath.Join(t.TempDir(), "loop-provider.sh")
	if err := os.WriteFile(providerScript, []byte(`#!/bin/sh
if [ "$PALLIUM_WORKFLOW_MODE" = "edit" ]; then
  printf 'fixed\n' > marker.txt
  printf '{"summary":"created marker"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
elif [ -f marker.txt ]; then
  printf '{"ok":true,"command":"check marker","summary":"marker present","output_tail":"","failures":[]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
else
  printf '{"ok":false,"command":"check marker","summary":"marker missing","output_tail":"no marker","failures":[{"name":"marker","message":"missing"}]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_LOOP_COMMAND", providerScript)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("verify");
const result = verify.untilGreen("check marker", { label: "green", maxRounds: 2, provider: "loop" });
return result;`
	scriptPath, err := WriteRunScript("wf-until-green-resume", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-until-green-resume", Task: "until green resume", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	// First execution: the budget covers check-1 and fix-1 only, so check-2
	// aborts the loop mid-flight after the fix agent already edited the
	// worktree.
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10, MaxBudgetUSD: "0.025"}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected mid-loop budget exhaustion, got %v", err)
	}
	worktreeRoot, err := RunArtifactDir(run.ID, "worktrees")
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(worktreeRoot); err == nil && len(entries) > 0 {
		t.Fatalf("expected no leftover loop worktree after mid-loop error, got %v", entries)
	}
	patchRoot, err := RunArtifactDir(run.ID, "patches")
	if err != nil {
		t.Fatal(err)
	}
	patchEntries, err := os.ReadDir(patchRoot)
	if err != nil || len(patchEntries) != 1 {
		t.Fatalf("expected one durable loop patch after mid-loop error, got %v err=%v", patchEntries, err)
	}
	durable, err := os.ReadFile(filepath.Join(patchRoot, patchEntries[0].Name()))
	if err != nil || !strings.Contains(string(durable), "marker.txt") {
		t.Fatalf("expected durable patch to keep the fix edits, err=%v patch=%s", err, string(durable))
	}
	// Second execution: check-1 and fix-1 replay from cache (output only);
	// the durable patch must restore the fix edits so the loop goes green and
	// the registered combined patch is not empty.
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) {
		t.Fatalf("expected resumed loop to converge, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	patchAgent := agents[len(agents)-1]
	if patchAgent.Label != "green-patch" || patchAgent.Status != "completed" || patchAgent.PatchPath == "" {
		t.Fatalf("expected completed untilGreen patch agent, got %+v", patchAgent)
	}
	raw, err := os.ReadFile(patchAgent.PatchPath)
	if err != nil || !strings.Contains(string(raw), "marker.txt") {
		t.Fatalf("expected non-empty combined patch with prior fix edits, err=%v patch=%s", err, string(raw))
	}
	if content, err := os.ReadFile(filepath.Join(tmp, "marker.txt")); err != nil || string(content) != "fixed\n" {
		t.Fatalf("expected restored fix applied to repo on completion, got %q err=%v", string(content), err)
	}
	if _, err := os.Stat(patchAgent.Worktree); !os.IsNotExist(err) {
		t.Fatalf("expected loop worktree removed after completion, stat err=%v", err)
	}
}

// TestRunnerResumeExcludesInternalUntilGreenPatchRowFromMaxAgents guards
// against a regression where the internal bookkeeping row
// registerUntilGreenPatch stores for the untilGreen combined patch (provider
// "internal") counted toward the --max-agents cap on resume even though it
// never spawned a worker. Store.AgentUsage seeds the resumed run's agent
// count from all persisted workflow_agents rows, so a stale internal row
// from a prior execution could trip the cap one agent early.
func TestRunnerResumeExcludesInternalUntilGreenPatchRowFromMaxAgents(t *testing.T) {
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
	providerScript := filepath.Join(t.TempDir(), "loop-provider.sh")
	if err := os.WriteFile(providerScript, []byte(`#!/bin/sh
if [ "$PALLIUM_WORKFLOW_MODE" = "edit" ]; then
  printf 'fixed\n' > marker.txt
  printf '{"summary":"created marker"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
elif [ -f marker.txt ]; then
  printf '{"ok":true,"command":"check marker","summary":"marker present","output_tail":"","failures":[]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
else
  printf '{"ok":false,"command":"check marker","summary":"marker missing","output_tail":"no marker","failures":[{"name":"marker","message":"missing"}]}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_LOOP_COMMAND", providerScript)
	// "one" uses its own configured provider rather than
	// PALLIUM_WORKFLOW_AGENT_STUB, which would short-circuit every agent call
	// (including the "loop" provider ones) before the provider dispatch is
	// even reached.
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_ONE_COMMAND", `printf '{"ok":true}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// First execution: one plain agent plus an untilGreen loop that needs
	// exactly one fix round to converge. Real spawns: "one" (1) +
	// check-1/fix-1/check-2 (3) = 4. The loop also registers one internal
	// "green-patch" bookkeeping row that is not a real spawn, for 5 rows
	// total in workflow_agents.
	firstScript := `phase("scan");
agent("one", { label: "one", provider: "one" });
const result = verify.untilGreen("check marker", { label: "green", maxRounds: 2, provider: "loop" });
return result;`
	scriptPath, err := WriteRunScript("wf-until-green-max-agents", tmp, firstScript)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-until-green-max-agents", Task: "until green max agents", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 4}).Execute(context.Background(), firstScript, nil); err != nil {
		t.Fatalf("expected first execution to converge within 4 real agents, got %v", err)
	}
	agentsAfterFirst, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentsAfterFirst) != 5 {
		t.Fatalf("expected 4 real agents plus 1 internal patch row, got %d: %+v", len(agentsAfterFirst), agentsAfterFirst)
	}
	usedAgents, _, err := store.AgentUsage(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usedAgents != 4 {
		t.Fatalf("expected AgentUsage to exclude the internal patch row from the count, got %d", usedAgents)
	}
	// Second execution (resume): the identical prefix replays entirely from
	// cache (including the internal patch row, via its own completed-agent
	// cache check), so it costs zero fresh spawns. A new "two" call at the
	// tail needs exactly one more real spawn to reach 5 total. --max-agents 5
	// must allow it: under the old buggy counting, the seeded agent count
	// would already be 5 (4 real + 1 internal) before "two" even runs.
	secondScript := `phase("scan");
agent("one", { label: "one", provider: "one" });
const result = verify.untilGreen("check marker", { label: "green", maxRounds: 2, provider: "loop" });
agent("two", { label: "two", provider: "one" });
return { result, two: true };`
	if err := os.WriteFile(scriptPath, []byte(secondScript), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 5}).Execute(context.Background(), secondScript, nil)
	if err != nil {
		t.Fatalf("expected resume to succeed with the internal patch row excluded from the cap, got %v", err)
	}
	if !strings.Contains(result, `"two": true`) {
		t.Fatalf("expected the new agent call to run after resume, got %s", result)
	}
	agentsAfterSecond, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentsAfterSecond) != 6 {
		t.Fatalf("expected exactly one new agent row after resume (5 real + 1 internal), got %d: %+v", len(agentsAfterSecond), agentsAfterSecond)
	}
}

func TestUntilGreenAlreadyGreenSkipsWorktreeAndPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true,"command":"go test ./...","summary":"passed","output_tail":"","failures":[]}`)
	// The cwd is intentionally not a git repo: an already-green check must
	// succeed without ever creating a worktree.
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("verify");
const result = verify.untilGreen("go test ./...", { label: "green", maxRounds: 2 });
return result;`
	scriptPath, err := WriteRunScript("wf-until-green-lazy", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-until-green-lazy", Task: "until green lazy", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok": true`) {
		t.Fatalf("expected green result, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Mode != "test" {
		t.Fatalf("expected a single check agent and no patch row, got %+v", agents)
	}
	worktreeRoot, err := RunArtifactDir(run.ID, "worktrees")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktreeRoot); !os.IsNotExist(err) {
		t.Fatalf("expected no worktree directory for an already-green check, stat err=%v", err)
	}
}

func TestRecordDroppedItemPersistsAllConcurrentDrops(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.CreateRun(Run{ID: "wf-drop-race", Task: "drop race", CWD: tmp, ScriptPath: filepath.Join(tmp, "workflow.js")})
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: store, Run: run}
	const drops = 64
	var wg sync.WaitGroup
	for i := 0; i < drops; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runner.recordDroppedItem(fmt.Sprintf("item-%d", i), "boom")
		}(i)
	}
	wg.Wait()
	stored, err := store.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	inMemory := len(runner.failures)
	runner.mu.Unlock()
	if inMemory != drops {
		t.Fatalf("expected %d in-memory failures, got %d", drops, inMemory)
	}
	if len(stored.Failures) != drops {
		t.Fatalf("expected %d persisted failures, got %d", drops, len(stored.Failures))
	}
}

func TestConfiguredProviderUsageAccumulatesAcrossSchemaRetry(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SCHEMA_MARKER", filepath.Join(tmp, "marker"))
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_METERED_COMMAND", `printf '{"input_tokens":100,"output_tokens":50,"cost_usd":0.1}' > "$PALLIUM_WORKFLOW_USAGE_FILE"; if [ -f "$SCHEMA_MARKER" ]; then printf '{"summary":"corrected"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"; else touch "$SCHEMA_MARKER"; printf 'not json at all' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"; fi`)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("usage");
return await agent("structured", {
  label: "structured",
  provider: "metered",
  schema: {
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"]
  }
});`
	scriptPath, err := WriteRunScript("wf-usage-retry", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-usage-retry", Task: "usage retry", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"summary": "corrected"`) {
		t.Fatalf("expected corrective retry output, got %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Status != "completed" {
		t.Fatalf("expected one completed agent, got %+v", agents)
	}
	if agents[0].EstimatedCostUSD != 0.2 {
		t.Fatalf("expected cost summed across both attempts (0.2), got %+v", agents[0])
	}
	if agents[0].UsageJSON != `{"cost_usd":0.2,"input_tokens":200,"output_tokens":100}` {
		t.Fatalf("expected merged usage json across attempts, got %q", agents[0].UsageJSON)
	}
}

func TestEditModeSchemaFailureSkipsCorrectiveRetry(t *testing.T) {
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
	callLog := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("EDIT_CALL_LOG", callLog)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_EDITY_COMMAND", `echo call >> "$EDIT_CALL_LOG"; printf 'not json at all' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"`)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("edit");
return await agent("edit something", {
  label: "editor",
  provider: "edity",
  mode: "edit",
  schema: {
    type: "object",
    properties: { summary: { type: "string" } },
    required: ["summary"]
  }
});`
	scriptPath, err := WriteRunScript("wf-edit-no-retry", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-edit-no-retry", Task: "edit no retry", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	// An edit agent's schema failure is now NON-fatal: the completed edit is
	// preserved and the schema failure is reported in run failures, rather than
	// discarding the work by raising. (It still must not run the corrective
	// retry — that stays read-only-only.)
	if err != nil {
		t.Fatalf("edit-mode schema failure must be non-fatal (edit preserved), got %v", err)
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Run.Failures) != 1 || !strings.Contains(snapshot.Run.Failures[0].Error, "schema") {
		t.Fatalf("expected the schema failure reported in run failures, got %+v", snapshot.Run.Failures)
	}
	raw, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Count(string(raw), "call"); calls != 1 {
		t.Fatalf("edit-mode agent must not run the corrective retry, expected 1 provider call, got %d", calls)
	}
}

func TestScriptCannotUseIsolationNone(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"x"}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("iso");
return agent("edit the live repo", { label: "live", mode: "edit", isolation: "none" });`
	scriptPath, err := WriteRunScript("wf-isolation-none", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-isolation-none", Task: "isolation none", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), `invalid agent isolation "none"`) {
		t.Fatalf("expected isolation validation error, got %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("expected no agent to launch, got %+v", agents)
	}
}

func TestCaughtPauseInterruptThenScriptErrorFailsAsScriptError(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS", "2000")
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("catch");
try {
  await agent("slow", { label: "slow" });
} catch (e) {
  throw new Error("unrelated script failure");
}
return "unreachable";`
	scriptPath, err := WriteRunScript("wf-caught-pause", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-caught-pause", Task: "caught pause", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
		errCh <- err
	}()
	deadline := time.After(2 * time.Second)
	for {
		agents, err := store.ListAgents(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(agents) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for running agent, got %d", len(agents))
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := store.SetRunStatus(run.ID, "paused", "", "test pause"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil || errors.Is(err, ErrWorkflowPaused) {
			t.Fatalf("caught interrupt followed by a script error must not classify as paused, got %v", err)
		}
		if !strings.Contains(err.Error(), "unrelated script failure") {
			t.Fatalf("expected the script's own error to surface, got %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("runner did not finish after pause")
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != "failed" {
		t.Fatalf("expected failed run, got %+v", snapshot.Run)
	}
}

// TestCodexSandboxArgsRespectsNetwork locks in the codex `exec` sandbox flag
// construction. The default (no network) mapping must not regress, and a
// network-allowed agent must get the granular workspace-scoped network toggle
// rather than danger-full-access.
func TestCodexSandboxArgsRespectsNetwork(t *testing.T) {
	cases := []struct {
		name           string
		mode           string
		networkAllowed bool
		want           []string
	}{
		{"read-only default", "", false, []string{"--sandbox", "read-only"}},
		{"edit default", "edit", false, []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=false"}},
		{"test default", "test", false, []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=false"}},
		{"check default", "check", false, []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=false"}},
		{"read-only with network", "", true, []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true"}},
		{"edit with network", "edit", true, []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codexSandboxArgs(tc.mode, tc.networkAllowed)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("codexSandboxArgs(%q,%v) = %v, want %v", tc.mode, tc.networkAllowed, got, tc.want)
			}
			// The network-enabling toggle must never appear in the default,
			// locked-down mappings.
			joined := strings.Join(got, " ")
			hasToggle := strings.Contains(joined, "network_access=true")
			if tc.networkAllowed != hasToggle {
				t.Fatalf("network toggle presence = %v, want %v (args %v)", hasToggle, tc.networkAllowed, got)
			}
			// A non-network workspace-write worker must explicitly pin the
			// toggle OFF so an ambient config can't grant silent egress.
			if !tc.networkAllowed && (tc.mode == "edit" || tc.mode == "test" || tc.mode == "check") {
				if !strings.Contains(joined, "sandbox_workspace_write.network_access=false") {
					t.Fatalf("non-network %q workspace-write must pin network_access=false, got %v", tc.mode, got)
				}
			}
		})
	}
}

// TestAgentNetworkForcesWorktree covers the containment guarantee for a
// networked read-only agent: it is forced into an isolated worktree (network
// forces workspace-write filesystem access that must never touch the live
// checkout), AND any file it writes there is DISCARDED, not captured as a
// patch and auto-applied to the operator's live repo at run completion. The
// provider here writes a real repo file to prove the write is thrown away.
func TestAgentNetworkForcesWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")

	// Provider writes a real file into its cwd (simulating a prompt-injected
	// networked worker dropping a payload) and records the cwd it ran in, so we
	// can prove both that it ran in an isolated worktree and that its write was
	// discarded rather than applied to the live repo.
	provider := filepath.Join(tmp, "cwd-provider.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf 'pwned by networked worker\\n' > \"$PALLIUM_WORKFLOW_CWD/payload.txt\"\nprintf '{\"ok\":true,\"cwd\":\"%s\"}' \"$PALLIUM_WORKFLOW_CWD\" > \"$PALLIUM_WORKFLOW_OUTPUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_CWD_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("go", { provider: "cwd", mode: "read-only", label: "netter", network: true });`
	scriptPath, err := WriteRunScript("wf-net-worktree", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net-worktree", Task: "net worktree", CWD: tmp, ScriptPath: scriptPath, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Worktree == "" || agents[0].Worktree == tmp {
		t.Fatalf("expected networked read-only agent to get an isolated worktree != repo root %q, got %q", tmp, agents[0].Worktree)
	}
	// The recorded run cwd must be the worktree, not the live repo root.
	if strings.Contains(agents[0].Output, `"cwd":"`+tmp+`"`) {
		t.Fatalf("networked agent ran in live repo root, output=%q", agents[0].Output)
	}
	if !strings.Contains(agents[0].Output, `"cwd":"`+agents[0].Worktree+`"`) {
		t.Fatalf("expected agent cwd to be worktree %q, output=%q", agents[0].Worktree, agents[0].Output)
	}
	// KEY containment guard: the worktree existed ONLY to contain the networked
	// worker's forced workspace-write access. Execute() runs ApplyPatches at
	// completion, so if this write had been captured as a patch it would now be
	// in the live repo. It must not be.
	if _, statErr := os.Stat(filepath.Join(tmp, "payload.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("networked read-only agent's worktree write leaked into the live repo (payload.txt present); want it discarded")
	}
	// No patch should have been captured for a containment-only worktree.
	if agents[0].PatchPath != "" {
		t.Fatalf("expected no patch for containment-only worktree, got %q", agents[0].PatchPath)
	}
	if !agents[0].Networked {
		t.Fatalf("expected agent row to record networked=true")
	}
}

// TestAgentNetworkContainmentPreservesSubdirCWD covers a gap in the
// containment worktree fix: `git worktree add` always checks out the WHOLE
// repo, so when the run's CWD is a subdirectory of the repo (not the repo
// root), landing the agent at the worktree's root silently relocates it out
// of that subdirectory. The agent must land at the same relative subdirectory
// inside the new worktree, matching where it would have run without
// containment.
func TestAgentNetworkContainmentPreservesSubdirCWD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	subdir := filepath.Join(tmp, "services", "api")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "initial")

	provider := filepath.Join(tmp, "cwd-provider.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '{\"ok\":true,\"cwd\":\"%s\"}' \"$PALLIUM_WORKFLOW_CWD\" > \"$PALLIUM_WORKFLOW_OUTPUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_CWD_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// The run's CWD is the subdirectory, not the repo root, matching a CLI
	// invocation launched from inside that subdirectory.
	script := `return agent("go", { provider: "cwd", mode: "read-only", label: "netter", network: true });`
	scriptPath, err := WriteRunScript("wf-net-subdir", subdir, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net-subdir", Task: "net subdir", CWD: subdir, ScriptPath: scriptPath, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Worktree == "" {
		t.Fatalf("expected networked read-only agent to get an isolated worktree")
	}
	wantCWD := filepath.Join(agents[0].Worktree, "services", "api")
	if !strings.Contains(agents[0].Output, `"cwd":"`+wantCWD+`"`) {
		t.Fatalf("expected agent to run in worktree subdir %q (preserving the launch subdirectory), got output=%q", wantCWD, agents[0].Output)
	}
}

// TestNetworkedEditAgentAppliesPatch is the regression guard for the other
// side of the containment fix: an EDIT agent that also has network still has
// genuine edit intent, so its worktree writes ARE captured as a patch and
// applied to the live repo at completion. Network forcing the worktree must
// not suppress a real edit agent's output.
func TestNetworkedEditAgentAppliesPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")

	provider := filepath.Join(tmp, "editnet-provider.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf 'edited by agent\\n' > \"$PALLIUM_WORKFLOW_CWD/edited.txt\"\nprintf '{\"ok\":true}' > \"$PALLIUM_WORKFLOW_OUTPUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_EDITNET_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("go", { provider: "editnet", mode: "edit", isolation: "worktree", label: "editor", network: true });`
	scriptPath, err := WriteRunScript("wf-net-edit", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net-edit", Task: "net edit", CWD: tmp, ScriptPath: scriptPath, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "edited.txt"))
	if err != nil {
		t.Fatalf("expected edit agent patch applied to live repo: %v", err)
	}
	if string(raw) != "edited by agent\n" {
		t.Fatalf("unexpected applied content: %q", string(raw))
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].PatchPath == "" {
		t.Fatalf("expected one agent with a captured patch, got %#v", agents)
	}
}

// TestNetworkWorktreeRequiresGitRepo covers the improved error when network
// forces a containment worktree but the run cwd is not a git repository. The
// raw `git ... exit status 128` is replaced with actionable guidance.
func TestNetworkWorktreeRequiresGitRepo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir() // deliberately NOT a git repo
	provider := filepath.Join(tmp, "ok-provider.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '{\"ok\":true}' > \"$PALLIUM_WORKFLOW_OUTPUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_OK_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("go", { provider: "ok", mode: "read-only", label: "netter", network: true });`
	scriptPath, err := WriteRunScript("wf-net-nogit", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net-nogit", Task: "net nogit", CWD: tmp, ScriptPath: scriptPath, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil {
		t.Fatal("expected error when networked run cwd is not a git repo")
	}
	if !strings.Contains(err.Error(), "network access requires the run to execute inside a git repository") {
		t.Fatalf("expected actionable git-repo guidance, got %v", err)
	}
}

// TestNetworkedReadOnlyAgentCleansWorktreeOnError covers the containment-worktree
// leak on a FAILED networked agent: network forces a read-only worker into a
// throwaway worktree, and if its command then errors, that worktree must still
// be removed from disk rather than leaked. The success path removes it via
// finalizeWorktreePatch; the error path relies on the cleanup defer.
func TestNetworkedReadOnlyAgentCleansWorktreeOnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")

	// Provider exits nonzero and writes no output, so the provider command path
	// returns an error after the containment worktree has been created.
	provider := filepath.Join(tmp, "fail-provider.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAIL_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("go", { provider: "fail", mode: "read-only", label: "netter", network: true });`
	scriptPath, err := WriteRunScript("wf-net-fail", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net-fail", Task: "net fail", CWD: tmp, ScriptPath: scriptPath, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err == nil {
		t.Fatal("expected execute to fail when the networked provider errors")
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	// A containment worktree was forced (network + read-only), so the row must
	// record one distinct from the repo root...
	if agents[0].Worktree == "" || agents[0].Worktree == tmp {
		t.Fatalf("expected a forced containment worktree != repo root %q, got %q", tmp, agents[0].Worktree)
	}
	// ...and it must not survive the failure on disk.
	if _, statErr := os.Stat(agents[0].Worktree); !os.IsNotExist(statErr) {
		t.Fatalf("containment worktree %q leaked after a failed networked agent; want it removed", agents[0].Worktree)
	}
}

// TestNetworkedEditAgentKeepsWorktreeOnError is the over-reach guard for the
// cleanup: a networked EDIT agent has genuine edit intent, so a command failure
// must NOT delete its worktree — that tree holds the edits and is kept for
// debugging (matching non-networked edit-agent failure behavior).
func TestNetworkedEditAgentKeepsWorktreeOnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")

	provider := filepath.Join(tmp, "faildit-provider.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_FAILEDIT_COMMAND", provider)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("go", { provider: "failedit", mode: "edit", isolation: "worktree", label: "editor", network: true });`
	scriptPath, err := WriteRunScript("wf-net-editfail", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net-editfail", Task: "net editfail", CWD: tmp, ScriptPath: scriptPath, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err == nil {
		t.Fatal("expected execute to fail when the edit provider errors")
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Worktree == "" {
		t.Fatalf("expected one agent with a recorded worktree, got %#v", agents)
	}
	if _, statErr := os.Stat(agents[0].Worktree); statErr != nil {
		t.Fatalf("edit-agent worktree %q should be preserved on failure, stat error: %v", agents[0].Worktree, statErr)
	}
}

// TestNetworkOffRerunIgnoresStaleNetworkedCache covers the completed-agent
// cache identity: rerunning the same run-id via `workflow run` WITHOUT
// --allow-network must NOT reuse output produced by an earlier networked run.
// The operator believes the rerun is sandboxed, so it must actually re-run the
// worker with network off rather than serving networked-origin output.
func TestNetworkOffRerunIgnoresStaleNetworkedCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	writeNetworkProvider(t, tmp)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`
	scriptPath, err := WriteRunScript("wf-cache-net", tmp, script)
	if err != nil {
		t.Fatal(err)
	}

	// Run 1: --allow-network on. The worker sees network=1 and the row is
	// stored with networked=true.
	run, err := store.CreateRun(Run{ID: "wf-cache-net", Task: "cache", CWD: tmp, ScriptPath: scriptPath, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	_, out1, err := captureStderr(t, func() (string, error) {
		return (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	})
	if err != nil {
		t.Fatalf("run 1 failed: %v", err)
	}
	if !strings.Contains(out1, `"network": "1"`) {
		t.Fatalf("expected run 1 to run networked, got %q", out1)
	}

	// Run 2: same run-id reused via `workflow run` WITHOUT --allow-network,
	// which resets the ceiling to off (UpsertRun does not fold the old value).
	run2, err := store.UpsertRun(Run{ID: "wf-cache-net", Task: "cache", CWD: tmp, ScriptPath: scriptPath, AllowNetwork: false, Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	_, out2, err := captureStderr(t, func() (string, error) {
		return (&Runner{Store: store, Run: run2, MaxAgents: 10}).Execute(context.Background(), script, nil)
	})
	if err != nil {
		t.Fatalf("run 2 failed: %v", err)
	}
	if !strings.Contains(out2, `"network": "0"`) {
		t.Fatalf("network-off rerun reused stale networked output; want sandboxed re-run, got %q", out2)
	}
}

// networkProviderScript writes the value of PALLIUM_WORKFLOW_NETWORK so a test
// can assert exactly what env the configured provider saw.
const networkProviderScript = `#!/bin/sh
printf '{"ok":true,"network":"%s"}' "$PALLIUM_WORKFLOW_NETWORK" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
`

func writeNetworkProvider(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "net-provider.sh")
	if err := os.WriteFile(path, []byte(networkProviderScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_NET_COMMAND", path)
}

func runNetworkAgent(t *testing.T, allowNetwork bool, script string) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	// A granted-network agent is now forced into an isolated worktree (FIX 2),
	// so the run cwd must be a git repo it can branch from.
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "add", "README.md")
	runGit(t, tmp, "commit", "-m", "initial")
	writeNetworkProvider(t, tmp)
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scriptPath, err := WriteRunScript("wf-net", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net", Task: "net", CWD: tmp, ScriptPath: scriptPath, AllowNetwork: allowNetwork})
	if err != nil {
		t.Fatal(err)
	}
	logs, result, runErr := captureStderr(t, func() (string, error) {
		return (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	})
	if runErr != nil {
		t.Fatalf("execute failed: %v", runErr)
	}
	return result, logs
}

// TestAgentNetworkRejectsNonBoolean covers the JS option boundary: a
// non-boolean `network` value is a script error, not a silently coerced truthy
// value.
func TestAgentNetworkRejectsNonBoolean(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("go", { network: "yes" });`
	scriptPath, err := WriteRunScript("wf-net-bad", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net-bad", Task: "net", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "must be a boolean") {
		t.Fatalf("expected non-boolean network to be rejected, got %v", err)
	}
}

// TestAgentNetworkRejectsNonBooleanCapitalKey covers FIX 5: goja preserves JS
// property case and json.Unmarshal matches case-insensitively, so a
// capitalized `Network` key with a non-boolean value must still be rejected —
// it cannot bypass validation via casing.
func TestAgentNetworkRejectsNonBooleanCapitalKey(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return agent("go", { Network: "yes" });`
	scriptPath, err := WriteRunScript("wf-net-bad-cap", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-net-bad-cap", Task: "net", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "must be a boolean") {
		t.Fatalf("expected capitalized non-boolean network to be rejected, got %v", err)
	}
}

// TestAgentNetworkParsedWithCeiling covers the happy path: network: true is
// parsed, the run granted the ceiling, so the provider sees
// PALLIUM_WORKFLOW_NETWORK=1 and the enabled line is logged.
func TestAgentNetworkParsedWithCeiling(t *testing.T) {
	script := `return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`
	result, logs := runNetworkAgent(t, true, script)
	if !strings.Contains(result, `"network": "1"`) {
		t.Fatalf("expected provider to see PALLIUM_WORKFLOW_NETWORK=1, got %s", result)
	}
	if !strings.Contains(logs, "agent netter running with network access enabled") {
		t.Fatalf("expected network-enabled log line, got stderr: %s", logs)
	}
}

// TestAgentNetworkWithoutCeilingRunsSandboxed covers the operator-consent
// requirement: a script asking for network on a run that lacks --allow-network
// runs sandboxed (NETWORK=0) and a warning is logged.
func TestAgentNetworkWithoutCeilingRunsSandboxed(t *testing.T) {
	script := `return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`
	result, logs := runNetworkAgent(t, false, script)
	if !strings.Contains(result, `"network": "0"`) {
		t.Fatalf("expected no network granted (PALLIUM_WORKFLOW_NETWORK=0), got %s", result)
	}
	if !strings.Contains(logs, "requested network but run was not started with --allow-network") {
		t.Fatalf("expected sandboxed warning, got stderr: %s", logs)
	}
}

// TestAgentNetworkDefaultOff guards the safe default: an agent that does not
// opt into network never gets it, even on a run that granted the ceiling, and
// no network-enabled log is emitted.
func TestAgentNetworkDefaultOff(t *testing.T) {
	script := `return agent("go", { provider: "net", mode: "read-only", label: "netter" });`
	result, logs := runNetworkAgent(t, true, script)
	if !strings.Contains(result, `"network": "0"`) {
		t.Fatalf("expected default PALLIUM_WORKFLOW_NETWORK=0, got %s", result)
	}
	if strings.Contains(logs, "running with network access enabled") {
		t.Fatalf("did not expect network-enabled log for a non-opted agent, got stderr: %s", logs)
	}
}
