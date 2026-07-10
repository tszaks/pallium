package console

import (
	"path/filepath"
	"testing"
)

func TestManifestRoundTrip(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	saved, err := store.UpsertManifest(Manifest{
		SessionKey:  SessionKey{Provider: "codex", SessionID: "session-1", Machine: "test-host"},
		CWD:         "/tmp/repo",
		Goal:        "Fix production issue",
		CurrentStep: "Inspect logs",
		Files:       []string{"cmd/app.go"},
		NextActions: []string{"run tests"},
		Risks:       []string{"production"},
		Blockers:    []string{"auth token"},
		Stop:        "before deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.UpdatedAt == "" {
		t.Fatal("expected updated_at")
	}

	got, err := store.Manifest(SessionKey{Provider: "codex", SessionID: "session-1", Machine: "test-host"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "Fix production issue" || got.Files[0] != "cmd/app.go" || got.NextActions[0] != "run tests" {
		t.Fatalf("manifest mismatch: %+v", got)
	}
}

func TestClaimDetectsActiveConflict(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	first, err := store.AcquireClaim(Claim{
		Provider:   "codex",
		SessionID:  "session-1",
		Machine:    "test-host",
		RepoRoot:   "/tmp/repo",
		TargetType: "file",
		Target:     "cmd/app.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Conflict {
		t.Fatalf("first claim should not conflict: %+v", first)
	}

	second, err := store.AcquireClaim(Claim{
		Provider:   "claude",
		SessionID:  "session-2",
		Machine:    "test-host",
		RepoRoot:   "/tmp/repo",
		TargetType: "file",
		Target:     "cmd/app.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Conflict || len(second.ConflictIDs) != 1 || second.ConflictIDs[0] != first.ID {
		t.Fatalf("expected conflict with first claim, got %+v", second)
	}
}

func TestAuthorityGateAndReviewLifecycle(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	event, err := store.RequestAuthority(AuthorityEvent{
		Provider:  "codex",
		SessionID: "session-1",
		Machine:   "test-host",
		Level:     "deploy",
		Action:    "ship production",
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.Status != "requested" {
		t.Fatalf("status=%q", event.Status)
	}

	gate, err := store.OpenGate(RiskGate{AuthorityEventID: event.ID, RequiredAttestations: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddAttestation(Attestation{GateID: gate.ID, Verdict: "allow", Evidence: "tests passed"}); err != nil {
		t.Fatal(err)
	}
	gates, err := store.ListGates(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].Status != "satisfied" {
		t.Fatalf("gate not satisfied: %+v", gates)
	}

	review, err := store.CreateReview(ReviewCase{Topic: "Should this ship?", OpenedBy: "session-1", Reviewer: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CloseReview(review.ID, "proceed"); err != nil {
		t.Fatal(err)
	}
	closed, err := store.Review(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "closed" || closed.Decision != "proceed" {
		t.Fatalf("review not closed: %+v", closed)
	}
}

func TestOwnedSessionLifecycle(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	session, err := store.CreateOwnedSession(OwnedSession{
		ID:      "owned-1",
		Command: []string{"/bin/echo", "hello"},
		CWD:     "/tmp",
		LogPath: "/tmp/owned-1.log",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != "starting" {
		t.Fatalf("status=%q", session.Status)
	}
	if err := store.UpdateOwnedSessionStarted(session.ID, 100, 200); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishOwnedSession(session.ID, 0); err != nil {
		t.Fatal(err)
	}
	got, err := store.OwnedSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "exited" || got.RunnerPID != 100 || got.ChildPID != 200 || got.Command[1] != "hello" {
		t.Fatalf("owned session mismatch: %+v", got)
	}
}

func TestOwnedSessionRejectsUnsafeID(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	_, err := store.CreateOwnedSession(OwnedSession{
		ID:      "../escape",
		Command: []string{"/bin/echo", "hello"},
		CWD:     "/tmp",
		LogPath: "/tmp/owned.log",
	})
	if err == nil {
		t.Fatal("expected unsafe owned session id to fail")
	}
}

func TestOwnedSessionUpdateMissingRowFails(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	if err := store.FinishOwnedSession("missing-owned", 0); err == nil {
		t.Fatal("expected missing owned session update to fail")
	}
}

// TestCreateOwnedSessionReenteringSameIDUpserts is the regression test for
// the crash behind `workflow resume <id> --background`: cmd/workflow.go
// derives an owned-session id deterministically from the workflow run's id
// ("workflow-"+run.ID), so resuming the SAME run in the background a
// second time — the whole point of resume — reuses the SAME
// owned_sessions.id. A plain INSERT hit the primary key on that legitimate
// re-entry and surfaced a raw "UNIQUE constraint failed" instead of
// starting the new background attempt; foreground resume never went
// through this path, so only --background crashed. CreateOwnedSession must
// upsert: the second call succeeds and resets the row to the NEW launch's
// values rather than erroring or silently keeping the stale one.
func TestCreateOwnedSessionReenteringSameIDUpserts(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	first, err := store.CreateOwnedSession(OwnedSession{
		ID:      "workflow-wf-resume-bg",
		Command: []string{"pallium", "workflow", "run", "--id", "wf-resume-bg"},
		CWD:     "/tmp/repo",
		LogPath: "/tmp/repo/first.log",
	})
	if err != nil {
		t.Fatalf("first CreateOwnedSession should succeed, got: %v", err)
	}
	if err := store.UpdateOwnedSessionStarted(first.ID, 111, 222); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishOwnedSession(first.ID, 0); err != nil {
		t.Fatal(err)
	}
	before, err := store.OwnedSession(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != "exited" {
		t.Fatalf("expected the first launch to have exited before the resume re-entry, got %+v", before)
	}

	// Resume re-enters with the SAME id — this must not error, and must
	// reset the row (fresh "starting" status, new log path) rather than
	// leave the stale "exited" state from the first launch in place.
	second, err := store.CreateOwnedSession(OwnedSession{
		ID:      "workflow-wf-resume-bg",
		Command: []string{"pallium", "workflow", "run", "--id", "wf-resume-bg"},
		CWD:     "/tmp/repo",
		LogPath: "/tmp/repo/second.log",
	})
	if err != nil {
		t.Fatalf("expected background resume's re-entrant CreateOwnedSession to upsert, got: %v", err)
	}
	if second.Status != "starting" {
		t.Fatalf("expected the resumed session to start fresh, got %+v", second)
	}

	after, err := store.OwnedSession("workflow-wf-resume-bg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "starting" {
		t.Fatalf("expected the row reset to the new launch's starting status, got %+v", after)
	}
	if after.LogPath != "/tmp/repo/second.log" {
		t.Fatalf("expected the row updated to the new launch's log path, got %+v", after)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}
