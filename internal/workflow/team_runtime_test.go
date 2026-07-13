package workflow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTasksForPromptFiltersToOpenPlusRecentCompletions is the regression
// test for a P3 found by review: buildTeamTurnPrompt used to dump the FULL
// task board — including every completed task ever, full result text — into
// EVERY turn's prompt, growing without bound as a long-running team
// accumulates history. tasksForPrompt keeps all open work in full, but caps
// completed tasks to the most recent few with truncated results.
func TestTasksForPromptFiltersToOpenPlusRecentCompletions(t *testing.T) {
	tasks := []TeamTask{
		{ID: "open-1", Status: "pending"},
		{ID: "open-2", Status: "in_progress"},
	}
	for i := 0; i < teamPromptRecentCompletedTasks+3; i++ {
		tasks = append(tasks, TeamTask{
			ID:          fmt.Sprintf("done-%d", i),
			Status:      "completed",
			CompletedAt: fmt.Sprintf("2026-01-01T00:00:%02dZ", i),
			Result:      strings.Repeat("x", teamPromptResultTruncateChars+50),
		})
	}

	visible := tasksForPrompt(tasks)

	var openCount, completedCount int
	seenMostRecent := false
	for _, v := range visible {
		if v.Status == "completed" {
			completedCount++
			if len(v.Result) > teamPromptResultTruncateChars+len("... [truncated]") {
				t.Fatalf("expected completed task %q result truncated, got %d chars", v.ID, len(v.Result))
			}
			// The most recently completed task (highest index/CompletedAt)
			// must survive the cap — an unordered or wrong-direction cap
			// would silently keep the OLDEST completions instead.
			if v.ID == fmt.Sprintf("done-%d", teamPromptRecentCompletedTasks+2) {
				seenMostRecent = true
			}
		} else {
			openCount++
		}
	}
	if openCount != 2 {
		t.Fatalf("expected both open tasks preserved in full, got %d", openCount)
	}
	if completedCount != teamPromptRecentCompletedTasks {
		t.Fatalf("expected completed tasks capped to %d, got %d", teamPromptRecentCompletedTasks, completedCount)
	}
	if !seenMostRecent {
		t.Fatalf("expected the MOST RECENTLY completed task to survive the cap, it did not: %+v", visible)
	}
}

func newTeamTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, tmp
}

func newTeamTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial")
	return dir
}

func TestRunTeamTurnAppliesDecisionAndDeliversMail(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("review the diff", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "researcher-1", "claude", "", "researcher", "read-only"); err != nil {
		t.Fatal(err)
	}
	// Simulate what spawn (cmd/team.go) does for claude: mint a session id
	// up front so turn 1 can use --session-id.
	if err := store.PersistMemberSession(team.ID, "researcher-1", "seed-session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SendTeamMessage(team.ID, "lead", "researcher-1", "start with module A"); err != nil {
		t.Fatal(err)
	}

	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"{\"status\":\"idle\",\"summary\":\"looked at module A\",\"messages\":[{\"to\":\"lead\",\"body\":\"module A looks fine\"}]}"}`))

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "researcher-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}

	member, err := store.GetMember(team.ID, "researcher-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "idle" || member.TurnCount != 1 || member.TurnStartedAt != "" {
		t.Fatalf("unexpected member state after turn: %+v", member)
	}
	if member.SessionToken != "seed-session-1" {
		t.Fatalf("expected the minted session id preserved, got %q", member.SessionToken)
	}
	// The lead's own inbound message must now be delivered (consumed by the turn).
	stillUndelivered, err := store.UndeliveredMessages(team.ID, "researcher-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stillUndelivered) != 0 {
		t.Fatalf("expected the injected message marked delivered, got %+v", stillUndelivered)
	}
	// The decision's reply to "lead" must have been sent.
	leadInbox, err := store.UndeliveredMessages(team.ID, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if len(leadInbox) != 1 || leadInbox[0].Body != "module A looks fine" || leadInbox[0].From != "researcher-1" {
		t.Fatalf("expected the decision's reply delivered to lead, got %+v", leadInbox)
	}
}

// TestRunTeamTurnZombieDecisionMutatesNothing is the regression test for
// ticket #32 (M1 review round 2, closed in M2): decision side effects used to
// apply BEFORE FinishMemberTurn's lease check, so a turn whose lease was
// stolen out from under it by a stale takeover WHILE its provider call was
// still in flight could still send messages and claim/complete tasks — the
// zombie's mutation landed, and only afterward did FinishMemberTurn discover
// the lease was gone. This simulates that exact race with a real (slow) fake
// claude binary: steal the lease mid-turn, then verify the in-flight turn's
// decision — which claims one task, completes another it already owned, and
// messages "lead" — mutates NOTHING once it finally finishes.
func TestRunTeamTurnZombieDecisionMutatesNothing(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	claimable, err := store.CreateTeamTask(team.ID, "claim me", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := store.CreateTeamTask(team.ID, "already mine", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(team.ID, owned.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	// A deliberately SLOW fake claude: sleeps long enough for the test to
	// steal the lease while this "turn" is still in flight, then returns a
	// decision that messages lead, claims `claimable`, and completes `owned`.
	slow := filepath.Join(t.TempDir(), "fake-claude-slow.sh")
	decision := fmt.Sprintf(`{"result":"{\"status\":\"idle\",\"summary\":\"done\",\"messages\":[{\"to\":\"lead\",\"body\":\"zombie speaking\"}],\"claim_task_id\":\"%s\",\"complete_task_id\":\"%s\",\"complete_result\":\"zombie result\"}"}`, claimable.ID, owned.ID)
	script := "#!/bin/sh\ncat >/dev/null\nsleep 0.3\nprintf '%s' '" + decision + "'\n"
	if err := os.WriteFile(slow, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, slow)

	r := &Runner{Run: Run{ID: team.ID}}
	var wg sync.WaitGroup
	var turnErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, turnErr = r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{})
	}()

	// Give BeginMemberTurn time to acquire the real lease and dispatch into
	// the slow provider call, then steal it: force turn_started_at stale and
	// re-acquire, exactly what ReconcileInterruptedMembers/a second `team
	// run` process does to a genuinely dead turn.
	time.Sleep(100 * time.Millisecond)
	if _, err := store.db.Exec(`UPDATE team_members SET turn_started_at='2000-01-01T00:00:00Z' WHERE team_id=? AND name='worker-1'`, team.ID); err != nil {
		t.Fatal(err)
	}
	stolenLease, err := store.BeginMemberTurn(team.ID, "worker-1", nowString())
	if err != nil {
		t.Fatalf("expected the stale-takeover steal itself to succeed: %v", err)
	}
	if stolenLease == "" {
		t.Fatal("expected a non-empty stolen lease")
	}

	wg.Wait()
	if turnErr == nil {
		t.Fatal("expected the zombie turn to surface an error once it discovers its lease is gone")
	}
	if !strings.Contains(turnErr.Error(), "not owned") {
		t.Fatalf("expected a lease-not-owned error, got: %v", turnErr)
	}

	// The zombie's message to lead must never have been sent.
	leadInbox, err := store.UndeliveredMessages(team.ID, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if len(leadInbox) != 0 {
		t.Fatalf("expected NO message from the zombie decision, got %+v", leadInbox)
	}
	// The task it tried to claim must still be pending/unowned.
	gotClaimable, err := store.GetTeamTask(team.ID, claimable.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotClaimable.Status != "pending" || gotClaimable.Owner != "" {
		t.Fatalf("expected the claim blocked, got %+v", gotClaimable)
	}
	// The task it tried to complete must still be in_progress, not completed.
	gotOwned, err := store.GetTeamTask(team.ID, owned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOwned.Status != "in_progress" || gotOwned.Result != "" {
		t.Fatalf("expected the completion blocked, got %+v", gotOwned)
	}
}

// TestRunTeamTurnPlanPendingMemberCannotClaimOrComplete is the enforcement
// test for the M2 plan-approval flow: buildTeamTurnPrompt politely asks a
// plan-pending member not to claim/complete, but RunTeamTurn must actually
// refuse to apply those decision fields regardless of what a member's turn
// returns — a prompt is not enforcement.
func TestRunTeamTurnPlanPendingMemberCannotClaimOrComplete(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnPlanRequiredMember(team.ID, "planner-1", "claude", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "planner-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	claimable, err := store.CreateTeamTask(team.ID, "claim me", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Despite being read-only and plan-pending, the member's decision tries
	// to claim a task anyway (a misbehaving or confused agent) — RunTeamTurn
	// must ignore it.
	setClaudeCLI(t, fakeClaudeBinary(t, fmt.Sprintf(`{"result":"{\"status\":\"blocked\",\"summary\":\"here is my plan\",\"messages\":[{\"to\":\"lead\",\"body\":\"my plan is X\"}],\"claim_task_id\":\"%s\"}"}`, claimable.ID)))

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "planner-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetTeamTask(team.ID, claimable.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" || got.Owner != "" {
		t.Fatalf("expected the claim ignored while plan is pending, got %+v", got)
	}
	// The plan message itself must still have gone through — enforcement
	// blocks claim/complete, not the plan submission that's the whole point
	// of this mode.
	leadInbox, err := store.UndeliveredMessages(team.ID, "lead")
	if err != nil {
		t.Fatal(err)
	}
	sawPlan, sawEnforcementNote := false, false
	for _, m := range leadInbox {
		if strings.Contains(m.Body, "my plan is X") {
			sawPlan = true
		}
	}
	planFeedback, err := store.UndeliveredMessages(team.ID, "planner-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range planFeedback {
		if strings.Contains(m.Body, "still pending approval") {
			sawEnforcementNote = true
		}
	}
	if !sawPlan {
		t.Fatalf("expected the plan message delivered to lead, got %+v", leadInbox)
	}
	if !sawEnforcementNote {
		t.Fatalf("expected an enforcement explanation delivered to the member, got %+v", planFeedback)
	}
}

// TestCreateTeamTaskWithGateBlocksRejectedTask exercises the task_created
// quality-gate hook directly (no member turn involved — this hook fires
// synchronously when a task is added, from the CLI or a workflow primitive,
// never from inside a teammate's own decision).
func TestCreateTeamTaskWithGateBlocksRejectedTask(t *testing.T) {
	// runTeamGate's own ResolveProvider call is independent of any member's
	// provider — pin it explicitly so this test's expectations don't depend
	// on the ambient environment (a dev running this from inside Claude Code
	// has CLAUDECODE set for real; CI has neither, so it falls through to
	// codex instead of the "claude" this test's fake binary stands in for).
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude")
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "reject anything vague", []string{"task_created"}); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"{\"approved\":false,\"reason\":\"title is too vague to act on\"}"}`))

	r := &Runner{Run: Run{ID: team.ID}}
	task, err := r.CreateTeamTaskWithGate(context.Background(), store, team.ID, "do stuff", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "blocked" || !strings.Contains(task.Result, "too vague") {
		t.Fatalf("expected the task blocked with the gate's reason recorded, got %+v", task)
	}
}

// TestCreateTeamTaskWithGateAllowsApprovedTask is the positive-path sibling:
// an approved task must land exactly as CreateTeamTask alone would leave it.
func TestCreateTeamTaskWithGateAllowsApprovedTask(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see the sibling test's comment for why
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "reject anything vague", []string{"task_created"}); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"{\"approved\":true,\"reason\":\"clear enough\"}"}`))

	r := &Runner{Run: Run{ID: team.ID}}
	task, err := r.CreateTeamTaskWithGate(context.Background(), store, team.ID, "fix the specific bug in auth.go line 42", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" {
		t.Fatalf("expected the approved task to land pending as normal, got %+v", task)
	}
}

// TestCreateTeamTaskWithGateParksTeamWhenGateSpendCrossesBudget is the
// regression test for the finding that AddTeamSpend's overBudget return
// value was discarded at every gate-spend call site: a one-off call like
// this (outside RunTeam's own round loop, which separately re-derives the
// same fact from team.SpendUSD at the end of each round) never parked the
// team even after a gate's own reported cost pushed spend over the limit.
func TestCreateTeamTaskWithGateParksTeamWhenGateSpendCrossesBudget(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see TestCreateTeamTaskWithGateBlocksRejectedTask's comment
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0.01) // tiny budget
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "reject anything vague", []string{"task_created"}); err != nil {
		t.Fatal(err)
	}
	// The gate's own reported cost alone (0.05) already exceeds the 0.01
	// budget — no team turn cost involved at all.
	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"{\"approved\":true,\"reason\":\"clear enough\"}","total_cost_usd":0.05}`))

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.CreateTeamTaskWithGate(context.Background(), store, team.ID, "fix the specific bug in auth.go line 42", "", nil); err != nil {
		t.Fatal(err)
	}
	gotTeam, err := store.GetTeam(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTeam.Status != "parked" {
		t.Fatalf("expected the team parked once the gate's own cost crossed the budget, got status=%q spend=%v", gotTeam.Status, gotTeam.SpendUSD)
	}
}

// TestCreateTeamTaskWithGateNeverClaimableWhileGateInFlight is the
// regression test for the race a live adversarial-review team found in this
// batch's own new code (same session, same PR): a task_created-gated task
// used to be created "pending" (claimable) and only flipped to "blocked"
// AFTER the gate's provider round-trip returned, leaving a real window where
// a concurrent claim (or even an edit-mode completion) could land before the
// gate ever resolved. This simulates that exact race with a deliberately
// slow fake claude binary standing in for the gate's verifier call, and
// asserts a concurrent claim attempt fails throughout.
func TestCreateTeamTaskWithGateNeverClaimableWhileGateInFlight(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see TestCreateTeamTaskWithGateBlocksRejectedTask's comment
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "reject anything vague", []string{"task_created"}); err != nil {
		t.Fatal(err)
	}

	slow := filepath.Join(t.TempDir(), "fake-claude-slow-gate.sh")
	script := "#!/bin/sh\ncat >/dev/null\nsleep 0.3\nprintf '%s' '{\"result\":\"{\\\"approved\\\":true,\\\"reason\\\":\\\"fine\\\"}\"}'\n"
	if err := os.WriteFile(slow, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, slow)

	r := &Runner{}
	var wg sync.WaitGroup
	var task TeamTask
	var createErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		task, createErr = r.CreateTeamTaskWithGate(context.Background(), store, team.ID, "do the thing", "", nil)
	}()

	// Give CreateTeamTaskWithGate time to insert the row and dispatch into
	// the slow gate call, then attempt to claim it WHILE the gate is still
	// in flight — this must fail: the row must already be "blocked", not
	// the claimable "pending" the old create-then-flip order left exposed.
	time.Sleep(100 * time.Millisecond)
	tasks, err := store.ListTeamTasks(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected exactly one task visible while the gate is in flight, got %+v", tasks)
	}
	if tasks[0].Status != "blocked" {
		t.Fatalf("expected the task already blocked while the gate is in flight (not claimable pending), got %+v", tasks[0])
	}
	if _, err := store.ClaimTask(team.ID, tasks[0].ID, "worker-1"); err != errTaskNotClaimable {
		t.Fatalf("expected the concurrent claim to fail with errTaskNotClaimable while gated, got %v", err)
	}

	wg.Wait()
	if createErr != nil {
		t.Fatal(createErr)
	}
	if task.Status != "pending" {
		t.Fatalf("expected the task to land pending once the gate approved it, got %+v", task)
	}
	// Now that the gate resolved, it must be claimable.
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-1"); err != nil {
		t.Fatalf("expected the task claimable after gate approval, got %v", err)
	}
}

// fakeClaudeBinaryBranching writes a fake claude CLI that inspects the piped
// prompt to decide which of two envelopes to return — needed for any test
// exercising a quality gate fired FROM WITHIN a member's own turn
// (teammate_idle, task_completed): that turn makes TWO provider calls in
// sequence (its own decision, then the gate's verdict), and both go through
// the same fake binary. buildGatePrompt's fixed opening line ("You are an
// autonomous workflow gate verifier") is what the branch checks for.
func fakeClaudeBinaryBranching(t *testing.T, decisionEnvelope, gateEnvelope string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude-branch.sh")
	script := "#!/bin/sh\ninput=\"$(cat)\"\nif echo \"$input\" | grep -q 'autonomous workflow gate verifier'; then\n  printf '%s' '" + gateEnvelope + "'\nelse\n  printf '%s' '" + decisionEnvelope + "'\nfi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunTeamTurnTaskCompletedGateRejectsAndDeliversFeedback is Tyler's own
// specified example for item 3: a completed task whose gate fails stays
// in_progress (never actually transitioned, so nothing to "revert") with the
// gate's output delivered to the owner as feedback.
func TestRunTeamTurnTaskCompletedGateRejectsAndDeliversFeedback(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see TestCreateTeamTaskWithGateBlocksRejectedTask's comment
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "the result must mention tests passing", []string{"task_completed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTeamTask(team.ID, "fix the bug", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	decision := fmt.Sprintf(`{"result":"{\"status\":\"idle\",\"summary\":\"done\",\"complete_task_id\":\"%s\",\"complete_result\":\"fixed it, did not run tests\"}"}`, task.ID)
	gate := `{"result":"{\"approved\":false,\"reason\":\"no evidence tests were run\"}"}`
	setClaudeCLI(t, fakeClaudeBinaryBranching(t, decision, gate))

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetTeamTask(team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in_progress" || got.Owner != "worker-1" {
		t.Fatalf("expected the task to remain in_progress (gate-rejected completion never lands), got %+v", got)
	}
	feedback, err := store.UndeliveredMessages(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range feedback {
		if strings.Contains(m.Body, "no evidence tests were run") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the gate's reason delivered to the owner, got %+v", feedback)
	}
}

// TestRunTeamTurnTeammateIdleGateForcesActiveOnRejection covers the third
// hook: a member declares idle, the gate disagrees (there's still real work
// it should keep doing), and the member's persisted status is forced back
// to "active" with the gate's reason as the note instead of the member's
// own claimed idle status.
func TestRunTeamTurnTeammateIdleGateForcesActiveOnRejection(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see TestCreateTeamTaskWithGateBlocksRejectedTask's comment
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "don't go idle while any task is still pending", []string{"teammate_idle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}

	decision := `{"result":"{\"status\":\"idle\",\"summary\":\"nothing to do\"}"}`
	gate := `{"result":"{\"approved\":false,\"reason\":\"there is still pending work on the board\"}"}`
	setClaudeCLI(t, fakeClaudeBinaryBranching(t, decision, gate))

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}

	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "active" {
		t.Fatalf("expected the gate to force status back to active, got %+v", member)
	}
	if !strings.Contains(member.LastTurnError, "still pending work") {
		t.Fatalf("expected the gate's reason recorded as the turn note, got %+v", member)
	}
	// Regression proof for the review finding: forcing status back to
	// active alone doesn't guarantee RunTeam's scheduler re-offers this
	// member a turn — it only does if the board looks NEW since the
	// member's own LastTurnAt (team.TasksUpdatedAt > member.LastTurnAt).
	// Check the EXACT condition the scheduler evaluates, not just the
	// member's own status field.
	gotTeam, err := store.GetTeam(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !(gotTeam.TasksUpdatedAt > member.LastTurnAt) {
		t.Fatalf("expected the task-board watermark bumped past this member's own LastTurnAt so RunTeam's scheduler re-offers it a turn, got team.TasksUpdatedAt=%q member.LastTurnAt=%q", gotTeam.TasksUpdatedAt, member.LastTurnAt)
	}
}

// TestRunTeamTurnTeammateIdleGateFailsClosedOnMalfunction is the regression
// test for the review finding that a teammate_idle gate malfunction (the
// verifier call itself erroring, not just a clean rejection) used to
// proceed idle unchanged — the one hook point that quietly approved-by-
// default instead of failing closed like task_created/task_completed
// already do.
func TestRunTeamTurnTeammateIdleGateFailsClosedOnMalfunction(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see TestCreateTeamTaskWithGateBlocksRejectedTask's comment
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "don't go idle while any task is still pending", []string{"teammate_idle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}

	decision := `{"result":"{\"status\":\"idle\",\"summary\":\"nothing to do\"}"}`
	gate := `{"is_error":true,"result":"verifier crashed"}` // a genuine gate-run failure, not a clean rejection
	setClaudeCLI(t, fakeClaudeBinaryBranching(t, decision, gate))

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}

	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "active" {
		t.Fatalf("expected a gate malfunction to fail closed (force active), not approve idle by default, got %+v", member)
	}
	if !strings.Contains(member.LastTurnError, "failed to run") {
		t.Fatalf("expected the malfunction recorded as the turn note, got %+v", member)
	}
	gotTeam, err := store.GetTeam(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !(gotTeam.TasksUpdatedAt > member.LastTurnAt) {
		t.Fatalf("expected the watermark bumped so the scheduler re-offers this member a turn, same as an explicit rejection, got team.TasksUpdatedAt=%q member.LastTurnAt=%q", gotTeam.TasksUpdatedAt, member.LastTurnAt)
	}
}

// TestDescribeClaimableWork covers the teammate_idle gate's task-board
// summary (found by review: the gate used to see only the teammate's own
// summary, with no factual board state to check an idle claim against).
func TestDescribeClaimableWork(t *testing.T) {
	if got := describeClaimableWork(nil); got != "The task board is empty." {
		t.Fatalf("expected the empty-board message, got %q", got)
	}
	blocked := []TeamTask{
		{ID: "t1", Title: "needs a dependency", Status: "pending", DependsOn: []string{"t0"}},
		{ID: "t0", Title: "the dependency", Status: "in_progress"},
	}
	if got := describeClaimableWork(blocked); !strings.Contains(got, "No pending task is currently claimable") {
		t.Fatalf("expected the all-blocked message when every pending task has an unmet dependency, got %q", got)
	}
	claimable := []TeamTask{
		{ID: "t0", Title: "the dependency", Status: "completed"},
		{ID: "t1", Title: "unblocked now", Status: "pending", DependsOn: []string{"t0"}},
		{ID: "t2", Title: "already done", Status: "completed"},
	}
	got := describeClaimableWork(claimable)
	if !strings.Contains(got, "unblocked now") || strings.Contains(got, "already done") {
		t.Fatalf("expected only the claimable pending task named, got %q", got)
	}
}

// TestCompleteTaskWithGateSkipsGateForIneligibleTask is the regression test
// for the review finding that CompleteTaskWithGate ran a real (costly)
// verifier call even for a completion request that could never succeed —
// wrong owner, not actually in_progress. The fake claude binary here writes
// a marker file on invocation; asserting that file never appears is the
// only way to prove the gate itself was never called, not just that its
// answer was later ignored.
func TestCompleteTaskWithGateSkipsGateForIneligibleTask(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see TestCreateTeamTaskWithGateBlocksRejectedTask's comment
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "verify the result", []string{"task_completed"}); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTeamTask(team.ID, "do the thing", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately left "pending", never claimed — no owner can legitimately
	// complete it yet.
	marker := filepath.Join(t.TempDir(), "gate-was-called")
	script := "#!/bin/sh\ncat >/dev/null\ntouch '" + marker + "'\nprintf '%s' '{\"result\":\"{\\\"approved\\\":true,\\\"reason\\\":\\\"ok\\\"}\"}'\n"
	fakeBin := filepath.Join(t.TempDir(), "fake-claude-marker.sh")
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, fakeBin)

	r := &Runner{Run: Run{ID: team.ID}}
	_, approved, err := r.CompleteTaskWithGate(context.Background(), store, team.ID, task.ID, "worker-1", "done")
	if approved {
		t.Fatalf("expected an ineligible completion to never be approved")
	}
	if err == nil || !strings.Contains(err.Error(), "not owned by") {
		t.Fatalf("expected the same not-owned error CompleteTask itself produces, got %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("expected the gate verifier NEVER invoked for an ineligible completion, but the marker file exists")
	}

	// Positive control: the exact same fake binary DOES get called once the
	// task is actually eligible, proving the marker technique itself works
	// and this isn't just a fake binary that silently never runs.
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, approved, err := r.CompleteTaskWithGate(context.Background(), store, team.ID, task.ID, "worker-1", "done"); err != nil || !approved {
		t.Fatalf("expected the now-eligible completion to be approved, got approved=%v err=%v", approved, err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("expected the gate verifier invoked once the task became eligible: %v", statErr)
	}
}

// TestRunTeamTurnCompletionGateRunsBeforeFinishMemberTurn is the regression
// test for the durability finding: the task_completed gate's provider call
// must happen BEFORE FinishMemberTurn releases the lease, not after — a
// crash during a slow gate round-trip must never durably record the turn as
// "finished" while the completion (and its gate verdict) was never applied.
// Proven by observation, not simulated crash: the fake gate verifier sleeps
// briefly, and this asserts the member's lease (turn_started_at) is STILL
// held partway through that sleep — i.e. FinishMemberTurn has not run yet
// even though the gate call is already in flight.
func TestRunTeamTurnCompletionGateRunsBeforeFinishMemberTurn(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see TestCreateTeamTaskWithGateBlocksRejectedTask's comment
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "verify the result", []string{"task_completed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTeamTask(team.ID, "fix the bug", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	const gateSleep = "0.4"
	decision := fmt.Sprintf(`{"result":"{\"status\":\"idle\",\"summary\":\"done\",\"complete_task_id\":\"%s\",\"complete_result\":\"fixed it\"}"}`, task.ID)
	path := filepath.Join(t.TempDir(), "fake-claude-slow-gate.sh")
	script := "#!/bin/sh\ninput=\"$(cat)\"\nif echo \"$input\" | grep -q 'autonomous workflow gate verifier'; then\n  sleep " + gateSleep + "\n  printf '%s' '{\"result\":\"{\\\"approved\\\":true,\\\"reason\\\":\\\"ok\\\"}\"}'\nelse\n  printf '%s' '" + decision + "'\nfi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, path)

	r := &Runner{Run: Run{ID: team.ID}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{}); err != nil {
			t.Error(err)
		}
	}()

	time.Sleep(150 * time.Millisecond) // well inside the 400ms gate sleep
	mid, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if mid.TurnStartedAt == "" {
		t.Fatalf("expected the lease STILL held while the task_completed gate's provider call is in flight (FinishMemberTurn must run after, not before, the gate resolves), but turn_started_at was already cleared")
	}

	<-done
	final, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if final.TurnStartedAt != "" {
		t.Fatalf("expected the lease released once the turn actually finished, got %+v", final)
	}
	gotTask, err := store.GetTeamTask(team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask.Status != "completed" {
		t.Fatalf("expected the approved completion applied after the turn finished, got %+v", gotTask)
	}
}

func TestRunTeamTurnProviderFailureNotifiesLeadWithError(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, _ := store.CreateTeam("goal", repo, 0)
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	// A claude CLI that exits nonzero with a quota-style message on stderr —
	// mirrors the exact "teammate dies on an API error" bug the research
	// digest calls out: the lead must be told WHY, not just that it stopped.
	fail := filepath.Join(t.TempDir(), "fake-claude-fail.sh")
	if err := os.WriteFile(fail, []byte("#!/bin/sh\ncat >/dev/null\necho 'You have hit your usage limit, try again later' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, fail)

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}
	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "error" || member.LastTurnError == "" {
		t.Fatalf("expected the member's own status/error to record the failure, got %+v", member)
	}
	leadInbox, err := store.UndeliveredMessages(team.ID, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if len(leadInbox) != 1 || leadInbox[0].From != "worker-1" {
		t.Fatalf("expected the lead notified of the failure, got %+v", leadInbox)
	}
	if !strings.Contains(strings.ToLower(leadInbox[0].Body), "usage limit") {
		t.Fatalf("expected the lead's notification to carry the REAL error, got %q", leadInbox[0].Body)
	}
}

// TestDispatchTeamTurnRetriesSessionIDAfterFailedFirstTurn is the regression
// test for a P1 found by independent review: a claude member's SessionToken
// is pre-minted at spawn (see SpawnTeamMember), so if the FIRST turn's
// provider call crashes before claude ever actually creates that native
// session, TurnCount still increments (see FinishMemberTurn) — a naive
// "isFirstTurn := TurnCount==0" would then use --resume on the next attempt
// against a session claude never established, permanently bricking the
// member. dispatchTeamTurn must key off SessionEstablished (sticky-true only
// once a turn succeeds) instead, so a failed first turn retries with
// --session-id again.
func TestDispatchTeamTurnRetriesSessionIDAfterFailedFirstTurn(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "seed-session-1"); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	argLog := filepath.Join(tmp, "argv.log")
	countFile := filepath.Join(tmp, "calls")
	script := `#!/bin/sh
cat >/dev/null
echo "$@" >> "` + argLog + `"
N=0
if [ -f "` + countFile + `" ]; then N=$(cat "` + countFile + `"); fi
N=$((N+1))
echo "$N" > "` + countFile + `"
if [ "$N" = "1" ]; then
  echo 'boom: simulated crash before any session was ever established' >&2
  exit 1
fi
printf '%s' '{"result":"{\"status\":\"idle\",\"summary\":\"ok\"}"}'
`
	path := filepath.Join(tmp, "fake-claude.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, path)

	r := &Runner{Run: Run{ID: team.ID}}

	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}
	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "error" || member.TurnCount != 1 {
		t.Fatalf("expected turn 1 to record an error and still count as an attempt, got %+v", member)
	}
	if member.SessionEstablished {
		t.Fatalf("expected session_established to stay false after a failed first turn, got true")
	}

	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}
	member, err = store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "idle" || !member.SessionEstablished {
		t.Fatalf("expected turn 2 to succeed and establish the session, got %+v", member)
	}

	rawLog, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawLog)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 claude invocations, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "--session-id seed-session-1") {
		t.Fatalf("expected turn 1 to use --session-id, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "--session-id seed-session-1") || strings.Contains(lines[1], "--resume") {
		t.Fatalf("expected turn 2 to RETRY with --session-id (turn 1 never established a session), not --resume — got %q", lines[1])
	}
}

func TestRunTeamTurnMalformedDecisionPreservesEditPatch(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, _ := store.CreateTeam("goal", repo, 0)
	if _, err := store.SpawnMember(team.ID, "editor-1", "claude", "", "", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "editor-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	// This fake claude EDITS a file in its cwd (the teammate's worktree) but
	// returns prose, not the structured decision JSON — the malformed-output
	// case. The edit must still land in the real repo (the #37 lesson,
	// applied here to teams): a formatting slip in the coordination output
	// must never discard completed work.
	editScript := filepath.Join(t.TempDir(), "fake-claude-edit.sh")
	// Deliberately does NOT `git add` — Pallium's own patch capture handles
	// staging (intent-to-add) an untracked file; pre-staging it here would
	// make the working-tree-vs-index diff it takes come up empty.
	script := "#!/bin/sh\ncat >/dev/null\necho changed > note.txt\nprintf 'sure, done editing, no JSON here'\n"
	if err := os.WriteFile(editScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, editScript)

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "editor-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(repo, "note.txt"))
	if err != nil {
		t.Fatalf("expected the edit applied to the real repo despite the malformed decision: %v", err)
	}
	if string(raw) != "changed\n" {
		t.Fatalf("unexpected file content: %q", raw)
	}
	member, err := store.GetMember(team.ID, "editor-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "active" || member.TurnCount != 1 || member.LastTurnError == "" {
		t.Fatalf("expected an active, retryable member with the parse failure recorded, got %+v", member)
	}
}

// TestRunTeamConvergesAcrossRoundsWithMultipleMembers drives a real
// multi-member, multi-round exchange through the actual scheduler
// (RunTeam), not just a single RunTeamTurn call: the lead messages
// researcher-1, who replies to researcher-2, who then goes idle. Proves
// round-based scheduling actually delivers a message sent DURING one
// member's turn to another member's NEXT turn.
func TestRunTeamConvergesAcrossRoundsWithMultipleMembers(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"researcher-1", "researcher-2"} {
		if _, err := store.SpawnMember(team.ID, name, "claude", "", "", "read-only"); err != nil {
			t.Fatal(err)
		}
		if err := store.PersistMemberSession(team.ID, name, "sess-"+name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SendTeamMessage(team.ID, "lead", "researcher-1", "go"); err != nil {
		t.Fatal(err)
	}

	// A stateful fake binary: it looks at what it was asked (via the piped
	// prompt) to decide its reply. Simplify by keying off argv --session-id
	// vs --resume and the recipient names baked into two distinct scripts is
	// awkward with one shared binary, so use two DIFFERENT session ids
	// mapped to two different scripted responses via a small dispatcher
	// script that reads its own session id from argv.
	dispatcher := filepath.Join(t.TempDir(), "fake-claude-dispatch.sh")
	script := `#!/bin/sh
cat >/dev/null
SESS=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--session-id" ] || [ "$prev" = "--resume" ]; then SESS="$a"; fi
  prev="$a"
done
case "$SESS" in
  sess-researcher-1) printf '{"result":"{\"status\":\"idle\",\"summary\":\"handed off\",\"messages\":[{\"to\":\"researcher-2\",\"body\":\"your turn\"}]}"}' ;;
  sess-researcher-2) printf '{"result":"{\"status\":\"idle\",\"summary\":\"done\"}"}' ;;
  *) printf '{"result":"{\"status\":\"idle\",\"summary\":\"unknown\"}"}' ;;
esac
`
	if err := os.WriteFile(dispatcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, dispatcher)

	r := &Runner{}
	summary, err := r.RunTeam(context.Background(), store, team.ID, TeamTurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Rounds < 2 {
		t.Fatalf("expected at least 2 rounds (researcher-1's message only becomes claimable by researcher-2 in the NEXT round), got %+v", summary)
	}
	r2, err := store.GetMember(team.ID, "researcher-2")
	if err != nil {
		t.Fatal(err)
	}
	if r2.TurnCount != 1 || r2.Status != "idle" {
		t.Fatalf("expected researcher-2 to have taken exactly one turn and gone idle, got %+v", r2)
	}
}

// TestDispatchTeamTurnReadsBackWrapperProviderUsage is the regression test
// for a P2 (wrapper-provider team spend silently untracked) found by
// independent review: runConfiguredProviderTeamTurn already handed a
// PALLIUM_WORKFLOW_USAGE_FILE to the wrapper (same env contract as the
// regular, non-team worker path) but never read it back — every wrapper-
// provider team turn reported costUSD=0 regardless of what the wrapper
// actually spent, so `--budget-usd` could never trigger for such a team.
func TestDispatchTeamTurnReadsBackWrapperProviderUsage(t *testing.T) {
	clearProviderEnv(t)
	script := `#!/bin/sh
printf '%s' '{"status":"idle","summary":"done"}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
printf '%s' 'grok-session-1' > "$PALLIUM_WORKFLOW_SESSION_FILE"
printf '%s' '{"cost_usd":0.42}' > "$PALLIUM_WORKFLOW_USAGE_FILE"
`
	path := filepath.Join(t.TempDir(), "fake-grok.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_GROK_COMMAND", path)

	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.SpawnMember(team.ID, "worker-1", "grok", "", "", "read-only")
	if err != nil {
		t.Fatal(err)
	}

	r := &Runner{}
	output, token, cost, err := r.dispatchTeamTurn(context.Background(), store, team.ID, "any-lease", &member, repo, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if output != `{"status":"idle","summary":"done"}` {
		t.Fatalf("unexpected output: %q", output)
	}
	if token != "grok-session-1" {
		t.Fatalf("expected session token read back, got %q", token)
	}
	if cost != 0.42 {
		t.Fatalf("expected the wrapper's reported cost_usd read back (was silently dropped to 0 before this fix), got %v", cost)
	}
}

// TestRunTeamIdleMembersDoNotReclaimUnchangedBoardEveryRound is the
// regression test for a P2 (idle-spin cost runaway) found by independent
// review: HasClaimableWork only reports whether ANY task is claimable, with
// no notion of "already offered to this member and declined". The OLD
// scheduler re-scheduled every non-busy member on every single round for as
// long as one task stayed unclaimed — up to maxRounds (50) paid provider
// calls per member for a board nobody has any intention of touching. The fix
// (Team.TasksUpdatedAt, a board-wide watermark) means a member is only
// re-offered claimable work if the board changed since ITS last turn.
func TestRunTeamIdleMembersDoNotReclaimUnchangedBoardEveryRound(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"worker-1", "worker-2"} {
		if _, err := store.SpawnMember(team.ID, name, "claude", "", "", "read-only"); err != nil {
			t.Fatal(err)
		}
		if err := store.PersistMemberSession(team.ID, name, "sess-"+name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateTeamTask(team.ID, "investigate something nobody wants", "", nil); err != nil {
		t.Fatal(err)
	}

	// Every member goes idle and never claims the one open task.
	script := `#!/bin/sh
cat >/dev/null
printf '%s' '{"result":"{\"status\":\"idle\",\"summary\":\"nothing here for me\"}"}'
`
	path := filepath.Join(t.TempDir(), "fake-claude-idle.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, path)

	r := &Runner{}
	summary, err := r.RunTeam(context.Background(), store, team.ID, TeamTurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Rounds != 1 {
		t.Fatalf("expected convergence in exactly 1 round once every member has seen the unchanged board once, got %d rounds (%+v)", summary.Rounds, summary)
	}
	for _, name := range []string{"worker-1", "worker-2"} {
		m, err := store.GetMember(team.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if m.TurnCount != 1 {
			t.Fatalf("expected %s to take exactly one turn, got %d turns (a re-spin keeps re-showing the same unclaimed task)", name, m.TurnCount)
		}
	}
}

// TestRunTeamConcurrentTurnsAreRaceFree is the direct proof for the fix made
// while writing this scheduler: RunTeamTurn must never itself mutate
// r.Run.ID (RunTeam sets it once, before any goroutines start) — several
// members eligible in the SAME round take their turns concurrently on one
// shared *Runner, and this test only passes real-scenario cleanly under
// `go test -race`.
func TestRunTeamConcurrentTurnsAreRaceFree(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 5
	for i := 0; i < n; i++ {
		name := "worker-" + string(rune('a'+i))
		if _, err := store.SpawnMember(team.ID, name, "claude", "", "", "read-only"); err != nil {
			t.Fatal(err)
		}
		if err := store.PersistMemberSession(team.ID, name, "sess-"+name); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SendTeamMessage(team.ID, "lead", name, "go"); err != nil {
			t.Fatal(err)
		}
	}
	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"{\"status\":\"idle\",\"summary\":\"ok\"}"}`))

	r := &Runner{}
	summary, err := r.RunTeam(context.Background(), store, team.ID, TeamTurnOptions{MaxConcurrent: n})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TurnsTaken != n {
		t.Fatalf("expected all %d members to take exactly one turn, got %+v", n, summary)
	}
	members, err := store.ListMembers(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.TurnCount != 1 || m.Status != "idle" {
			t.Fatalf("member %s did not converge cleanly: %+v", m.Name, m)
		}
	}
}

// TestRunTeamConcurrentRunsTurnsTakenCountsOnlyActualTurns is the regression
// test for a P3 found by review: TurnsTaken used to be incremented by
// len(eligible) — the number of members OFFERED a turn this round — not the
// number that actually got one. A member offered a turn by TWO concurrent
// `team run` invocations (a real scenario: `team attach` + `team run` from
// two processes/agents) has ONE of them lose BeginMemberTurn's CAS
// (ranTurn=false, see errMemberTurnInFlight); the old code counted it as a
// turn taken anyway, in EVERY concurrent caller's own summary, inflating
// TurnsTaken above the number of provider calls actually made — misleading
// anyone using it as a cost/activity proxy. This races N=4 independent
// `RunTeam` calls (on N=4 independent *Runner instances, same store/team) over
// a SINGLE member and checks the summed TurnsTaken across all of them.
func TestRunTeamConcurrentRunsTurnsTakenCountsOnlyActualTurns(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SendTeamMessage(team.ID, "lead", "worker-1", "go"); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"{\"status\":\"idle\",\"summary\":\"ok\"}"}`))

	const n = 4
	var wg sync.WaitGroup
	summaries := make([]TeamRunSummary, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := &Runner{}
			s, rerr := r.RunTeam(context.Background(), store, team.ID, TeamTurnOptions{})
			if rerr != nil {
				t.Error(rerr)
				return
			}
			summaries[i] = s
		}(i)
	}
	wg.Wait()

	total := 0
	for _, s := range summaries {
		total += s.TurnsTaken
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 real turn summed across %d concurrent RunTeam calls (one member, one turn), got %d: %+v", n, total, summaries)
	}
	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.TurnCount != 1 {
		t.Fatalf("expected the member to have actually taken exactly one turn, got TurnCount=%d", member.TurnCount)
	}
}

func TestRunTeamReconcilesInterruptedMemberThenResumeSucceeds(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTeamTask(team.ID, "investigate", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(team.ID, task.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	// Simulate: worker-1's turn started (the owning `pallium team run`
	// process then got killed before it could finish) — an old
	// turn_started_at with no matching FinishMemberTurn.
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.Exec(`UPDATE team_members SET turn_started_at=? WHERE team_id=? AND name=?`, old, team.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SendTeamMessage(team.ID, "lead", "worker-1", "any update?"); err != nil {
		t.Fatal(err)
	}
	decision := fmt.Sprintf(`{\"status\":\"active\",\"summary\":\"resumed and working\",\"complete_task_id\":\"%s\",\"complete_result\":\"found it\"}`, task.ID)
	setClaudeCLI(t, fakeClaudeBinary(t, fmt.Sprintf(`{"result":"%s"}`, decision)))

	// "Resume": a fresh RunTeam call (as if a new `pallium team run`
	// invocation attached to the same team) must reconcile the interrupted
	// turn and successfully drive the member to completion.
	r := &Runner{}
	summary, err := r.RunTeam(context.Background(), store, team.ID, TeamTurnOptions{StaleTurnAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Interrupted) != 1 || summary.Interrupted[0] != "worker-1" {
		t.Fatalf("expected worker-1 reported as reconciled-interrupted, got %+v", summary)
	}
	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	// worker-1 has a session token (sess-1), so reconciliation must NOT have
	// stripped its owned task away — it resumes its OWN session and finishes
	// what it was doing, converging in exactly one round/turn. If
	// reconciliation had reverted the task instead (the pre-fix behavior),
	// CompleteTask would fail ownership and the scheduler would loop forever
	// on "claimable work still exists" — this assertion is what would catch
	// that regression.
	if member.TurnCount != 1 || member.TurnStartedAt != "" {
		t.Fatalf("expected the resumed member to complete a clean turn in round 1 (its task must survive reconciliation since it has a resumable session), got %+v", member)
	}
	completed, err := store.GetTeamTask(team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" || completed.Result != "found it" {
		t.Fatalf("expected the task completed after resume, got %+v", completed)
	}
}

// TestReconcileInterruptedMembersDoesNotClobberARestartedTurn is the direct
// regression test for the TOCTOU race a live dogfooded review of this exact
// file caught: without a staleness re-check in the UPDATE's WHERE clause, a
// member whose turn restarted (fresh turn_started_at) between the earlier
// SELECT and this UPDATE would get clobbered back to "interrupted" with its
// live turn wiped out from under it.
func TestReconcileInterruptedMembersDoesNotClobberARestartedTurn(t *testing.T) {
	store, tmp := newTeamTestStore(t)
	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	// The staleness window a caller observed earlier (e.g. via the SELECT in
	// ReconcileInterruptedMembers) — anything with turn_started_at older
	// than this counts as stale.
	staleAfter := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	// Simulate the restart: BeginMemberTurn succeeds because there is no
	// turn in flight yet, giving the member a FRESH turn_started_at — as if
	// a legitimate stale-takeover (or a brand new turn) happened in the
	// window between an earlier caller's SELECT and this reconcile call.
	if _, err := store.BeginMemberTurn(team.ID, "worker-1", staleAfter); err != nil {
		t.Fatal(err)
	}
	reconciled, err := store.ReconcileInterruptedMembers(team.ID, staleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 0 {
		t.Fatalf("expected the freshly-restarted member NOT to be reconciled, got %v", reconciled)
	}
	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "active" || member.TurnStartedAt == "" {
		t.Fatalf("expected the restarted turn left untouched (still in flight), got %+v", member)
	}
}

// TestRunTeamTurnFailedProviderCallDiscardsWorktreeWithoutApplying is the
// regression test for the comment/code mismatch a live dogfooded review
// caught: patch capture/apply used to run BEFORE the turnErr check, so a
// crashed/failed provider call's untrustworthy partial edits could still
// land in the real repo.
func TestRunTeamTurnFailedProviderCallDiscardsWorktreeWithoutApplying(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, _ := store.CreateTeam("goal", repo, 0)
	if _, err := store.SpawnMember(team.ID, "editor-1", "claude", "", "", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "editor-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	// Edits a file, THEN fails (nonzero exit) — the edit must never reach
	// the real repo since the overall provider call did not succeed.
	failScript := filepath.Join(t.TempDir(), "fake-claude-edit-then-fail.sh")
	script := "#!/bin/sh\ncat >/dev/null\necho should-not-land > note.txt\necho boom >&2\nexit 1\n"
	if err := os.WriteFile(failScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, failScript)

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "editor-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "note.txt")); err == nil {
		t.Fatal("expected the failed turn's edit NOT to land in the real repo")
	}
	member, err := store.GetMember(team.ID, "editor-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.Status != "error" {
		t.Fatalf("expected the member marked error, got %+v", member)
	}
}

// TestRunTeamConcurrentEditModeMembersBothLand is the functional regression
// test for the unsynchronized-git-apply race a live dogfooded review caught:
// two edit-mode teammates completing in the SAME round both add a file.
// go test -race also runs this — it cannot see a subprocess-level `git
// apply` race directly, but it DOES cover the Go-level access to
// r.teamApplyMu and everything around it.
func TestRunTeamConcurrentEditModeMembersBothLand(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"editor-a", "editor-b"}
	files := map[string]string{"editor-a": "a.txt", "editor-b": "b.txt"}
	for _, name := range names {
		if _, err := store.SpawnMember(team.ID, name, "claude", "", "", "edit"); err != nil {
			t.Fatal(err)
		}
		if err := store.PersistMemberSession(team.ID, name, "sess-"+name); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SendTeamMessage(team.ID, "lead", name, "go"); err != nil {
			t.Fatal(err)
		}
	}
	dispatcher := filepath.Join(t.TempDir(), "fake-claude-concurrent-edit.sh")
	// Each invocation writes to the file matching its OWN session id, so two
	// concurrently-running instances touch different files but both apply
	// against the SAME shared repoRoot at (as close to) the same time.
	script := `#!/bin/sh
cat >/dev/null
SESS=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--session-id" ] || [ "$prev" = "--resume" ]; then SESS="$a"; fi
  prev="$a"
done
case "$SESS" in
  sess-editor-a) echo from-a > a.txt ;;
  sess-editor-b) echo from-b > b.txt ;;
esac
printf '{"result":"{\"status\":\"idle\",\"summary\":\"done\"}"}'
`
	if err := os.WriteFile(dispatcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, dispatcher)

	r := &Runner{}
	if _, err := r.RunTeam(context.Background(), store, team.ID, TeamTurnOptions{MaxConcurrent: 2}); err != nil {
		t.Fatal(err)
	}
	for name, file := range files {
		raw, err := os.ReadFile(filepath.Join(repo, file))
		if err != nil {
			t.Fatalf("expected %s's edit (%s) to land: %v", name, file, err)
		}
		_ = raw
	}
}

// TestRunTeamTurnRecordsRealClaudeSpend is the regression test for the dead
// budget-tracking bug a live dogfooded review caught: AddTeamSpend was never
// called and every FinishMemberTurn call site hardcoded spendDelta=0, so
// team/member spend never moved and a --budget-usd ceiling silently did
// nothing.
func TestRunTeamTurnRecordsRealClaudeSpend(t *testing.T) {
	store, _ := newTeamTestStore(t)
	repo := newTeamTestRepo(t)
	team, err := store.CreateTeam("goal", repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"{\"status\":\"idle\",\"summary\":\"done\"}","total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":5}}`))

	r := &Runner{Run: Run{ID: team.ID}}
	if _, err := r.RunTeamTurn(context.Background(), store, team.ID, "worker-1", TeamTurnOptions{}); err != nil {
		t.Fatal(err)
	}
	member, err := store.GetMember(team.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if member.SpendUSD != 0.05 {
		t.Fatalf("expected member spend recorded as 0.05, got %v", member.SpendUSD)
	}
	updatedTeam, err := store.GetTeam(team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedTeam.SpendUSD != 0.05 {
		t.Fatalf("expected team spend recorded as 0.05, got %v", updatedTeam.SpendUSD)
	}
}

// TestGateCallContextPreservesALongerCallerDeadline is the regression test
// for the review finding that unconditionally wrapping with
// context.WithTimeout(ctx, 600s) let the SHORTER deadline always win: an
// operator who intentionally allowed a longer turn (--agent-timeout 1800)
// still had every gate call capped at 600s. A context with no deadline at
// all must still get the 600s default (a hung verifier must never be
// literally unbounded).
func TestGateCallContextPreservesALongerCallerDeadline(t *testing.T) {
	longDeadline := time.Now().Add(2 * time.Hour)
	withLongDeadline, cancel := context.WithDeadline(context.Background(), longDeadline)
	defer cancel()

	gotCtx, gotCancel := gateCallContext(withLongDeadline)
	defer gotCancel()
	gotDeadline, ok := gotCtx.Deadline()
	if !ok || !gotDeadline.Equal(longDeadline) {
		t.Fatalf("expected the caller's own 2-hour deadline preserved unchanged, got ok=%v deadline=%v (wanted %v)", ok, gotDeadline, longDeadline)
	}

	noDeadlineCtx, cancel2 := gateCallContext(context.Background())
	defer cancel2()
	_, hasDeadline := noDeadlineCtx.Deadline()
	if !hasDeadline {
		t.Fatalf("expected the 600s default applied when the caller's context has no deadline at all")
	}
}

// TestResolveWaitAgentTimeoutSecondsHonorsExplicitZero is the regression
// test for the finding that team.wait couldn't distinguish "the script
// omitted agentTimeoutSeconds" from "the script explicitly asked for 0"
// (unlimited, matching the CLI's --agent-timeout 0) — both used to decode
// to Go's int zero value and get silently overridden to the 600s default.
func TestResolveWaitAgentTimeoutSecondsHonorsExplicitZero(t *testing.T) {
	if got := resolveWaitAgentTimeoutSeconds(nil); got != 600 {
		t.Fatalf("expected omitted to default to 600, got %d", got)
	}
	zero := 0
	if got := resolveWaitAgentTimeoutSeconds(&zero); got != 0 {
		t.Fatalf("expected an explicit 0 honored as unlimited, got %d", got)
	}
	custom := 120
	if got := resolveWaitAgentTimeoutSeconds(&custom); got != 120 {
		t.Fatalf("expected an explicit non-zero value honored as-is, got %d", got)
	}
}

// TestCreateWorktreeRecoversFromStaleDirectoryFromAKilledTurn is the
// regression test for a real M1-era bug this batch's own live kill/resume
// acceptance proof surfaced: killing the steering process mid-turn leaves
// that turn's worktree directory on disk (its own cleanup never ran); a
// team member reuses the SAME deterministic worktree path (keyed on
// member.ID) on every turn, so the member's next turn's createWorktree call
// used to fail outright with "already exists", permanently blocking that
// member's edit mode. Not introduced by M2 — found because M2's live proof
// exercised a real kill/resume for the first time in a while.
func TestCreateWorktreeRecoversFromStaleDirectoryFromAKilledTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // RunArtifactDir resolves under $HOME/.pallium — never touch the real one
	repo := newTeamTestRepo(t)
	r := &Runner{Run: Run{ID: "wf-stale-worktree-test"}}

	path, err := r.createWorktree("tm-stale", repo)
	if err != nil {
		t.Fatalf("first createWorktree call: %v", err)
	}
	// Simulate the kill: leave the worktree exactly as a real interrupted
	// turn would — never call removeWorktree/finalizeWorktreePatch. Confirm
	// the deterministic path is genuinely still there before retrying.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected the first worktree to still exist (simulating a killed turn's leftover): %v", err)
	}

	secondPath, err := r.createWorktree("tm-stale", repo)
	if err != nil {
		t.Fatalf("expected createWorktree to recover from the stale leftover directory, got: %v", err)
	}
	if secondPath != path {
		t.Fatalf("expected the same deterministic path reused, got %q vs %q", secondPath, path)
	}
	if _, err := os.Stat(secondPath); err != nil {
		t.Fatalf("expected a genuinely fresh worktree to exist after recovery: %v", err)
	}
}
