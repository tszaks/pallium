package workflow

import (
	"path/filepath"
	"testing"
)

func TestCheckActiveRunCapacity(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateRun(Run{ID: "wf-active-1", Task: "one", CWD: tmp, ScriptPath: "workflow.js", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckActiveRunCapacity(store, 1); err == nil {
		t.Fatal("expected fleet limit error with one active run and max 1")
	}
	if err := CheckActiveRunCapacity(store, 2); err != nil {
		t.Fatalf("expected capacity with max 2, got %v", err)
	}
	if err := store.SetRunStatus("wf-active-1", "completed", "{}", ""); err != nil {
		t.Fatal(err)
	}
	if err := CheckActiveRunCapacity(store, 1); err != nil {
		t.Fatalf("expected capacity after completion, got %v", err)
	}
}
