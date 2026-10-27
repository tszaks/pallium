package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setCursorCLI(t *testing.T, path string) {
	t.Helper()
	old := cursorCLIName
	cursorCLIName = path
	t.Cleanup(func() { cursorCLIName = old })
}

func writeFakeCursor(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cursor-agent")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setCursorCLI(t, path)
	return path
}

// cursorFixturePath returns a recorded stream under testdata/cursor.
func cursorFixturePath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "cursor", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatal(err)
	}
	return abs
}

// writeFakeCursorReplay makes a fake cursor-agent that drains stdin and
// replays a recorded stream, optionally logging its argv.
func writeFakeCursorReplay(t *testing.T, fixture, argvLog string) {
	t.Helper()
	script := "#!/bin/sh\ncat >/dev/null\n"
	if argvLog != "" {
		script += "echo \"$@\" >> '" + argvLog + "'\n"
	}
	script += "cat '" + cursorFixturePath(t, fixture) + "'\n"
	writeFakeCursor(t, script)
}

func TestBuildCursorArgsReadOnlyIsAskMode(t *testing.T) {
	for _, mode := range []string{"", "read-only"} {
		got := buildCursorArgs(mode, "", "")
		want := []string{"-p", "--output-format", "stream-json", "--trust", "--mode", "ask"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mode %q: got %v, want %v", mode, got, want)
		}
		for _, a := range got {
			if a == "--force" || a == "--yolo" {
				t.Fatalf("read-only must never carry %s: %v", a, got)
			}
		}
	}
}

func TestBuildCursorArgsEditModesForce(t *testing.T) {
	for _, mode := range []string{"edit", "test", "check"} {
		got := buildCursorArgs(mode, "", "")
		if got[len(got)-1] != "--force" {
			t.Fatalf("mode %q: expected --force, got %v", mode, got)
		}
		for _, a := range got {
			if a == "--mode" {
				t.Fatalf("mode %q must not pass --mode: %v", mode, got)
			}
		}
	}
}

func TestBuildCursorArgsModelAndResumeAndNoPromptInArgv(t *testing.T) {
	got := buildCursorArgs("read-only", "composer-2.5", "chat-abc")
	want := []string{"-p", "--output-format", "stream-json", "--trust", "--resume", "chat-abc", "--model", "composer-2.5", "--mode", "ask"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func readCursorFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(cursorFixturePath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseCursorStreamPlain(t *testing.T) {
	s := parseCursorStream(readCursorFixture(t, "plain"))
	if s.SessionID == "" || s.Text != "PONG" || s.IsError {
		t.Fatalf("stream = %+v", s)
	}
	if s.Usage.InputTokens != 14895 || s.Usage.OutputTokens != 33 {
		t.Fatalf("usage = %+v", s.Usage)
	}
}

func TestParseCursorStreamPrefersLastAssistantOverConcatenatedResult(t *testing.T) {
	s := parseCursorStream(readCursorFixture(t, "ask_write"))
	if strings.HasPrefix(s.Text, "Ask mode blocks edits and shell commands. Checking") {
		t.Fatalf("Text kept mid-turn chatter: %q", s.Text)
	}
	if !strings.HasPrefix(s.Text, "**DONE**") {
		t.Fatalf("Text = %q, want the final assistant message", s.Text)
	}
	if !strings.Contains(s.ResultText, "Checking") {
		t.Fatalf("ResultText should keep the concatenated result: %q", s.ResultText)
	}
}

func TestParseCursorStreamSkipsGarbage(t *testing.T) {
	in := "not json\n{\"type\":\"mystery\",\"x\":1}\n{broken\n\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"x"},{"type":"text","text":"hello"}]},"session_id":"s1"}` + "\n" +
		`{"type":"assistant","message":null}` + "\n"
	s := parseCursorStream(in)
	if s.SessionID != "s1" || s.Text != "hello" {
		t.Fatalf("stream = %+v", s)
	}
	if got := parseCursorStream(""); got.Text != "" || got.SessionID != "" {
		t.Fatalf("empty stream = %+v", got)
	}
}

func TestParseCursorStreamResultFallback(t *testing.T) {
	s := parseCursorStream(`{"type":"result","subtype":"success","is_error":false,"result":"only result","session_id":"s2"}`)
	if s.Text != "only result" || s.SessionID != "s2" {
		t.Fatalf("stream = %+v", s)
	}
}

func TestExtractCursorOutputSchemaFenceAndProse(t *testing.T) {
	got, err := extractCursorOutput(cursorStream{Text: "```json\n{\"ok\":true}\n```"}, true)
	if err != nil || got != `{"ok":true}` {
		t.Fatalf("got %q err %v", got, err)
	}
	got, err = extractCursorOutput(cursorStream{Text: "Here you go: {\"ok\":true} done"}, true)
	if err != nil || got != `{"ok":true}` {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := extractCursorOutput(cursorStream{}, false); err == nil {
		t.Fatal("empty output must be an error")
	}
	if _, err := extractCursorOutput(cursorStream{IsError: true}, false); err == nil {
		t.Fatal("error result with no text must be an error")
	}
}

func TestRunBuiltinCursorCommandReplaysRecordingAndWritesUsage(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	writeFakeCursorReplay(t, "plain", argvLog)
	usageFile := filepath.Join(t.TempDir(), "usage.json")
	r := &Runner{}
	agent := &Agent{ID: "a1", Mode: "read-only", Provider: "cursor"}
	got, err := r.runBuiltinCursorCommand(context.Background(), usageFile, t.TempDir(), "say pong", agent, AgentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "PONG" {
		t.Fatalf("output = %q", got)
	}
	raw, err := os.ReadFile(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatal(err)
	}
	if usage["input_tokens"] != float64(14895) || usage["output_tokens"] != float64(33) {
		t.Fatalf("usage = %v", usage)
	}
	if _, ok := usage["cost_usd"]; ok {
		t.Fatalf("cursor must not report cost_usd: %v", usage)
	}
	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "--mode ask") || strings.Contains(string(argv), "say pong") {
		t.Fatalf("argv = %q (want --mode ask and no prompt text)", argv)
	}
}

func TestRunBuiltinCursorCommandPromptGoesOnStdin(t *testing.T) {
	stdinCopy := filepath.Join(t.TempDir(), "stdin.txt")
	writeFakeCursor(t, "#!/bin/sh\ncat > '"+stdinCopy+"'\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"ok\",\"session_id\":\"s\"}'\n")
	r := &Runner{}
	agent := &Agent{ID: "a1", Mode: "edit", Provider: "cursor"}
	if _, err := r.runBuiltinCursorCommand(context.Background(), filepath.Join(t.TempDir(), "u.json"), t.TempDir(), "unique-prompt-text", agent, AgentOptions{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(stdinCopy)
	if !strings.Contains(string(raw), "unique-prompt-text") {
		t.Fatalf("stdin = %q", raw)
	}
}

func TestRunBuiltinCursorCommandSchemaOutput(t *testing.T) {
	writeFakeCursor(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"```json\\n{\\\"n\\\":1}\\n```\"}]},\"session_id\":\"s\"}'\n")
	r := &Runner{}
	schema := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}}
	got, err := r.runBuiltinCursorCommand(context.Background(), filepath.Join(t.TempDir(), "u.json"), t.TempDir(), "q", &Agent{ID: "a", Mode: "read-only"}, AgentOptions{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"n":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestRunBuiltinCursorCommandFailureIsReported(t *testing.T) {
	writeFakeCursor(t, "#!/bin/sh\ncat >/dev/null\necho 'auth required' >&2\nexit 3\n")
	r := &Runner{}
	_, err := r.runBuiltinCursorCommand(context.Background(), filepath.Join(t.TempDir(), "u.json"), t.TempDir(), "q", &Agent{ID: "a", Mode: "read-only"}, AgentOptions{})
	if err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunBuiltinCursorCommandReadOnlyRefusalPassesThrough(t *testing.T) {
	// The recorded ask-mode run: the model reports that neither the edit
	// nor the shell command ran. Pallium relays it; the fail-closed part is
	// that --mode ask is on argv and --force is not.
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	writeFakeCursorReplay(t, "ask_write", argvLog)
	r := &Runner{}
	got, err := r.runBuiltinCursorCommand(context.Background(), filepath.Join(t.TempDir(), "u.json"), t.TempDir(), "append a line", &Agent{ID: "a", Mode: "read-only"}, AgentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Neither action succeeded") {
		t.Fatalf("got %q", got)
	}
	argv, _ := os.ReadFile(argvLog)
	if strings.Contains(string(argv), "--force") || !strings.Contains(string(argv), "--mode ask") {
		t.Fatalf("argv = %q", argv)
	}
}

func TestRunCursorTeamTurnCapturesSessionAndResumes(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	writeFakeCursorReplay(t, "plain", argvLog)
	r := &Runner{}
	out, token, usage, err := r.runCursorTeamTurn(context.Background(), "read-only", "", "", t.TempDir(), "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "PONG" || token == "" {
		t.Fatalf("out=%q token=%q", out, token)
	}
	if usage["input_tokens"] != int64(14895) {
		t.Fatalf("usage = %v", usage)
	}
	if _, _, _, err := r.runCursorTeamTurn(context.Background(), "read-only", "", token, t.TempDir(), "second", nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(argvLog)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || strings.Contains(lines[0], "--resume") || !strings.Contains(lines[1], "--resume "+token) {
		t.Fatalf("argv log = %q", raw)
	}
}

func TestRunCursorTeamTurnReturnsTokenOnFailure(t *testing.T) {
	writeFakeCursor(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"partial-chat\"}'\necho boom >&2\nexit 1\n")
	r := &Runner{}
	_, token, _, err := r.runCursorTeamTurn(context.Background(), "read-only", "", "", t.TempDir(), "first", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if token != "partial-chat" {
		t.Fatalf("token = %q", token)
	}
}

func TestRunCursorTeamTurnKeepsOldTokenWhenStreamHasNone(t *testing.T) {
	writeFakeCursor(t, "#!/bin/sh\ncat >/dev/null\nexit 1\n")
	r := &Runner{}
	_, token, _, err := r.runCursorTeamTurn(context.Background(), "read-only", "", "old-chat", t.TempDir(), "again", nil)
	if err == nil || token != "old-chat" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestCursorTeamTurnHonorsContextDeadline(t *testing.T) {
	writeFakeCursor(t, "#!/bin/sh\ncat >/dev/null\nsleep 30\n")
	r := &Runner{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, _, err := r.runCursorTeamTurn(ctx, "read-only", "", "", t.TempDir(), "prompt", nil); err == nil {
		t.Fatal("expected deadline error")
	}
}

func TestDetectSteeringProviderCursorWinsOverClaude(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv(cursorAgentEnv, "1")
	if got := DetectSteeringProvider(); got != "cursor" {
		t.Fatalf("DetectSteeringProvider = %q, want cursor (innermost driver)", got)
	}
}

func TestDetectSteeringProviderCursorAlone(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv(cursorAgentEnv, "1")
	if got := DetectSteeringProvider(); got != "cursor" {
		t.Fatalf("DetectSteeringProvider = %q", got)
	}
}

func TestUntrackedCostProvidersIncludesCursor(t *testing.T) {
	got := UntrackedCostProviders([]TeamMember{{Provider: "cursor"}, {Provider: "claude"}, {Provider: "cursor"}})
	if !reflect.DeepEqual(got, []string{"cursor"}) {
		t.Fatalf("got %v", got)
	}
}

func cursorInvocationStatus(t *testing.T, mode string, script string, missing bool) (string, error) {
	t.Helper()
	clearProviderEnv(t)
	if missing {
		setCursorCLI(t, filepath.Join(t.TempDir(), "no-such-cursor-agent"))
	} else {
		writeFakeCursor(t, script)
	}
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.CreateRun(Run{ID: "wf-cursor-dispatch", Task: "t", CWD: tmp, ScriptPath: "workflow.js"})
	if err != nil {
		t.Fatal(err)
	}
	agent := &Agent{ID: "agent-cursor", Mode: mode, Prompt: "hello"}
	r := &Runner{Store: store, Run: run}
	_, callErr := r.runProviderCommand(context.Background(), "cursor", tmp, filepath.Join(tmp, "out.txt"), filepath.Join(tmp, "usage.json"), tmp, agent.Prompt, agent, AgentOptions{}, false)
	invocations, err := store.ListInvocations(run.ID)
	if err != nil || len(invocations) != 1 {
		t.Fatalf("invocations = %+v err=%v", invocations, err)
	}
	return invocations[0].ConfigurationStatus, callErr
}

func TestCursorInvocationRecordedAsDispatchedOnSuccess(t *testing.T) {
	status, err := cursorInvocationStatus(t, "read-only", "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"ok\",\"session_id\":\"s\"}'\n", false)
	if err != nil || status == "not_dispatched" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestCursorInvocationRecordedAsDispatchedOnPostStartFailure(t *testing.T) {
	status, err := cursorInvocationStatus(t, "read-only", "#!/bin/sh\ncat >/dev/null\necho boom >&2\nexit 2\n", false)
	if err == nil || status == "not_dispatched" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestCursorInvocationNotDispatchedWhenBinaryMissing(t *testing.T) {
	status, err := cursorInvocationStatus(t, "read-only", "", true)
	if err == nil || status != "not_dispatched" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestCursorTeamTurnMarksProviderStartedOnlyAfterStart(t *testing.T) {
	var dispatched bool
	ctx := withProviderStartTracking(context.Background(), &dispatched)
	writeFakeCursor(t, "#!/bin/sh\ncat >/dev/null\necho boom >&2\nexit 2\n")
	r := &Runner{}
	if _, _, _, err := r.runCursorTeamTurn(ctx, "read-only", "", "", t.TempDir(), "p", nil); err == nil {
		t.Fatal("expected failure")
	}
	if !dispatched {
		t.Fatal("post-start failure must count as dispatched")
	}
	dispatched = false
	setCursorCLI(t, filepath.Join(t.TempDir(), "missing"))
	if _, _, _, err := r.runCursorTeamTurn(ctx, "read-only", "", "", t.TempDir(), "p", nil); err == nil {
		t.Fatal("expected failure")
	}
	if dispatched {
		t.Fatal("start failure must not count as dispatched")
	}
}

func TestProviderAvailableCursorUsesCursorAgentExecutable(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv(providerCommandEnvName("cursor"), "")
	write := func(dir, name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	onlyAgent := t.TempDir()
	write(onlyAgent, "cursor-agent")
	t.Setenv("PATH", onlyAgent)
	if !ProviderAvailable("cursor", "") {
		t.Fatal("cursor-agent on PATH must make cursor available")
	}
	onlyIDE := t.TempDir()
	write(onlyIDE, "cursor")
	t.Setenv("PATH", onlyIDE)
	if ProviderAvailable("cursor", "") {
		t.Fatal("the cursor IDE launcher alone must not make cursor available")
	}
}
