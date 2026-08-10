package sessionmemory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenHardensSessionStorePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".pallium")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory permissions=%#o, want 0700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(dir, "codex-sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("database permissions=%#o, want 0600", got)
	}
}

func TestUpsertRemovesEmbeddingsForReplacedChunks(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	parsed := testParsedSession("replace-me", "first answer")
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	chunk := buildChunks(parsed)[0]
	if _, err := store.db.Exec(`INSERT INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, chunk.ID, "openai", DefaultEmbeddingModel, 2, packVector([]float64{1, 0}), chunk.TextSHA256, "2026-08-07T12:00:00Z"); err != nil {
		t.Fatal(err)
	}

	parsed.Session.LastAgentMessage = "replacement answer"
	parsed.Messages[1].Text = "replacement answer"
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_embeddings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("embeddings after replacement=%d, want 0", count)
	}
}

func TestUpsertPreservesEmbeddingsForUnchangedChunks(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parsed := testParsedSession("keep-embedding", "same answer")
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	chunk := buildChunks(parsed)[0]
	if _, err := store.db.Exec(`INSERT INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, chunk.ID, "openai", DefaultEmbeddingModel, 2, packVector([]float64{1, 0}), chunk.TextSHA256, "2026-08-07T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_embeddings WHERE chunk_id=?`, chunk.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unchanged embedding count=%d, want 1", count)
	}
}

func TestDoctorRepairsEmbeddingIntegrityAndRawEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := testParsedSession("doctor-me", "answer")
	parsed.RawEvents = []RawEvent{{LineNo: 1, Type: "event_msg", RawJSON: `{}`}}
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	chunk := buildChunks(parsed)[0]
	for _, embedding := range []struct{ id, sha string }{
		{id: chunk.ID, sha: "stale-sha"},
		{id: "missing-chunk", sha: "orphan-sha"},
	} {
		if _, err := store.db.Exec(`INSERT INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, embedding.id, "openai", DefaultEmbeddingModel, 2, packVector([]float64{1, 0}), embedding.sha, "2026-08-07T12:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := DoctorSessions(SessionDoctorOptions{DBPath: path, Repair: true, PruneRawEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Repair.OrphanEmbeddingsRemoved != 1 || report.Repair.StaleEmbeddingsRemoved != 1 || report.Repair.RawEventsRemoved != 1 {
		t.Fatalf("repair=%+v, want one orphan, stale, and raw event removed", report.Repair)
	}
	if report.OrphanEmbeddings != 0 || report.StaleEmbeddings != 0 || report.StoredRawEvents != 0 {
		t.Fatalf("post-repair report still has invalid rows: %+v", report)
	}
}

func TestForgetSessionRequiresConfirmation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := testParsedSession("forget-me-1234", "answer")
	parsed.Session.Source = "claude"
	parsed.Session.RolloutPath = filepath.Join(t.TempDir(), "forget-me-1234.jsonl")
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	chunk := buildChunks(parsed)[0]
	if _, err := store.db.Exec(`INSERT INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, chunk.ID, "openai", DefaultEmbeddingModel, 2, packVector([]float64{1, 0}), chunk.TextSHA256, "2026-08-07T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	preview, err := ForgetSession(path, "forget-me", false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Deleted || preview.Confirmed || preview.Messages == 0 || preview.Chunks == 0 || preview.Embeddings == 0 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	confirmed, err := ForgetSession(path, "forget-me", true)
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed.Deleted || !confirmed.Confirmed {
		t.Fatalf("unexpected confirmed result: %+v", confirmed)
	}
	verify, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var count int
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM codex_sessions WHERE id='forget-me-1234'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("session remains after confirmed forget")
	}
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM codex_session_tombstones WHERE session_id='forget-me-1234' AND reason='forget'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("forget did not preserve a deletion tombstone")
	}
}

func TestPruneSessionsRequiresConfirmation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := testParsedSession("old-session", "answer")
	parsed.Session.CreatedAt = "2020-01-01T00:00:00Z"
	parsed.Session.UpdatedAt = "2020-01-01T00:00:00Z"
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	preview, err := PruneSessions(path, 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Matched != 1 || preview.Deleted != 0 || preview.Confirmed {
		t.Fatalf("unexpected retention preview: %+v", preview)
	}
	confirmed, err := PruneSessions(path, 24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Deleted != 1 || !confirmed.Confirmed {
		t.Fatalf("unexpected confirmed retention: %+v", confirmed)
	}
	verify, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var count int
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM codex_session_tombstones WHERE session_id='old-session' AND reason='retention'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("prune did not preserve a deletion tombstone")
	}
}

func TestForgottenSessionIsNotReindexedByForcedSync(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	transcript := filepath.Join(claudeHome, "projects", "repo", "forgotten-session.jsonl")
	writeJSONLines(t, transcript, []any{
		map[string]any{"type": "user", "sessionId": "forgotten-session", "cwd": "/repo", "message": map[string]any{"role": "user", "content": "forget this session"}},
		map[string]any{"type": "assistant", "sessionId": "forgotten-session", "cwd": "/repo", "message": map[string]any{"role": "assistant", "content": "done"}},
	})
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	opts := Options{DBPath: dbPath, ClaudeHome: claudeHome, Provider: "claude", Force: true, Machine: "test-host"}
	indexed, err := Index(context.Background(), opts, nil)
	if err != nil || indexed != 1 {
		t.Fatalf("initial index: indexed=%d err=%v", indexed, err)
	}
	if _, err := ForgetSession(dbPath, "forgotten-session", true); err != nil {
		t.Fatal(err)
	}
	indexed, err = Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if indexed != 0 {
		t.Fatalf("forced sync reindexed a forgotten session: %d", indexed)
	}
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM codex_sessions WHERE id='forgotten-session'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("forgotten session reappeared after forced sync")
	}
}

func TestUpsertRechecksTombstoneInsideTransaction(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parsed := testParsedSession("race-session", "must stay forgotten")
	parsed.Session.RolloutPath = filepath.Join(t.TempDir(), "race-session.jsonl")

	tombstoned, err := store.sessionTombstoned(parsed.Session.ID, parsed.Session.RolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	if tombstoned {
		t.Fatal("test setup unexpectedly started with a tombstone")
	}
	if _, err := store.db.Exec(`INSERT INTO codex_session_tombstones(session_id,source,rollout_path,deleted_at,reason) VALUES(?,?,?,?,?)`, parsed.Session.ID, "codex", parsed.Session.RolloutPath, time.Now().UTC().Format(time.RFC3339Nano), "forget"); err != nil {
		t.Fatal(err)
	}

	if err := store.upsert(parsed, nil); !errors.Is(err, errSessionTombstoned) {
		t.Fatalf("upsert error=%v, want tombstone rejection", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM codex_sessions WHERE id=?`, parsed.Session.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("upsert recreated a session after the stale outer tombstone check")
	}
}

func TestImmediateSessionTransactionOwnsWriterBeforeTombstoneRead(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.sqlite")
	writer, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	competitor, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer competitor.Close()
	if _, err := competitor.db.Exec(`PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}

	tx, err := writer.beginImmediateSessionTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := competitor.db.Exec(`INSERT INTO codex_session_tombstones(session_id,deleted_at,reason) VALUES('concurrent-delete','2026-08-10T00:00:00Z','forget')`); err == nil {
		t.Fatal("competing tombstone writer was not serialized behind the immediate transaction")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := competitor.db.Exec(`INSERT INTO codex_session_tombstones(session_id,deleted_at,reason) VALUES('concurrent-delete','2026-08-10T00:00:00Z','forget')`); err != nil {
		t.Fatalf("competing tombstone writer did not proceed after rollback: %v", err)
	}
}

func TestSemanticIgnoresStaleHashEmbeddings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PALLIUM_EMBED_PROVIDER", "openai")
	originalEmbedTexts := embedTexts
	t.Cleanup(func() { embedTexts = originalEmbedTexts })
	embedTexts = func(context.Context, string, []string) ([][]float64, error) {
		return [][]float64{{1, 0}}, nil
	}
	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	parsed := testParsedSession("stale-semantic", "answer")
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	chunk := buildChunks(parsed)[0]
	if _, err := store.db.Exec(`INSERT INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, chunk.ID, "openai", DefaultEmbeddingModel, 2, packVector([]float64{1, 0}), "old-sha", "2026-08-07T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	hits, err := Semantic(context.Background(), "answer", DefaultEmbeddingModel, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("semantic returned stale embedding: %+v", hits)
	}
}

func testParsedSession(id, answer string) ParsedSession {
	return ParsedSession{
		Session: Session{
			ID:               id,
			Machine:          "test",
			Title:            "Test session",
			FirstUserMessage: "test request",
			LastAgentMessage: answer,
			CWD:              "/tmp/repo",
			CreatedAt:        "2026-08-07T11:00:00Z",
			UpdatedAt:        "2026-08-07T12:00:00Z",
		},
		Messages: []Message{
			{LineNo: 1, Role: "user", Kind: "message", Text: "test request"},
			{LineNo: 2, Role: "assistant", Kind: "message", Text: answer},
		},
		SearchBlob: "test request " + answer,
	}
}
