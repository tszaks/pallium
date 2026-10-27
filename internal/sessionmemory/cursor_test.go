package sessionmemory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cursorFixtureRoot builds a synthetic ~/.cursor tree. All text is invented;
// only the line shapes mirror Cursor's real transcripts.
func cursorFixtureRoot(t *testing.T) (root, workdir, slug string) {
	t.Helper()
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	workdir = filepath.Join(tmp, "work", "demo-app")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	slug = strings.ReplaceAll(strings.TrimPrefix(workdir, "/"), "/", "-")
	root = filepath.Join(tmp, ".cursor")
	return root, workdir, slug
}

func writeCursorChat(t *testing.T, root, slug, id string, lines []string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, "projects", slug, "agent-transcripts", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

var cursorChatLines = []string{
	`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Tuesday, Oct 27, 2026, 2:50 PM (UTC-4)</timestamp>\n<user_query>\nAdd a gizmo endpoint to the demo server\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"[REDACTED]"},{"type":"tool_use","name":"Shell","input":{"command":"go build ./...","description":"Build"}}]}}`,
	`this line is not json`,
	`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/srv/demo/gizmo.go","old_string":"a","new_string":"b"}}]}}`,
	`{"type":"mystery","status":"?"}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Gizmo endpoint added and the build passes."}]}}`,
	`{"type":"turn_ended","status":"success"}`,
}

func TestIndexCursorChats(t *testing.T) {
	root, workdir, slug := cursorFixtureRoot(t)
	id := "d1d1d1d1-0000-4000-8000-000000000001"
	writeCursorChat(t, root, slug, id, cursorChatLines, time.Now().Add(-time.Hour))
	subDir := filepath.Join(root, "projects", slug, "agent-transcripts", id, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subLines := `{"role":"user","message":{"content":[{"type":"text","text":"delegated prompt that must not become the first message"}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/srv/demo/helper.go"}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "sub1.jsonl"), []byte(subLines), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(subDir, "sub1.jsonl"), old, old)

	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	count, err := Index(context.Background(), Options{DBPath: storeDB, CursorRoot: root, Provider: "cursor", Force: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("indexed %d chats, want 1", count)
	}
	sess, messages, err := ShowPath(storeDB, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Source != "cursor" || sess.ModelProvider != "cursor" || sess.CWD != workdir {
		t.Fatalf("session metadata: %+v", sess)
	}
	if sess.FirstUserMessage != "Add a gizmo endpoint to the demo server" {
		t.Fatalf("first user message = %q", sess.FirstUserMessage)
	}
	if sess.LastAgentMessage != "Gizmo endpoint added and the build passes." {
		t.Fatalf("last agent message = %q", sess.LastAgentMessage)
	}
	if sess.RolloutPath != "cursor://"+slug+"/"+id {
		t.Fatalf("rollout path = %q", sess.RolloutPath)
	}
	wantTools := []string{"Read", "Shell", "StrReplace"}
	if strings.Join(sess.ToolNames, ",") != strings.Join(wantTools, ",") {
		t.Fatalf("tool names = %v", sess.ToolNames)
	}
	if len(sess.Commands) != 1 || sess.Commands[0] != "go build ./..." {
		t.Fatalf("commands = %v", sess.Commands)
	}
	files := strings.Join(sess.FilesTouched, ",")
	if !strings.Contains(files, "/srv/demo/gizmo.go") || !strings.Contains(files, "/srv/demo/helper.go") {
		t.Fatalf("files touched = %v", sess.FilesTouched)
	}
	if !strings.HasPrefix(sess.CreatedAt, "2026-10-27T18:50:00") {
		t.Fatalf("created at = %q (want the user <timestamp> in UTC)", sess.CreatedAt)
	}
	for _, m := range messages {
		if strings.Contains(m.Text, "[REDACTED]") || strings.Contains(m.Text, "delegated prompt") || strings.Contains(m.Text, "<user_query>") {
			t.Fatalf("unwanted text indexed: %+v", m)
		}
	}
	// user + Shell + StrReplace + final assistant + subagent Read.
	if len(messages) != 5 {
		t.Fatalf("messages = %+v", messages)
	}
}

func TestIndexCursorChatsSearchableWithCursorSource(t *testing.T) {
	root, _, slug := cursorFixtureRoot(t)
	writeCursorChat(t, root, slug, "d2d2d2d2-0000-4000-8000-000000000002", cursorChatLines, time.Now().Add(-time.Hour))
	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	if _, err := Index(context.Background(), Options{DBPath: storeDB, CursorRoot: root, Provider: "cursor", Force: true}, nil); err != nil {
		t.Fatal(err)
	}
	results, err := SearchWithOptions(context.Background(), SessionSearchOptions{DBPath: storeDB, Query: "gizmo", Limit: 5, Source: "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].Citation.Source != "cursor" {
		t.Fatalf("results = %+v", results)
	}
	other, err := SearchWithOptions(context.Background(), SessionSearchOptions{DBPath: storeDB, Query: "gizmo", Limit: 5, Source: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("source filter leaked: %+v", other)
	}
}

func TestIndexCursorChatsSkipsUnchangedAndRecent(t *testing.T) {
	root, _, slug := cursorFixtureRoot(t)
	writeCursorChat(t, root, slug, "d3d3d3d3-0000-4000-8000-000000000003", cursorChatLines, time.Now().Add(-time.Hour))
	writeCursorChat(t, root, slug, "d4d4d4d4-0000-4000-8000-000000000004", cursorChatLines, time.Now().Add(-5*time.Second))
	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	opts := Options{DBPath: storeDB, CursorRoot: root, Provider: "cursor"}
	count, err := Index(context.Background(), opts, nil)
	if err != nil || count != 1 {
		t.Fatalf("first index: count=%d err=%v (recent chat must be skipped)", count, err)
	}
	count, err = Index(context.Background(), opts, nil)
	if err != nil || count != 0 {
		t.Fatalf("second index: count=%d err=%v (unchanged must skip)", count, err)
	}
	// Appending to the chat changes the fingerprint.
	path := filepath.Join(root, "projects", slug, "agent-transcripts", "d3d3d3d3-0000-4000-8000-000000000003", "d3d3d3d3-0000-4000-8000-000000000003.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"role":"assistant","message":{"content":[{"type":"text","text":"one more thing"}]}}` + "\n")
	f.Close()
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	opts.Force = false
	count, err = Index(context.Background(), opts, nil)
	if err != nil || count != 1 {
		t.Fatalf("after append: count=%d err=%v", count, err)
	}
}

func TestIndexCursorChatsMissingRoot(t *testing.T) {
	storeDB := filepath.Join(t.TempDir(), "pallium-sessions.sqlite")
	count, err := Index(context.Background(), Options{DBPath: storeDB, CursorRoot: filepath.Join(t.TempDir(), "absent"), Provider: "cursor"}, nil)
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestParseCursorTimestamp(t *testing.T) {
	cases := map[string]string{
		"Tuesday, Oct 27, 2026, 2:50 PM (UTC-4)":     "2026-10-27T18:50:00Z",
		"Monday, Jan 5, 2026, 9:05 AM (UTC+5:30)":    "2026-01-05T03:35:00Z",
		"Tuesday, October 27, 2026, 2:50 PM (UTC-4)": "2026-10-27T18:50:00Z",
	}
	for in, want := range cases {
		got := parseCursorTimestamp(in)
		if got.IsZero() || got.UTC().Format(time.RFC3339) != want {
			t.Errorf("parseCursorTimestamp(%q) = %v, want %s", in, got, want)
		}
	}
	if !parseCursorTimestamp("garbage").IsZero() {
		t.Fatal("garbage must parse to zero")
	}
}

func TestCursorSlugToDir(t *testing.T) {
	_, workdir, slug := cursorFixtureRoot(t)
	if got := cursorSlugToDir(slug); got != workdir {
		t.Fatalf("got %q want %q", got, workdir)
	}
}
