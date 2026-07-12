package sessionmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIndexClaudeSessions(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	projectDir := filepath.Join(claudeHome, "projects", "-tmp-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"Fix the billing auth bug"},"timestamp":"2026-06-10T12:00:00Z","sessionId":"claude-1","cwd":"/tmp/repo","version":"2.1.128","gitBranch":"main"}`,
		`{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"I will inspect the auth code."},{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]},"timestamp":"2026-06-10T12:01:00Z","sessionId":"claude-1","cwd":"/tmp/repo","version":"2.1.128","gitBranch":"main"}`,
		`{"type":"ai-title","aiTitle":"Fix billing auth bug","sessionId":"claude-1"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(projectDir, "claude-1.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(tmp, "sessions.sqlite")
	count, err := Index(context.Background(), Options{DBPath: dbPath, ClaudeHome: claudeHome, Provider: "claude", Machine: "test-host", Force: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("indexed %d sessions, want 1", count)
	}
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sessions, err := store.search("billing auth", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("search returned %d sessions, want 1", len(sessions))
	}
	s := sessions[0].Session
	if s.Source != "claude" {
		t.Fatalf("source=%q, want claude", s.Source)
	}
	if s.ModelProvider != "anthropic" {
		t.Fatalf("model_provider=%q, want anthropic", s.ModelProvider)
	}
	if s.Model != "claude-sonnet-4" {
		t.Fatalf("model=%q, want claude-sonnet-4", s.Model)
	}
	if s.Title != "Fix billing auth bug" {
		t.Fatalf("title=%q", s.Title)
	}
	if len(s.Commands) != 1 || s.Commands[0] != "go test ./..." {
		t.Fatalf("commands=%v", s.Commands)
	}
}

func TestIndexProviderCodexSkipsClaudeIncludes(t *testing.T) {
	tmp := t.TempDir()
	codexHome := filepath.Join(tmp, ".codex")
	claudeDir := filepath.Join(tmp, ".claude", "projects", "-tmp-repo")
	if err := os.MkdirAll(filepath.Join(codexHome, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "claude-1.jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"Claude only"},"sessionId":"claude-1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	count, err := Index(context.Background(), Options{DBPath: filepath.Join(tmp, "sessions.sqlite"), CodexHome: codexHome, Provider: "codex"}, []string{claudeDir})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("indexed %d sessions, want 0", count)
	}
}

func TestIndexAllDirectClaudeIncludeIsNotClaimedByCodex(t *testing.T) {
	tmp := t.TempDir()
	claudePath := filepath.Join(tmp, "claude-1.jsonl")
	content := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"Claude include only"},"timestamp":"2026-06-10T12:00:00Z","sessionId":"claude-1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"Claude response"},"timestamp":"2026-06-10T12:01:00Z","sessionId":"claude-1"}`,
		"",
	}, "\n")
	if err := os.WriteFile(claudePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(claudePath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(tmp, "sessions.sqlite")
	count, err := Index(context.Background(), Options{DBPath: dbPath, CodexHome: filepath.Join(tmp, ".codex"), ClaudeHome: filepath.Join(tmp, ".claude"), Provider: "all", Machine: "test-host"}, []string{claudePath})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("indexed %d sessions, want 1", count)
	}
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var source string
	if err := store.db.QueryRow(`SELECT source FROM codex_sessions WHERE id=?`, "claude-1").Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "claude" {
		t.Fatalf("source=%q, want claude", source)
	}
}

func TestIndexSkipsUnchangedRollouts(t *testing.T) {
	tmp := t.TempDir()
	codexHome := filepath.Join(tmp, ".codex")
	sessionDir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(sessionDir, "rollout-2026-06-22T12-00-00-codex-1.jsonl")
	first := strings.Join([]string{
		`{"type":"session_meta","timestamp":"2026-06-22T12:00:00Z","payload":{"id":"codex-1","cwd":"/tmp/repo","source":"codex"}}`,
		`{"type":"event_msg","timestamp":"2026-06-22T12:01:00Z","payload":{"type":"user_message","message":"first prompt"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(rolloutPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(tmp, "sessions.sqlite")
	opts := Options{DBPath: dbPath, CodexHome: codexHome, Provider: "codex", Machine: "test-host"}
	count, err := Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("indexed %d sessions, want 1", count)
	}
	count, err = Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("indexed unchanged rollout %d times, want 0", count)
	}
	indexedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE codex_sessions SET indexed_at=? WHERE id=?`, indexedAt, "codex-1"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	changed := strings.Join([]string{
		strings.TrimSpace(first),
		`{"type":"event_msg","timestamp":"2026-06-22T12:02:00Z","payload":{"type":"agent_message","message":"changed answer"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	changedTime := time.Now().Add(-5 * time.Minute)
	if err := os.Chtimes(rolloutPath, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	count, err = Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("indexed changed rollout %d times, want 1", count)
	}
	opts.Force = true
	count, err = Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("force indexed %d sessions, want 1", count)
	}
}

func TestShouldSkipRolloutUsesIndexedAtBeforeHash(t *testing.T) {
	tmp := t.TempDir()
	rolloutPath := filepath.Join(tmp, "rollout-2026-06-22T12-00-00-codex-1.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("changed content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileTime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(rolloutPath, fileTime, fileTime); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	indexedAt := fileTime.Add(1 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.Exec(`INSERT INTO codex_sessions(id,machine,indexed_at,rollout_path,rollout_sha256) VALUES(?,?,?,?,?)`, "codex-1", "test", indexedAt, rolloutPath, "different-sha"); err != nil {
		t.Fatal(err)
	}

	skip, err := store.rolloutSkipReason(rolloutPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if skip != rolloutSkipUnchanged {
		t.Fatalf("skip=%v, want rolloutSkipUnchanged", skip)
	}

	newTime := fileTime.Add(2 * time.Minute)
	if err := os.Chtimes(rolloutPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	skip, err = store.rolloutSkipReason(rolloutPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if skip != rolloutSkipNone {
		t.Fatalf("skip=%v, want rolloutSkipNone", skip)
	}
}

func TestCodexStateFreshnessCanForceSkippedRolloutReindex(t *testing.T) {
	tmp := t.TempDir()
	rolloutPath := filepath.Join(tmp, "rollout-2026-06-22T12-00-00-codex-1.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("unchanged content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileTime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(rolloutPath, fileTime, fileTime); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	indexedAt := fileTime.Add(1 * time.Minute).UTC()
	if _, err := store.db.Exec(`INSERT INTO codex_sessions(id,machine,indexed_at,rollout_path,rollout_sha256) VALUES(?,?,?,?,?)`, "codex-1", "test", indexedAt.Format(time.RFC3339Nano), rolloutPath, "sha"); err != nil {
		t.Fatal(err)
	}

	fresh, err := store.codexStateFreshForRollout(rolloutPath, map[string]map[string]any{
		"codex-1": {"updated_at_ms": fmt.Sprint(indexedAt.Add(-1 * time.Minute).UnixMilli())},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatalf("expected older state metadata to be fresh")
	}

	fresh, err = store.codexStateFreshForRollout(rolloutPath, map[string]map[string]any{
		"codex-1": {"updated_at_ms": fmt.Sprint(indexedAt.Add(1 * time.Minute).UnixMilli())},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatalf("expected newer state metadata to require re-index")
	}
}

func TestActiveRolloutSkipReasonWinsOverStateFreshness(t *testing.T) {
	tmp := t.TempDir()
	rolloutPath := filepath.Join(tmp, "rollout-2026-06-22T12-00-00-codex-1.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("active content"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	indexedAt := time.Now().Add(-10 * time.Minute).UTC()
	if _, err := store.db.Exec(`INSERT INTO codex_sessions(id,machine,indexed_at,rollout_path,rollout_sha256) VALUES(?,?,?,?,?)`, "codex-1", "test", indexedAt.Format(time.RFC3339Nano), rolloutPath, "sha"); err != nil {
		t.Fatal(err)
	}

	reason, err := store.rolloutSkipReason(rolloutPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if reason != rolloutSkipActive {
		t.Fatalf("skip reason=%v, want rolloutSkipActive", reason)
	}

	fresh, err := store.codexStateFreshForRollout(rolloutPath, map[string]map[string]any{
		"codex-1": {"updated_at_ms": fmt.Sprint(indexedAt.Add(1 * time.Minute).UnixMilli())},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatalf("expected newer state metadata to be stale")
	}
}

func TestIndexUsesEmbeddingCursorWithSafetyBuffer(t *testing.T) {
	tmp := t.TempDir()
	codexHome := filepath.Join(tmp, ".codex")
	sessionDir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldRollout := filepath.Join(sessionDir, "rollout-2026-06-22T10-00-00-old.jsonl")
	recentRollout := filepath.Join(sessionDir, "rollout-2026-06-22T11-00-00-recent.jsonl")
	for path, id := range map[string]string{
		oldRollout:    "old",
		recentRollout: "recent",
	} {
		content := strings.Join([]string{
			fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-06-22T12:00:00Z","payload":{"id":%q,"cwd":"/tmp/repo","source":"codex"}}`, id),
			fmt.Sprintf(`{"type":"event_msg","timestamp":"2026-06-22T12:01:00Z","payload":{"type":"user_message","message":"%s prompt"}}`, id),
			"",
		}, "\n")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(oldRollout, now.Add(-3*time.Hour), now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recentRollout, now.Add(-70*time.Minute), now.Add(-70*time.Minute)); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(tmp, "sessions.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setEmbeddingCursor(DefaultEmbeddingModel, now.Add(-1*time.Hour)); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	count, err := Index(context.Background(), Options{
		DBPath:         dbPath,
		CodexHome:      codexHome,
		Provider:       "codex",
		Machine:        "test-host",
		SafetyBuffer:   15 * time.Minute,
		EmbeddingModel: DefaultEmbeddingModel,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("indexed %d sessions, want only recent session", count)
	}

	verify, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var oldCount, recentCount int
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM codex_sessions WHERE id='old'`).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM codex_sessions WHERE id='recent'`).Scan(&recentCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 || recentCount != 1 {
		t.Fatalf("old indexed=%d recent indexed=%d, want 0 and 1", oldCount, recentCount)
	}
}

func TestIndexSkipsActiveRolloutsUnlessForced(t *testing.T) {
	tmp := t.TempDir()
	codexHome := filepath.Join(tmp, ".codex")
	sessionDir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(sessionDir, "rollout-2026-06-22T12-00-00-codex-1.jsonl")
	content := strings.Join([]string{
		`{"type":"session_meta","timestamp":"2026-06-22T12:00:00Z","payload":{"id":"codex-1","cwd":"/tmp/repo","source":"codex"}}`,
		`{"type":"event_msg","timestamp":"2026-06-22T12:01:00Z","payload":{"type":"user_message","message":"active prompt"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(tmp, "sessions.sqlite")
	opts := Options{DBPath: dbPath, CodexHome: codexHome, Provider: "codex", Machine: "test-host"}
	count, err := Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("indexed active rollout %d times, want 0", count)
	}

	opts.Force = true
	count, err = Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("force indexed active rollout %d times, want 1", count)
	}
}

func TestUpsertRedactsSessionSummarySearchSurfaces(t *testing.T) {
	tmp := t.TempDir()
	secret := "sk-1234567890abcdefghijklmnopqrst"
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	parsed := ParsedSession{
		Session: Session{
			ID:               "redact-session",
			Title:            "Investigate " + secret,
			FirstUserMessage: "Use api_key=" + secret,
			LastAgentMessage: "Finished with token=" + secret,
			CWD:              "/tmp/repo/" + secret,
			Source:           "codex",
			CreatedAt:        "2026-06-10T12:00:00Z",
			UpdatedAt:        "2026-06-10T12:01:00Z",
			FilesTouched:     []string{"internal/" + secret + ".go"},
			Commands:         []string{"curl -H authorization=" + secret + " https://example.test"},
			ToolNames:        []string{"exec_command"},
			Errors:           []string{"failed with secret=" + secret},
		},
		SearchBlob: "search blob mentions " + secret,
		Messages: []Message{
			{LineNo: 1, Timestamp: "2026-06-10T12:00:00Z", Role: "user", Kind: "message", Text: "user text " + secret},
			{LineNo: 2, Timestamp: "2026-06-10T12:01:00Z", Role: "assistant", Kind: "message", Text: "assistant text " + secret},
		},
		EventCounts: map[string]int{"response_item": 2},
	}
	metadata := map[string]any{
		"api_key": secret,
		"nested":  map[string]any{"value": "password=" + secret},
	}
	if err := store.upsert(parsed, metadata); err != nil {
		t.Fatal(err)
	}

	assertNoSecretInQuery(t, store, secret, `SELECT title || first_user_message || last_agent_message || cwd || files_touched_json || commands_json || errors_json || metadata_json FROM codex_sessions WHERE id='redact-session'`)
	assertNoSecretInQuery(t, store, secret, `SELECT title || cwd || first_user_message || last_agent_message || files || commands || text FROM codex_session_fts WHERE session_id='redact-session'`)
	assertNoSecretInQuery(t, store, secret, `SELECT group_concat(text, char(10)) FROM codex_session_messages WHERE session_id='redact-session'`)
	assertNoSecretInQuery(t, store, secret, `SELECT group_concat(text, char(10)) FROM codex_message_fts WHERE session_id='redact-session'`)
	assertNoSecretInQuery(t, store, secret, `SELECT group_concat(text || metadata_json, char(10)) FROM codex_session_chunks WHERE session_id='redact-session'`)
}

func assertNoSecretInQuery(t *testing.T, store *Store, secret, query string) {
	t.Helper()
	var value string
	if err := store.db.QueryRow(query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(value, secret) {
		t.Fatalf("query leaked secret %q in %q", secret, value)
	}
	if !strings.Contains(value, "<REDACTED>") {
		t.Fatalf("query did not include redaction marker in %q", value)
	}
}

func TestEmbedSessionOnlyEmbedsRequestedSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	originalEmbedTexts := embedTexts
	t.Cleanup(func() { embedTexts = originalEmbedTexts })

	var batches [][]string
	embedTexts = func(ctx context.Context, model string, texts []string) ([][]float64, error) {
		batches = append(batches, append([]string(nil), texts...))
		vecs := make([][]float64, len(texts))
		for i := range texts {
			vecs[i] = []float64{float64(i + 1), 0.5}
		}
		return vecs, nil
	}

	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := "2026-06-10T12:00:00Z"
	for _, session := range []struct {
		id    string
		title string
	}{
		{id: "session-target", title: "Target"},
		{id: "session-other", title: "Other"},
	} {
		_, err := store.db.Exec(`INSERT INTO codex_sessions(id,machine,title,indexed_at,updated_at) VALUES(?,?,?,?,?)`, session.id, "test", session.title, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, chunk := range []struct {
		id        string
		sessionID string
		index     int
		text      string
		sha       string
	}{
		{id: "target-1", sessionID: "session-target", index: 0, text: "first target chunk", sha: "sha-target-1"},
		{id: "target-2", sessionID: "session-target", index: 1, text: "second target chunk", sha: "sha-target-2"},
		{id: "other-1", sessionID: "session-other", index: 0, text: "other chunk", sha: "sha-other-1"},
	} {
		_, err := store.db.Exec(`INSERT INTO codex_session_chunks(id,session_id,chunk_index,kind,text,text_sha256) VALUES(?,?,?,?,?,?)`, chunk.id, chunk.sessionID, chunk.index, "message", chunk.text, chunk.sha)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	count, err := EmbedSession(context.Background(), "session-target", "test-model", 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("embedded %d chunks, want 2", count)
	}
	if !reflect.DeepEqual(batches, [][]string{{"first target chunk", "second target chunk"}}) {
		t.Fatalf("batches=%v", batches)
	}

	verify, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var targetCount, otherCount int
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM codex_session_embeddings e JOIN codex_session_chunks c ON c.id=e.chunk_id WHERE c.session_id='session-target'`).Scan(&targetCount); err != nil {
		t.Fatal(err)
	}
	if err := verify.db.QueryRow(`SELECT COUNT(*) FROM codex_session_embeddings e JOIN codex_session_chunks c ON c.id=e.chunk_id WHERE c.session_id='session-other'`).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if targetCount != 2 || otherCount != 0 {
		t.Fatalf("target embeddings=%d other embeddings=%d, want 2 and 0", targetCount, otherCount)
	}
}

func TestEmbedSessionAdvancesEmbeddingCursorWhenBacklogIsClear(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	originalEmbedTexts := embedTexts
	t.Cleanup(func() { embedTexts = originalEmbedTexts })
	embedTexts = func(ctx context.Context, model string, texts []string) ([][]float64, error) {
		vecs := make([][]float64, len(texts))
		for i := range texts {
			vecs[i] = []float64{float64(i + 1), 0.5}
		}
		return vecs, nil
	}

	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	now := "2026-06-10T12:00:00Z"
	if _, err := store.db.Exec(`INSERT INTO codex_sessions(id,machine,title,indexed_at,updated_at) VALUES(?,?,?,?,?)`, "session-target", "test", "Target", now, now); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	for _, chunk := range []struct {
		id    string
		index int
		text  string
		sha   string
	}{
		{id: "target-1", index: 0, text: "first target chunk", sha: "sha-target-1"},
		{id: "target-2", index: 1, text: "second target chunk", sha: "sha-target-2"},
	} {
		_, err := store.db.Exec(`INSERT INTO codex_session_chunks(id,session_id,chunk_index,kind,text,text_sha256) VALUES(?,?,?,?,?,?)`, chunk.id, "session-target", chunk.index, "message", chunk.text, chunk.sha)
		if err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	count, err := EmbedSession(context.Background(), "", "test-model", 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("embedded %d chunks, want 2", count)
	}

	verify, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	cursor := verify.embeddingCursor("test-model")
	if cursor.IsZero() {
		t.Fatalf("expected embedding cursor to be advanced")
	}
	backlog, err := verify.embeddingBacklog("test-model")
	if err != nil {
		t.Fatal(err)
	}
	if backlog != 0 {
		t.Fatalf("backlog=%d, want 0", backlog)
	}
}

func TestRelatedRanksRepoAndFileMatches(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	// Relative to time.Now(), not a fixed calendar date: recencyScore's own
	// 30-day cutoff (search.go) means a hardcoded absolute timestamp here
	// decays past that cutoff as real time passes, dropping "other"'s score
	// to exactly 0 (it has no repo/file/query match, only recency) and
	// filtering it out of the results entirely — degenerating this test's
	// two-result comparison into comparing "target" against itself. Found
	// 2026-07-12: a fixed 2026-06-10 date was 32 real days old and already
	// past the cutoff.
	now := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	insertSessionForRelatedTest(t, store, "target", "/repo", "Fix auth file", []string{"src/auth.go"}, []string{"go test ./..."}, now)
	insertSessionForRelatedTest(t, store, "other", "/other", "Unrelated work", []string{"README.md"}, []string{"npm test"}, now)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	results, err := Related(RelatedOptions{RepoRoot: "/repo", Files: []string{"src/auth.go"}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected related results")
	}
	if results[0].ID != "target" {
		t.Fatalf("expected target first, got %#v", results)
	}
	if results[0].Score <= results[len(results)-1].Score {
		t.Fatalf("expected target score to lead, got %#v", results)
	}
}

func insertSessionForRelatedTest(t *testing.T, store *Store, id, cwd, title string, files, commands []string, updatedAt string) {
	t.Helper()
	j := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	_, err := store.db.Exec(`INSERT INTO codex_sessions(id,machine,title,first_user_message,last_agent_message,cwd,source,model_provider,model,cli_version,git_branch,git_origin_url,created_at,updated_at,indexed_at,tokens_used,status,rollout_path,rollout_sha256,event_counts_json,files_touched_json,commands_json,tool_names_json,errors_json,metadata_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "test", title, title, "", cwd, "codex", "openai", "gpt", "dev", "main", "", updatedAt, updatedAt, updatedAt, 0, "complete", "", "", "{}", j(files), j(commands), "[]", "[]", "{}")
	if err != nil {
		t.Fatal(err)
	}
}
