package codexsessions

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cursorFixture builds a synthetic ~/.cursor tree. Transcript text is
// invented; only the line shapes mirror Cursor's real files.
type cursorFixture struct {
	home    string
	workdir string
	slug    string
}

func newCursorFixture(t *testing.T) cursorFixture {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	workdir := filepath.Join(root, "work", "demo-app")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	slug := strings.ReplaceAll(strings.TrimPrefix(workdir, "/"), "/", "-")
	home := filepath.Join(root, ".cursor")
	if err := os.MkdirAll(filepath.Join(home, "projects", slug, "agent-transcripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cursorFixture{home: home, workdir: workdir, slug: slug}
}

func (f cursorFixture) useAsCursorHome(t *testing.T, procs []liveAgentProcess) {
	t.Helper()
	oh, op := cursorHomeDirFunc, listLiveCursorProcessesVar
	t.Cleanup(func() { cursorHomeDirFunc, listLiveCursorProcessesVar = oh, op })
	cursorHomeDirFunc = func() (string, error) { return f.home, nil }
	listLiveCursorProcessesVar = func(context.Context) ([]liveAgentProcess, error) { return procs, nil }
}

func (f cursorFixture) writeChat(t *testing.T, id, body string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(f.home, "projects", f.slug, "agent-transcripts", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f cursorFixture) writeTerminal(t *testing.T, name string, pid int, status string) {
	t.Helper()
	dir := filepath.Join(f.home, "projects", f.slug, "terminals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\npid: " + strconv.Itoa(pid) + "\ncwd: \"" + f.workdir + "\"\ncommand: sleep 300\ntitle: \"Demo long task\"\nstatus: " + status + "\nstarted_at: 2026-01-01T00:00:00.000Z\n---\noutput line\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const cursorUserLine = `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Monday, Jan 1, 2026, 9:00 AM (UTC-5)</timestamp>\n<user_query>\nAdd a demo widget to the settings page\n</user_query>"}]}}` + "\n"
const cursorToolLine = `{"role":"assistant","message":{"content":[{"type":"text","text":"[REDACTED]"},{"type":"tool_use","name":"Shell","input":{"command":"npm test -- widget","description":"Run widget tests"}}]}}` + "\n"
const cursorFinalLine = `{"role":"assistant","message":{"content":[{"type":"text","text":"Widget added and tests pass."}]}}` + "\n"
const cursorTurnEnded = `{"type":"turn_ended","status":"success"}` + "\n"

func findCursorSession(sessions []SessionSummary, id string) *SessionSummary {
	for i := range sessions {
		if sessions[i].ThreadID == id {
			return &sessions[i]
		}
	}
	return nil
}

func TestCursorSlugToPathResolvesHyphenatedDirectories(t *testing.T) {
	f := newCursorFixture(t)
	if got := cursorSlugToPath(f.slug); got != f.workdir {
		t.Fatalf("cursorSlugToPath = %q, want %q", got, f.workdir)
	}
	if got := cursorSlugToPath("no-such-place-here"); got != "/no/such/place/here" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestCollectCursorSessionsRunningMidTurn(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	f.writeChat(t, "11111111-1111-1111-1111-111111111111", cursorUserLine+cursorToolLine, now.Add(-30*time.Second))
	f.useAsCursorHome(t, nil)
	sessions, err := collectCursorSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	s := findCursorSession(sessions, "11111111-1111-1111-1111-111111111111")
	if s == nil {
		t.Fatalf("session missing: %+v", sessions)
	}
	if s.Provider != "cursor" || s.Status != activeSessionStatus || s.StatusConfidence != "medium" {
		t.Fatalf("status = %s/%s/%s (%s)", s.Provider, s.Status, s.StatusConfidence, s.StatusReason)
	}
	if !strings.Contains(s.StatusReason, "file modification time") {
		t.Fatalf("reason must disclose the clock: %q", s.StatusReason)
	}
	if s.FirstUserMessage != "Add a demo widget to the settings page" || s.SessionCWD != f.workdir {
		t.Fatalf("first=%q cwd=%q", s.FirstUserMessage, s.SessionCWD)
	}
	if s.RecentAction != "Shell: npm test -- widget" {
		t.Fatalf("recent action = %q", s.RecentAction)
	}
}

func TestCollectCursorSessionsTurnEndedIsIdle(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	f.writeChat(t, "22222222-2222-2222-2222-222222222222", cursorUserLine+cursorToolLine+cursorFinalLine+cursorTurnEnded, now.Add(-time.Minute))
	f.useAsCursorHome(t, nil)
	sessions, err := collectCursorSessions(context.Background(), SessionCollectOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	s := findCursorSession(sessions, "22222222-2222-2222-2222-222222222222")
	if s == nil || s.Status != idleSessionStatus || s.CompletionStatus != "finished" {
		t.Fatalf("session = %+v", s)
	}
}

func TestCollectCursorSessionsStaleUnfinishedTurnIsIdleNotActive(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	f.writeChat(t, "33333333-3333-3333-3333-333333333333", cursorUserLine+cursorToolLine, now.Add(-time.Hour))
	f.useAsCursorHome(t, nil)
	sessions, _ := collectCursorSessions(context.Background(), SessionCollectOptions{}, now)
	s := findCursorSession(sessions, "33333333-3333-3333-3333-333333333333")
	if s == nil || s.Status != idleSessionStatus {
		t.Fatalf("session = %+v", s)
	}
}

func TestCollectCursorSessionsOldChatsOnlyWithAll(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	f.writeChat(t, "44444444-4444-4444-4444-444444444444", cursorUserLine+cursorTurnEnded, now.Add(-72*time.Hour))
	f.useAsCursorHome(t, nil)
	live, _ := collectCursorSessions(context.Background(), SessionCollectOptions{}, now)
	if len(live) != 0 {
		t.Fatalf("old chat listed without --all: %+v", live)
	}
	all, _ := collectCursorSessions(context.Background(), SessionCollectOptions{IncludeAll: true}, now)
	s := findCursorSession(all, "44444444-4444-4444-4444-444444444444")
	if s == nil || s.Status != inactiveSessionStatus {
		t.Fatalf("session = %+v", s)
	}
}

func TestCollectCursorSessionsRunningTerminalMakesChatActive(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	// Finished chat, but the workspace has a live background command.
	f.writeChat(t, "55555555-5555-5555-5555-555555555555", cursorUserLine+cursorFinalLine+cursorTurnEnded, now.Add(-20*time.Minute))
	// Older chat in the same workspace must NOT get the terminal.
	f.writeChat(t, "66666666-6666-6666-6666-666666666666", cursorUserLine+cursorFinalLine+cursorTurnEnded, now.Add(-3*time.Hour))
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Skip("cannot start helper process")
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _, _ = sleeper.Process.Wait() })
	f.writeTerminal(t, "1.txt", sleeper.Process.Pid, "running")
	f.useAsCursorHome(t, nil)
	sessions, _ := collectCursorSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, now)
	s := findCursorSession(sessions, "55555555-5555-5555-5555-555555555555")
	if s == nil || s.Status != activeSessionStatus || !strings.Contains(s.RecentAction, "terminal running: Demo long task") || !strings.Contains(s.RecentAction, "sleep 300") {
		t.Fatalf("session = %+v", s)
	}
	other := findCursorSession(sessions, "66666666-6666-6666-6666-666666666666")
	if other != nil && other.Status == activeSessionStatus {
		t.Fatalf("terminal credited to the wrong chat: %+v", other)
	}
}

func TestCollectCursorSessionsIgnoresDeadAndFinishedTerminals(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	f.writeChat(t, "77777777-7777-7777-7777-777777777777", cursorUserLine+cursorFinalLine+cursorTurnEnded, now.Add(-20*time.Minute))
	f.writeTerminal(t, "1.txt", 2147483000, "running") // pid that does not exist
	f.writeTerminal(t, "2.txt", os.Getpid(), "exited")
	f.useAsCursorHome(t, nil)
	sessions, _ := collectCursorSessions(context.Background(), SessionCollectOptions{}, now)
	s := findCursorSession(sessions, "77777777-7777-7777-7777-777777777777")
	if s == nil || s.Status != idleSessionStatus {
		t.Fatalf("session = %+v", s)
	}
}

func TestCollectCursorSessionsSubagentActivityCountsForParent(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	id := "88888888-8888-8888-8888-888888888888"
	f.writeChat(t, id, cursorUserLine+cursorFinalLine+cursorTurnEnded, now.Add(-2*time.Hour))
	sub := filepath.Join(f.home, "projects", f.slug, "agent-transcripts", id, "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	subFile := filepath.Join(sub, "99999999-9999-9999-9999-999999999999.jsonl")
	if err := os.WriteFile(subFile, []byte(cursorUserLine+cursorToolLine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(subFile, now.Add(-10*time.Second), now.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	f.useAsCursorHome(t, nil)
	sessions, _ := collectCursorSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, now)
	if len(sessions) != 1 {
		t.Fatalf("subagent must not be its own session: %+v", sessions)
	}
	if sessions[0].ThreadID != id || sessions[0].Status != activeSessionStatus || sessions[0].RecentAction != "Shell: npm test -- widget" {
		t.Fatalf("session = %+v", sessions[0])
	}
}

func TestCollectCursorSessionsSurvivesGarbageAndMissingHome(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	f.writeChat(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "not json\n{\"role\":\n{\"type\":\"mystery\"}\n[1,2]\n"+cursorUserLine, now.Add(-time.Minute))
	f.writeChat(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "", now.Add(-time.Minute))
	f.useAsCursorHome(t, nil)
	if _, err := collectCursorSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, now); err != nil {
		t.Fatal(err)
	}
	missing := cursorFixture{home: filepath.Join(t.TempDir(), "nope")}
	missing.useAsCursorHome(t, nil)
	sessions, err := collectCursorSessions(context.Background(), SessionCollectOptions{}, now)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("missing home: %v %+v", err, sessions)
	}
}

func TestCollectCursorSessionsMatchesLiveCLIProcess(t *testing.T) {
	f := newCursorFixture(t)
	now := time.Now()
	id := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	f.writeChat(t, id, cursorUserLine+cursorToolLine, now.Add(-5*time.Second))
	proc := liveAgentProcess{Provider: providerCursor, PID: 4242, PPID: 1, TTY: "ttys001", State: "S", AgeSeconds: 600, CWD: f.workdir}
	f.useAsCursorHome(t, []liveAgentProcess{proc})
	sessions, _ := collectCursorSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, now)
	s := findCursorSession(sessions, id)
	if s == nil || s.PID != 4242 || s.Status != activeSessionStatus {
		t.Fatalf("session = %+v", s)
	}
}

func TestParseCursorTerminalHeader(t *testing.T) {
	f := newCursorFixture(t)
	f.writeTerminal(t, "7.txt", os.Getpid(), "running")
	path := filepath.Join(f.home, "projects", f.slug, "terminals", "7.txt")
	term, ok := parseCursorTerminalHeader(path)
	if !ok || term.PID != os.Getpid() || term.Title != "Demo long task" || term.Command != "sleep 300" {
		t.Fatalf("term = %+v ok=%v", term, ok)
	}
	if _, ok := parseCursorTerminalHeader(filepath.Join(f.home, "missing.txt")); ok {
		t.Fatal("missing file must not parse")
	}
	bad := filepath.Join(f.home, "bad.txt")
	_ = os.WriteFile(bad, []byte("no header here\npid: 1\nstatus: running\n"), 0o644)
	if _, ok := parseCursorTerminalHeader(bad); ok {
		t.Fatal("headerless file must not parse")
	}
}

func TestLooksLikeCursorCommand(t *testing.T) {
	yes := []string{
		"cursor-agent",
		"/Users/x/.local/bin/cursor-agent",
		"/Users/x/.local/share/cursor-agent/versions/2026.10.22-abc/node",
	}
	no := []string{
		"",
		"node",
		"/usr/local/bin/node",
		"/Users/x/Library/Application Support/Cursor/User/globalStorage/anysphere.cursor-agent-worker/agent-cli/.local/bin/cursor-agent",
		"/Applications/Cursor.app/Contents/MacOS/Cursor",
		"cursor",
	}
	for _, c := range yes {
		if !looksLikeCursorCommand(c) {
			t.Errorf("%q should match", c)
		}
	}
	for _, c := range no {
		if looksLikeCursorCommand(c) {
			t.Errorf("%q should not match", c)
		}
	}
}
