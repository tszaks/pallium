package db

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func runGitDB(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
	}
}

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
		// Recent, not a fixed historical date: ActiveTask() now ages out
		// anything older than activeTaskStaleAfter (see
		// TestActiveTaskAgesOutAfterStaleWindow for that behavior itself).
		StartedAt: time.Now().UTC().Add(-1 * time.Minute),
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

// TestActiveTaskScopedPerBranch is the regression test for the P1 leak
// found dogfooding: two sessions working the SAME checkout path on
// DIFFERENT branches used to share one repo-only-keyed active_tasks row —
// whichever session ran `task start` most recently silently overwrote the
// other's task, and `workflow preflight` could present either session's
// task_scope depending on write order, not which one was actually asking.
func TestActiveTaskScopedPerBranch(t *testing.T) {
	repo := t.TempDir()
	runGitDB(t, repo, "init", "-q", "-b", "main")
	runGitDB(t, repo, "config", "user.email", "test@example.com")
	runGitDB(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDB(t, repo, "add", "f.txt")
	runGitDB(t, repo, "commit", "-q", "-m", "initial")
	runGitDB(t, repo, "checkout", "-q", "-b", "feature-a")
	runGitDB(t, repo, "checkout", "-q", "main")

	store, err := OpenPath(repo, filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`INSERT INTO repos (root, branch, last_indexed_commit, indexed_at) VALUES (?, 'main', 'abc123', '2026-03-13T12:00:00Z')`, repo); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Session on "main" starts a task.
	mainTask := ActiveTask{Goal: "main branch task", StartedAt: time.Now().UTC()}
	if err := store.SaveActiveTask(mainTask); err != nil {
		t.Fatal(err)
	}

	// A DIFFERENT session, same checkout, switches to "feature-a" — it must
	// see NO task at all (not main's), since it never started one on this branch.
	runGitDB(t, repo, "checkout", "-q", "feature-a")
	if _, err := store.ActiveTask(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected feature-a to see no leaked task from main, got err=%v", err)
	}
	featureTask := ActiveTask{Goal: "feature-a branch task", StartedAt: time.Now().UTC()}
	if err := store.SaveActiveTask(featureTask); err != nil {
		t.Fatal(err)
	}

	// Switching back to "main" must show MAIN's own task, untouched by
	// feature-a's save — not overwritten, not feature-a's task leaking back.
	runGitDB(t, repo, "checkout", "-q", "main")
	got, err := store.ActiveTask()
	if err != nil {
		t.Fatalf("expected main's own task to still be present, got err=%v", err)
	}
	if got.Goal != mainTask.Goal {
		t.Fatalf("expected main's task %q, got %q (feature-a's task leaked across branches)", mainTask.Goal, got.Goal)
	}
}

// TestActiveTaskAgesOutAfterStaleWindow is the second half of the leak fix:
// even ON the same branch, a task left over from a genuinely abandoned past
// session must not keep surfacing forever.
func TestActiveTaskAgesOutAfterStaleWindow(t *testing.T) {
	repo := t.TempDir()
	store, err := OpenPath(repo, filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`INSERT INTO repos (root, branch, last_indexed_commit, indexed_at) VALUES (?, 'main', 'abc123', '2026-03-13T12:00:00Z')`, repo); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	old := ActiveTask{Goal: "abandoned task", StartedAt: time.Now().UTC().Add(-activeTaskStaleAfter - time.Hour)}
	if err := store.SaveActiveTask(old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActiveTask(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected a task older than activeTaskStaleAfter to be aged out, got err=%v", err)
	}
}

// TestMigrateActiveTasksBranchScopingPreservesOldRows proves the migration
// from the original repo-only PRIMARY KEY to (repo_id, branch) doesn't lose
// data: an old-shaped row survives with branch='' (its true branch at
// migration time is unknowable — see the migration's own doc comment for
// why '' is a safe placeholder, not silent data loss).
func TestMigrateActiveTasksBranchScopingPreservesOldRows(t *testing.T) {
	repo := t.TempDir()
	store, err := OpenPath(repo, filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	defer store.Close()

	// Simulate a PRE-migration database: drop the (already new-shaped)
	// table and recreate it in the original single-column-PK shape, with a
	// row that predates this fix.
	if _, err := store.DB().Exec(`DROP TABLE active_tasks`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`CREATE TABLE active_tasks (repo_id INTEGER PRIMARY KEY, goal TEXT NOT NULL, scope_paths TEXT NOT NULL, started_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO active_tasks (repo_id, goal, scope_paths, started_at) VALUES (42, 'pre-migration task', '[]', ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	if err := store.migrateActiveTasksBranchScoping(); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	var goal, branch string
	row := store.DB().QueryRow(`SELECT goal, branch FROM active_tasks WHERE repo_id = 42`)
	if err := row.Scan(&goal, &branch); err != nil {
		t.Fatalf("expected the pre-migration row to survive, got: %v", err)
	}
	if goal != "pre-migration task" {
		t.Fatalf("expected the row's goal preserved, got %q", goal)
	}
	if branch != "" {
		t.Fatalf("expected the migrated row's branch to be the '' placeholder, got %q", branch)
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
