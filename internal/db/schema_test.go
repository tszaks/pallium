package db

import (
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSchemaInitializes(t *testing.T) {
	repo := t.TempDir()
	store, err := OpenPath(repo, t.TempDir()+"/test.sqlite")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	defer store.Close()

	tables := []string{"repos", "files", "commits", "file_commits", "cochange_edges", "decision_notes", "active_tasks", "verification_runs"}
	for _, table := range tables {
		row := store.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table)
		var name string
		if err := row.Scan(&name); err != nil {
			t.Fatalf("expected table %s to exist: %v", table, err)
		}
	}
}

func TestSchemaMigratesExistingFilesTable(t *testing.T) {
	repo := t.TempDir()
	dbPath := t.TempDir() + "/test.sqlite"

	store, err := OpenPath(repo, dbPath)
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}

	if _, err := store.DB().Exec(`DROP TABLE files`); err != nil {
		t.Fatalf("drop files table: %v", err)
	}
	if _, err := store.DB().Exec(`
CREATE TABLE files (
  repo_id INTEGER NOT NULL,
  path TEXT NOT NULL,
  extension TEXT NOT NULL,
  churn_score INTEGER NOT NULL DEFAULT 0,
  recent_touch_count INTEGER NOT NULL DEFAULT 0,
  exists_on_disk INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (repo_id, path)
)`); err != nil {
		t.Fatalf("create legacy files table: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected sqlite db to exist: %v", err)
	}

	migrated, err := OpenPath(repo, dbPath)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	defer migrated.Close()

	columns := []string{"author_count", "last_touched_at"}
	for _, column := range columns {
		row := migrated.DB().QueryRow(`SELECT 1 FROM pragma_table_info('files') WHERE name = ?`, column)
		var found int
		if err := row.Scan(&found); err != nil {
			t.Fatalf("expected files.%s to exist after migration: %v", column, err)
		}
	}
}

func TestSchemaMigrationBackfillsNewFileSignals(t *testing.T) {
	repo := t.TempDir()
	dbPath := t.TempDir() + "/test.sqlite"

	store, err := OpenPath(repo, dbPath)
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}

	if _, err := store.DB().Exec(`DROP TABLE files`); err != nil {
		t.Fatalf("drop files table: %v", err)
	}
	if _, err := store.DB().Exec(`
CREATE TABLE files (
  repo_id INTEGER NOT NULL,
  path TEXT NOT NULL,
  extension TEXT NOT NULL,
  churn_score INTEGER NOT NULL DEFAULT 0,
  recent_touch_count INTEGER NOT NULL DEFAULT 0,
  exists_on_disk INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (repo_id, path)
)`); err != nil {
		t.Fatalf("create legacy files table: %v", err)
	}

	if _, err := store.DB().Exec(`INSERT INTO repos (id, root, branch, last_indexed_commit, indexed_at) VALUES (1, ?, 'main', 'abc123', '2026-03-13T12:00:00Z')`, repo); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO files (repo_id, path, extension, churn_score, recent_touch_count, exists_on_disk) VALUES (1, 'main.go', 'go', 4, 2, 1)`); err != nil {
		t.Fatalf("insert file: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO commits (repo_id, sha, author_name, author_email, committed_at, subject, body) VALUES
		(1, 'a1', 'One', 'one@example.com', '2026-03-10T12:00:00Z', 'first', ''),
		(1, 'b2', 'Two', 'two@example.com', '2026-03-12T12:00:00Z', 'second', '')`); err != nil {
		t.Fatalf("insert commits: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO file_commits (repo_id, file_path, commit_sha, committed_at) VALUES
		(1, 'main.go', 'a1', '2026-03-10T12:00:00Z'),
		(1, 'main.go', 'b2', '2026-03-12T12:00:00Z')`); err != nil {
		t.Fatalf("insert file commits: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	migrated, err := OpenPath(repo, dbPath)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	defer migrated.Close()

	row := migrated.DB().QueryRow(`SELECT author_count, last_touched_at FROM files WHERE repo_id = 1 AND path = 'main.go'`)
	var authorCount int
	var lastTouchedAt string
	if err := row.Scan(&authorCount, &lastTouchedAt); err != nil {
		t.Fatalf("read migrated file stats: %v", err)
	}

	if authorCount != 2 {
		t.Fatalf("expected author_count=2 after backfill, got %d", authorCount)
	}
	if lastTouchedAt != "2026-03-12T12:00:00Z" {
		t.Fatalf("expected last_touched_at to backfill from newest file commit, got %q", lastTouchedAt)
	}
}

func TestRepoReturnsHelpfulErrorWhenNotIndexedYet(t *testing.T) {
	repo := t.TempDir()
	store, err := OpenPath(repo, t.TempDir()+"/test.sqlite")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	defer store.Close()

	_, err = store.Repo()
	if !errors.Is(err, ErrRepoNotIndexed) {
		t.Fatalf("expected ErrRepoNotIndexed, got %v", err)
	}
}

func TestActiveTaskRoundTrip(t *testing.T) {
	repo := t.TempDir()
	store, err := OpenPath(repo, t.TempDir()+"/test.sqlite")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	defer store.Close()

	if _, err := store.DB().Exec(`INSERT INTO repos (root, branch, last_indexed_commit, indexed_at) VALUES (?, 'main', 'abc123', '2026-03-13T12:00:00Z')`, repo); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	task := ActiveTask{
		Goal:       "Tighten review output",
		ScopePaths: []string{"cmd/review.go", "internal/analysis"},
		StartedAt:  time.Date(2026, 3, 13, 12, 30, 0, 0, time.UTC),
	}
	if err := store.SaveActiveTask(task); err != nil {
		t.Fatalf("SaveActiveTask failed: %v", err)
	}

	saved, err := store.ActiveTask()
	if err != nil {
		t.Fatalf("ActiveTask failed: %v", err)
	}
	if saved.Goal != task.Goal {
		t.Fatalf("expected goal %q, got %q", task.Goal, saved.Goal)
	}
	if len(saved.ScopePaths) != 2 {
		t.Fatalf("expected scope paths, got %#v", saved.ScopePaths)
	}

	if err := store.ClearActiveTask(); err != nil {
		t.Fatalf("ClearActiveTask failed: %v", err)
	}
	if _, err := store.ActiveTask(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows after clear, got %v", err)
	}
}

func TestVerificationRunRoundTrip(t *testing.T) {
	repo := t.TempDir()
	store, err := OpenPath(repo, t.TempDir()+"/test.sqlite")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	defer store.Close()

	if _, err := store.DB().Exec(`INSERT INTO repos (root, branch, last_indexed_commit, indexed_at) VALUES (?, 'main', 'abc123', '2026-03-13T12:00:00Z')`, repo); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	saved, err := store.SaveVerificationRun(VerificationRun{
		Tier:         "fast",
		Command:      "go test ./...",
		ExitCode:     1,
		DurationMS:   250,
		ChangedFiles: []string{"main.go"},
		CWD:          repo,
		RanAt:        "2026-03-13T12:30:00Z",
	})
	if err != nil {
		t.Fatalf("SaveVerificationRun failed: %v", err)
	}
	if saved.ID == 0 {
		t.Fatalf("expected saved id, got %#v", saved)
	}

	runs, err := store.RecentVerificationRuns(5)
	if err != nil {
		t.Fatalf("RecentVerificationRuns failed: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one run, got %#v", runs)
	}
	if runs[0].Command != "go test ./..." || runs[0].ExitCode != 1 || len(runs[0].ChangedFiles) != 1 {
		t.Fatalf("unexpected run: %#v", runs[0])
	}
}
