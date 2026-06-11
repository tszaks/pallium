package sessionmemory

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	count, err := Index(context.Background(), Options{DBPath: dbPath, ClaudeHome: claudeHome, Provider: "claude", Machine: "test-host"}, nil)
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
