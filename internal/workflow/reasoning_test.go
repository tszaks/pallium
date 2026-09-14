package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReasoningValidation(t *testing.T) {
	for _, tc := range []struct {
		provider, model, effort string
		valid                   bool
	}{
		{"codex", "gpt-5.6-luna", "xhigh", true}, {"codex", "gpt-6-astra", "max", true},
		{"codex", "gpt-5.5", "max", false}, {"codex", "gpt-6-astra", "ultra", false},
		{"claude", "claude-sonnet-5", "xhigh", true}, {"claude", "claude-haiku-4-5", "high", false},
		{"gemini", "gemini-3.5-flash", "high", false}, {"custom", "any", "", true},
		{"codex", "gpt-6-astra", "high; echo bad", false},
	} {
		if err := ValidateReasoningEffort(tc.provider, tc.model, tc.effort); (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestConfiguredWrapperOwnsReasoningEffortValidation(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_GEMINI_COMMAND", "wrapper")
	if err := ValidateReasoningEffort("gemini", "gemini-3.5-flash", "provider-defined"); err != nil {
		t.Fatalf("configured wrapper effort was rejected: %v", err)
	}
	if err := ValidateReasoningEffort("gemini", "gemini-3.5-flash", "bad value"); err == nil {
		t.Fatal("accepted whitespace-bearing wrapper effort")
	}
}

func TestWorkflowReasoningValidationFailureIsRecordedNotDispatched(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return await agent("invalid effort", {model:"gpt-5.5", reasoning_effort:"max"});`
	path, err := WriteRunScript("wf-invalid-effort", dir, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-invalid-effort", Task: "invalid", CWD: dir, ScriptPath: path})
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: store, Run: run, CodexBinary: filepath.Join(dir, "must-not-run"), MaxAgents: 10}
	if _, err := runner.Execute(context.Background(), script, nil); err == nil {
		t.Fatal("expected invalid reasoning effort error")
	}
	invocations, err := store.ListInvocations(run.ID)
	if err != nil || len(invocations) != 1 || invocations[0].ConfigurationStatus != "not_dispatched" {
		t.Fatalf("public workflow validation failure was not recorded: %+v %v", invocations, err)
	}
}

func TestCodexEffortAndUsage(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	binary := fakeCodexBinary(t, log, `{"ok":true}`)
	f, err := os.OpenFile(binary, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("echo '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":100,\"cached_input_tokens\":20,\"output_tokens\":30}}'\n")
	f.Close()
	r := Runner{CodexBinary: binary}
	_, err = r.runCodexCommand(context.Background(), dir, filepath.Join(dir, "out"), dir, "task", &Agent{Mode: "read-only"}, AgentOptions{Model: "gpt-5.6-luna", ReasoningEffort: "xhigh"}, false)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "model_reasoning_effort=xhigh") || !strings.Contains(string(args), "--json") {
		t.Fatalf("args %s", args)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "usage.json"))
	var u map[string]any
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	if u["output_tokens"] != float64(30) || u["cost_usd"] != nil || u["cost_status"] != "unknown" {
		t.Fatalf("usage %s", raw)
	}
}

func TestClaudeEffortFlag(t *testing.T) {
	args := strings.Join(buildClaudeArgs("read-only", "claude-opus-5", "low"), " ")
	if !strings.Contains(args, "--effort low") {
		t.Fatal(args)
	}
}

func TestCodexUsageIgnoresToolOutputAndSumsTurns(t *testing.T) {
	u := codexUsage("{\"type\":\"item.completed\",\"usage\":{\"output_tokens\":999}}\n{\"type\":\"turn.completed\",\"usage\":{\"output_tokens\":2}}\n{\"type\":\"turn.completed\",\"usage\":{\"output_tokens\":3}}")
	if u["output_tokens"] != float64(5) {
		t.Fatal(u)
	}
	if codexUsage("bad data") != nil {
		t.Fatal("invented usage")
	}
}

func TestConsumeCodexEventsDrainsOversizedLines(t *testing.T) {
	oversized := append([]byte(`{"type":"item.completed","output":"`), bytes.Repeat([]byte("x"), maxCodexEventBytes+1)...)
	oversized = append(oversized, []byte(`"}`+"\n"+`{"type":"turn.completed","usage":{"output_tokens":7}}`+"\n")...)
	seen := 0
	tail, usage, err := consumeCodexEvents(bytes.NewReader(oversized), func([]byte) { seen++ })
	if err != nil {
		t.Fatal(err)
	}
	if usage["output_tokens"] != float64(7) || seen != 1 {
		t.Fatalf("oversized event prevented later accounting: usage=%+v seen=%d", usage, seen)
	}
	if len(tail) > maxErrorOutputBytes {
		t.Fatalf("tail grew beyond bound: %d", len(tail))
	}
}

func TestGateEvaluationKeyInvalidatesApproval(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.EnsureGate("wf-gate", "review", "message", "model-high"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveGate("wf-gate", "review"); err != nil {
		t.Fatal(err)
	}
	g, err := s.EnsureGate("wf-gate", "review", "message", "model-high")
	if err != nil || g.Status != "approved" {
		t.Fatalf("%+v %v", g, err)
	}
	g, err = s.EnsureGate("wf-gate", "review", "message", "model-low")
	if err != nil || g.Status != "open" {
		t.Fatalf("stale approval: %+v %v", g, err)
	}
}
