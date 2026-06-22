package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/db"
)

func TestCommandForVerificationTier(t *testing.T) {
	plan := analysis.VerificationPlan{
		Fast: []string{"go test ."},
		Safe: []string{"go test ./pkg"},
		Full: []string{"go test ./..."},
	}

	command, err := commandForVerificationTier(plan, "safe")
	if err != nil {
		t.Fatalf("commandForVerificationTier failed: %v", err)
	}
	if command != "go test ./pkg" {
		t.Fatalf("command=%q, want go test ./pkg", command)
	}
}

func TestCommandForVerificationTierRequiresInferredCommand(t *testing.T) {
	_, err := commandForVerificationTier(analysis.VerificationPlan{}, "fast")
	if err == nil {
		t.Fatal("expected missing command error")
	}
	if !strings.Contains(err.Error(), "no fast verification command inferred") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunVerificationTierRequiresIndexedRepoBeforeExecuting(t *testing.T) {
	store, err := db.OpenPath(t.TempDir(), t.TempDir()+"/test.sqlite")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	defer store.Close()

	_, err = runVerificationTier(store, "fast")
	if !errors.Is(err, db.ErrRepoNotIndexed) {
		t.Fatalf("expected ErrRepoNotIndexed, got %v", err)
	}
}
