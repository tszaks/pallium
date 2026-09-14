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

func TestGatePersistsOriginalAutoRoutingDecision(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	c.Mode = "auto"
	c.Rules["bounded-edit"] = "luna-xhigh"
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"approved":true,"reason":"ok","evidence":[]}`)
	store, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return await gate("release", "verify", {task_class:"bounded-edit"});`
	path, _ := WriteRunScript("wf-gate-routing", dir, script)
	run, _ := store.CreateRun(Run{ID: "wf-gate-routing", Task: "gate", CWD: dir, ScriptPath: path})
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10, AssumeCodexAvailable: true}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	agents, _ := store.ListAgents(run.ID)
	if len(agents) != 1 {
		t.Fatalf("agents %+v", agents)
	}
	var decision routing.Decision
	if err := json.Unmarshal([]byte(agents[0].RoutingJSON), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Requested.TaskClass != "bounded-edit" || decision.Reason != "configured rule for task class bounded-edit" {
		t.Fatalf("gate lost original auto-routing provenance: %+v", decision)
	}
}

func TestGateRoutingRejectionIsRecordedAsNotDispatched(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	c.Mode = "auto"
	c.AllowedProviders = []string{"claude"}
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	store, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return await gate("release", "verify");`
	path, _ := WriteRunScript("wf-gate-route-reject", dir, script)
	run, _ := store.CreateRun(Run{ID: "wf-gate-route-reject", Task: "gate", CWD: dir, ScriptPath: path})
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10, AssumeCodexAvailable: true}).Execute(context.Background(), script, nil); err == nil {
		t.Fatal("expected gate routing rejection")
	}
	invocations, err := store.ListInvocations(run.ID)
	if err != nil || len(invocations) != 1 || invocations[0].ConfigurationStatus != "not_dispatched" {
		t.Fatalf("gate routing rejection was not recorded: %+v %v", invocations, err)
	}
}

func TestTeamGatePersistsShadowRoutingDecision(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	store, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("ship", dir, 0)
	log := filepath.Join(dir, "args")
	runner := &Runner{Store: store, Run: Run{ID: team.ID}, CodexBinary: fakeCodexBinary(t, log, `{"approved":true,"reason":"ok","evidence":[]}`)}
	approved, _, _, err := runner.runTeamGate(context.Background(), team, "verify")
	if err != nil || !approved {
		t.Fatalf("team gate failed: approved=%v err=%v", approved, err)
	}
	invocations, err := store.ListInvocations(team.ID)
	if err != nil || len(invocations) != 1 || invocations[0].RoutingJSON == "" {
		t.Fatalf("team gate lost routing evidence: %+v %v", invocations, err)
	}
	var decision routing.Decision
	if err := json.Unmarshal([]byte(invocations[0].RoutingJSON), &decision); err != nil || decision.PolicyHash == "" || decision.Recommended == nil {
		t.Fatalf("invalid team gate routing evidence: %+v %v", decision, err)
	}
}

func TestRoutingCacheHitRefreshesShadowDecision(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	t.Setenv("PALLIUM_WORKFLOW_AGENT_STUB", `{"ok":true}`)
	store, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return await agent("inspect", {label:"stable"});`
	path, _ := WriteRunScript("wf-shadow-refresh", dir, script)
	run, _ := store.CreateRun(Run{ID: "wf-shadow-refresh", Task: "shadow", CWD: dir, ScriptPath: path})
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10, AssumeCodexAvailable: true}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	c.Default = "luna-medium"
	raw, _ = json.Marshal(c)
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10, AssumeCodexAvailable: true}).Execute(context.Background(), script, nil); err != nil {
		t.Fatal(err)
	}
	agents, _ := store.ListAgents(run.ID)
	if len(agents) != 1 {
		t.Fatalf("cache hit unexpectedly dispatched: %+v", agents)
	}
	var decision routing.Decision
	if err := json.Unmarshal([]byte(agents[0].RoutingJSON), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Recommended == nil || decision.Recommended.ID != "luna-medium" || decision.PolicyHash != c.Hash() {
		t.Fatalf("cached routing evidence stayed stale: %+v", decision)
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

func TestRoutingRejectsNetworklessBuiltinClaude(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	c := routing.Config{
		Version: 1, Mode: "auto", AllowedProviders: []string{"claude"}, Default: "claude-network",
		Candidates: []routing.Candidate{{ID: "claude-network", Provider: "claude", Model: "claude-opus-5", Effort: "high", Enabled: true, Network: true, Modes: []string{"read-only"}}},
		Rules:      map[string]string{},
	}
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	r := Runner{Run: Run{CWD: dir, AllowNetwork: true}}
	if _, _, err := r.resolveRouting(AgentOptions{Network: true}, "read-only"); err == nil {
		t.Fatal("selected networkless built-in Claude for a network-required call")
	}
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND", "wrapper")
	if opts, _, err := r.resolveRouting(AgentOptions{Network: true}, "read-only"); err != nil || opts.Provider != "claude" {
		t.Fatalf("configured Claude wrapper should satisfy network routing: %+v %v", opts, err)
	}
}

func TestRoutingTreatsWhitespaceWrapperAsUnavailable(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER_GEMINI_COMMAND", "  \t")
	if ProviderAvailable("gemini", "") {
		t.Fatal("whitespace-only wrapper was treated as available")
	}
}

func TestRoutingRejectionIsRecordedAsNotDispatched(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	c.Mode = "auto"
	c.AllowedProviders = []string{"claude"}
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	store, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	script := `return await agent("inspect");`
	path, _ := WriteRunScript("wf-route-reject", dir, script)
	run, _ := store.CreateRun(Run{ID: "wf-route-reject", Task: "reject", CWD: dir, ScriptPath: path})
	if _, err := (&Runner{Store: store, Run: run, MaxAgents: 10, AssumeCodexAvailable: true}).Execute(context.Background(), script, nil); err == nil {
		t.Fatal("expected routing rejection")
	}
	invocations, err := store.ListInvocations(run.ID)
	if err != nil || len(invocations) != 1 || invocations[0].ConfigurationStatus != "not_dispatched" || invocations[0].Status != "failed" {
		t.Fatalf("routing rejection was not recorded: %+v %v", invocations, err)
	}
}

func TestPlainAutoRoutedMemberCannotPromoteToIneligibleEditMode(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	c.Mode = "auto"
	for i := range c.Candidates {
		c.Candidates[i].Modes = []string{"read-only"}
	}
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	store, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team, _ := store.CreateTeam("test", dir, 0)
	member, err := store.SpawnMember(team.ID, "reader", "", "", "inspect", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if member.Mode != "read-only" {
		t.Fatalf("unexpected member: %+v", member)
	}
	if err := store.SetMemberMode(team.ID, member.Name, "edit"); err == nil {
		t.Fatal("promoted member through an edit-ineligible auto route")
	}
	unchanged, _ := store.GetMember(team.ID, member.Name)
	if unchanged.Mode != "read-only" {
		t.Fatalf("failed promotion still changed mode: %+v", unchanged)
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

func TestTeamRoutingUsesTaskClass(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	c.Mode = "auto"
	c.Rules["bounded-edit"] = "luna-xhigh"
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	s, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	team, _ := s.CreateTeam("test", dir, 0)
	m, err := s.SpawnMemberWithRouting(team.ID, "worker", "", "", "test", "edit", TeamRoutingOptions{TaskClass: "bounded-edit"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Model != "gpt-5.6-luna" || m.ReasoningEffort != "xhigh" {
		t.Fatalf("team task class did not select its rule: %+v", m)
	}
}

func TestTeamSpawnDefersCodexBinaryAvailabilityToRunner(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	c := routing.Starter()
	c.Mode = "auto"
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	s, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	team, _ := s.CreateTeam("test", dir, 0)
	m, err := s.SpawnMember(team.ID, "worker", "", "", "test", "read-only")
	if err != nil || m.Provider != "codex" {
		t.Fatalf("spawn should defer Codex executable validation until team run: %+v %v", m, err)
	}
}

func TestPlanRequiredRoutingRequiresEditEligibleCandidate(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	c := routing.Starter()
	c.Mode = "auto"
	for i := range c.Candidates {
		c.Candidates[i].Modes = []string{"read-only"}
	}
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_ROUTING_CONFIG", config)
	s, err := Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	team, _ := s.CreateTeam("test", dir, 0)
	if _, err := s.SpawnPlanRequiredMember(team.ID, "planner", "", "", "test"); err == nil {
		t.Fatal("plan-required member accepted a route that cannot run after edit approval")
	}
}
