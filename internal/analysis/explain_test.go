package analysis

import (
	"testing"

	"github.com/tszaks/pallium/internal/index"
)

func TestExplain(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	report, err := Explain(store, "main.go")
	if err != nil {
		t.Fatalf("Explain failed: %v", err)
	}

	if len(report.RecentCommits) == 0 {
		t.Fatalf("expected recent commits in explain report")
	}

	if len(report.Decisions) == 0 {
		t.Fatalf("expected decision notes in explain report")
	}

	if report.Summary == "" {
		t.Fatalf("expected explain report summary")
	}

	if len(report.EditChecklist) == 0 {
		t.Fatalf("expected explain report checklist")
	}

	if len(report.SuggestedTests) == 0 {
		t.Fatalf("expected explain report suggested tests")
	}

	if len(report.BlastRadius) == 0 {
		t.Fatalf("expected explain report blast radius")
	}

	if len(report.StructuralLinks) == 0 {
		t.Fatalf("expected explain report structural links")
	}

	if len(report.TestCommands) == 0 {
		t.Fatalf("expected explain report test commands")
	}
	if len(report.Verification.Fast) == 0 {
		t.Fatalf("expected explain report verification plan")
	}

	if report.Confidence.Level == "" {
		t.Fatalf("expected explain confidence")
	}
	if report.ActionGuidance.RecommendedNextCommand == "" {
		t.Fatalf("expected explain recommended next command")
	}
	if !report.ActionGuidance.MustVerify {
		t.Fatalf("expected explain to require verification")
	}
	if report.Freshness.IndexedAt == "" {
		t.Fatalf("expected explain freshness metadata")
	}
	if len(report.Evidence.Sources) == 0 {
		t.Fatalf("expected explain evidence metadata")
	}
}
