package sessionmemory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecallReturnsStructuredContinuationWithEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := testParsedSession("recall-session", `Checkout migration status.

Completed:
- Added the migration.
- Ran unit tests.

Remaining:
- Run the production smoke test.

Blockers: Production credentials are unavailable.

Next action: Ask the owner to run the smoke test.`)
	parsed.Session.Title = "Checkout migration"
	parsed.Session.Source = "codex"
	parsed.Session.CWD = "/repo"
	parsed.SearchBlob = "checkout migration production smoke test"
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := Recall(context.Background(), RecallOptions{Search: SessionSearchOptions{DBPath: path, Query: "checkout migration", RepoRoot: "/repo"}, LexicalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.SessionID != "recall-session" || len(report.Completed) != 2 || len(report.Remaining) != 1 || len(report.Blockers) != 1 {
		t.Fatalf("unexpected recall: %+v", report)
	}
	if report.NextAction != "Ask the owner to run the smoke test." || len(report.Evidence) != 2 {
		t.Fatalf("recall lost next action or evidence: %+v", report)
	}
	if report.Confidence.Level == "low" || report.Confidence.Score < 0.5 {
		t.Fatalf("unexpected confidence: %+v", report.Confidence)
	}
}

func TestRecallEmptyResultUsesStableEmptyArrays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Recall(context.Background(), RecallOptions{Search: SessionSearchOptions{DBPath: path, Query: "nothing here"}, LexicalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{`"completed":null`, `"remaining":null`, `"blockers":null`, `"evidence":null`, `"matches":null`} {
		if strings.Contains(string(payload), unwanted) {
			t.Fatalf("unstable null collection in %s", payload)
		}
	}
}

func TestRecallRejectsWeakSemanticOnlyCandidate(t *testing.T) {
	t.Setenv("PALLIUM_EMBED_PROVIDER", "test-local")
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := testParsedSession("unrelated-session", "checkout migration completed")
	parsed.Session.Title = "Checkout migration"
	parsed.SearchBlob = "checkout migration"
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.Query(`SELECT id,text_sha256 FROM codex_session_chunks WHERE session_id=?`, parsed.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var chunks [][2]string
	for rows.Next() {
		var chunk [2]string
		if err := rows.Scan(&chunk[0], &chunk[1]); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks {
		if _, err := store.db.Exec(`INSERT INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, chunk[0], "test-local", "test-model", 2, packVector([]float64{0.1, 0.995}), chunk[1], "2026-08-07T12:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	originalEmbedTexts := embedTexts
	t.Cleanup(func() { embedTexts = originalEmbedTexts })
	embedTexts = func(context.Context, string, []string) ([][]float64, error) {
		return [][]float64{{1, 0}}, nil
	}
	report, err := Recall(context.Background(), RecallOptions{Search: SessionSearchOptions{DBPath: path, Query: "orbital jellyfish", Model: "test-model"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.SessionID != "" || len(report.Matches) != 0 || report.Confidence.Level != "low" {
		t.Fatalf("weak semantic-only match produced a continuation: %+v", report)
	}
}

func TestRecallFiltersWeakSemanticCandidatesBeforeLimit(t *testing.T) {
	t.Setenv("PALLIUM_EMBED_PROVIDER", "test-local")
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	lexical := testParsedSession("lexical-evidence", "orbital jellyfish evidence")
	lexical.Session.Title = "Orbital jellyfish"
	lexical.Session.UpdatedAt = "2026-08-07T11:00:00Z"
	lexical.SearchBlob = "orbital jellyfish"
	if err := store.upsert(lexical, nil); err != nil {
		t.Fatal(err)
	}
	weak := testParsedSession("weak-semantic", "unrelated newer session")
	weak.Session.Title = "Unrelated"
	weak.Session.UpdatedAt = "2026-08-07T12:00:00Z"
	weak.SearchBlob = "unrelated newer session"
	if err := store.upsert(weak, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.Query(`SELECT id,text_sha256 FROM codex_session_chunks WHERE session_id=?`, weak.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var chunks [][2]string
	for rows.Next() {
		var chunk [2]string
		if err := rows.Scan(&chunk[0], &chunk[1]); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks {
		if _, err := store.db.Exec(`INSERT INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, chunk[0], "test-local", "test-model", 2, packVector([]float64{0.1, 0.995}), chunk[1], "2026-08-07T12:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	originalEmbedTexts := embedTexts
	t.Cleanup(func() { embedTexts = originalEmbedTexts })
	embedTexts = func(context.Context, string, []string) ([][]float64, error) {
		return [][]float64{{1, 0}}, nil
	}
	report, err := Recall(context.Background(), RecallOptions{Search: SessionSearchOptions{DBPath: path, Query: "orbital jellyfish", Limit: 1, Model: "test-model"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.SessionID != lexical.Session.ID || len(report.Matches) != 1 {
		t.Fatalf("weak semantic candidate displaced exact lexical evidence: %+v", report)
	}
}

func TestReadMessagesPagesTranscriptAndLocateSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := testParsedSession("paged-session", "done")
	parsed.Session.RolloutPath = "/tmp/source-rollout.jsonl"
	parsed.Messages = nil
	for i := 1; i <= 120; i++ {
		parsed.Messages = append(parsed.Messages, Message{LineNo: i, Role: "assistant", Kind: "message", Text: "message"})
	}
	if err := store.upsert(parsed, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	sess, messages, err := ReadMessages(path, "paged", 50, 10)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "paged-session" || len(messages) != 10 || messages[0].LineNo != 50 || messages[9].LineNo != 59 {
		t.Fatalf("unexpected transcript page: session=%+v messages=%+v", sess, messages)
	}
	location, err := LocateSession(path, "paged")
	if err != nil {
		t.Fatal(err)
	}
	if location.RolloutPath != "/tmp/source-rollout.jsonl" {
		t.Fatalf("unexpected source location: %+v", location)
	}
}

func TestSyncIndexesCapsulesAndExplainsEmbeddingSkip(t *testing.T) {
	t.Setenv("PALLIUM_EMBED_PROVIDER", "openai")
	t.Setenv("PALLIUM_EMBED_BASE_URL", "")
	t.Setenv("PALLIUM_EMBED_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_ADMIN_API_KEY", "")
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	transcript := filepath.Join(claudeHome, "projects", "repo", "sync-session.jsonl")
	writeJSONLines(t, transcript, []any{
		map[string]any{"type": "user", "sessionId": "sync-session", "message": map[string]any{"role": "user", "content": "sync this session"}},
		map[string]any{"type": "assistant", "sessionId": "sync-session", "message": map[string]any{"role": "assistant", "content": "synced"}},
	})
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	var phases []string
	report, err := Sync(context.Background(), SyncOptions{Index: Options{DBPath: dbPath, ClaudeHome: claudeHome, Provider: "claude"}}, func(progress SyncProgress) {
		phases = append(phases, progress.Phase)
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Indexed != 1 || report.Stats.Capsules != 1 || report.Stats.Events != 0 {
		t.Fatalf("unexpected sync report: %+v", report)
	}
	if !report.EmbeddingSkipped || !strings.Contains(report.EmbeddingWarning, "no OpenAI key") {
		t.Fatalf("sync did not explain semantic skip: %+v", report)
	}
	if len(phases) == 0 || phases[0] != "index" || phases[len(phases)-1] != "complete" {
		t.Fatalf("unexpected progress phases: %v", phases)
	}
}

func TestSyncRepairsLegacyNoiseWithoutForcingUnchangedSources(t *testing.T) {
	t.Setenv("PALLIUM_EMBED_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_ADMIN_API_KEY", "")
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	transcript := filepath.Join(claudeHome, "projects", "repo", "sync-legacy-noise.jsonl")
	writeJSONLines(t, transcript, []any{
		map[string]any{"type": "user", "sessionId": "sync-legacy-noise", "message": map[string]any{"role": "user", "content": "ship the repaired sync"}},
	})
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	if _, err := Sync(context.Background(), SyncOptions{Index: Options{DBPath: dbPath, ClaudeHome: claudeHome, Provider: "claude"}, NoEmbed: true}, nil); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE codex_sessions SET title='# AGENTS.md instructions for /repo',first_user_message='# AGENTS.md instructions for /repo' WHERE id='sync-legacy-noise'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO codex_session_messages(session_id,line_no,role,kind,text) VALUES('sync-legacy-noise',0,'user','message','# AGENTS.md instructions for /repo')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Sync(context.Background(), SyncOptions{Index: Options{DBPath: dbPath, ClaudeHome: claudeHome, Provider: "claude"}, NoEmbed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.FullReindex || report.Indexed != 0 || report.LegacyBackfilled != 1 {
		t.Fatalf("legacy-only repair forced source reindex: %+v", report)
	}
	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := store.loadSession("sync-legacy-noise")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title != "ship the repaired sync" || sess.FirstUserMessage != "ship the repaired sync" {
		t.Fatalf("legacy-only repair did not recover the real ask: %+v", sess)
	}
}

func TestSyncMigratesOutdatedCapsulesIncrementally(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	transcript := filepath.Join(claudeHome, "projects", "repo", "sync-capsule-schema.jsonl")
	writeJSONLines(t, transcript, []any{
		map[string]any{"type": "user", "sessionId": "sync-capsule-schema", "message": map[string]any{"role": "user", "content": "migrate the capsule"}},
	})
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	opts := SyncOptions{Index: Options{DBPath: dbPath, ClaudeHome: claudeHome, Provider: "claude"}, NoEmbed: true}
	if _, err := Sync(context.Background(), opts, nil); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE codex_session_capsules SET schema_version=1 WHERE session_id='sync-capsule-schema'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Sync(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.FullReindex || report.Indexed != 0 || report.LegacyBackfilled != 1 {
		t.Fatalf("capsule migration forced source reindex: %+v", report)
	}
	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`SELECT schema_version FROM codex_session_capsules WHERE session_id='sync-capsule-schema'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != sessionCapsuleSchemaVersion {
		t.Fatalf("capsule schema=%d, want %d", version, sessionCapsuleSchemaVersion)
	}
}
