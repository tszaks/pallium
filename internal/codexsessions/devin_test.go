package codexsessions

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// devinFixtureDB builds a minimal sessions.db fixture through the same
// sqlite3 CLI the collector shells out to. Tests skip when the binary is
// absent (the collector itself would then report the provider unavailable).
func devinFixtureDB(t *testing.T, stmts ...string) string {
	t.Helper()
	if _, err := exec.LookPath(sqlite3Command); err != nil {
		t.Skip("sqlite3 not available")
	}
	path := filepath.Join(t.TempDir(), "sessions.db")
	schema := `
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  working_directory TEXT NOT NULL,
  backend_type TEXT NOT NULL,
  model TEXT NOT NULL,
  agent_mode TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_activity_at INTEGER NOT NULL,
  title TEXT,
  main_chain_id INTEGER,
  shell_last_seen_index INTEGER DEFAULT 0,
  cogs_json TEXT,
  workspace_dirs TEXT,
  hidden INTEGER NOT NULL DEFAULT 0,
  metadata TEXT
);
CREATE TABLE prompt_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  content TEXT NOT NULL,
  timestamp INTEGER NOT NULL,
  session_id TEXT NOT NULL,
  is_shell INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE message_nodes (
  row_id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  node_id INTEGER NOT NULL,
  parent_node_id INTEGER,
  chat_message TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  metadata TEXT
);
`
	for _, stmt := range append([]string{schema}, stmts...) {
		if out, err := exec.Command(sqlite3Command, path, stmt).CombinedOutput(); err != nil {
			t.Fatalf("fixture statement failed: %v: %s", err, out)
		}
	}
	return path
}

func stubDevinDiscovery(t *testing.T, dbPath string, processes []liveAgentProcess) {
	t.Helper()
	originalDB := devinDBPathFunc
	originalProcesses := listLiveDevinProcessesVar
	t.Cleanup(func() {
		devinDBPathFunc = originalDB
		listLiveDevinProcessesVar = originalProcesses
	})
	devinDBPathFunc = func() (string, error) { return dbPath, nil }
	listLiveDevinProcessesVar = func(context.Context) ([]liveAgentProcess, error) { return processes, nil }
}

func TestLooksLikeDevinCommand(t *testing.T) {
	for _, command := range []string{"devin", "/usr/local/bin/devin", `C:\tools\devin.exe`} {
		if !looksLikeDevinCommand(command) {
			t.Fatalf("expected %q to match", command)
		}
	}
	for _, command := range []string{"", "devin-server", "mydevin", "codex"} {
		if looksLikeDevinCommand(command) {
			t.Fatalf("expected %q to not match", command)
		}
	}
}

func TestCollectDevinSessionsMatchesLiveProcess(t *testing.T) {
	generatedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	now := generatedAt.Unix()
	userMsg := `{"message_id":"m1","role":"user","content":"review the repo","metadata":{"is_user_input":true}}`
	agentMsg := `{"message_id":"m2","role":"assistant","content":"done","metadata":{"is_user_input":false,"finish_reason":"stop"}}`
	dbPath := devinFixtureDB(t,
		fmt.Sprintf(`INSERT INTO sessions(id,working_directory,backend_type,model,agent_mode,created_at,last_activity_at,title,hidden) VALUES('sess-a','/repo','cli','SWE-2','plan',%d,%d,'Review session',0);`, now-3600, now-30),
		fmt.Sprintf(`INSERT INTO sessions(id,working_directory,backend_type,model,agent_mode,created_at,last_activity_at,title,hidden) VALUES('sess-hidden','/repo','cli','SWE-2','plan',%d,%d,'hidden',1);`, now-3600, now-30),
		fmt.Sprintf(`INSERT INTO prompt_history(content,timestamp,session_id,is_shell) VALUES('review the repo',%d,'sess-a',0);`, now-60),
		fmt.Sprintf(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES('sess-a',1,'%s',%d);`, userMsg, now-60),
		fmt.Sprintf(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES('sess-a',2,'%s',%d);`, agentMsg, now-30),
	)
	stubDevinDiscovery(t, dbPath, []liveAgentProcess{
		{Provider: providerDevin, PID: 42, TTY: "ttys001", AgeSeconds: 3600, CWD: "/repo"},
	})

	sessions, err := collectDevinSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 visible session (hidden excluded), got %+v", sessions)
	}
	s := sessions[0]
	if s.ThreadID != "sess-a" || s.Provider != providerDevin || s.PID != 42 {
		t.Fatalf("session not matched to live process: %+v", s)
	}
	if s.Status != finishedSessionStatus || s.CompletionStatus != "finished" {
		t.Fatalf("finished assistant turn should classify finished: %+v", s)
	}
	if s.SessionCWD != "/repo" || s.FirstUserMessage != "review the repo" || s.Title != "Review session" {
		t.Fatalf("metadata not mapped: %+v", s)
	}
}

func TestCollectDevinSessionsClassifiesPendingTool(t *testing.T) {
	generatedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	now := generatedAt.Unix()
	userMsg := `{"message_id":"m1","role":"user","content":"ask me something","metadata":{"is_user_input":true}}`
	agentMsg := `{"message_id":"m2","role":"assistant","content":"","tool_calls":[{"id":"call-1","name":"AskUserQuestion","arguments":{"message":"which one?"}}],"metadata":{"finish_reason":"tool_calls"}}`
	dbPath := devinFixtureDB(t,
		fmt.Sprintf(`INSERT INTO sessions(id,working_directory,backend_type,model,agent_mode,created_at,last_activity_at,title) VALUES('sess-b','/repo','cli','SWE-2','plan',%d,%d,NULL);`, now-3600, now-30),
		fmt.Sprintf(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES('sess-b',1,'%s',%d);`, userMsg, now-60),
		fmt.Sprintf(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES('sess-b',2,'%s',%d);`, agentMsg, now-30),
	)
	stubDevinDiscovery(t, dbPath, []liveAgentProcess{
		{Provider: providerDevin, PID: 43, TTY: "ttys002", AgeSeconds: 3600, CWD: "/repo"},
	})

	sessions, err := collectDevinSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Status != blockedSessionStatus {
		t.Fatalf("pending ask-user tool should classify blocked: %+v", sessions)
	}
	if sessions[0].RecentAction == "" {
		t.Fatalf("expected a recent action for the pending tool call: %+v", sessions[0])
	}
}

func TestCollectDevinSessionsDedupesRecommittedMessages(t *testing.T) {
	// The same message_id committed twice (streaming re-commit): only the
	// newest row counts, so a stale tool_calls row can't leave a phantom
	// pending tool.
	generatedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	now := generatedAt.Unix()
	userMsg := `{"message_id":"m1","role":"user","content":"go","metadata":{"is_user_input":true}}`
	streaming := `{"message_id":"m2","role":"assistant","content":"","tool_calls":[{"id":"call-1","name":"exec","arguments":{"command":"ls"}}],"metadata":{"finish_reason":"tool_calls"}}`
	finished := `{"message_id":"m2","role":"assistant","content":"all done","metadata":{"finish_reason":"stop"}}`
	toolResult := `{"message_id":"m3","role":"tool","content":"file.go","tool_call_id":"call-1","metadata":{}}`
	dbPath := devinFixtureDB(t,
		fmt.Sprintf(`INSERT INTO sessions(id,working_directory,backend_type,model,agent_mode,created_at,last_activity_at) VALUES('sess-c','/repo','cli','SWE-2','plan',%d,%d);`, now-3600, now-30),
		fmt.Sprintf(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES('sess-c',1,'%s',%d);`, userMsg, now-90),
		fmt.Sprintf(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES('sess-c',2,'%s',%d);`, streaming, now-60),
		fmt.Sprintf(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES('sess-c',3,'%s',%d);`, toolResult, now-50),
		fmt.Sprintf(`INSERT INTO message_nodes(session_id,node_id,chat_message,created_at) VALUES('sess-c',2,'%s',%d);`, finished, now-30),
	)
	stubDevinDiscovery(t, dbPath, []liveAgentProcess{
		{Provider: providerDevin, PID: 44, TTY: "ttys003", AgeSeconds: 3600, CWD: "/repo"},
	})

	sessions, err := collectDevinSessions(context.Background(), SessionCollectOptions{}, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Status != finishedSessionStatus {
		t.Fatalf("re-committed finished message should classify finished: %+v", sessions)
	}
}

func TestCollectDevinSessionsNoProcessNoAllReturnsEmpty(t *testing.T) {
	dbPath := devinFixtureDB(t,
		`INSERT INTO sessions(id,working_directory,backend_type,model,agent_mode,created_at,last_activity_at) VALUES('sess-d','/repo','cli','SWE-2','plan',1,2);`,
	)
	stubDevinDiscovery(t, dbPath, nil)
	sessions, err := collectDevinSessions(context.Background(), SessionCollectOptions{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected no sessions without a live process or --all: %+v", sessions)
	}
}

func TestCollectDevinSessionsMissingDBFallsBackToProcess(t *testing.T) {
	stubDevinDiscovery(t, filepath.Join(t.TempDir(), "absent.db"), []liveAgentProcess{
		{Provider: providerDevin, PID: 99, TTY: "ttys009", AgeSeconds: 60, CWD: "/repo"},
	})
	sessions, err := collectDevinSessions(context.Background(), SessionCollectOptions{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].PID != 99 || sessions[0].Title != startingSessionTitle {
		t.Fatalf("expected a starting-up session from the live process: %+v", sessions)
	}
}

func TestNormalizeDevinWorkdirResolvesSymlinks(t *testing.T) {
	tmp := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(tmp, link); err != nil {
		t.Skip(err)
	}
	want, _ := filepath.EvalSymlinks(tmp)
	if got := normalizeDevinWorkdir(link); got != want {
		t.Fatalf("normalizeDevinWorkdir = %q, want %q", got, want)
	}
}
