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

// setDevinCLI points the built-in devin provider at a fake binary, matching
// setClaudeCLI's pattern.
func setDevinCLI(t *testing.T, path string) {
	t.Helper()
	old := devinCLIName
	devinCLIName = path
	t.Cleanup(func() { devinCLIName = old })
}

func writeFakeDevin(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devin")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setDevinCLI(t, path)
	return path
}

func TestBuildDevinArgsReadOnlyModes(t *testing.T) {
	for _, mode := range []string{"", "read-only"} {
		got := buildDevinArgs(mode, "", "/tmp/p.txt", "/tmp/e.json", "")
		want := []string{"-p", "--respect-workspace-trust", "false", "--prompt-file", "/tmp/p.txt", "--export", "/tmp/e.json", "--permission-mode", "auto"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mode %q: buildDevinArgs = %v, want %v", mode, got, want)
		}
	}
}

func TestBuildDevinArgsEditTestCheckModes(t *testing.T) {
	for _, mode := range []string{"edit", "test", "check"} {
		got := buildDevinArgs(mode, "", "/tmp/p.txt", "/tmp/e.json", "")
		if got[len(got)-2] != "--permission-mode" || got[len(got)-1] != "dangerous" {
			t.Fatalf("mode %q: expected --permission-mode dangerous, got %v", mode, got)
		}
	}
}

func TestBuildDevinArgsModelAndResume(t *testing.T) {
	got := buildDevinArgs("read-only", "gpt-5", "/tmp/p.txt", "/tmp/e.json", "session-abc")
	want := []string{"-p", "--respect-workspace-trust", "false", "--prompt-file", "/tmp/p.txt", "--export", "/tmp/e.json", "-r", "session-abc", "--model", "gpt-5", "--permission-mode", "auto"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildDevinArgs = %v, want %v", got, want)
	}
}

func devinExportFixture(t *testing.T, raw string) *devinExport {
	t.Helper()
	var export devinExport
	if err := json.Unmarshal([]byte(raw), &export); err != nil {
		t.Fatal(err)
	}
	return &export
}

func TestExtractDevinOutputPrefersExportOverConcatenatedStdout(t *testing.T) {
	// `devin -p` concatenates every assistant message on stdout; the ATIF
	// export's last agent step is the authoritative final answer.
	export := devinExportFixture(t, `{"session_id":"s","steps":[{"source":"agent","message":"step one"},{"source":"agent","message":"step two: done"}]}`)
	got, err := extractDevinOutput("step onestep two: done", export, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "step two: done" {
		t.Fatalf("extractDevinOutput = %q, want last agent step", got)
	}
}

func TestExtractDevinOutputFallsBackToStdout(t *testing.T) {
	got, err := extractDevinOutput("  plain answer  ", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "plain answer" {
		t.Fatalf("extractDevinOutput = %q", got)
	}
}

func TestExtractDevinOutputSkipsNonAgentSteps(t *testing.T) {
	export := devinExportFixture(t, `{"session_id":"s","steps":[{"source":"agent","message":"the answer"},{"source":"system","message":"trailing system note"}]}`)
	got, err := extractDevinOutput("", export, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "the answer" {
		t.Fatalf("extractDevinOutput = %q", got)
	}
}

func TestExtractDevinOutputStripsFenceWhenSchemaRequested(t *testing.T) {
	export := devinExportFixture(t, `{"session_id":"s","steps":[{"source":"agent","message":"`+"```json\\n{\\\"ok\\\":true}\\n```"+`"}]}`)
	got, err := extractDevinOutput("", export, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"ok":true}` {
		t.Fatalf("extractDevinOutput = %q", got)
	}
}

func TestExtractDevinOutputRecoversJSONFromProse(t *testing.T) {
	export := devinExportFixture(t, `{"session_id":"s","steps":[{"source":"agent","message":"Sure: {\"ok\":true} — done."}]}`)
	got, err := extractDevinOutput("", export, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"ok":true}` {
		t.Fatalf("extractDevinOutput = %q", got)
	}
}

func TestExtractDevinOutputEmptyIsError(t *testing.T) {
	if _, err := extractDevinOutput("   ", nil, false); err == nil {
		t.Fatal("expected error for empty output")
	}
	if _, err := extractDevinOutput("", &devinExport{}, false); err == nil {
		t.Fatal("expected error for export with no agent steps")
	}
}

func TestWriteDevinUsage(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.json")
	export := &devinExport{}
	export.SessionID = "s-1"
	export.FinalMetrics.PromptTokens = 120
	export.FinalMetrics.CompletionTokens = 30
	writeDevinUsage(usageFile, export)
	raw, err := os.ReadFile(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatal(err)
	}
	if usage["input_tokens"] != float64(120) || usage["output_tokens"] != float64(30) {
		t.Fatalf("usage = %v", usage)
	}
	if _, ok := usage["cost_usd"]; ok {
		t.Fatalf("devin usage must not claim a USD cost: %v", usage)
	}
}

func TestWriteDevinUsageSkipsEmptyMetrics(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.json")
	writeDevinUsage(usageFile, &devinExport{})
	writeDevinUsage(usageFile, nil)
	if _, err := os.Stat(usageFile); !os.IsNotExist(err) {
		t.Fatalf("usage file should not exist for empty metrics, stat err=%v", err)
	}
}

func TestRunBuiltinDevinCommandReadsExportAndUsage(t *testing.T) {
	exportJSON := `{"session_id":"fake-session-1","steps":[{"source":"user","message":"hi"},{"source":"agent","message":"intermediate"},{"source":"agent","message":"final answer"}],"final_metrics":{"total_prompt_tokens":11,"total_completion_tokens":7}}`
	writeFakeDevin(t, `#!/bin/sh
prompt_file=""
export_file=""
prev=""
for a in "$@"; do
  case "$prev" in
    --prompt-file) prompt_file="$a";;
    --export) export_file="$a";;
  esac
  prev="$a"
done
cat "$prompt_file" > /dev/null
printf 'intermediate\nfinal answer\n'
printf '%s' '`+exportJSON+`' > "$export_file"
`)
	tmpDir := t.TempDir()
	usageFile := filepath.Join(tmpDir, "usage.json")
	r := &Runner{}
	agent := &Agent{ID: "a1", Mode: "read-only", Provider: "devin"}
	got, err := r.runBuiltinDevinCommand(context.Background(), tmpDir, usageFile, t.TempDir(), "say hi", agent, AgentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "final answer" {
		t.Fatalf("output = %q, want last agent step from export", got)
	}
	raw, err := os.ReadFile(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatal(err)
	}
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(7) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestRunBuiltinDevinCommandPassesPromptFile(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "captured-prompt.txt")
	writeFakeDevin(t, `#!/bin/sh
prompt_file=""
export_file=""
prev=""
for a in "$@"; do
  case "$prev" in
    --prompt-file) prompt_file="$a";;
    --export) export_file="$a";;
  esac
  prev="$a"
done
cp "$prompt_file" "`+capture+`"
printf 'ok\n'
printf '%s' '{"session_id":"s","steps":[{"source":"agent","message":"ok"}],"final_metrics":{}}' > "$export_file"
`)
	tmpDir := t.TempDir()
	r := &Runner{}
	agent := &Agent{ID: "a1", Mode: "read-only", Provider: "devin"}
	if _, err := r.runBuiltinDevinCommand(context.Background(), tmpDir, filepath.Join(tmpDir, "u.json"), t.TempDir(), "the prompt body", agent, AgentOptions{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "the prompt body" {
		t.Fatalf("prompt file = %q", raw)
	}
}

func TestRunDevinTeamTurnCapturesSessionAndResumes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "resume-args.txt")
	writeFakeDevin(t, `#!/bin/sh
export_file=""
prev=""
for a in "$@"; do
  case "$prev" in
    --export) export_file="$a";;
    -r|--resume) echo "$a" >> "`+marker+`";;
  esac
  prev="$a"
done
printf 'turn output\n'
printf '%s' '{"session_id":"devin-session-9","steps":[{"source":"agent","message":"turn output"}],"final_metrics":{"total_prompt_tokens":3,"total_completion_tokens":2}}' > "$export_file"
`)
	r := &Runner{}
	out, token, usage, err := r.runDevinTeamTurn(context.Background(), "read-only", "", "", t.TempDir(), "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "turn output" || token != "devin-session-9" {
		t.Fatalf("out=%q token=%q", out, token)
	}
	if usage["input_tokens"] != int64(3) || usage["output_tokens"] != int64(2) {
		t.Fatalf("usage = %v", usage)
	}
	// Second turn must pass -r <token> so the teammate keeps its session.
	out, token, _, err = r.runDevinTeamTurn(context.Background(), "read-only", "", token, t.TempDir(), "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal("fake devin was not resumed with -r")
	}
	if strings.TrimSpace(string(raw)) != "devin-session-9" {
		t.Fatalf("resume args = %q", raw)
	}
}

func TestRunDevinTeamTurnReturnsTokenOnFailure(t *testing.T) {
	// A first turn that mints a session then dies should still surface the
	// token so the retry resumes rather than starting over.
	writeFakeDevin(t, `#!/bin/sh
export_file=""
prev=""
for a in "$@"; do
  case "$prev" in
    --export) export_file="$a";;
  esac
  prev="$a"
done
printf '%s' '{"session_id":"partial-session","steps":[],"final_metrics":{}}' > "$export_file"
echo boom >&2
exit 1
`)
	r := &Runner{}
	_, token, _, err := r.runDevinTeamTurn(context.Background(), "read-only", "", "", t.TempDir(), "first", nil)
	if err == nil {
		t.Fatal("expected error from failing provider")
	}
	if token != "partial-session" {
		t.Fatalf("token = %q, want partial-session", token)
	}
}

func TestDetectSteeringProviderDevin(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv(devinSessionDBEnv, "/tmp/devin-sessions.db")
	if got := DetectSteeringProvider(); got != "devin" {
		t.Fatalf("DetectSteeringProvider = %q, want devin", got)
	}
}

func TestDetectSteeringProviderClaudeWinsOverDevin(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv(devinSessionDBEnv, "/tmp/devin-sessions.db")
	if got := DetectSteeringProvider(); got != "claude" {
		t.Fatalf("DetectSteeringProvider = %q, want claude", got)
	}
}

func TestDevinTeamTurnHonorsContextDeadline(t *testing.T) {
	writeFakeDevin(t, "#!/bin/sh\ncat >/dev/null\nsleep 30\n")
	r := &Runner{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, _, err := r.runDevinTeamTurn(ctx, "read-only", "", "", t.TempDir(), "prompt", nil)
	if err == nil {
		t.Fatal("expected deadline error")
	}
}
