package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tszaks/pallium/internal/workflow"
)

// newLoopTestRepo sets up a throwaway git repo + sqlite db path for loop CLI
// tests, mirroring the workflow/trigger test setup convention.
func newLoopTestRepo(t *testing.T) (repo, dbPath string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	repo = t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	return repo, filepath.Join(repo, "sessions.sqlite")
}

func writeLoopScript(t *testing.T, repo, body string) string {
	t.Helper()
	path := filepath.Join(repo, "loop.js")
	full := `export const meta = { name: "test-loop", description: "d", kind: "loop", loop: { stagnationThreshold: 3 } };
` + body
	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoopTickPropagatesTestDBToChildRun is the regression test for a bug
// found live while proving this PR's acceptance criteria: `loop tick`
// resolved PALLIUM_TEST_DB for its OWN store correctly, but passed the
// original (empty) --db straight through to the child run it spawns —
// which independently falls through to the REAL default global DB inside
// runWorkflowRun, landing the child run in a different database than the
// loop's own bookkeeping. The very next store.Run(childID) lookup then
// failed with sql.ErrNoRows because it was looking in the wrong DB. Caught
// against a REAL throwaway ~/.pallium-shaped HOME during the live
// acceptance proof, not by an existing test — none of the other tests in
// this file exercise the "no --db, PALLIUM_TEST_DB set" combination since
// they all pass --db explicitly.
func TestLoopTickPropagatesTestDBToChildRun(t *testing.T) {
	repo, dbPath := newLoopTestRepo(t)
	t.Setenv("PALLIUM_TEST_DB", dbPath)
	scriptPath := writeLoopScript(t, repo, `return { state: "success" };`)

	var out bytes.Buffer
	// Deliberately no --db on EITHER call — this is exactly what a real
	// dogfood/test session does after `export PALLIUM_TEST_DB=...` once.
	if err := runLoop(&out, []string{"start", "test-loop", "--script", scriptPath, "--cwd", repo}, true); err != nil {
		t.Fatalf("loop start failed: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runLoop(&out, []string{"tick", "test-loop"}, true); err != nil {
		t.Fatalf("tick failed (this is the exact failure mode the bug produced): %v\n%s", err, out.String())
	}
	var tick map[string]any
	if err := json.Unmarshal(out.Bytes(), &tick); err != nil {
		t.Fatalf("decode tick result: %v\n%s", err, out.String())
	}
	if tick["state"] != workflow.LoopStateSuccess {
		t.Fatalf("unexpected tick result: %#v", tick)
	}
	runID, _ := tick["run_id"].(string)

	// The child run must be findable in the SAME (redirected) database the
	// loop's own row lives in — not silently split across two files.
	store, err := openPalliumStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Run(runID); err != nil {
		t.Fatalf("expected the child run %q to be found in the PALLIUM_TEST_DB-redirected database, got: %v", runID, err)
	}
}

func TestLoopStartRejectsScriptWithoutLoopKind(t *testing.T) {
	repo, dbPath := newLoopTestRepo(t)
	scriptPath := filepath.Join(repo, "not-a-loop.js")
	if err := os.WriteFile(scriptPath, []byte(`export const meta = { name: "x", description: "y", phases: ["p"] };
return {};`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runLoop(&out, []string{"start", "l1", "--script", scriptPath, "--cwd", repo, "--db", dbPath}, false)
	if err == nil {
		t.Fatal("expected loop start to reject a script whose meta.kind is not \"loop\"")
	}
}

// TestLoopTickTwiceSpawnsDistinctRunsNoCrossTickCache is one of the three
// regression tests re-pinned explicitly for this PR, in its simpler shape
// after the option-B design change: two ticks of the SAME loop, same
// script, same agent() call, must each do FRESH work in their own child
// run — no cached agent result crosses a tick boundary. Under Option B
// (fresh child run per tick, no shared run id) this is actually
// structurally guaranteed rather than merely likely, but the guarantee is
// only real if `loop tick` truly mints a distinct run id every time and
// each one's agent call actually executes — this test proves that
// observably rather than trusting the architecture description.
func TestLoopTickTwiceSpawnsDistinctRunsNoCrossTickCache(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `observed`)
	repo, dbPath := newLoopTestRepo(t)
	scriptPath := writeLoopScript(t, repo, `const result = agent("check something", { label: "observe" });
return { state: "success", signature: "sig-" + result };`)

	var out bytes.Buffer
	if err := runLoop(&out, []string{"start", "test-loop", "--script", scriptPath, "--cwd", repo, "--db", dbPath}, true); err != nil {
		t.Fatalf("loop start failed: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := runLoop(&out, []string{"tick", "test-loop", "--db", dbPath}, true); err != nil {
		t.Fatalf("first tick failed: %v\n%s", err, out.String())
	}
	var tick1 map[string]any
	if err := json.Unmarshal(out.Bytes(), &tick1); err != nil {
		t.Fatalf("decode tick1: %v\n%s", err, out.String())
	}
	if tick1["state"] != workflow.LoopStateSuccess || tick1["cycle"] != float64(1) {
		t.Fatalf("unexpected first tick result: %#v", tick1)
	}
	run1ID, _ := tick1["run_id"].(string)

	out.Reset()
	if err := runLoop(&out, []string{"tick", "test-loop", "--db", dbPath}, true); err != nil {
		t.Fatalf("second tick failed: %v\n%s", err, out.String())
	}
	var tick2 map[string]any
	if err := json.Unmarshal(out.Bytes(), &tick2); err != nil {
		t.Fatalf("decode tick2: %v\n%s", err, out.String())
	}
	if tick2["cycle"] != float64(2) {
		t.Fatalf("expected cycle 2, got %#v", tick2)
	}
	run2ID, _ := tick2["run_id"].(string)
	if run1ID == "" || run2ID == "" || run1ID == run2ID {
		t.Fatalf("expected two DISTINCT non-empty child run ids across ticks, got %q and %q", run1ID, run2ID)
	}

	store, err := openPalliumStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runs, err := store.RunsByLoop("test-loop", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected RunsByLoop to show 2 distinct child runs, got %d: %+v", len(runs), runs)
	}
	for _, r := range runs {
		count, _, uerr := store.AgentUsage(r.ID)
		if uerr != nil {
			t.Fatal(uerr)
		}
		if count != 1 {
			t.Fatalf("expected each tick's own run to have made exactly ONE fresh agent call (no cross-tick cache reuse), run %s had count=%d", r.ID, count)
		}
	}
}

// TestLoopTickAlreadyRunningIsExitCodeOnlyWithZeroStateMutation is one of
// the three regression tests re-pinned explicitly for this PR: a tick that
// loses BeginLoopTick's CAS must exit with the already_running code and
// touch NOTHING on the loop row — no cycle increment, no terminal-state
// write, no stagnation change. Simulated the same way team turn-in-flight
// tests do: hand-set tick_started_at to "now" so the CAS is genuinely held.
func TestLoopTickAlreadyRunningIsExitCodeOnlyWithZeroStateMutation(t *testing.T) {
	repo, dbPath := newLoopTestRepo(t)
	scriptPath := writeLoopScript(t, repo, `return { state: "success" };`)

	var out bytes.Buffer
	if err := runLoop(&out, []string{"start", "test-loop", "--script", scriptPath, "--cwd", repo, "--db", dbPath}, true); err != nil {
		t.Fatalf("loop start failed: %v\n%s", err, out.String())
	}
	store, err := openPalliumStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	staleAfter := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339Nano)
	lease, err := store.BeginLoopTick("test-loop", staleAfter)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease // hold the lease open — do NOT finish it, simulating an in-flight tick
	before, err := store.GetLoop("test-loop")
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	out.Reset()
	err = runLoop(&out, []string{"tick", "test-loop", "--db", dbPath}, true)
	var exitErr *LoopTickExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected a *LoopTickExitError, got %v", err)
	}
	if exitErr.State != workflow.LoopStateAlreadyRunning {
		t.Fatalf("expected already_running state, got %q", exitErr.State)
	}
	if workflow.LoopExitCode(workflow.LoopStateAlreadyRunning) != exitErr.Code {
		t.Fatalf("exit code mismatch: %d", exitErr.Code)
	}

	store2, err := openPalliumStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	after, err := store2.GetLoop("test-loop")
	if err != nil {
		t.Fatal(err)
	}
	if after.Cycle != before.Cycle || after.LastTerminalState != before.LastTerminalState || after.StagnationCount != before.StagnationCount {
		t.Fatalf("expected ZERO state mutation from an already_running tick, before=%+v after=%+v", before, after)
	}
}

// TestLoopTickErrorDoesNotResetStagnation is one of the three regression
// tests re-pinned explicitly for this PR, exercised end-to-end (not just at
// the pure-function level): a tick whose script throws must leave
// stagnation_count exactly where it was, not reset to 0 and not advanced.
func TestLoopTickErrorDoesNotResetStagnation(t *testing.T) {
	repo, dbPath := newLoopTestRepo(t)
	// First tick: a real, repeatable observation that stagnates the counter
	// partway (count=1, not yet at the threshold of 3).
	scriptPath := writeLoopScript(t, repo, `return { state: "no_op", signature: "stuck" };`)
	var out bytes.Buffer
	if err := runLoop(&out, []string{"start", "test-loop", "--script", scriptPath, "--cwd", repo, "--db", dbPath}, true); err != nil {
		t.Fatalf("loop start failed: %v\n%s", err, out.String())
	}
	for i := 0; i < 2; i++ {
		out.Reset()
		// no_op is an EXPECTED non-success terminal state here (the script
		// deliberately never reports success), which runLoop surfaces as a
		// non-nil *LoopTickExitError by design — only a genuinely unexpected
		// error type is a real test failure.
		var exitErr *LoopTickExitError
		if err := runLoop(&out, []string{"tick", "test-loop", "--db", dbPath}, true); err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("tick %d failed: %v\n%s", i+1, err, out.String())
		}
	}
	store, err := openPalliumStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := store.GetLoop("test-loop")
	if err != nil {
		t.Fatal(err)
	}
	if mid.StagnationCount == 0 {
		t.Fatalf("expected the repeated signature to have advanced stagnation_count above 0 already, got %+v", mid)
	}
	store.Close()

	// Overwrite the script to throw, forcing an error tick, then tick again.
	if err := os.WriteFile(scriptPath, []byte(`export const meta = { name: "test-loop", description: "d", kind: "loop", loop: { stagnationThreshold: 3 } };
throw new Error("simulated failure");`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	_ = runLoop(&out, []string{"tick", "test-loop", "--db", dbPath}, true)

	store2, err := openPalliumStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	after, err := store2.GetLoop("test-loop")
	if err != nil {
		t.Fatal(err)
	}
	if after.LastTerminalState != workflow.LoopStateError {
		t.Fatalf("expected the throwing tick to record state=error, got %+v", after)
	}
	if after.StagnationCount != mid.StagnationCount {
		t.Fatalf("expected an error tick to leave stagnation_count UNCHANGED (%d), got %d", mid.StagnationCount, after.StagnationCount)
	}
}

func TestLoopStopPreventsFurtherTicks(t *testing.T) {
	repo, dbPath := newLoopTestRepo(t)
	scriptPath := writeLoopScript(t, repo, `return { state: "success" };`)
	var out bytes.Buffer
	if err := runLoop(&out, []string{"start", "test-loop", "--script", scriptPath, "--cwd", repo, "--db", dbPath}, true); err != nil {
		t.Fatalf("loop start failed: %v\n%s", err, out.String())
	}
	if err := runLoop(&out, []string{"stop", "test-loop", "--db", dbPath}, true); err != nil {
		t.Fatalf("loop stop failed: %v", err)
	}
	out.Reset()
	if err := runLoop(&out, []string{"tick", "test-loop", "--db", dbPath}, true); err == nil {
		t.Fatal("expected tick to fail against a stopped loop")
	}
}

func TestLoopResetClearsStagnationKeepsCycle(t *testing.T) {
	repo, dbPath := newLoopTestRepo(t)
	scriptPath := writeLoopScript(t, repo, `return { state: "no_op", signature: "stuck" };`)
	var out bytes.Buffer
	if err := runLoop(&out, []string{"start", "test-loop", "--script", scriptPath, "--cwd", repo, "--db", dbPath}, true); err != nil {
		t.Fatalf("loop start failed: %v\n%s", err, out.String())
	}
	for i := 0; i < 2; i++ {
		out.Reset()
		_ = runLoop(&out, []string{"tick", "test-loop", "--db", dbPath}, true)
	}
	if err := runLoop(&out, []string{"reset", "test-loop", "--db", dbPath}, true); err != nil {
		t.Fatalf("loop reset failed: %v", err)
	}
	store, err := openPalliumStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loop, err := store.GetLoop("test-loop")
	if err != nil {
		t.Fatal(err)
	}
	if loop.StagnationCount != 0 || loop.LastSignature != "" {
		t.Fatalf("expected reset to clear stagnation, got %+v", loop)
	}
	if loop.Cycle != 2 {
		t.Fatalf("expected reset to preserve cycle count, got %d", loop.Cycle)
	}
}
