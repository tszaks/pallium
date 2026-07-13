package workflow

import (
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
