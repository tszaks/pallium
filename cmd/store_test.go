package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenPalliumStoreUsesExplicitDBPathOverTestDBEnv verifies precedence:
// an explicit --db must always win over PALLIUM_TEST_DB. The safety net is
// only for the "forgot --db" case, never an override of deliberate intent.
func TestOpenPalliumStoreUsesExplicitDBPathOverTestDBEnv(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "explicit.sqlite")
	t.Setenv("PALLIUM_TEST_DB", filepath.Join(dir, "should-not-be-used.sqlite"))

	store, err := openPalliumStore(explicit)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := os.Stat(explicit); err != nil {
		t.Fatalf("expected the explicit --db path to be used, got: %v", err)
	}
}

// TestOpenPalliumStoreRedirectsToTestDBWhenNoExplicitPathGiven is the
// regression test for the hygiene fix: a test/dogfood session that forgets
// --db previously landed silently in the real, shared, global
// ~/.pallium/codex-sessions.sqlite (the exact incident that required a
// manual SQL cleanup during Agent Teams M1 development). With
// PALLIUM_TEST_DB set, an empty --db now redirects there instead. Applies
// to every service that shares this one store, not just teams — hence
// living alongside store.go rather than under a team-specific test file.
func TestOpenPalliumStoreRedirectsToTestDBWhenNoExplicitPathGiven(t *testing.T) {
	dir := t.TempDir()
	testDB := filepath.Join(dir, "throwaway.sqlite")
	t.Setenv("PALLIUM_TEST_DB", testDB)

	store, err := openPalliumStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := os.Stat(testDB); err != nil {
		t.Fatalf("expected PALLIUM_TEST_DB to be used when --db is empty, got: %v", err)
	}
}
