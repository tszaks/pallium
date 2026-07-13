package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tszaks/pallium/internal/workflow"
)

// M1 shipped with no cmd-level team tests at all (the CLI is a thin router
// onto internal/workflow, which carries the real coverage). This file adds
// smoke coverage for M2's new CLI surface specifically — reject, gate set,
// spawn --plan-required, and approve's plan-aware branch — since those are
// genuinely new routing, not just internal logic already tested elsewhere.

func newTeamCmdTestDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sessions.sqlite")
}

func TestTeamSpawnPlanRequiredForcesReadOnly(t *testing.T) {
	dbPath := newTeamCmdTestDB(t)
	var out bytes.Buffer
	if err := runTeam(&out, []string{"start", "goal", "--db", dbPath, "--cwd", t.TempDir()}, true); err != nil {
		t.Fatal(err)
	}
	var team workflow.Team
	if err := json.Unmarshal(out.Bytes(), &team); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runTeam(&out, []string{"spawn", team.ID, "planner-1", "--provider", "claude", "--mode", "edit", "--plan-required", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var member workflow.TeamMember
	if err := json.Unmarshal(out.Bytes(), &member); err != nil {
		t.Fatal(err)
	}
	if member.Mode != "read-only" {
		t.Fatalf("expected --plan-required to force read-only despite --mode edit, got %+v", member)
	}
	if !member.PlanRequired || member.PlanStatus != "pending" {
		t.Fatalf("expected plan_required/pending, got %+v", member)
	}
}

func TestTeamApproveRejectRoundTripViaCLI(t *testing.T) {
	dbPath := newTeamCmdTestDB(t)
	var out bytes.Buffer
	if err := runTeam(&out, []string{"start", "goal", "--db", dbPath, "--cwd", t.TempDir()}, true); err != nil {
		t.Fatal(err)
	}
	var team workflow.Team
	if err := json.Unmarshal(out.Bytes(), &team); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runTeam(&out, []string{"spawn", team.ID, "planner-1", "--provider", "claude", "--plan-required", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runTeam(&out, []string{"reject", team.ID, "planner-1", "narrow the scope", "--db", dbPath}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rejected") {
		t.Fatalf("expected rejection confirmation, got %q", out.String())
	}

	out.Reset()
	if err := runTeam(&out, []string{"approve", team.ID, "planner-1", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var approved workflow.TeamMember
	if err := json.Unmarshal(out.Bytes(), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Mode != "edit" || approved.PlanStatus != "approved" {
		t.Fatalf("expected approved member in edit mode, got %+v", approved)
	}

	// A plain (non-plan-required) member's approve must still be the simple
	// M1 mode-flip, unaffected by any of the above.
	out.Reset()
	if err := runTeam(&out, []string{"spawn", team.ID, "worker-1", "--provider", "claude", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runTeam(&out, []string{"approve", team.ID, "worker-1", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var plainApproved workflow.TeamMember
	if err := json.Unmarshal(out.Bytes(), &plainApproved); err != nil {
		t.Fatal(err)
	}
	if plainApproved.Mode != "edit" {
		t.Fatalf("expected plain non-plan-required approve to still flip to edit, got %+v", plainApproved)
	}
}

func TestTeamGateSetRejectsUnknownHookAndRoundTrips(t *testing.T) {
	dbPath := newTeamCmdTestDB(t)
	var out bytes.Buffer
	if err := runTeam(&out, []string{"start", "goal", "--db", dbPath, "--cwd", t.TempDir()}, true); err != nil {
		t.Fatal(err)
	}
	var team workflow.Team
	if err := json.Unmarshal(out.Bytes(), &team); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	err := runTeam(&out, []string{"gate", "set", team.ID, "--hooks", "not-a-hook", "prompt text", "--db", dbPath}, false)
	if err == nil {
		t.Fatal("expected an unknown hook name to be rejected")
	}

	out.Reset()
	if err := runTeam(&out, []string{"gate", "set", team.ID, "--hooks", "task_completed,teammate_idle", "verify quality", "--db", dbPath}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "task_completed") {
		t.Fatalf("expected confirmation naming the configured hooks, got %q", out.String())
	}
}

func TestTeamTasksAddRoutesThroughGate(t *testing.T) {
	dbPath := newTeamCmdTestDB(t)
	var out bytes.Buffer
	if err := runTeam(&out, []string{"start", "goal", "--db", dbPath, "--cwd", t.TempDir()}, true); err != nil {
		t.Fatal(err)
	}
	var team workflow.Team
	if err := json.Unmarshal(out.Bytes(), &team); err != nil {
		t.Fatal(err)
	}

	// No gate configured (M1 default): task lands pending exactly as before.
	out.Reset()
	if err := runTeam(&out, []string{"tasks", "add", team.ID, "do the thing", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var task workflow.TeamTask
	if err := json.Unmarshal(out.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" {
		t.Fatalf("expected a normal pending task with no gate configured, got %+v", task)
	}
}
