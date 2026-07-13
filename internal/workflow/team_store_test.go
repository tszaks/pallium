package workflow

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTeamCRUDRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	team, err := store.CreateTeam("review the PR diff", tmp, 5.0)
	if err != nil {
		t.Fatal(err)
	}
	if team.Status != "active" {
		t.Fatalf("expected active team, got %+v", team)
	}

	m, err := store.SpawnMember(team.ID, "researcher-1", "claude", "", "researcher", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "idle" || m.TurnCount != 0 {
		t.Fatalf("expected idle fresh member, got %+v", m)
	}
	if _, err := store.SpawnMember(team.ID, "researcher-1", "claude", "", "researcher", "read-only"); err == nil {
		t.Fatal("expected spawning a duplicate name to fail")
	}

	task, err := store.CreateTeamTask(team.ID, "read module A", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" {
		t.Fatalf("expected pending task, got %+v", task)
	}

	msg, err := store.SendTeamMessage(team.ID, "lead", "researcher-1", "start with module A")
	if err != nil {
		t.Fatal(err)
	}
	undelivered, err := store.UndeliveredMessages(team.ID, "researcher-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(undelivered) != 1 || undelivered[0].ID != msg.ID {
		t.Fatalf("expected one undelivered message, got %+v", undelivered)
	}
	if err := store.MarkMessagesDelivered([]string{msg.ID}, 1); err != nil {
		t.Fatal(err)
	}
	undelivered, err = store.UndeliveredMessages(team.ID, "researcher-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(undelivered) != 0 {
		t.Fatalf("expected the message to be delivered, got %+v", undelivered)
	}
}

func TestClaimTaskRespectsDependencies(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	dep, _ := store.CreateTeamTask(team.ID, "dep task", "", nil)
	blocked, _ := store.CreateTeamTask(team.ID, "blocked task", "", []string{dep.ID})

	if _, err := store.ClaimTask(team.ID, blocked.ID, "worker-1"); err != errTaskNotClaimable {
		t.Fatalf("expected errTaskNotClaimable while dependency is pending, got %v", err)
	}
	if _, err := store.ClaimTask(team.ID, dep.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTask(team.ID, dep.ID, "worker-1", "done"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTask(team.ID, blocked.ID, "worker-2")
	if err != nil {
		t.Fatalf("expected the blocked task to be claimable once its dependency completed: %v", err)
	}
	if claimed.Owner != "worker-2" || claimed.Status != "in_progress" {
		t.Fatalf("unexpected claimed task state: %+v", claimed)
	}
}

// TestClaimTaskConcurrentOnlyOneWins races N teammates claiming the SAME
// task. The UPDATE ... WHERE status='pending' AND owner IS NULL guard is the
// compare-and-swap: only one racer's UPDATE can match the row, so exactly
// one must win — the same shape as the repo-lock CAS fix, applied here from
// the start rather than found by a later review.
func TestClaimTaskConcurrentOnlyOneWins(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "sessions.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	team, err := seed.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	task, err := seed.CreateTeamTask(team.ID, "contested task", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	seed.Close()

	const n = 8
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
			_, err := stores[i].ClaimTask(team.ID, task.ID, fmt.Sprintf("worker-%d", i))
			results[i] = err == nil
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
		t.Fatalf("expected exactly one claim winner, got %d", wins)
	}
	final, err := stores[0].GetTeamTask(team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "in_progress" || final.Owner == "" {
		t.Fatalf("expected the task to be claimed by exactly one owner, got %+v", final)
	}
}

func TestBeginMemberTurnPreventsDoubleSchedule(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	longAgo := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)

	lease, err := store.BeginMemberTurn(team.ID, "worker-1", longAgo)
	if err != nil || lease == "" {
		t.Fatalf("expected the first BeginMemberTurn to succeed with a lease, got lease=%q err=%v", lease, err)
	}
	// A second scheduler trying to start the SAME member's turn while it is
	// already in flight (and not stale) must be refused, not double-run.
	if _, err := store.BeginMemberTurn(team.ID, "worker-1", longAgo); err != errMemberTurnInFlight {
		t.Fatalf("expected errMemberTurnInFlight for a turn already in progress, got %v", err)
	}
	if err := store.FinishMemberTurn(team.ID, "worker-1", lease, "idle", "sess-1", "", 0); err != nil {
		t.Fatal(err)
	}
	m, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.TurnStartedAt != "" || m.TurnCount != 1 || m.SessionToken != "sess-1" {
		t.Fatalf("expected a closed-out turn with the session captured, got %+v", m)
	}
}

// TestStopRequestedMidTurnSurvivesFinishMemberTurn is the core regression
// test for M2 individual supervision's central correctness property: a
// `team member stop` issued WHILE a turn is in flight must not be silently
// discarded the instant that turn finishes. FinishMemberTurn unconditionally
// overwrote the live status column with whatever the turn itself decided
// (active/idle/error), keyed only on the lease — StopRequested has to be an
// independent column FinishMemberTurn itself respects, not a race against
// that same write.
func TestStopRequestedMidTurnSurvivesFinishMemberTurn(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}

	lease, err := store.BeginMemberTurn(team.ID, "worker-1", staleAfterLongAgo(t))
	if err != nil {
		t.Fatal(err)
	}

	// The stop lands WHILE the turn above is still in flight.
	if _, err := store.RequestMemberStop(team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	mid, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if !mid.StopRequested {
		t.Fatalf("expected StopRequested set immediately, got %+v", mid)
	}
	if mid.Status == "stopped" {
		t.Fatalf("expected Status left alone while the turn is still in flight (display-only, flips at the turn boundary), got %+v", mid)
	}

	// The in-flight turn finishes and, having no idea a stop was requested,
	// decides "active" — exactly the scenario that used to silently discard
	// the stop.
	if err := store.FinishMemberTurn(team.ID, "worker-1", lease, "active", "sess-1", "", 0); err != nil {
		t.Fatal(err)
	}
	final, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "stopped" {
		t.Fatalf("expected the stop to take effect once the in-flight turn finished, got status=%q (StopRequested=%v)", final.Status, final.StopRequested)
	}
	if final.LastTurnStatus != "active" {
		t.Fatalf("expected LastTurnStatus to still record the turn's OWN decision for history, got %q", final.LastTurnStatus)
	}
	if !final.SessionEstablished || final.SessionToken != "sess-1" {
		t.Fatalf("expected the turn's own successful outcome (session established) unaffected by the stop override, got %+v", final)
	}
}

// TestBeginMemberTurnRejectsStopRequestedMember is the regression test for
// the review finding that stop_requested was only checked by RunTeam's
// eligibility SNAPSHOT (ListMembers, read before any goroutine dispatches),
// not by the actual acquisition CAS. A stop landing in the gap between that
// snapshot and this call used to still succeed and dispatch one more real
// provider call regardless — this asserts the ACQUISITION itself now
// refuses, independent of any scheduler-level check.
func TestBeginMemberTurnRejectsStopRequestedMember(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	// Simulates the exact race: eligibility was computed on a snapshot
	// where this member looked schedulable, but the stop lands before this
	// call — the only thing that actually matters is whether THIS call
	// succeeds.
	if _, err := store.RequestMemberStop(team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginMemberTurn(team.ID, "worker-1", staleAfterLongAgo(t)); err != errMemberTurnInFlight {
		t.Fatalf("expected the acquisition itself to refuse a stop-requested member, got %v", err)
	}
}

// TestRunTeamSkipsStopRequestedMember proves the scheduler actually
// respects StopRequested, not just that the flag persists correctly.
func TestRunTeamSkipsStopRequestedMember(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see other gate tests' comment for why
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTeamTask(team.ID, "do something", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestMemberStop(team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	r := &Runner{Run: Run{ID: team.ID}}
	summary, err := r.RunTeam(context.Background(), store, team.ID, TeamTurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TurnsTaken != 0 {
		t.Fatalf("expected a stop-requested member to never be scheduled despite a real claimable task, got %+v", summary)
	}
}

// TestRunTeamSkipsExternalMembers is the M3 external-attach counterpart to
// the stop-requested test above: an external member has no provider to
// dispatch a turn through at all, so RunTeam's scheduler must never offer
// it one, no matter how much claimable work sits on the board.
func TestRunTeamSkipsExternalMembers(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.JoinExternalMember(team.ID, "advisor"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTeamTask(team.ID, "do something", "", nil); err != nil {
		t.Fatal(err)
	}

	r := &Runner{Run: Run{ID: team.ID}}
	summary, err := r.RunTeam(context.Background(), store, team.ID, TeamTurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TurnsTaken != 0 || summary.Rounds != 0 {
		t.Fatalf("expected an external member to never be scheduled despite a real claimable task, got %+v", summary)
	}
	member, err := store.GetMember(team.ID, "advisor")
	if err != nil {
		t.Fatal(err)
	}
	if member.TurnCount != 0 {
		t.Fatalf("expected the external member's turn count untouched, got %+v", member)
	}
}

// TestRestartMemberClearsStopAndReconcilesStaleTurn covers restart's two
// jobs together: clearing the stop, and immediately unsticking a turn that
// was ALSO left stale (rather than requiring a separate manual `team
// attach` on top of the restart).
func TestRestartMemberClearsStopAndReconcilesStaleTurn(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginMemberTurn(team.ID, "worker-1", staleAfterLongAgo(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestMemberStop(team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	// The turn never finishes on its own — simulates a hung/dead provider
	// process, the exact scenario ReconcileInterruptedMembers exists for.
	// BeginMemberTurn always stamps turn_started_at with the CURRENT time
	// regardless of the staleAfter argument (that argument only governs
	// whether an EXISTING lease can be taken over) — backdating it directly,
	// matching the established pattern elsewhere in this file, is the only
	// way to make it actually look stale to the cutoff passed below.
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.Exec(`UPDATE team_members SET turn_started_at=? WHERE team_id=? AND name=?`, old, team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)

	if _, err := store.RestartMember(team.ID, "worker-1", cutoff); err != nil {
		t.Fatal(err)
	}
	final, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if final.StopRequested {
		t.Fatalf("expected StopRequested cleared, got %+v", final)
	}
	if final.TurnStartedAt != "" {
		t.Fatalf("expected the stale in-flight turn reconciled (lease released) as part of restart, got %+v", final)
	}
	if final.SessionToken != "sess-1" {
		t.Fatalf("expected the session token preserved across reconciliation, got %+v", final)
	}
	if final.Status != "interrupted" {
		t.Fatalf("expected the reconciled member's status to reflect the interrupted turn (ReconcileInterruptedMembers' own status, unmodified by restart), got %q", final.Status)
	}
	if final.NudgedAt == "" {
		t.Fatalf("expected restart to nudge the member so RunTeam actually schedules it again — clearing stop_requested and reconciling a stale lease do not by themselves create a scheduling signal, got %+v", final)
	}
}

// TestRestartMemberDoesNotReconcileOtherMembersInFlightTurns is the
// regression test for the review finding that restart used to call the
// team-wide ReconcileInterruptedMembers sweep: an operator force-restarting
// ONE hung teammate with a short --stale-after-minutes cutoff would also
// reconcile every OTHER stale-looking member on the team, potentially
// clobbering a turn that was legitimately still in flight (just running
// long) and reverting its owned task while the real provider call keeps
// going — letting a second turn double-run the same member.
func TestRestartMemberDoesNotReconcileOtherMembersInFlightTurns(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-2", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	// worker-1: genuinely stopped, its turn old enough to look stale to any
	// reasonable cutoff — the one restart is actually targeting.
	if _, err := store.BeginMemberTurn(team.ID, "worker-1", staleAfterLongAgo(t)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.Exec(`UPDATE team_members SET turn_started_at=? WHERE team_id=? AND name=?`, old, team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestMemberStop(team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	// worker-2: a DIFFERENT member whose turn is merely SLOW (a real
	// provider call still genuinely running), not dead — but old enough
	// that it ALSO looks stale to the same short cutoff an operator would
	// reasonably use to force-restart worker-1's genuinely dead turn. This
	// is the exact scenario the finding describes: a short
	// --stale-after-minutes catches both the truly-dead member being
	// restarted AND a merely-slow one that isn't.
	if _, err := store.BeginMemberTurn(team.ID, "worker-2", staleAfterLongAgo(t)); err != nil {
		t.Fatal(err)
	}
	// Backdating directly replaces whatever lease BeginMemberTurn just
	// returned — FinishMemberTurn below must be given THIS value, the one
	// actually stored, not BeginMemberTurn's now-stale return value.
	lease2 := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.Exec(`UPDATE team_members SET turn_started_at=? WHERE team_id=? AND name=?`, lease2, team.ID, "worker-2"); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTeamTask(team.ID, "worker-2's real work", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-2"); err != nil {
		t.Fatal(err)
	}

	// A cutoff that catches BOTH: worker-1 (2 hours old) and worker-2 (30
	// minutes old) are both stale relative to 15 minutes ago. Only the
	// member-scoped fix distinguishes "the one restart actually named"
	// from "everyone who happens to look stale right now."
	cutoff := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := store.RestartMember(team.ID, "worker-1", cutoff); err != nil {
		t.Fatal(err)
	}

	other, err := store.GetMember(team.ID, "worker-2")
	if err != nil {
		t.Fatal(err)
	}
	if other.TurnStartedAt == "" {
		t.Fatalf("expected worker-2's genuinely in-flight turn left untouched by restarting worker-1, but its lease was cleared: %+v", other)
	}
	if other.Status == "interrupted" {
		t.Fatalf("expected worker-2 NOT reconciled as a side effect of restarting a different member, got %+v", other)
	}
	otherTask, err := store.GetTeamTask(team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if otherTask.Status != "in_progress" || otherTask.Owner != "worker-2" {
		t.Fatalf("expected worker-2's owned task left in_progress, not reverted as a side effect of restarting worker-1, got %+v", otherTask)
	}
	// worker-2's own turn can still finish normally afterward — proves its
	// lease is genuinely intact, not just superficially unchanged.
	if err := store.FinishMemberTurn(team.ID, "worker-2", lease2, "idle", "sess-2", "", 0); err != nil {
		t.Fatalf("expected worker-2's untouched lease to still finish normally: %v", err)
	}
}

// staleAfterLongAgo returns a staleAfter timestamp far enough in the past
// that BeginMemberTurn always succeeds and any turn started "now" is never
// itself considered stale by that same threshold.
func staleAfterLongAgo(t *testing.T) string {
	t.Helper()
	return time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
}

// TestConcurrentBeginMemberTurnStaleTakeoverOnlyOneWins mirrors the repo-lock
// double-takeover race test: several separate Store handles race to take
// over the SAME member's stale (owning-process-died) turn. Exactly one must
// win the CAS, or two schedulers would both spawn a provider call for the
// same teammate identity at once.
func TestConcurrentBeginMemberTurnStaleTakeoverOnlyOneWins(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "sessions.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	team, err := seed.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	// Put a stale in-flight turn on the row (as if a process died mid-turn a
	// long time ago).
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := seed.db.Exec(`UPDATE team_members SET turn_started_at=? WHERE team_id=? AND name=?`, old, team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	const n = 8
	stores := make([]*Store, n)
	for i := range stores {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
		defer s.Close()
	}
	staleAfter := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	results := make([]bool, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := stores[i].BeginMemberTurn(team.ID, "worker-1", staleAfter)
			results[i] = err == nil
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

func TestReconcileInterruptedMembersRevertsOwnedTasks(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTeamTask(team.ID, "task", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.Exec(`UPDATE team_members SET turn_started_at=? WHERE team_id=? AND name=?`, old, team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	staleAfter := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	interrupted, err := store.ReconcileInterruptedMembers(team.ID, staleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 1 || interrupted[0] != "worker-1" {
		t.Fatalf("expected worker-1 to be reported interrupted, got %v", interrupted)
	}
	m, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "interrupted" || m.TurnStartedAt != "" {
		t.Fatalf("expected the member marked interrupted with no turn in flight, got %+v", m)
	}
	reverted, err := store.GetTeamTask(team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Status != "pending" || reverted.Owner != "" {
		t.Fatalf("expected the interrupted member's task reverted to pending/unowned, got %+v", reverted)
	}
	// The task is claimable again by someone else.
	if _, err := store.SpawnMember(team.ID, "worker-2", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-2"); err != nil {
		t.Fatalf("expected the reverted task to be claimable by another member: %v", err)
	}
}

// TestReconcileInterruptedMembersLeavesTaskOwnedWhenSessionResumable covers
// the opposite case from the test above: an interrupted member that HAS a
// session token is expected to resume its own conversation and finish what
// it owns, so its task must NOT be stripped away — only a member with no
// session at all (nothing to resume) has genuinely orphaned work.
func TestReconcileInterruptedMembersLeavesTaskOwnedWhenSessionResumable(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("goal", tmp, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTeamTask(team.ID, "task", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.Exec(`UPDATE team_members SET turn_started_at=? WHERE team_id=? AND name=?`, old, team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	staleAfter := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	interrupted, err := store.ReconcileInterruptedMembers(team.ID, staleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 1 || interrupted[0] != "worker-1" {
		t.Fatalf("expected worker-1 reported interrupted, got %v", interrupted)
	}
	stillOwned, err := store.GetTeamTask(team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillOwned.Status != "in_progress" || stillOwned.Owner != "worker-1" {
		t.Fatalf("expected the task to remain owned by worker-1 (it has a resumable session), got %+v", stillOwned)
	}
}

func TestSpawnPlanRequiredMemberStartsReadOnlyPending(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.SpawnPlanRequiredMember(team.ID, "planner-1", "claude", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "read-only" || !m.PlanRequired || m.PlanStatus != "pending" {
		t.Fatalf("expected read-only pending plan-required member, got %+v", m)
	}
}

func TestApproveMemberPlanFlipsToEditAndJournalsMessage(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnPlanRequiredMember(team.ID, "planner-1", "claude", "", ""); err != nil {
		t.Fatal(err)
	}

	m, err := store.ApproveMemberPlan(team.ID, "planner-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "edit" || m.PlanStatus != "approved" {
		t.Fatalf("expected edit mode + approved plan, got %+v", m)
	}
	inbox, err := store.UndeliveredMessages(team.ID, "planner-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].From != "lead" {
		t.Fatalf("expected the approval journaled as a message from lead, got %+v", inbox)
	}

	if _, err := store.ApproveMemberPlan(team.ID, "planner-1"); err == nil {
		t.Fatal("expected approving an already-approved plan to fail")
	}
}

func TestRejectMemberPlanKeepsReadOnlyAndDeliversFeedback(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnPlanRequiredMember(team.ID, "planner-1", "claude", "", ""); err != nil {
		t.Fatal(err)
	}

	m, err := store.RejectMemberPlan(team.ID, "planner-1", "scope is too broad, narrow it to module A")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "read-only" || m.PlanStatus != "pending" {
		t.Fatalf("expected rejection to leave the member read-only and still pending (not terminal), got %+v", m)
	}
	inbox, err := store.UndeliveredMessages(team.ID, "planner-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || !strings.Contains(inbox[0].Body, "narrow it to module A") {
		t.Fatalf("expected the feedback delivered as a message, got %+v", inbox)
	}

	if _, err := store.RejectMemberPlan(team.ID, "planner-1", ""); err == nil {
		t.Fatal("expected rejecting with empty feedback to fail")
	}
}

func TestApproveOrRejectPlanFailsForNonPlanRequiredMember(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveMemberPlan(team.ID, "worker-1"); err == nil {
		t.Fatal("expected approving a non-plan-required member to fail")
	}
	if _, err := store.RejectMemberPlan(team.ID, "worker-1", "feedback"); err == nil {
		t.Fatal("expected rejecting a non-plan-required member to fail")
	}
}

func TestSetTeamGateRoundTripAndRejectsUnknownHook(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "verify the result actually satisfies the task", []string{"task_completed", "teammate_idle"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTeam(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GatePrompt != "verify the result actually satisfies the task" || len(got.GateHooks) != 2 {
		t.Fatalf("expected gate config round-tripped, got %+v", got)
	}
	if err := store.SetTeamGate(team.ID, "prompt", []string{"not-a-real-hook"}); err == nil {
		t.Fatal("expected an unknown hook name to be rejected")
	}
	// Regression test for the review finding: an empty prompt with non-empty
	// hooks used to persist silently — teamGateHasHook always treats an
	// empty GatePrompt as "no gating" regardless of GateHooks, so this
	// combination would report configured hooks that never actually fire.
	if err := store.SetTeamGate(team.ID, "", []string{"task_completed"}); err == nil {
		t.Fatal("expected an empty prompt with non-empty hooks to be rejected")
	}
	// The one legitimate use of an empty prompt — disabling gating
	// entirely via an empty hooks list — must still work.
	if err := store.SetTeamGate(team.ID, "", nil); err != nil {
		t.Fatalf("expected an empty prompt with an empty hooks list (disabling gating) to be allowed, got %v", err)
	}
}

// TestJoinExternalMemberCreatesAndReattaches is the core M3 external-attach
// round trip: a first join creates a real, listable member with no
// provider dispatch; a second join for the SAME name is a re-attach (fresh
// liveness, not an error), matching how a restarted or just-quiet external
// session re-announces itself.
func TestJoinExternalMemberCreatesAndReattaches(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.JoinExternalMember(team.ID, "advisor")
	if err != nil {
		t.Fatal(err)
	}
	if first.Provider != "external" || first.LastActiveAt == "" {
		t.Fatalf("expected a fresh external member with liveness set, got %+v", first)
	}

	time.Sleep(2 * time.Millisecond)
	second, err := store.JoinExternalMember(team.ID, "advisor")
	if err != nil {
		t.Fatalf("expected re-joining the same name to succeed, got %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected re-join to reuse the SAME member row, not create a second one: %+v vs %+v", first, second)
	}
	if second.LastActiveAt <= first.LastActiveAt {
		t.Fatalf("expected re-join to refresh liveness (first=%q second=%q)", first.LastActiveAt, second.LastActiveAt)
	}

	members, err := store.ListMembers(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("expected exactly one member after create+reattach, got %d: %+v", len(members), members)
	}
}

// TestJoinExternalMemberRejectsNonExternalNameCollision guards the real
// hazard JoinExternalMember's own doc comment names: joining under a name
// already held by a real provider-driven teammate would let an external
// session start acting as a name RunTeam still schedules turns for.
func TestJoinExternalMemberRejectsNonExternalNameCollision(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.JoinExternalMember(team.ID, "worker-1"); err == nil {
		t.Fatal("expected joining under a real provider-driven member's name to be rejected")
	}
}

// TestJoinExternalMemberConcurrentJoinsOfBrandNewNameNeverDuplicate is the
// regression test for the real TOCTOU an adversarial review found in the
// original check-then-act (GetMember, then branch to SpawnMember)
// implementation: two concurrent `team join` calls for the SAME brand-new
// name could both observe "not found" and both attempt SpawnMember,
// producing either two member rows (if nothing enforced uniqueness) or an
// ugly constraint error surfaced to the loser (if something did) — either
// way breaking the documented "re-joining is NOT an error" contract under
// a genuine race, not just a sequential re-join. The fix makes SpawnMember
// itself (a single atomic INSERT guarded by team_members' own
// UNIQUE(team_id,name)) the primary path, with the loser gracefully
// falling back to the existing row.
func TestJoinExternalMemberConcurrentJoinsOfBrandNewNameNeverDuplicate(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}

	const concurrency = 8
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	var ready sync.WaitGroup
	ready.Add(concurrency)
	release := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-release
			_, errs[i] = store.JoinExternalMember(team.ID, "advisor")
		}(i)
	}
	ready.Wait()
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: expected every concurrent join of a brand-new name to succeed, got %v", i, err)
		}
	}
	members, err := store.ListMembers(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("expected exactly ONE member despite %d concurrent joins racing to create it, got %d: %+v", concurrency, len(members), members)
	}
}

// TestTouchMemberActivityIsNoOpForNonExternal locks in the safety property
// TouchMemberActivity's own doc comment claims: callers (team send, team
// tasks claim/complete) touch activity unconditionally without checking
// who they're touching first, so it must be a true no-op — no error, no
// LastActiveAt written — for a provider-driven member or a name that
// doesn't exist at all.
func TestTouchMemberActivityIsNoOpForNonExternal(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchMemberActivity(team.ID, "worker-1"); err != nil {
		t.Fatalf("expected a no-op, got error: %v", err)
	}
	if err := store.TouchMemberActivity(team.ID, "does-not-exist"); err != nil {
		t.Fatalf("expected a no-op for an unknown name, got error: %v", err)
	}
	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.LastActiveAt != "" {
		t.Fatalf("expected LastActiveAt to stay empty for a non-external member, got %q", member.LastActiveAt)
	}
}
