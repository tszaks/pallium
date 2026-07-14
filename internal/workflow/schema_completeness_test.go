package workflow

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseCreateTableColumnsHandlesNestedParens exercises the exact shape
// that makes a naive single-regex parse unsafe: team_members' UNIQUE(team_id,
// name) table constraint has its own parens before the block's real closing
// paren, and its own line must not be mistaken for a column declaration.
func TestParseCreateTableColumnsHandlesNestedParens(t *testing.T) {
	cols := parseCreateTableColumns(teamSchema)
	members := cols["team_members"]
	if len(members) == 0 {
		t.Fatalf("expected team_members columns, got none (tables found: %v)", keysOf(cols))
	}
	want := map[string]bool{"id": true, "team_id": true, "name": true, "spend_usd": true}
	got := map[string]bool{}
	for _, c := range members {
		got[c] = true
		if c == "UNIQUE" {
			t.Fatalf("UNIQUE(...) table constraint leaked into column list: %v", members)
		}
	}
	for c := range want {
		if !got[c] {
			t.Fatalf("expected column %q in team_members, got %v", c, members)
		}
	}
	if _, ok := cols["teams"]; !ok {
		t.Fatalf("expected teams table to also be parsed, got tables: %v", keysOf(cols))
	}
}

func keysOf(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestVerifySchemaCompletenessCatchesColumnOnlyInBaseCreateTable reproduces
// the real failure class this check exists for: a column present in a
// schema's base CREATE TABLE text but with no matching ALTER TABLE
// migration. That's invisible against a brand-new database (CREATE TABLE
// just includes it), but against a database where the table was already
// created under an older shape, CREATE TABLE IF NOT EXISTS is a no-op and
// the column never actually arrives.
func TestVerifySchemaCompletenessCatchesColumnOnlyInBaseCreateTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	// Pre-create team_members in an old shape: every real column except
	// spend_usd, which lives only in teamSchema's base CREATE TABLE text
	// with no ALTER TABLE fallback.
	if _, err := db.Exec(`CREATE TABLE team_members (
  id TEXT PRIMARY KEY,
  team_id TEXT NOT NULL,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  mode TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`); err != nil {
		t.Fatalf("create old-shaped table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close scratch db: %v", err)
	}

	store, err := Open(dbPath)
	if err == nil {
		store.Close()
		t.Fatalf("expected Open to fail on a table missing an expected column, got nil error")
	}
	if !strings.Contains(err.Error(), "team_members.spend_usd") {
		t.Fatalf("expected error naming team_members.spend_usd, got: %v", err)
	}
}

// TestVerifySchemaCompletenessPassesOnFreshDB pins the positive path: a
// brand-new database, where every CREATE TABLE runs for the first time, must
// open cleanly. (Every other test in this package also exercises this path
// via Open, so a regression here would fail the whole suite — this test just
// names the behavior explicitly.)
func TestVerifySchemaCompletenessPassesOnFreshDB(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatalf("Open on a fresh db should succeed, got: %v", err)
	}
	store.Close()
}
