package sessionmemory

import (
	"path/filepath"
	"testing"
)

func TestUpsertBuildsStructuredContinuityCapsule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := testParsedSession("capsule-session", `Implemented the migration.

Completed:
- Added the schema.
- Verified the focused tests.

Remaining:
- Run the production smoke test.

Blockers: Production credentials are unavailable.

Next action: Ask the owner to run the smoke test.`)
	parsed.Session.Status = "seen"
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	capsule, err := ReadCapsule(path, "capsule-session")
	if err != nil {
		t.Fatal(err)
	}
	if capsule.Goal != "test request" || len(capsule.Completed) != 2 || len(capsule.Remaining) != 1 || len(capsule.Blockers) != 1 {
		t.Fatalf("unexpected capsule structure: %+v", capsule)
	}
	if capsule.NextAction != "Ask the owner to run the smoke test." {
		t.Fatalf("next action=%q", capsule.NextAction)
	}
	if len(capsule.Evidence) != 2 || capsule.Evidence[0].LineNo != 1 || capsule.Evidence[1].LineNo != 2 {
		t.Fatalf("unexpected evidence: %+v", capsule.Evidence)
	}
}

func TestCapsuleDoesNotPromoteHistoricalErrorsOrTurnCompletion(t *testing.T) {
	parsed := testParsedSession("clean-capsule", "I am checking the remaining migration.")
	parsed.Session.Status = "complete"
	parsed.Session.Errors = []string{"old test failed before the fix"}
	capsule := buildSessionCapsule(parsed)
	if len(capsule.Blockers) != 0 || len(capsule.Completed) != 0 {
		t.Fatalf("historical state became a current handoff claim: %+v", capsule)
	}
	if capsule.SchemaVersion != sessionCapsuleSchemaVersion {
		t.Fatalf("schema version=%d, want %d", capsule.SchemaVersion, sessionCapsuleSchemaVersion)
	}
}

func TestRedactExpandedSecretForms(t *testing.T) {
	for _, secret := range []string{
		"postgresql://user:very-secret-password@example.test/db",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.verylongsignaturesegment",
		"-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----",
	} {
		redacted := redact("value=" + secret)
		if redacted == "value="+secret || redacted == "" {
			t.Fatalf("secret was not redacted: %q", secret)
		}
	}
}
