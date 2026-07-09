package workflow

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildClaudeTeamArgsFirstTurnUsesSessionID(t *testing.T) {
	got := buildClaudeTeamArgs("read-only", "", "abc-123", true)
	want := append(buildClaudeArgs("read-only", ""), "--session-id", "abc-123")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildClaudeTeamArgs(first turn) = %v, want %v", got, want)
	}
}

func TestBuildClaudeTeamArgsLaterTurnUsesResume(t *testing.T) {
	got := buildClaudeTeamArgs("edit", "claude-sonnet-5", "abc-123", false)
	want := append(buildClaudeArgs("edit", "claude-sonnet-5"), "--resume", "abc-123")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildClaudeTeamArgs(later turn) = %v, want %v", got, want)
	}
}

func TestRunClaudeTeamTurnExtractsStructuredDecision(t *testing.T) {
	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"{\"status\":\"idle\",\"summary\":\"nothing to do\"}"}`))
	r := &Runner{}
	out, _, err := r.runClaudeTeamTurn(context.Background(), "read-only", "", "sess-1", true, t.TempDir(), "what's next?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"status":"idle","summary":"nothing to do"}` {
		t.Fatalf("unexpected output: %q", out)
	}
}

// fakeCodexTeamBinary writes a shell script standing in for `codex` that
// prints a `thread.started` --json event (only when not resuming — argv
// contains "resume") before writing the last-message file, matching a real
// first-turn `codex exec --json --output-last-message <file>` invocation.
func fakeCodexTeamBinary(t *testing.T, argvLog, threadID, lastMessage string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-codex-team.sh")
	script := `#!/bin/sh
echo "$@" > "` + argvLog + `"
OUT=""
RESUMING=0
for a in "$@"; do
  if [ "$a" = "resume" ]; then RESUMING=1; fi
done
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-last-message) OUT="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ "$RESUMING" = "0" ]; then
  echo '{"type":"thread.started","thread_id":"` + threadID + `"}'
fi
echo '{"type":"turn.completed"}'
printf '%s' '` + lastMessage + `' > "$OUT"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCodexTeamTurnCapturesThreadIDImmediately(t *testing.T) {
	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "argv.log")
	fakeCodex := fakeCodexTeamBinary(t, argvLog, "thread-xyz", `{"status":"active","summary":"reading module A"}`)
	r := &Runner{CodexBinary: fakeCodex}

	var captured string
	calls := 0
	out, err := r.runCodexTeamTurn(context.Background(), tmp, filepath.Join(tmp, "last.txt"), t.TempDir(), "", "", "read-only", false, "start", nil, func(threadID string) {
		captured = threadID
		calls++
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured != "thread-xyz" || calls != 1 {
		t.Fatalf("expected onSessionCaptured to fire exactly once with thread-xyz, got %q x%d", captured, calls)
	}
	if out != `{"status":"active","summary":"reading module A"}` {
		t.Fatalf("unexpected output: %q", out)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "resume") {
		t.Fatalf("first turn must not use resume, got argv: %s", argv)
	}
}

func TestRunCodexTeamTurnResumeUsesSessionTokenAndSkipsCapture(t *testing.T) {
	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "argv.log")
	fakeCodex := fakeCodexTeamBinary(t, argvLog, "should-not-appear", `{"status":"idle","summary":"done"}`)
	r := &Runner{CodexBinary: fakeCodex}

	calls := 0
	_, err := r.runCodexTeamTurn(context.Background(), tmp, filepath.Join(tmp, "last.txt"), t.TempDir(), "", "thread-xyz", "read-only", false, "continue", nil, func(string) {
		calls++
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("expected onSessionCaptured NOT to fire on a resumed turn (session already known), got %d calls", calls)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "resume thread-xyz") {
		t.Fatalf("expected argv to resume the known thread id, got: %s", argv)
	}
}

func TestRunCodexTeamTurnPropagatesMeaningfulError(t *testing.T) {
	tmp := t.TempDir()
	failing := filepath.Join(tmp, "fake-codex-fail.sh")
	script := "#!/bin/sh\necho 'ERROR: usage limit reached, try again at Aug 7th' >&2\nexit 1\n"
	if err := os.WriteFile(failing, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{CodexBinary: failing}
	_, err := r.runCodexTeamTurn(context.Background(), tmp, filepath.Join(tmp, "last.txt"), t.TempDir(), "", "", "read-only", false, "hi", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("expected the meaningful quota error surfaced, got %v", err)
	}
}
