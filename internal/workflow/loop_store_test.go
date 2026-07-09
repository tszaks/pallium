package workflow

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func newLoopTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestLoopCRUDRoundTrip(t *testing.T) {
	store := newLoopTestStore(t)

	loop, err := store.CreateLoop(Loop{Name: "review-until-clean", ScriptPath: "review.js", CWD: "/tmp/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if loop.Status != "active" || loop.Cycle != 0 || loop.StagnationThreshold != 3 {
		t.Fatalf("unexpected new loop state: %+v", loop)
	}

	got, err := store.GetLoop("review-until-clean")
	if err != nil {
		t.Fatal(err)
	}
	if got.ScriptPath != "review.js" || got.CWD != "/tmp/repo" {
		t.Fatalf("unexpected loop round-trip: %+v", got)
	}

	loops, err := store.ListLoops()
	if err != nil {
		t.Fatal(err)
	}
	if len(loops) != 1 || loops[0].Name != "review-until-clean" {
		t.Fatalf("unexpected ListLoops result: %+v", loops)
	}

	if err := store.SetLoopStatus("review-until-clean", "stopped"); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetLoop("review-until-clean")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("expected stopped status, got %+v", got)
	}
}

func TestCreateLoopRejectsDuplicateName(t *testing.T) {
	store := newLoopTestStore(t)
	if _, err := store.CreateLoop(Loop{Name: "dup", ScriptPath: "a.js"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateLoop(Loop{Name: "dup", ScriptPath: "b.js"}); err == nil {
		t.Fatal("expected an error creating a loop with a name that already exists")
	}
}

func TestResetLoopClearsStagnationNotCycle(t *testing.T) {
	store := newLoopTestStore(t)
	if _, err := store.CreateLoop(Loop{Name: "l1", ScriptPath: "a.js"}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.BeginLoopTick("l1", nowString())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLoopTick("l1", lease, "stagnated", "sig-1", 4, 0.01); err != nil {
		t.Fatal(err)
	}
	loop, err := store.GetLoop("l1")
	if err != nil {
		t.Fatal(err)
	}
	if loop.Cycle != 1 || loop.StagnationCount != 4 || loop.LastSignature != "sig-1" {
		t.Fatalf("unexpected state before reset: %+v", loop)
	}

	if err := store.ResetLoop("l1"); err != nil {
		t.Fatal(err)
	}
	loop, err = store.GetLoop("l1")
	if err != nil {
		t.Fatal(err)
	}
	if loop.StagnationCount != 0 || loop.LastSignature != "" {
		t.Fatalf("expected stagnation/signature cleared, got %+v", loop)
	}
	if loop.Cycle != 1 {
		t.Fatalf("expected cycle count UNTOUCHED by reset, got %d", loop.Cycle)
	}
}

func TestBeginLoopTickRejectsStoppedLoop(t *testing.T) {
	store := newLoopTestStore(t)
	if _, err := store.CreateLoop(Loop{Name: "l1", ScriptPath: "a.js"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetLoopStatus("l1", "stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginLoopTick("l1", nowString()); err == nil {
		t.Fatal("expected BeginLoopTick to refuse a stopped loop")
	}
}

func TestFinishLoopTickRejectsStaleLease(t *testing.T) {
	store := newLoopTestStore(t)
	if _, err := store.CreateLoop(Loop{Name: "l1", ScriptPath: "a.js"}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.BeginLoopTick("l1", nowString())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLoopTick("l1", "not-the-real-lease", "success", "", 0, 0); err == nil {
		t.Fatal("expected FinishLoopTick to reject a lease that doesn't match")
	}
	// The real lease must still work afterward — the rejected call above must
	// not have corrupted the row.
	if err := store.FinishLoopTick("l1", lease, "success", "", 0, 0); err != nil {
		t.Fatal(err)
	}
}

// TestFinishLoopTickAsErrorReleasesLease is the regression test for a P1
// found by adversarial review: runLoopTick used to leak the lease
// (permanently, for up to --stale-after-minutes) if anything failed between
// a successful BeginLoopTick and the next explicit FinishLoopTick call —
// e.g. a transient GetLoop error that had nothing to do with the tick
// itself. FinishLoopTickAsError is the minimal release the fix's defer-based
// safety net calls on any such early return.
func TestFinishLoopTickAsErrorReleasesLease(t *testing.T) {
	store := newLoopTestStore(t)
	if _, err := store.CreateLoop(Loop{Name: "l1", ScriptPath: "a.js"}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.BeginLoopTick("l1", nowString())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLoopTickAsError("l1", lease); err != nil {
		t.Fatal(err)
	}
	loop, err := store.GetLoop("l1")
	if err != nil {
		t.Fatal(err)
	}
	if loop.TickStartedAt != "" {
		t.Fatalf("expected the lease to be released (tick_started_at cleared), got %+v", loop)
	}
	if loop.Cycle != 1 || loop.LastTerminalState != LoopStateError {
		t.Fatalf("expected cycle incremented and terminal state=error, got %+v", loop)
	}
	// Must be immediately tickable again — the whole point of releasing it —
	// not stuck until --stale-after-minutes elapses.
	if _, err := store.BeginLoopTick("l1", nowString()); err != nil {
		t.Fatalf("expected the loop to be immediately tickable again after the error release, got: %v", err)
	}
}

// TestFinishLoopTickAsErrorIsHarmlessNoOpWhenAlreadyReleased proves the
// safety-net defer is safe to fire UNCONDITIONALLY on every return path,
// including the normal happy path where a real FinishLoopTick already ran:
// calling it again with the now-stale lease must not overwrite whatever the
// real Finish already wrote.
func TestFinishLoopTickAsErrorIsHarmlessNoOpWhenAlreadyReleased(t *testing.T) {
	store := newLoopTestStore(t)
	if _, err := store.CreateLoop(Loop{Name: "l1", ScriptPath: "a.js"}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.BeginLoopTick("l1", nowString())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLoopTick("l1", lease, LoopStateSuccess, "sig-real", 0, 0.5); err != nil {
		t.Fatal(err)
	}
	// Simulates the deferred safety net firing after a real Finish already
	// happened — must be a silent no-op (see FinishLoopTickAsError's own
	// doc comment: RowsAffected==0 here is the expected common case).
	if err := store.FinishLoopTickAsError("l1", lease); err != nil {
		t.Fatal(err)
	}
	loop, err := store.GetLoop("l1")
	if err != nil {
		t.Fatal(err)
	}
	if loop.Cycle != 1 || loop.LastTerminalState != LoopStateSuccess || loop.LastSignature != "sig-real" {
		t.Fatalf("expected the real Finish's values UNTOUCHED by the harmless no-op, got %+v", loop)
	}
}

// TestConcurrentLoopTicksOnlyOneWins is one of the three regression tests
// re-pinned explicitly for this PR: N racing `loop tick` invocations on the
// SAME loop (the real scenario — cron firing again while a slow previous
// tick is still running) must have exactly one winner; every loser gets
// ErrLoopTickInFlight, not a corrupted or double-counted cycle.
func TestConcurrentLoopTicksOnlyOneWins(t *testing.T) {
	store := newLoopTestStore(t)
	if _, err := store.CreateLoop(Loop{Name: "l1", ScriptPath: "a.js"}); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	leases := make([]string, n)
	errs := make([]error, n)
	staleAfter := nowString()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = store.BeginLoopTick("l1", staleAfter)
		}(i)
	}
	wg.Wait()

	wins, contended := 0, 0
	for i := 0; i < n; i++ {
		switch {
		case errs[i] == nil:
			wins++
		case errs[i] == ErrLoopTickInFlight:
			contended++
		default:
			t.Fatalf("unexpected error from BeginLoopTick: %v", errs[i])
		}
	}
	if wins != 1 || contended != n-1 {
		t.Fatalf("expected exactly 1 winner and %d contended losers, got wins=%d contended=%d", n-1, wins, contended)
	}
}

func TestWithTxCommitsOnSuccessAndRollsBackOnError(t *testing.T) {
	store := newLoopTestStore(t)
	if _, err := store.CreateLoop(Loop{Name: "l1", ScriptPath: "a.js"}); err != nil {
		t.Fatal(err)
	}

	if err := store.WithTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE workflow_loops SET cycle=99 WHERE name=?`, "l1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	loop, err := store.GetLoop("l1")
	if err != nil {
		t.Fatal(err)
	}
	if loop.Cycle != 99 {
		t.Fatalf("expected the committed transaction's write to be visible, got cycle=%d", loop.Cycle)
	}

	failure := fmt.Errorf("deliberate failure")
	err = store.WithTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE workflow_loops SET cycle=12345 WHERE name=?`, "l1"); err != nil {
			return err
		}
		return failure
	})
	if err != failure {
		t.Fatalf("expected WithTx to propagate fn's error, got %v", err)
	}
	loop, err = store.GetLoop("l1")
	if err != nil {
		t.Fatal(err)
	}
	if loop.Cycle != 99 {
		t.Fatalf("expected the failed transaction's write rolled back, got cycle=%d", loop.Cycle)
	}
}
