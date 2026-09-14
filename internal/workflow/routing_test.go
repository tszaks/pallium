package workflow

import (
	"context"
	"encoding/json"
	"github.com/tszaks/pallium/internal/routing"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoutingExecutesAndPersistsSelectedPair(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	c.Mode = "auto"
	c.Rules["bounded-edit"] = "luna-xhigh"
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	os.WriteFile(config, raw, 0o600)
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	store, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return await agent("explain", {task_class:"bounded-edit"});`
	path, err := WriteRunScript("wf-route", dir, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-route", Task: "route", CWD: dir, ScriptPath: path})
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "args")
	r := Runner{Store: store, Run: run, CodexBinary: fakeCodexBinary(t, log, `{"ok":true}`), MaxAgents: 10}
	if _, err := r.Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Model != "gpt-5.6-luna" || agents[0].ReasoningEffort != "xhigh" {
		t.Fatalf("agents %+v", agents)
	}
	var d routing.Decision
	if err := json.Unmarshal([]byte(agents[0].RoutingJSON), &d); err != nil {
		t.Fatal(err)
	}
	if d.Selected.Model != "gpt-5.6-luna" || d.PolicyHash == "" {
		t.Fatalf("decision %+v", d)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "model_reasoning_effort=xhigh") {
		t.Fatalf("args %s", args)
	}
	// A changed effort at the same call index must execute another worker.
	c.Candidates[len(c.Candidates)-1].Effort = "medium"
	raw, _ = json.Marshal(c)
	os.WriteFile(config, raw, 0o600)
	r = Runner{Store: store, Run: run, CodexBinary: r.CodexBinary, MaxAgents: 10}
	if _, err := r.Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	agents, err = store.ListAgents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[1].ReasoningEffort != "medium" {
		t.Fatalf("reused wrong effort: %+v", agents)
	}
}

func TestRoutingShadowPreservesExplicitAndProviderBoundary(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	raw, _ := json.Marshal(c)
	path := filepath.Join(dir, "policy")
	os.WriteFile(path, raw, 0o600)
	t.Setenv("PALLIUM_ROUTING_CONFIG", path)
	r := Runner{Run: Run{CWD: dir}, CodexBinary: fakeCodexBinary(t, filepath.Join(dir, "args"), "ok")}
	opts, record, err := r.resolveRouting(AgentOptions{Model: "gpt-5.5", ReasoningEffort: "high"}, "read-only")
	if err != nil || opts.Model != "gpt-5.5" || opts.ReasoningEffort != "high" || record == "" {
		t.Fatalf("%+v %s %v", opts, record, err)
	}
	t.Setenv("CLAUDECODE", "1")
	if _, _, err := r.resolveRouting(AgentOptions{}, "read-only"); err == nil {
		t.Fatal("shadow executed disallowed implicit provider")
	}
}

func TestTeamEffortSurvivesStoreRoundTrip(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("PALLIUM_ROUTING_CONFIG", "")
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	team, err := s.CreateTeam("test", dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SpawnMember(team.ID, "worker", "codex", "gpt-5.6-luna", "test", "read-only", "xhigh"); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetMember(team.ID, "worker")
	if err != nil || m.ReasoningEffort != "xhigh" {
		t.Fatalf("%+v %v", m, err)
	}
	ms, err := s.ListMembers(team.ID)
	if err != nil || len(ms) != 1 || ms[0].ReasoningEffort != "xhigh" {
		t.Fatalf("%+v %v", ms, err)
	}
}

func TestTeamAutoRoutingCanSelectProviderWhenCallerOmitsIt(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	c.Mode = "auto"
	c.AllowedProviders = []string{"codex", "claude"}
	c.Candidates = append(c.Candidates, routing.Candidate{
		ID: "claude-auto", Provider: "claude", Model: "claude-opus-5",
		Effort: "xhigh", Enabled: true, Modes: []string{"read-only"},
	})
	c.Default = "claude-auto"
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND", "configured")
	s, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	team, err := s.CreateTeam("test", dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.SpawnMember(team.ID, "worker", "", "", "test", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != "claude" || m.Model != "claude-opus-5" || m.ReasoningEffort != "xhigh" {
		t.Fatalf("auto routing was pinned to the default provider: %+v", m)
	}
}
