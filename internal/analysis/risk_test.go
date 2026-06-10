package analysis

import (
	"testing"

	"github.com/tszaks/pallium/internal/index"
)

func TestRisk(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	report, err := Risk(store, "main.go")
	if err != nil {
		t.Fatalf("Risk failed: %v", err)
	}

	if report.Score <= 0 {
		t.Fatalf("expected positive risk score, got %+v", report)
	}

	if report.AuthorCount < 2 {
		t.Fatalf("expected author count to be indexed, got %+v", report)
	}

	if report.LastTouchedAt == "" {
		t.Fatalf("expected last touched timestamp in risk report")
	}

	if len(report.Reasons) == 0 {
		t.Fatalf("expected risk report to explain why this file matters")
	}
}
