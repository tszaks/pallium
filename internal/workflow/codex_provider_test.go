package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCodexBinary writes a shell script standing in for the `codex` CLI: it
// parses argv for --output-last-message and writes the given last-message
// body there, matching `codex exec --output-last-message <file>`. It also
// records the full argv it was invoked with to argvLog so tests can assert
// on sandbox/model/schema flags.
func fakeCodexBinary(t *testing.T, argvLog, lastMessage string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-codex.sh")
	script := `#!/bin/sh
echo "$@" > "` + argvLog + `"
OUT=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-last-message) OUT="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s' '` + lastMessage + `' > "$OUT"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCodexCommandInvokesRealBinary(t *testing.T) {
	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "argv.log")
	fakeCodex := fakeCodexBinary(t, argvLog, `{"ok":true}`)

	outFile := filepath.Join(tmp, "last-message.txt")
	cwd := t.TempDir()
	r := &Runner{CodexBinary: fakeCodex}
	agent := &Agent{Mode: "edit", Prompt: "do the thing"}
	out, err := r.runCodexCommand(context.Background(), tmp, outFile, cwd, agent.Prompt, agent, AgentOptions{Model: "gpt-5"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Fatalf("unexpected output: %q", out)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"exec", "--cd", cwd, "--sandbox", "workspace-write", "--model", "gpt-5", "do the thing"} {
		if !strings.Contains(string(argv), want) {
			t.Fatalf("expected argv to contain %q, got: %s", want, argv)
		}
	}
}

func TestRunCodexCommandFailurePropagatesStderr(t *testing.T) {
	tmp := t.TempDir()
	failing := filepath.Join(tmp, "fake-codex-fail.sh")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{CodexBinary: failing}
	agent := &Agent{Mode: "read-only", Prompt: "hi"}
	out, err := r.runCodexCommand(context.Background(), tmp, filepath.Join(tmp, "last-message.txt"), t.TempDir(), agent.Prompt, agent, AgentOptions{}, false)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing stderr, got %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty output on failure, got %q", out)
	}
}

func TestRunCodexCommandSurfacesMeaningfulErrorLineFromStdout(t *testing.T) {
	tmp := t.TempDir()
	failing := filepath.Join(tmp, "fake-codex-quota.sh")
	script := "#!/bin/sh\n" +
		"echo \"ERROR: You've hit your usage limit, try again at Aug 7th, 2026\"\n" +
		"echo \"signal: killed: Reading additional input from stdin...\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(failing, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{CodexBinary: failing}
	agent := &Agent{Mode: "read-only", Prompt: "hi"}
	_, err := r.runCodexCommand(context.Background(), tmp, filepath.Join(tmp, "last-message.txt"), t.TempDir(), agent.Prompt, agent, AgentOptions{}, false)
	if err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("expected error to surface the usage-limit line from stdout, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "ERROR: You've hit your usage limit") {
		t.Fatalf("expected the meaningful line to lead the error message, got: %v", err)
	}
}

func TestRunnerDispatchesToRealCodexBinary(t *testing.T) {
	clearProviderEnv(t)
	tmp := t.TempDir()
	fakeCodex := fakeCodexBinary(t, filepath.Join(tmp, "argv.log"), `{"ok":true,"via":"codex"}`)

	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `const result = await agent("say hi", { label: "codex-worker" }); return result;`
	scriptPath, err := WriteRunScript("wf-real-codex", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-real-codex", Task: "provider", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10, CodexBinary: fakeCodex}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "codex") {
		t.Fatalf("expected codex-backed output, got: %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Provider != "codex" {
		t.Fatalf("expected codex provider (the zero-config default), got %+v", agents)
	}
}
