package analysis

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/index"
)

func TestSafe(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	report, err := Safe(store, "main.go")
	if err != nil {
		t.Fatalf("Safe failed: %v", err)
	}

	if report.Verdict == "" {
		t.Fatalf("expected safe verdict")
	}
	if len(report.RequiredChecks) == 0 {
		t.Fatalf("expected required checks")
	}
	if len(report.SuggestedTests) == 0 {
		t.Fatalf("expected suggested tests")
	}
	if len(report.TestCommands) == 0 {
		t.Fatalf("expected safe test commands")
	}
	if len(report.Verification.Fast) == 0 {
		t.Fatalf("expected safe verification plan")
	}
	if report.Confidence.Level == "" {
		t.Fatalf("expected safe confidence")
	}
	if len(report.ActionGuidance.RunNext) == 0 {
		t.Fatalf("expected safe action guidance")
	}
	if report.ActionGuidance.RecommendedNextCommand == "" {
		t.Fatalf("expected safe recommended next command")
	}
	if !report.ActionGuidance.MustVerify {
		t.Fatalf("expected safe report to require verification")
	}
	if report.Freshness.IndexedAt == "" || len(report.Evidence.Sources) == 0 {
		t.Fatalf("expected safe freshness and evidence metadata")
	}
}

func TestPlan(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	report, err := Plan(store, "main.go")
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	if len(report.Steps) == 0 {
		t.Fatalf("expected plan steps")
	}
	if len(report.FilesToInspect) == 0 {
		t.Fatalf("expected files to inspect")
	}
	if len(report.TestCommands) == 0 {
		t.Fatalf("expected plan test commands")
	}
	if len(report.Verification.Fast) == 0 {
		t.Fatalf("expected plan verification plan")
	}
	if len(report.ActionGuidance.InspectFirst) == 0 {
		t.Fatalf("expected plan action guidance")
	}
	if report.ActionGuidance.RecommendedNextCommand == "" {
		t.Fatalf("expected plan recommended next command")
	}
	if report.Freshness.IndexedAt == "" || len(report.Evidence.Sources) == 0 {
		t.Fatalf("expected plan freshness and evidence metadata")
	}
}

func TestReview(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	report, err := Review(store, "HEAD~1")
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}

	if len(report.ChangedFiles) == 0 {
		t.Fatalf("expected changed files in review report")
	}
	if len(report.RequiredTests) == 0 {
		t.Fatalf("expected review to suggest focused tests")
	}
	if len(report.TestCommands) == 0 {
		t.Fatalf("expected review test commands")
	}
	if len(report.Verification.Fast) == 0 {
		t.Fatalf("expected review verification plan")
	}
	if len(report.ActionGuidance.RunNext) == 0 {
		t.Fatalf("expected review action guidance")
	}
	if report.ActionGuidance.RecommendedNextCommand == "" {
		t.Fatalf("expected review recommended next command")
	}
	if containsString(report.Confidence.Reasons, "Structural links were found in the repo.") &&
		containsString(report.ActionGuidance.ConfidenceGaps, "No structural links were found.") {
		t.Fatalf("review guidance contradicts structural-link confidence: %#v", report.ActionGuidance.ConfidenceGaps)
	}
	if !report.ActionGuidance.MustVerify {
		t.Fatalf("expected review to require verification")
	}
	if report.Freshness.IndexedAt == "" || len(report.Evidence.Sources) == 0 {
		t.Fatalf("expected review freshness and evidence metadata")
	}
	if len(report.ChangedFiles[0].BoundaryLabels) == 0 && report.ChangedFiles[0].RiskLevel == "low" {
		t.Fatalf("expected prioritized review output, got %#v", report.ChangedFiles)
	}
}

func TestReviewIncludesWorkingTreeChanges(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() { println(\"changed\") }\n")

	report, err := Review(store, "HEAD~1")
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}

	found := false
	for _, file := range report.ChangedFiles {
		if file.Path == "main.go" {
			found = true
			if file.ChangeSource == "" {
				t.Fatalf("expected working tree change source for main.go")
			}
		}
	}
	if !found {
		t.Fatalf("expected working tree change to appear in review report")
	}
}

func TestReviewInfersVerificationForUnknownChangedFiles(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	writeFile(t, filepath.Join(repo, "cmd", "newtool.go"), "package cmd\n\nfunc newTool() string { return \"ok\" }\n")
	writeFile(t, filepath.Join(repo, "cmd", "newtool_test.go"), "package cmd\n\nimport \"testing\"\n\nfunc TestNewTool(t *testing.T) {}\n")

	report, err := Review(store, "HEAD~1")
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}

	if !containsString(report.Verification.Fast, "go test ./cmd") {
		t.Fatalf("expected focused verification for new Go file, got %#v", report.Verification)
	}

	found := false
	for _, file := range report.ChangedFiles {
		if file.Path == "cmd/newtool.go" {
			found = true
			if len(file.SuggestedTests) == 0 {
				t.Fatalf("expected suggested tests for new file, got %#v", file)
			}
		}
	}
	if !found {
		t.Fatalf("expected cmd/newtool.go in review, got %#v", report.ChangedFiles)
	}
}

func TestChangedNow(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() { println(\"changed\") }\n")

	report, err := ChangedNow(store)
	if err != nil {
		t.Fatalf("ChangedNow failed: %v", err)
	}

	if len(report.Files) == 0 {
		t.Fatalf("expected changed-now files")
	}
	if report.Freshness.IndexedAt == "" || len(report.Evidence.Sources) == 0 {
		t.Fatalf("expected changed-now freshness and evidence metadata")
	}
}

func TestWorkflowPreflight(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() { println(\"changed\") }\n")

	report, err := WorkflowPreflight(store, "tighten workflow tests", []string{"main.go"})
	if err != nil {
		t.Fatalf("WorkflowPreflight failed: %v", err)
	}

	if report.Task != "tighten workflow tests" {
		t.Fatalf("expected task, got %#v", report.Task)
	}
	if len(report.ScopePaths) == 0 || report.ScopePaths[0] != "main.go" {
		t.Fatalf("expected main.go scope, got %#v", report.ScopePaths)
	}
	if len(report.Safe) == 0 {
		t.Fatalf("expected safe reports")
	}
	if len(report.FilesToInspect) == 0 {
		t.Fatalf("expected files to inspect")
	}
	if len(report.TestCommands) == 0 {
		t.Fatalf("expected test commands")
	}
	if len(report.AgentInstructions) == 0 {
		t.Fatalf("expected agent instructions")
	}
}

func TestHandoff(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() { println(\"changed\") }\n")

	report, err := Handoff(store, "HEAD~1")
	if err != nil {
		t.Fatalf("Handoff failed: %v", err)
	}

	if report.Summary == "" {
		t.Fatalf("expected handoff summary")
	}
	if len(report.NextActions) == 0 {
		t.Fatalf("expected handoff next actions")
	}
	if report.Freshness.IndexedAt == "" || len(report.Evidence.Sources) == 0 {
		t.Fatalf("expected handoff freshness and evidence metadata")
	}
}

func TestReviewDetectsTaskScopeDrift(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	if err := store.SaveActiveTask(db.ActiveTask{
		Goal:       "Adjust main entrypoint only",
		ScopePaths: []string{"main.go"},
		StartedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveActiveTask failed: %v", err)
	}

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() { println(\"changed\") }\n")
	writeFile(t, filepath.Join(repo, "config.yaml"), "key: drifted\n")

	report, err := Review(store, "HEAD~1")
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}

	if !report.Task.HasScopeDrift {
		t.Fatalf("expected task scope drift, got %#v", report.Task)
	}
	if len(report.Task.OutOfScopeChanged) == 0 {
		t.Fatalf("expected out-of-scope changes, got %#v", report.Task)
	}
	if !report.ActionGuidance.MustReview {
		t.Fatalf("expected scope drift to require review")
	}
	if len(report.ActionGuidance.StopSignals) == 0 {
		t.Fatalf("expected scope drift stop signals")
	}
	if len(report.ChangedFiles) < 3 {
		t.Fatalf("expected prioritized changed files, got %#v", report.ChangedFiles)
	}
	found := false
	for _, file := range report.ChangedFiles[:3] {
		if file.Path == "config.yaml" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected out-of-scope config drift near the top of review priorities, got %#v", report.ChangedFiles)
	}
}

func TestReviewPrioritizesSensitiveFilesFirst(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	report, err := Review(store, "HEAD~1")
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}

	if len(report.ChangedFiles) < 2 {
		t.Fatalf("expected multiple changed files, got %#v", report.ChangedFiles)
	}
	topTier := report.ChangedFiles[:3]
	found := false
	for _, file := range topTier {
		if file.Path == "config.yaml" && len(file.BoundaryLabels) > 0 && file.NeedsReview {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected config.yaml to stay in the top review tier with boundary labels, got %#v", report.ChangedFiles)
	}
}

func TestReviewFlagsLatestFailedVerification(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	if _, err := store.SaveVerificationRun(db.VerificationRun{
		Tier:         "fast",
		Command:      "go test .",
		ExitCode:     1,
		DurationMS:   25,
		ChangedFiles: []string{"main.go"},
		CWD:          repo,
		RanAt:        "2026-03-13T12:00:00Z",
	}); err != nil {
		t.Fatalf("SaveVerificationRun failed: %v", err)
	}

	report, err := Review(store, "HEAD~1")
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}

	if len(report.VerificationHistory) != 1 {
		t.Fatalf("expected verification history, got %#v", report.VerificationHistory)
	}
	if !containsString(report.ActionGuidance.StopSignals, "Most recent verification failed") {
		t.Fatalf("expected failed verification stop signal, got %#v", report.ActionGuidance.StopSignals)
	}
	if !containsString(report.Notes, "Most recent verification failed") {
		t.Fatalf("expected failed verification note, got %#v", report.Notes)
	}
}

func TestReviewDoesNotFlagOlderFailedVerificationAfterPass(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	if _, err := store.SaveVerificationRun(db.VerificationRun{
		Tier:         "fast",
		Command:      "go test .",
		ExitCode:     1,
		DurationMS:   25,
		ChangedFiles: []string{"main.go"},
		CWD:          repo,
		RanAt:        "2026-03-13T12:00:00Z",
	}); err != nil {
		t.Fatalf("SaveVerificationRun failed: %v", err)
	}
	if _, err := store.SaveVerificationRun(db.VerificationRun{
		Tier:         "fast",
		Command:      "go test .",
		ExitCode:     0,
		DurationMS:   30,
		ChangedFiles: []string{"main.go"},
		CWD:          repo,
		RanAt:        "2026-03-13T12:01:00Z",
	}); err != nil {
		t.Fatalf("SaveVerificationRun failed: %v", err)
	}

	report, err := Review(store, "HEAD~1")
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}

	if len(report.VerificationHistory) != 2 {
		t.Fatalf("expected verification history, got %#v", report.VerificationHistory)
	}
	if containsString(report.ActionGuidance.StopSignals, "Most recent verification failed") {
		t.Fatalf("did not expect stale failure stop signal, got %#v", report.ActionGuidance.StopSignals)
	}
	if containsString(report.Notes, "Most recent verification failed") {
		t.Fatalf("did not expect stale failure note, got %#v", report.Notes)
	}
}

func TestExplainMarksStaleIndex(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	run(t, repo, "git", "config", "user.name", "Stale User")
	run(t, repo, "git", "config", "user.email", "stale@example.com")
	writeFile(t, filepath.Join(repo, "README.md"), "# changed after index\n")
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "commit", "-m", "docs: drift after index")

	report, err := Explain(store, "main.go")
	if err != nil {
		t.Fatalf("Explain failed: %v", err)
	}

	if !report.Freshness.IsStale {
		t.Fatalf("expected stale freshness after new commit, got %#v", report.Freshness)
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func TestChangedNowWorksBeforeRepoIsIndexed(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	writeFile(t, filepath.Join(repo, "scratch.go"), "package main\n\nfunc scratch() {}\n")

	report, err := ChangedNow(store)
	if err != nil {
		t.Fatalf("ChangedNow failed before index: %v", err)
	}

	if report.IndexStatus != "missing" {
		t.Fatalf("expected missing index status, got %#v", report)
	}
	if report.RecommendedNextCommand != "pallium index" {
		t.Fatalf("expected index recommendation, got %#v", report)
	}
	if len(report.Files) != 1 || report.Files[0].Path != "scratch.go" || report.Files[0].RiskLevel != "unknown" {
		t.Fatalf("expected unknown scratch.go working-tree report, got %#v", report.Files)
	}
}
