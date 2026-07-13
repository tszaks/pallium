package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestWorkflowScriptCanSuperviseIndividualTeammates exercises the M2 PR B
// goja bindings (team.member.*) — individual teammate supervision distinct
// from team.stop (the whole team). All three are soft (no in-flight kill),
// so this covers the JS<->Go marshaling boundary the same way the primary
// primitives test does, on an idle member (nothing in flight to interact
// with, matching that test's own scoping note).
func TestWorkflowScriptCanSuperviseIndividualTeammates(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	script := `
const newTeam = team.create("ship the feature", {cwd: "` + strings.ReplaceAll(tmp, `\`, `\\`) + `"});
const teamId = newTeam.id;
team.spawn(teamId, "worker-1", {provider: "claude"});
const stopped = team.member.stop(teamId, "worker-1");
const restarted = team.member.restart(teamId, "worker-1");
const steered = team.member.steer(teamId, "worker-1", "drop module B, focus on the auth bug");
return {teamId: teamId, stopped, restarted, steered};
`
	scriptPath, err := WriteRunScript("wf-team-supervision", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-team-supervision", Task: "team supervision", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	resultText, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		TeamID    string      `json:"teamId"`
		Stopped   TeamMember  `json:"stopped"`
		Restarted TeamMember  `json:"restarted"`
		Steered   TeamMessage `json:"steered"`
	}
	if err := json.Unmarshal([]byte(resultText), &result); err != nil {
		t.Fatalf("decode script result: %v\n%s", err, resultText)
	}

	if !result.Stopped.StopRequested || result.Stopped.Status != "stopped" {
		t.Fatalf("expected team.member.stop to stop an idle member immediately, got %+v", result.Stopped)
	}
	if result.Restarted.StopRequested || result.Restarted.Status != "active" {
		t.Fatalf("expected team.member.restart to make the member schedulable again, got %+v", result.Restarted)
	}
	if result.Steered.To != "worker-1" || !strings.Contains(result.Steered.Body, "STEERING DIRECTIVE") || !strings.Contains(result.Steered.Body, "drop module B") {
		t.Fatalf("expected team.member.steer to deliver a distinctly-framed directive, got %+v", result.Steered)
	}
}

// TestTeamWaitPrimitiveHonorsWorkflowStop is the regression test for the
// review finding that team.wait passed the raw script context into RunTeam
// instead of contextWithStoredStop's wrapper — the same stop/pause-aware
// context every agent() call already gets. A teammate turn dispatched
// through team.wait used to keep running (and spending) after `pallium
// workflow stop`/`pause` marked the run's status in the store, unlike every
// other provider call in a workflow run. Proven with a deliberately slow
// fake teammate provider: if this test takes anywhere near the full sleep
// duration, the stop was not honored.
func TestTeamWaitPrimitiveHonorsWorkflowStop(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see other gate tests' comment for why
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SpawnMember(team.ID, "worker-1", "claude", "", "", "read-only"); err != nil {
		t.Fatal(err)
	}
	// A claimable task, or RunTeam's scheduler finds zero eligible members
	// and returns immediately regardless of stop/pause — a fresh member
	// with no task board and no mail is NOT eligible for a first look
	// (HasClaimableWork is false with nothing on the board at all), which
	// would make this test pass for the wrong reason (nothing scheduled at
	// all, not the stop actually canceling an in-flight turn).
	if _, err := store.CreateTeamTask(team.ID, "do something", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistMemberSession(team.ID, "worker-1", "sess-1"); err != nil {
		t.Fatal(err)
	}

	// Sleeps far longer than this test should ever actually wait — if
	// contextWithStoredStop doesn't cancel the in-flight exec, the test
	// itself would need to wait out the whole sleep.
	path := filepath.Join(tmp, "fake-claude-slow.sh")
	startedMarker := filepath.Join(tmp, "started")
	finishedMarker := filepath.Join(tmp, "finished")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\ntouch '"+startedMarker+"'\nsleep 5\ntouch '"+finishedMarker+"'\nprintf '%s' '{\"result\":\"{\\\"status\\\":\\\"idle\\\",\\\"summary\\\":\\\"done\\\"}\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, path)

	script := `team.wait("` + team.ID + `"); return "done";`
	scriptPath, err := WriteRunScript("wf-team-wait-stop", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-team-wait-stop", Task: "team wait stop", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		_, execErr := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
		done <- outcome{execErr, time.Since(start)}
	}()

	// Long enough for Execute to reach team.wait and start the (sleeping)
	// teammate turn, short enough to land well before the 5s fake sleep or
	// the very first 250ms contextWithStoredStop poll tick would resolve it
	// on its own without this explicit stop.
	time.Sleep(100 * time.Millisecond)
	if err := store.SetRunStatus(run.ID, "stopped", "", "test-triggered-stop"); err != nil {
		t.Fatal(err)
	}

	// The REAL signal is whether the fake provider's process was actually
	// killed mid-sleep, checked directly via the marker file rather than
	// timing Execute's own return: exec.Cmd's WaitDelay (set on every real
	// team-turn provider call, e.g. claude_provider.go, to bound how long
	// Wait() drains a killed process's I/O) means cmd.Run() can legitimately
	// take several more seconds to actually RETURN even once the process
	// itself has already been killed — that grace period is orthogonal to
	// whether contextWithStoredStop's cancellation fired correctly.
	//
	// Checked AFTER the fake process's own 5s sleep would have naturally
	// elapsed (not before): checking any earlier is meaningless — "finished"
	// wouldn't exist yet either way, whether cancellation worked or not,
	// since 5 real seconds haven't passed regardless. Only checking past
	// that point actually distinguishes "killed early" from "still running
	// toward its own natural completion" (a mistake this test's own first
	// draft made, caught by the revert-proof below).
	time.Sleep(6 * time.Second)
	if _, err := os.Stat(startedMarker); err != nil {
		t.Fatalf("expected the fake provider to have started before the stop landed: %v", err)
	}
	if _, err := os.Stat(finishedMarker); err == nil {
		t.Fatal("expected the fake provider's process to have been killed mid-sleep once the run was marked stopped, but it ran to natural completion — contextWithStoredStop's cancellation did not take effect")
	}

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("expected an error once the stopped run canceled the in-flight teammate turn, got none (elapsed %v)", got.elapsed)
		}
	case <-time.After(8 * time.Second): // comfortably longer than WaitDelay's own grace period
		t.Fatal("team.wait did not return at all within 8s of the run being marked stopped")
	}
}

// TestTeamTasksCreatePrimitiveHonorsWorkflowStop is the regression test for
// the review finding that team.tasks.create()'s (and, by the identical
// fix, team.tasks.complete()'s) task_created gate call used the raw script
// context instead of contextWithStoredStop — the same gap team.wait had
// (see the test above). A paused/stopped workflow used to leave an
// in-flight gate verifier call running to its own timeout regardless.
func TestTeamTasksCreatePrimitiveHonorsWorkflowStop(t *testing.T) {
	t.Setenv("PALLIUM_WORKFLOW_PROVIDER", "claude") // see other gate tests' comment for why
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	team, err := store.CreateTeam("goal", tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamGate(team.ID, "reject anything vague", []string{"task_created"}); err != nil {
		t.Fatal(err)
	}

	startedMarker := filepath.Join(tmp, "gate-started")
	finishedMarker := filepath.Join(tmp, "gate-finished")
	path := filepath.Join(tmp, "fake-claude-slow-gate.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\ntouch '"+startedMarker+"'\nsleep 5\ntouch '"+finishedMarker+"'\nprintf '%s' '{\"result\":\"{\\\"approved\\\":true,\\\"reason\\\":\\\"ok\\\"}\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaudeCLI(t, path)

	script := `team.tasks.create("` + team.ID + `", "do the thing"); return "done";`
	scriptPath, err := WriteRunScript("wf-tasks-create-stop", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-tasks-create-stop", Task: "tasks create stop", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, execErr := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
		done <- execErr
	}()

	time.Sleep(100 * time.Millisecond)
	if err := store.SetRunStatus(run.ID, "stopped", "", "test-triggered-stop"); err != nil {
		t.Fatal(err)
	}

	// Same reasoning as TestTeamWaitPrimitiveHonorsWorkflowStop above: check
	// the marker file AFTER the fake gate's own 5s sleep would have
	// naturally elapsed, not before — checking earlier can't distinguish
	// "killed early" from "just hasn't gotten there yet" either way.
	time.Sleep(6 * time.Second)
	if _, err := os.Stat(startedMarker); err != nil {
		t.Fatalf("expected the fake gate verifier to have started before the stop landed: %v", err)
	}
	if _, err := os.Stat(finishedMarker); err == nil {
		t.Fatal("expected the fake gate verifier's process to have been killed mid-sleep once the run was marked stopped, but it ran to natural completion — contextWithStoredStop's cancellation did not take effect")
	}

	select {
	case execErr := <-done:
		if execErr == nil {
			t.Fatal("expected an error once the stopped run canceled the in-flight gate call, got none")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("team.tasks.create did not return at all within 8s of the run being marked stopped")
	}
}

// TestTeamCreatePrimitiveIsIdempotentAcrossResume is the regression test
// for the review finding that team.create() had no replay key, unlike
// agent()/gate() — a paused/resumed workflow re-executing the same script
// from the top would create a SECOND active team every time, orphaning the
// first one's state and spend. Simulated here by calling Execute twice on
// the identical Run (exactly what a real pause/resume replays): both calls
// must land on the same team id.
func TestTeamCreatePrimitiveIsIdempotentAcrossResume(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	script := `const t = team.create("ship the feature", {cwd: "` + strings.ReplaceAll(tmp, `\`, `\\`) + `"}); return t.id;`
	scriptPath, err := WriteRunScript("wf-team-create-resume", tmp, script)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(Run{ID: "wf-team-create-resume", Task: "team create resume", CWD: tmp, ScriptPath: scriptPath})
	if err != nil {
		t.Fatal(err)
	}

	firstID, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulates the resume: a fresh Runner (same as a new `pallium workflow
	// resume` process), same Run, same script — re-evaluates team.create()
	// from the top exactly once, same as the original call.
	secondID, err := (&Runner{Store: store, Run: run, MaxAgents: 10}).Execute(context.Background(), script, nil)
	if err != nil {
		t.Fatal(err)
	}

	if firstID != secondID {
		t.Fatalf("expected the resumed script's team.create() call to land on the SAME team id, got first=%q second=%q", firstID, secondID)
	}
	got, err := store.GetTeam(firstID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "ship the feature" {
		t.Fatalf("expected the original team's state intact (not recreated), got %+v", got)
	}
}
