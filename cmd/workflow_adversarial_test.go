package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tszaks/pallium/internal/workflow"
)

func TestWorkflowAdversarialInvalidMaxBudgetUSD(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("one"); return "done";`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runWorkflow(&out, []string{
		"run",
		"--id", "wf-bad-budget",
		"--db", dbPath,
		"--cwd", tmp,
		"--max-budget-usd", "not-a-number",
		"--script", scriptPath,
		"bad budget",
	}, false)
	if err == nil || !strings.Contains(err.Error(), "invalid max budget usd") {
		t.Fatalf("expected invalid budget error, got %v", err)
	}
}

func TestWorkflowAdversarialManualGateApproveCommandIsUnavailable(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"approved":true,"reason":"ok"}`)
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("gate");
gate("approve", "verify before worker");
return "done";`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-wrong-gate-cli", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "gate"}, false); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"gate", "approve", "wf-wrong-gate-cli", "wrong-name", "--db", dbPath}, true); err == nil {
		t.Fatal("expected manual gate approval command to be unavailable")
	} else if !strings.Contains(err.Error(), "unknown workflow gate subcommand") {
		t.Fatalf("expected unknown subcommand error, got %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"gate", "list", "wf-wrong-gate-cli", "--db", dbPath}, true); err != nil {
		t.Fatalf("gate list failed: %v", err)
	}
	var gates []map[string]any
	if err := json.Unmarshal(out.Bytes(), &gates); err != nil {
		t.Fatalf("decode gates: %v\n%s", err, out.String())
	}
	if len(gates) != 1 || gates[0]["name"] != "approve" || gates[0]["status"] != "approved" {
		t.Fatalf("expected original gate to stay approved, got %#v", gates)
	}
}

func TestWorkflowAdversarialRevertReadOnlyRunHasNoPatches(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"summary":"read only"}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("scan");
return agent("inspect", { label: "inspect", mode: "read-only" });`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-readonly", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "readonly"}, false); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"revert", "wf-readonly", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow revert failed: %v", err)
	}
	if !strings.Contains(out.String(), "No workflow patches to revert.") {
		t.Fatalf("expected no patches message, got %s", out.String())
	}
}

func TestWorkflowAdversarialResumeStoppedRunReusesCache(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"first","prompt":"{{PROMPT}}"}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("scan");
const result = await agent("stable prompt", { label: "stable" });
return result;`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-stop-resume", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "stop resume"}, false); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	out.Reset()
	if err := runWorkflow(&out, []string{"stop", "wf-stop-resume", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow stop failed: %v", err)
	}
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"source":"second","prompt":"{{PROMPT}}"}`)
	out.Reset()
	if err := runWorkflow(&out, []string{"resume", "wf-stop-resume", "--db", dbPath}, false); err != nil {
		t.Fatalf("workflow resume after stop failed: %v", err)
	}
	if !strings.Contains(out.String(), `"source": "first"`) {
		t.Fatalf("expected resume after stop to reuse cached agent output, got %s", out.String())
	}
}

func TestWorkflowAdversarialProviderRequiresPromptFile(t *testing.T) {
	tmp := t.TempDir()
	providerScript := filepath.Join(tmp, "check-prompt.sh")
	if err := os.WriteFile(providerScript, []byte(`#!/bin/sh
if [ ! -s "$PALLIUM_WORKFLOW_PROMPT_FILE" ]; then
  echo "missing prompt file" >&2
  exit 2
fi
printf '{"ok":true}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_PROBE_COMMAND", providerScript)
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("provider");
return agent("probe provider", { label: "probe", provider: "probe" });`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-probe-provider", "--db", dbPath, "--cwd", tmp, "--script", scriptPath, "probe"}, false); err != nil {
		t.Fatalf("workflow run failed: %v", err)
	}
	if !strings.Contains(out.String(), `"ok": true`) {
		t.Fatalf("expected provider to receive prompt file, got %s", out.String())
	}
}

func TestWorkflowAdversarialFleetLimitZeroIsUnlimited(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`phase("scan");
return agent("one", { label: "one" });`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(workflow.Run{
		ID: "wf-active-a", Task: "active", CWD: tmp, ScriptPath: scriptPath, Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	var out bytes.Buffer
	if err := runWorkflow(&out, []string{"run", "--id", "wf-active-b", "--db", dbPath, "--cwd", tmp, "--max-active-runs", "0", "--script", scriptPath, "fleet zero"}, false); err != nil {
		t.Fatalf("expected zero fleet limit to mean unlimited, got %v", err)
	}
}
