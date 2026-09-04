package router

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouteChoosesLiveSessionsAndExplainsAlternatives(t *testing.T) {
	report, err := Route(context.Background(), Options{
		Task:      "find all running sessions on this computer",
		CWD:       t.TempDir(),
		Authority: AuthorityObserve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Service != "session-awareness" || report.Action != "inspect-live-sessions" || !report.Allowed {
		t.Fatalf("unexpected route: %+v", report)
	}
	if report.CapabilityID != "sessions-live" || report.Capability.ID != report.CapabilityID {
		t.Fatalf("route must expose its capability contract: %+v", report)
	}
	if got := strings.Join(report.CommandArgs, "\x00"); got != strings.Join([]string{"sessions", "live", "--running-only", "--details", "--json"}, "\x00") {
		t.Fatalf("unexpected structured command args: %q", got)
	}
	if !strings.Contains(report.Command, "sessions live") || len(report.Alternatives) < 2 || len(report.Caveats) == 0 {
		t.Fatalf("route must be actionable and honest about tradeoffs: %+v", report)
	}
}

func TestRouteNeverWidensAuthority(t *testing.T) {
	report, err := Route(context.Background(), Options{
		Task:      "implement a new API and verify it",
		CWD:       t.TempDir(),
		Authority: AuthorityObserve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.RequiredAuthority != AuthorityEdit || report.Allowed || report.BlockedReason == "" {
		t.Fatalf("edit recommendation must remain blocked by observe ceiling: %+v", report)
	}

	report, err = Route(context.Background(), Options{
		Task:      "implement a new API and verify it",
		CWD:       report.CWD,
		Authority: AuthorityEdit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Allowed || report.AuthorityCeiling != AuthorityEdit {
		t.Fatalf("explicit edit ceiling should allow the recommendation: %+v", report)
	}
}

func TestRouteInspectsRepositoryState(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	path := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-m", "initial")
	if err := os.WriteFile(path, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Route(context.Background(), Options{Task: "review the diff", CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Repository.IsGit || !report.Repository.Dirty || report.Repository.Head == "" {
		t.Fatalf("repository state was not inspected: %+v", report.Repository)
	}
}

func TestRouteDistinguishesReviewFromReviewAndFix(t *testing.T) {
	dir := t.TempDir()
	review, err := Route(context.Background(), Options{Task: "review the current routing changes", CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	if review.Service != "repo-intelligence" || review.Action != "review-current-change" || review.DecisionConfidence != "high" {
		t.Fatalf("unexpected review route: %+v", review)
	}
	fix, err := Route(context.Background(), Options{Task: "review and fix the routing changes", CWD: dir, Authority: AuthorityEdit})
	if err != nil {
		t.Fatal(err)
	}
	if fix.Service != "workflow" || !fix.Allowed {
		t.Fatalf("review-and-fix should route to an editing workflow: %+v", fix)
	}
}

func TestRouteChoosesNaturalSessionFind(t *testing.T) {
	report, err := Route(context.Background(), Options{Task: "Which sessions finished a few minutes ago?", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Action != "find-sessions-by-state-and-time" || !strings.Contains(report.Command, "sessions find") || !report.Allowed {
		t.Fatalf("unexpected session find route: %+v", report)
	}
}

func TestRouteRecognizesAddAsEditingWork(t *testing.T) {
	report, err := Route(context.Background(), Options{Task: "add native session recency queries, then release", CWD: t.TempDir(), Authority: AuthorityEdit})
	if err != nil {
		t.Fatal(err)
	}
	if report.Service != "workflow" || report.RequiredAuthority != AuthorityEdit || !report.Allowed {
		t.Fatalf("unexpected editing route: %+v", report)
	}
}

func TestRouteRejectsUnknownAuthority(t *testing.T) {
	_, err := Route(context.Background(), Options{Task: "inspect this", CWD: t.TempDir(), Authority: "unlimited"})
	if err == nil || !strings.Contains(err.Error(), "invalid authority") {
		t.Fatalf("expected invalid authority error, got %v", err)
	}
}

func TestCapabilitiesAreUniqueAndHaveValidAuthority(t *testing.T) {
	seen := map[string]bool{}
	for _, capability := range Capabilities() {
		if capability.ID == "" || capability.Service == "" || capability.Description == "" {
			t.Fatalf("incomplete capability: %+v", capability)
		}
		if seen[capability.ID] {
			t.Fatalf("duplicate capability id %q", capability.ID)
		}
		seen[capability.ID] = true
		if _, ok := authorityRank(capability.RequiredAuthority); !ok {
			t.Fatalf("invalid authority for %q: %q", capability.ID, capability.RequiredAuthority)
		}
		if len(capability.UseWhen) == 0 || len(capability.AvoidWhen) == 0 || len(capability.SuccessEvidence) == 0 {
			t.Fatalf("capability %q needs selection and success criteria: %+v", capability.ID, capability)
		}
	}
	for _, id := range []string{"sessions-live", "sessions-find", "sessions-recall", "repo-review", "repo-preflight", "verify-safe", "workflow-start", "loop-design", "team-start"} {
		if !seen[id] {
			t.Fatalf("missing routed capability %q", id)
		}
	}
}

func TestRouteCommandDisplayQuotesWithoutChangingArgs(t *testing.T) {
	task := "find session called Tyler's latest"
	report, err := Route(context.Background(), Options{Task: task, CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CommandArgs) < 3 || report.CommandArgs[2] != task {
		t.Fatalf("task was not preserved as one argument: %+v", report.CommandArgs)
	}
	if !strings.Contains(report.Command, "Tyler'\\''s") {
		t.Fatalf("display command was not safely quoted: %s", report.Command)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
