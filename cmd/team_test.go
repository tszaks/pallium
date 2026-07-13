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

// TestTeamGateSetCanBeCleared is the regression test for the review finding
// that there was no supported way to turn a configured gate back off:
// --hooks "" used to be rejected as "no --hooks given" before it ever
// reached SetTeamGate's own empty-disables semantics.
func TestTeamGateSetCanBeCleared(t *testing.T) {
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
	if err := runTeam(&out, []string{"gate", "set", team.ID, "--hooks", "task_created", "no vague tasks", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var configured workflow.Team
	if err := json.Unmarshal(out.Bytes(), &configured); err != nil {
		t.Fatal(err)
	}
	if len(configured.GateHooks) == 0 {
		t.Fatalf("expected the gate hooks stored after configuring, got %+v", configured)
	}

	out.Reset()
	if err := runTeam(&out, []string{"gate", "set", team.ID, "--hooks", "", "--db", dbPath}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cleared") {
		t.Fatalf("expected a clear confirmation, got %q", out.String())
	}

	// Once cleared, a new task must land plain-pending with NO provider call
	// at all (teamGateHasHook is false, so CreateTeamTaskWithGate takes the
	// bare Store.CreateTeamTask branch) — this is what actually proves the
	// clear took effect, without this test needing to mock a real gate
	// verifier call itself (that's already covered by the workflow-package
	// gate tests).
	out.Reset()
	if err := runTeam(&out, []string{"tasks", "add", team.ID, "do the thing", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var task workflow.TeamTask
	if err := json.Unmarshal(out.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" {
		t.Fatalf("expected no gate to fire after clearing, got %+v", task)
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

// TestTeamMemberStopRestartRoundTrip covers the CLI routing for M2 PR B's
// individual supervision: stop an idle member (immediate, since nothing is
// in flight), then restart it back to schedulable.
func TestTeamMemberStopRestartRoundTrip(t *testing.T) {
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
	if err := runTeam(&out, []string{"spawn", team.ID, "worker-1", "--provider", "claude", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runTeam(&out, []string{"member", "stop", team.ID, "worker-1", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var stopped workflow.TeamMember
	if err := json.Unmarshal(out.Bytes(), &stopped); err != nil {
		t.Fatal(err)
	}
	if !stopped.StopRequested || stopped.Status != "stopped" {
		t.Fatalf("expected an idle member stopped immediately, got %+v", stopped)
	}

	out.Reset()
	if err := runTeam(&out, []string{"member", "restart", team.ID, "worker-1", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var restarted workflow.TeamMember
	if err := json.Unmarshal(out.Bytes(), &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.StopRequested || restarted.Status != "active" {
		t.Fatalf("expected the member schedulable again after restart, got %+v", restarted)
	}
}

// TestTeamMemberSteerDeliversDirective covers the CLI routing for steer:
// it must land as a real, retrievable mailbox message (same delivery path
// `team send` already uses), distinctly framed.
func TestTeamMemberSteerDeliversDirective(t *testing.T) {
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
	if err := runTeam(&out, []string{"spawn", team.ID, "worker-1", "--provider", "claude", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runTeam(&out, []string{"member", "steer", team.ID, "worker-1", "drop module B, focus on the auth bug", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var msg workflow.TeamMessage
	if err := json.Unmarshal(out.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.To != "worker-1" || !strings.Contains(msg.Body, "STEERING DIRECTIVE") || !strings.Contains(msg.Body, "drop module B") {
		t.Fatalf("expected a distinctly-framed steering message delivered to worker-1, got %+v", msg)
	}
}

// TestTeamStartWithTemplateSpawnsAllMembers is the round-trip test for M3's
// team templates: `team start --template` must actually spawn every
// templated member as a real, listable team member, not just describe them.
func TestTeamStartWithTemplateSpawnsAllMembers(t *testing.T) {
	dbPath := newTeamCmdTestDB(t)
	var out bytes.Buffer
	if err := runTeam(&out, []string{"start", "review the diff", "--template", "parallel-review", "--db", dbPath, "--cwd", t.TempDir()}, true); err != nil {
		t.Fatal(err)
	}
	var started struct {
		workflow.Team
		TemplateMembers []workflow.TeamMember `json:"template_members"`
	}
	if err := json.Unmarshal(out.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	tmpl, ok := workflow.TeamTemplate("parallel-review")
	if !ok {
		t.Fatal("expected the parallel-review template to exist")
	}
	if len(started.TemplateMembers) != len(tmpl.Members) {
		t.Fatalf("expected %d template members spawned, got %d: %+v", len(tmpl.Members), len(started.TemplateMembers), started.TemplateMembers)
	}
	for i, want := range tmpl.Members {
		got := started.TemplateMembers[i]
		if got.Name != want.Name || got.Mode != want.Mode {
			t.Fatalf("template member %d: expected name=%s mode=%s, got %+v", i, want.Name, want.Mode, got)
		}
	}

	out.Reset()
	if err := runTeam(&out, []string{"status", started.ID, "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var status struct {
		Members []workflow.TeamMember `json:"members"`
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Members) != len(tmpl.Members) {
		t.Fatalf("expected the template's members to be real, listable team members; got %d via team status", len(status.Members))
	}
}

func TestTeamStartWithUnknownTemplateErrors(t *testing.T) {
	dbPath := newTeamCmdTestDB(t)
	var out bytes.Buffer
	err := runTeam(&out, []string{"start", "goal", "--template", "does-not-exist", "--db", dbPath, "--cwd", t.TempDir()}, false)
	if err == nil {
		t.Fatal("expected an unknown template name to be rejected")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("expected the error to name the bad template, got %v", err)
	}
}

func TestTeamTemplateShowRendersMembers(t *testing.T) {
	var out bytes.Buffer
	if err := runTeam(&out, []string{"template", "show", "adversarial-debate"}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "advocate") || !strings.Contains(out.String(), "skeptic") {
		t.Fatalf("expected the rendered template to name its members, got %q", out.String())
	}
}
