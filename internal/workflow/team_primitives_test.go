package workflow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowScriptCanConveneAndDriveATeamBoard exercises the M2 item-1
// goja bindings (team.*) end to end for everything that doesn't itself
// dispatch a live provider call (create/spawn/send/tasks.create+list+claim/
// approve/reject/gate/status/stop) — the JS<->Go marshaling boundary for
// every primitive. team.wait/tasks.complete (which DO dispatch real turns)
// are covered by the Go-level RunTeam/CompleteTaskWithGate tests plus the
// PR's live proof, not duplicated here as a synthetic fake-provider round
// trip.
func TestWorkflowScriptCanConveneAndDriveATeamBoard(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	script := `
const newTeam = team.create("ship the feature", {cwd: "` + strings.ReplaceAll(tmp, `\`, `\\`) + `", budgetUsd: 5});
const teamId = newTeam.id;
const planner = team.spawn(teamId, "planner-1", {provider: "claude", planRequired: true});
const worker = team.spawn(teamId, "worker-1", {provider: "claude"});
team.gate(teamId, "verify the result actually satisfies the task", ["task_completed", "teammate_idle"]);
const sent = team.send(teamId, "planner-1", "start with module A");
const task = team.tasks.create(teamId, "read module A", {description: "understand the module"});
const claimed = team.tasks.claim(teamId, task.id, "worker-1");
const listed = team.tasks.list(teamId);
const rejected = team.reject(teamId, "planner-1", "too broad, narrow the scope");
const approved = team.approve(teamId, "planner-1");
const status = team.status(teamId);
team.stop(teamId);
return {teamId: teamId, planner, worker, sent, task, claimed, listed, rejected, approved, status};
`
	scriptPath, err := WriteRunScript("wf-team-primitives", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-team-primitives", Task: "team primitives", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	resultText, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		TeamID   string      `json:"teamId"`
		Planner  TeamMember  `json:"planner"`
		Worker   TeamMember  `json:"worker"`
		Sent     TeamMessage `json:"sent"`
		Task     TeamTask    `json:"task"`
		Claimed  TeamTask    `json:"claimed"`
		Listed   []TeamTask  `json:"listed"`
		Rejected TeamMember  `json:"rejected"`
		Approved TeamMember  `json:"approved"`
		Status   struct {
			Team    Team         `json:"team"`
			Members []TeamMember `json:"members"`
			Tasks   []TeamTask   `json:"tasks"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(resultText), &result); err != nil {
		t.Fatalf("decode script result: %v\n%s", err, resultText)
	}

	if result.Planner.Mode != "read-only" || !result.Planner.PlanRequired || result.Planner.PlanStatus != "pending" {
		t.Fatalf("expected team.spawn(planRequired) to produce a read-only pending-plan member, got %+v", result.Planner)
	}
	if result.Worker.Provider != "claude" || result.Worker.SessionToken == "" {
		t.Fatalf("expected team.spawn to mint a claude session, got %+v", result.Worker)
	}
	if result.Sent.To != "planner-1" || result.Sent.Body != "start with module A" {
		t.Fatalf("expected team.send to record the message, got %+v", result.Sent)
	}
	if result.Task.Title != "read module A" || result.Task.Description != "understand the module" {
		t.Fatalf("expected team.tasks.create to record title+description, got %+v", result.Task)
	}
	if result.Claimed.Status != "in_progress" || result.Claimed.Owner != "worker-1" {
		t.Fatalf("expected team.tasks.claim to claim it for worker-1, got %+v", result.Claimed)
	}
	if len(result.Listed) != 1 {
		t.Fatalf("expected team.tasks.list to see the one task, got %+v", result.Listed)
	}
	if result.Rejected.Mode != "read-only" || result.Rejected.PlanStatus != "pending" {
		t.Fatalf("expected team.reject to leave the planner read-only and pending, got %+v", result.Rejected)
	}
	if result.Approved.Mode != "edit" || result.Approved.PlanStatus != "approved" {
		t.Fatalf("expected team.approve to flip the planner to edit/approved, got %+v", result.Approved)
	}
	// team.status ran BEFORE team.stop in the script, so it must still show
	// active — the stop call's effect is checked separately below, directly
	// against the store, since the script's own captured `status` snapshot
	// predates it.
	if result.Status.Team.Status != "active" {
		t.Fatalf("expected team.status (captured before team.stop) to show active, got %+v", result.Status.Team)
	}
	if len(result.Status.Members) != 2 || len(result.Status.Tasks) != 1 {
		t.Fatalf("expected team.status to see both members and the one task, got %+v", result.Status)
	}
	if len(result.Status.Team.GateHooks) != 2 {
		t.Fatalf("expected team.gate's configured hooks visible on team.status, got %+v", result.Status.Team)
	}

	final, err := store.GetTeam(result.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "stopped" {
		t.Fatalf("expected team.stop to have actually persisted, got %+v", final)
	}
}
