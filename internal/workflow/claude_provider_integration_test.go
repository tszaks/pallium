package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaudeBinary writes a shell script standing in for the `claude` CLI:
// it reads (and discards) the piped prompt from stdin, then prints the given
// JSON envelope to stdout, matching `claude --output-format json`.
func fakeClaudeBinary(t *testing.T, envelope string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude.sh")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s' '" + envelope + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// setClaudeCLI points the built-in claude provider at a fake binary for the
// duration of the test and restores the real name afterward.
func setClaudeCLI(t *testing.T, path string) {
	t.Helper()
	old := claudeCLIName
	claudeCLIName = path
	t.Cleanup(func() { claudeCLIName = old })
}

func TestRunnerAdoptsClaudeProviderWhenSteering(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND", "")
	t.Setenv("CLAUDECODE", "1")
	setClaudeCLI(t, fakeClaudeBinary(t, `{"result":"adopted claude worker","total_cost_usd":0.01,"usage":{"input_tokens":3,"output_tokens":4}}`))

	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("adopt");
const out = await agent("say hi", { label: "adopt-worker" });
return { out };`
	scriptPath, err := WriteRunScript("wf-adopt-claude", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-adopt-claude", Task: "adopt", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "adopted claude worker") {
		t.Fatalf("expected adopted claude worker output, got: %s", result)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Provider != "claude" {
		t.Fatalf("expected claude provider (adopted from steering agent), got %+v", agents)
	}
	if agents[0].EstimatedCostUSD != 0.01 {
		t.Fatalf("expected reported cost_usd to be recorded, got %+v", agents[0])
	}
}

func TestRunnerExplicitCodexProviderWinsUnderClaudeCode(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)

	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `phase("providers");
await agent("codex provider", { label: "codex-worker", provider: "codex" });
return { ok: true };`
	scriptPath, err := WriteRunScript("wf-explicit-codex", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-explicit-codex", Task: "provider", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Provider != "codex" {
		t.Fatalf("expected explicit codex provider to win even under CLAUDECODE, got %+v", agents)
	}
}

func TestRunnerWorkflowProviderEnvOverrideBeatsDetection(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "loop")
	providerScript := filepath.Join(t.TempDir(), "loop-provider.sh")
	if err := os.WriteFile(providerScript, []byte(`#!/bin/sh
printf '{"ok":true}' > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_LOOP_COMMAND", providerScript)

	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `const result = await agent("no explicit provider", { label: "env-override" }); return result;`
	scriptPath, err := WriteRunScript("wf-env-override", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-env-override", Task: "provider", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Provider != "loop" {
		t.Fatalf("expected PALLIUM_WORKFLOW_PROVIDER override to beat steering detection, got %+v", agents)
	}
}
