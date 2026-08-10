package sessionmemory

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBackfillLegacySessionsRebuildsMissingContinuity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	noisy := `<recommended_plugins>plugin list</recommended_plugins>Recover the old checkout task`
	insertSessionForRelatedTest(t, store, "legacy-session", "/repo", noisy, []string{"src/db.go"}, []string{"go test ./..."}, "2026-08-07T12:00:00Z")
	if _, err := store.db.Exec(`UPDATE codex_sessions SET first_user_message=?,last_agent_message=?,status='skipped_large_rollout',errors_json=? WHERE id='legacy-session'`, noisy, "Next action: run the smoke test", `["skipped full parse: old safety limit"]`); err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{
		{LineNo: 0, Role: "user", Kind: "message", Text: "# AGENTS.md instructions for /repo"},
		{LineNo: 1, Timestamp: "2026-08-07T12:00:01Z", Role: "user", Kind: "message", Text: "Recover the old checkout task"},
		{LineNo: 2, Timestamp: "2026-08-07T12:00:01Z", Role: "user", Kind: "message", Text: "Recover the old checkout task"},
		{LineNo: 3, Role: "assistant", Kind: "message", Text: "Next action: run the smoke test"},
	} {
		if _, err := store.db.Exec(`INSERT INTO codex_session_messages(session_id,line_no,timestamp,role,kind,text) VALUES(?,?,?,?,?,?)`, "legacy-session", message.LineNo, message.Timestamp, message.Role, message.Kind, message.Text); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`INSERT INTO codex_session_events(session_id,line_no,raw_json) VALUES('legacy-session',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	count, err := store.backfillLegacySessions()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("backfilled=%d, want 1", count)
	}
	sess, err := store.loadSession("legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title != "Recover the old checkout task" || sess.FirstUserMessage != "Recover the old checkout task" || sess.Status != "legacy_recovered" {
		t.Fatalf("legacy metadata was not normalized: %+v", sess)
	}
	var noisyMessages int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_messages WHERE session_id='legacy-session' AND text LIKE '# AGENTS.md instructions%'`).Scan(&noisyMessages); err != nil {
		t.Fatal(err)
	}
	if noisyMessages != 0 {
		t.Fatalf("legacy injected messages remain: %d", noisyMessages)
	}
	var realUserMessages int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_messages WHERE session_id='legacy-session' AND text='Recover the old checkout task'`).Scan(&realUserMessages); err != nil {
		t.Fatal(err)
	}
	if realUserMessages != 1 {
		t.Fatalf("legacy paired messages remain: %d", realUserMessages)
	}
	var rawEvents int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_events WHERE session_id='legacy-session'`).Scan(&rawEvents); err != nil {
		t.Fatal(err)
	}
	if rawEvents != 0 {
		t.Fatalf("legacy raw events remain: %d", rawEvents)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	capsule, err := ReadCapsule(path, "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if capsule.Coverage.Mode != "legacy" || !strings.Contains(capsule.Coverage.Warning, "original source transcript was unavailable") || capsule.NextAction != "run the smoke test" {
		t.Fatalf("unexpected legacy capsule: %+v", capsule)
	}
}
