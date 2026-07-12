package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func newRunLivenessTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// deadPID spawns and waits out a real child process so the returned pid is
// guaranteed to no longer exist, without relying on a hardcoded "surely
// nothing uses this number" constant (fragile across darwin/linux pid_max
// differences).
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn throwaway process: %v", err)
	}
	return cmd.Process.Pid
}

func TestReconcileStaleRunsKilledRunBecomesInterrupted(t *testing.T) {
	store := newRunLivenessTestStore(t)
	run, err := store.CreateRun(Run{Task: "t", CWD: "/tmp", ScriptPath: "x.js", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	pid := deadPID(t)
	if _, err := store.db.Exec(`UPDATE workflow_runs SET owner_pid=?, heartbeat_at=? WHERE id=?`, pid, nowString(), run.ID); err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(Agent{RunID: run.ID, Prompt: "p", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}

	reconciled, err := store.ReconcileStaleRuns(StaleAfterString(15))
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0].RunID != run.ID || reconciled[0].AgentsReconciled != 1 {
		t.Fatalf("expected run %s reconciled with 1 agent, got %+v", run.ID, reconciled)
	}

	got, err := store.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" {
		t.Fatalf("expected run status interrupted, got %q", got.Status)
	}
	gotAgents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAgents) != 1 || gotAgents[0].ID != agent.ID || gotAgents[0].Status != "interrupted" {
		t.Fatalf("expected agent interrupted, got %+v", gotAgents)
	}
}

// TestReconcileStaleRunsLiveRunNeverReconciled is the race-safety proof the
// 0.9.15 batch explicitly calls for: a run whose owner_pid is genuinely
// alive (this test process itself) must never flip to interrupted, no
// matter how many concurrent reconcile passes run against it or how far in
// the past its heartbeat looks.
func TestReconcileStaleRunsLiveRunNeverReconciled(t *testing.T) {
	store := newRunLivenessTestStore(t)
	run, err := store.CreateRun(Run{Task: "t", CWD: "/tmp", ScriptPath: "x.js", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	// A heartbeat far in the past would trip the time-window fallback if the
	// pid check were skipped or wrong — this row must survive purely because
	// its pid is alive.
	ancientHeartbeat := "2000-01-01T00:00:00Z"
	if _, err := store.db.Exec(`UPDATE workflow_runs SET owner_pid=?, heartbeat_at=? WHERE id=?`, os.Getpid(), ancientHeartbeat, run.ID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.ReconcileStaleRuns(StaleAfterString(15)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	got, err := store.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("expected a live-owned run to stay running, got %q", got.Status)
	}
}

func TestReconcileStaleRunsOrphanedAgentUnderAlreadyStoppedRun(t *testing.T) {
	store := newRunLivenessTestStore(t)
	run, err := store.CreateRun(Run{Task: "t", CWD: "/tmp", ScriptPath: "x.js", Status: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(Agent{RunID: run.ID, Prompt: "p", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}

	reconciled, err := store.ReconcileStaleRuns(StaleAfterString(15))
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0].RunID != run.ID || reconciled[0].AgentsReconciled != 1 {
		t.Fatalf("expected orphaned agent under stopped run reconciled, got %+v", reconciled)
	}
	got, err := store.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("reconcile must not touch an already-terminal run's own status, got %q", got.Status)
	}
	gotAgent, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAgent) != 1 || gotAgent[0].ID != agent.ID || gotAgent[0].Status != "interrupted" {
		t.Fatalf("expected orphaned agent interrupted, got %+v", gotAgent)
	}
}

func TestReconcileStaleRunsUnclaimedLegacyRowUsesTimeWindowFallback(t *testing.T) {
	store := newRunLivenessTestStore(t)
	oldRun, err := store.CreateRun(Run{Task: "old", CWD: "/tmp", ScriptPath: "x.js", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	// owner_pid stays 0 (never claimed) — simulates a pre-0.9.15 row.
	if _, err := store.db.Exec(`UPDATE workflow_runs SET created_at=?, heartbeat_at='' WHERE id=?`, "2000-01-01T00:00:00Z", oldRun.ID); err != nil {
		t.Fatal(err)
	}
	freshRun, err := store.CreateRun(Run{Task: "fresh", CWD: "/tmp", ScriptPath: "x.js", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}

	reconciled, err := store.ReconcileStaleRuns(StaleAfterString(15))
	if err != nil {
		t.Fatal(err)
	}
	reconciledIDs := map[string]bool{}
	for _, r := range reconciled {
		reconciledIDs[r.RunID] = true
	}
	if !reconciledIDs[oldRun.ID] {
		t.Fatalf("expected unclaimed old row reconciled via time-window fallback, got %+v", reconciled)
	}
	if reconciledIDs[freshRun.ID] {
		t.Fatalf("unclaimed but fresh row must not be reconciled, got %+v", reconciled)
	}
	got, err := store.Run(freshRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("fresh unclaimed run should stay running, got %q", got.Status)
	}
}

func TestClaimRunOwnershipAndHeartbeat(t *testing.T) {
	store := newRunLivenessTestStore(t)
	run, err := store.CreateRun(Run{Task: "t", CWD: "/tmp", ScriptPath: "x.js", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimRunOwnership(run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerPID != os.Getpid() {
		t.Fatalf("expected owner_pid to be this test process, got %d", got.OwnerPID)
	}
	if got.HeartbeatAt == "" {
		t.Fatalf("expected heartbeat_at to be set after claim")
	}
	// A claimed, currently-alive run must survive reconcile even under a
	// very tight stale window.
	if _, err := store.ReconcileStaleRuns(StaleAfterString(1)); err != nil {
		t.Fatal(err)
	}
	got, err = store.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("expected claimed live run to survive reconcile, got %q", got.Status)
	}
}

func TestIsProcessAliveDetectsDeadPID(t *testing.T) {
	if isProcessAlive(deadPID(t)) {
		t.Fatal("expected dead pid to report not alive")
	}
	if !isProcessAlive(os.Getpid()) {
		t.Fatal("expected this test process's own pid to report alive")
	}
}
