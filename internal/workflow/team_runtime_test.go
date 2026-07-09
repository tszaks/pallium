package workflow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
