package sessionmemory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// createDevinFixtureDB writes a minimal sessions.db with the tables the
// indexer reads, using the same modernc sqlite driver the store opens it
// with.
func createDevinFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			working_directory TEXT NOT NULL,
			backend_type TEXT NOT NULL,
			model TEXT NOT NULL,
			agent_mode TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_activity_at INTEGER NOT NULL,
			title TEXT,
			hidden INTEGER NOT NULL DEFAULT 0,
			metadata TEXT
		)`,
		`CREATE TABLE message_nodes (
			row_id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			node_id INTEGER NOT NULL,
			parent_node_id INTEGER,
			chat_message TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			metadata TEXT
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func insertDevinFixtureSession(t *testing.T, dbPath, id, workdir string, lastActivity time.Time, hidden int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO sessions(id,working_directory,backend_type,model,agent_mode,created_at,last_activity_at,title,hidden,metadata)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, workdir, "cli", "SWE-2", "plan", lastActivity.Add(-time.Hour).Unix(), lastActivity.Unix(), "fixture "+id, hidden, `{"total_acu_cost":1.5}`)
	if err != nil {
		t.Fatal(err)
	}
}

func insertDevinFixtureMessage(t *testing.T, dbPath, sessionID string, nodeID int64, chatMessage string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES(?,?,?,?)`,
		sessionID, nodeID, chatMessage, at.Unix())
	if err != nil {
		t.Fatal(err)
	}
}

func TestIndexDevinSessions(t *testing.T) {
	devinDB := createDevinFixtureDB(t)
	old := time.Now().Add(-2 * time.Hour)
	insertDevinFixtureSession(t, devinDB, "sess-1", "/repo", old, 0)
	insertDevinFixtureSession(t, devinDB, "sess-hidden", "/repo", old, 1)
	insertDevinFixtureMessage(t, devinDB, "sess-1", 1, `{"message_id":"u1","role":"user","content":"add a health endpoint","metadata":{"is_user_input":true}}`, old)
	insertDevinFixtureMessage(t, devinDB, "sess-1", 2, `{"message_id":"u2","role":"user","content":"<system-rules>injected</system-rules>","metadata":{"is_user_input":false}}`, old.Add(time.Second))
	insertDevinFixtureMessage(t, devinDB, "sess-1", 3, `{"message_id":"a1","role":"assistant","content":"I'll add it","tool_calls":[{"id":"c1","name":"exec","arguments":{"command":"go build ./..."}}],"metadata":{"finish_reason":"tool_calls"}}`, old.Add(2*time.Second))
	insertDevinFixtureMessage(t, devinDB, "sess-1", 4, `{"message_id":"t1","role":"tool","content":"build ok","tool_call_id":"c1","metadata":{"extensions":{"chisel/tool_result_meta":{"success":true}}}}`, old.Add(3*time.Second))
	insertDevinFixtureMessage(t, devinDB, "sess-1", 5, `{"message_id":"a2","role":"assistant","content":"health endpoint added","metadata":{"finish_reason":"stop"}}`, old.Add(4*time.Second))
	insertDevinFixtureMessage(t, devinDB, "sess-hidden", 1, `{"message_id":"h1","role":"user","content":"hidden prompt","metadata":{"is_user_input":true}}`, old)

	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	count, err := Index(context.Background(), Options{DBPath: storeDB, DevinDBPath: devinDB, Provider: "devin", Force: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("indexed %d sessions, want 1 (hidden excluded)", count)
	}

	sess, messages, err := ShowPath(storeDB, "sess-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Source != "devin" || sess.ModelProvider != "devin" || sess.Model != "SWE-2" {
		t.Fatalf("session metadata not mapped: %+v", sess)
	}
	if sess.CWD != "/repo" || sess.Title != "fixture sess-1" {
		t.Fatalf("session fields not mapped: %+v", sess)
	}
	if sess.FirstUserMessage != "add a health endpoint" {
		t.Fatalf("first user message = %q (injected context must not count)", sess.FirstUserMessage)
	}
	if sess.LastAgentMessage != "health endpoint added" {
		t.Fatalf("last agent message = %q", sess.LastAgentMessage)
	}
	if len(sess.ToolNames) != 1 || sess.ToolNames[0] != "exec" {
		t.Fatalf("tool names = %v", sess.ToolNames)
	}
	if len(sess.Commands) != 1 || sess.Commands[0] != "go build ./..." {
		t.Fatalf("commands = %v", sess.Commands)
	}
	// user message, two assistant messages, exec tool call, tool result.
	if len(messages) != 5 {
		t.Fatalf("messages = %+v", messages)
	}
	for _, m := range messages {
		if m.Role == "user" && m.Text != "add a health endpoint" {
			t.Fatalf("non-input user content leaked into messages: %+v", m)
		}
	}
}

func TestIndexDevinSessionsSkipsUnchanged(t *testing.T) {
	devinDB := createDevinFixtureDB(t)
	old := time.Now().Add(-2 * time.Hour)
	insertDevinFixtureSession(t, devinDB, "sess-1", "/repo", old, 0)
	insertDevinFixtureMessage(t, devinDB, "sess-1", 1, `{"message_id":"u1","role":"user","content":"first","metadata":{"is_user_input":true}}`, old)

	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	opts := Options{DBPath: storeDB, DevinDBPath: devinDB, Provider: "devin"}
	count, err := Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("first index = %d, want 1", count)
	}
	// Nothing changed: same fingerprint → skipped without re-parse.
	count, err = Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unchanged reindex = %d, want 0", count)
	}
	// A new message bumps the fingerprint → re-indexed.
	insertDevinFixtureMessage(t, devinDB, "sess-1", 2, `{"message_id":"a1","role":"assistant","content":"answered","metadata":{"finish_reason":"stop"}}`, old.Add(time.Minute))
	count, err = Index(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reindex after new message = %d, want 1", count)
	}
	sess, _, err := ShowPath(storeDB, "sess-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if sess.LastAgentMessage != "answered" {
		t.Fatalf("last agent message not refreshed: %q", sess.LastAgentMessage)
	}
}

func TestIndexDevinSessionsSkipsRecentActivity(t *testing.T) {
	devinDB := createDevinFixtureDB(t)
	// last_activity inside the quiet period → treated as still being written.
	insertDevinFixtureSession(t, devinDB, "sess-live", "/repo", time.Now().Add(-10*time.Second), 0)
	insertDevinFixtureMessage(t, devinDB, "sess-live", 1, `{"message_id":"u1","role":"user","content":"working","metadata":{"is_user_input":true}}`, time.Now())

	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	count, err := Index(context.Background(), Options{DBPath: storeDB, DevinDBPath: devinDB, Provider: "devin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("active session should be skipped without --force, got %d", count)
	}
	count, err = Index(context.Background(), Options{DBPath: storeDB, DevinDBPath: devinDB, Provider: "devin", Force: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("forced index = %d, want 1", count)
	}
}

func TestIndexDevinSessionsMissingDB(t *testing.T) {
	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	count, err := Index(context.Background(), Options{DBPath: storeDB, DevinDBPath: filepath.Join(t.TempDir(), "absent.db"), Provider: "devin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("missing database should index 0, got %d", count)
	}
}

func TestIndexDevinSessionsProviderValidation(t *testing.T) {
	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	if _, err := Index(context.Background(), Options{DBPath: storeDB, Provider: "devin", DevinDBPath: filepath.Join(t.TempDir(), "absent.db")}, nil); err != nil {
		t.Fatalf("devin must be an accepted provider: %v", err)
	}
	if _, err := Index(context.Background(), Options{DBPath: storeDB, Provider: "bogus"}, nil); err == nil {
		t.Fatal("expected unsupported provider error")
	}
}

func TestDevinSessionACU(t *testing.T) {
	if got := devinSessionACU(`{"total_acu_cost":2.75}`); got != 2.75 {
		t.Fatalf("acu = %v", got)
	}
	if got := devinSessionACU(`{"other":1}`); got != 0 {
		t.Fatalf("acu = %v", got)
	}
	if got := devinSessionACU("not json"); got != 0 {
		t.Fatalf("acu = %v", got)
	}
}

func TestDevinToolFailed(t *testing.T) {
	if !devinToolFailed(map[string]any{"chisel/tool_result_meta": map[string]any{"success": false}}) {
		t.Fatal("success=false must report failed")
	}
	if devinToolFailed(map[string]any{"chisel/tool_result_meta": map[string]any{"success": true}}) {
		t.Fatal("success=true must not report failed")
	}
	if devinToolFailed(nil) {
		t.Fatal("missing extension must not report failed")
	}
}
