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

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}
