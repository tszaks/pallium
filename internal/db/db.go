package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	conn     *sql.DB
	RepoRoot string
	DBPath   string
}

var ErrRepoNotIndexed = errors.New("repo has not been indexed yet")

type RepoRecord struct {
	ID                int64
	Root              string
	Branch            string
	LastIndexedCommit string
	IndexedAt         time.Time
}

type FileStat struct {
	Path             string
	Extension        string
	ChurnScore       int
	RecentTouchCount int
	AuthorCount      int
	LastTouchedAt    time.Time
	ExistsOnDisk     bool
}

type CommitRecord struct {
	SHA         string
	AuthorName  string
	AuthorEmail string
	CommittedAt time.Time
	Subject     string
	Body        string
}

type CochangeEdge struct {
	SourcePath    string
	RelatedPath   string
	CochangeCount int
	RecencyWeight float64
}

type DecisionNote struct {
	SourceType  string
	SourceRef   string
	Title       string
	Body        string
	CommittedAt time.Time
}

type ActiveTask struct {
	Goal       string
	ScopePaths []string
	StartedAt  time.Time
}

type VerificationRun struct {
	ID           int64    `json:"id"`
	Tier         string   `json:"tier"`
	Command      string   `json:"command"`
	ExitCode     int      `json:"exit_code"`
	DurationMS   int64    `json:"duration_ms"`
	ChangedFiles []string `json:"changed_files"`
	CWD          string   `json:"cwd"`
	RanAt        string   `json:"ran_at"`
}

func Open(repoRoot string) (*Store, error) {
	dbPath := DefaultDBPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	return OpenPath(repoRoot, dbPath)
}

func DefaultDBPath(repoRoot string) string {
	current := filepath.Join(repoRoot, ".pallium", "pallium.sqlite")
	legacy := filepath.Join(repoRoot, ".codex-memory", "codex-memory.sqlite")
	if _, err := os.Stat(current); err == nil {
		return current
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return current
}

func OpenPath(repoRoot, dbPath string) (*Store, error) {
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)

	store := &Store{conn: conn, RepoRoot: repoRoot, DBPath: dbPath}
	if err := store.Init(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Init() error {
	pragmas := []string{
		"PRAGMA busy_timeout = 60000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	}
	for _, statement := range pragmas {
		if _, err := s.conn.Exec(statement); err != nil {
			return fmt.Errorf("initialize sqlite pragmas: %w", err)
		}
	}

	if _, err := s.conn.Exec(schema); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	if err := s.migrate(); err != nil {
		return err
	}
	return nil
}

func (s *Store) migrate() error {
	fileColumns := map[string]string{
		"author_count":    "ALTER TABLE files ADD COLUMN author_count INTEGER NOT NULL DEFAULT 0",
		"last_touched_at": "ALTER TABLE files ADD COLUMN last_touched_at TEXT NOT NULL DEFAULT ''",
	}

	existing, err := s.tableColumns("files")
	if err != nil {
		return fmt.Errorf("read files columns: %w", err)
	}

	for column, statement := range fileColumns {
		if _, ok := existing[column]; ok {
			continue
		}
		if _, err := s.conn.Exec(statement); err != nil {
			return fmt.Errorf("add files.%s column: %w", column, err)
		}
	}

	if _, err := s.conn.Exec(`
UPDATE files
SET author_count = COALESCE((
	SELECT COUNT(DISTINCT c.author_email)
	FROM file_commits fc
	JOIN commits c
	  ON c.repo_id = fc.repo_id AND c.sha = fc.commit_sha
	WHERE fc.repo_id = files.repo_id AND fc.file_path = files.path
), 0)
WHERE author_count = 0
`); err != nil {
		return fmt.Errorf("backfill files.author_count: %w", err)
	}

	if _, err := s.conn.Exec(`
UPDATE files
SET last_touched_at = COALESCE((
	SELECT MAX(fc.committed_at)
	FROM file_commits fc
	WHERE fc.repo_id = files.repo_id AND fc.file_path = files.path
), '')
WHERE last_touched_at = ''
`); err != nil {
		return fmt.Errorf("backfill files.last_touched_at: %w", err)
	}

	return nil
}

func (s *Store) tableColumns(table string) (map[string]struct{}, error) {
	rows, err := s.conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := make(map[string]struct{})
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			return nil, err
		}
		columns[name] = struct{}{}
	}

	return columns, rows.Err()
}

func (s *Store) Close() error {
	return s.conn.Close()
}

func (s *Store) DB() *sql.DB {
	return s.conn
}

func (s *Store) UpsertRepo(branch, lastIndexedCommit string, indexedAt time.Time) (RepoRecord, error) {
	if _, err := s.conn.Exec(`
INSERT INTO repos (root, branch, last_indexed_commit, indexed_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(root) DO UPDATE SET
  branch = excluded.branch,
  last_indexed_commit = excluded.last_indexed_commit,
  indexed_at = excluded.indexed_at
`, s.RepoRoot, branch, lastIndexedCommit, indexedAt.UTC().Format(time.RFC3339)); err != nil {
		return RepoRecord{}, fmt.Errorf("upsert repo: %w", err)
	}

	return s.Repo()
}

func (s *Store) Repo() (RepoRecord, error) {
	row := s.conn.QueryRow(`SELECT id, root, branch, COALESCE(last_indexed_commit, ''), indexed_at FROM repos WHERE root = ?`, s.RepoRoot)
	var repo RepoRecord
	var indexedAt string
	if err := row.Scan(&repo.ID, &repo.Root, &repo.Branch, &repo.LastIndexedCommit, &indexedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RepoRecord{}, fmt.Errorf("%w: run `pallium index` first", ErrRepoNotIndexed)
		}
		return RepoRecord{}, fmt.Errorf("read repo: %w", err)
	}
	repo.IndexedAt, _ = time.Parse(time.RFC3339, indexedAt)
	return repo, nil
}

func (s *Store) ResetRepoData(repoID int64) error {
	tables := []string{"files", "commits", "file_commits", "cochange_edges", "decision_notes", "active_tasks"}
	for _, table := range tables {
		if _, err := s.conn.Exec(fmt.Sprintf("DELETE FROM %s WHERE repo_id = ?", table), repoID); err != nil {
			return fmt.Errorf("reset %s: %w", table, err)
		}
	}
	return nil
}

func (s *Store) InsertCommit(repoID int64, commit CommitRecord) error {
	_, err := s.conn.Exec(`
INSERT OR REPLACE INTO commits (repo_id, sha, author_name, author_email, committed_at, subject, body)
VALUES (?, ?, ?, ?, ?, ?, ?)
`, repoID, commit.SHA, commit.AuthorName, commit.AuthorEmail, commit.CommittedAt.UTC().Format(time.RFC3339), commit.Subject, commit.Body)
	if err != nil {
		return fmt.Errorf("insert commit %s: %w", commit.SHA, err)
	}
	return nil
}

func (s *Store) InsertFileCommit(repoID int64, filePath, commitSHA string, committedAt time.Time) error {
	_, err := s.conn.Exec(`
INSERT OR REPLACE INTO file_commits (repo_id, file_path, commit_sha, committed_at)
VALUES (?, ?, ?, ?)
`, repoID, filePath, commitSHA, committedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert file commit %s -> %s: %w", filePath, commitSHA, err)
	}
	return nil
}

func (s *Store) UpsertFile(repoID int64, stat FileStat) error {
	exists := 0
	if stat.ExistsOnDisk {
		exists = 1
	}
	lastTouchedAt := ""
	if !stat.LastTouchedAt.IsZero() {
		lastTouchedAt = stat.LastTouchedAt.UTC().Format(time.RFC3339)
	}
	_, err := s.conn.Exec(`
INSERT OR REPLACE INTO files (repo_id, path, extension, churn_score, recent_touch_count, author_count, last_touched_at, exists_on_disk)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, repoID, stat.Path, stat.Extension, stat.ChurnScore, stat.RecentTouchCount, stat.AuthorCount, lastTouchedAt, exists)
	if err != nil {
		return fmt.Errorf("upsert file %s: %w", stat.Path, err)
	}
	return nil
}

func (s *Store) UpsertEdge(repoID int64, edge CochangeEdge) error {
	_, err := s.conn.Exec(`
INSERT OR REPLACE INTO cochange_edges (repo_id, source_path, related_path, cochange_count, recency_weight)
VALUES (?, ?, ?, ?, ?)
`, repoID, edge.SourcePath, edge.RelatedPath, edge.CochangeCount, edge.RecencyWeight)
	if err != nil {
		return fmt.Errorf("upsert edge %s -> %s: %w", edge.SourcePath, edge.RelatedPath, err)
	}
	return nil
}

func (s *Store) UpsertDecisionNote(repoID int64, note DecisionNote) error {
	_, err := s.conn.Exec(`
INSERT OR REPLACE INTO decision_notes (repo_id, source_type, source_ref, title, body, committed_at)
VALUES (?, ?, ?, ?, ?, ?)
`, repoID, note.SourceType, note.SourceRef, note.Title, note.Body, note.CommittedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("upsert decision note %s: %w", note.SourceRef, err)
	}
	return nil
}

func (s *Store) SaveActiveTask(task ActiveTask) error {
	repo, err := s.Repo()
	if err != nil {
		return err
	}

	scopeJSON, err := json.Marshal(task.ScopePaths)
	if err != nil {
		return fmt.Errorf("marshal task scope: %w", err)
	}

	startedAt := task.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	_, err = s.conn.Exec(`
INSERT INTO active_tasks (repo_id, goal, scope_paths, started_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(repo_id) DO UPDATE SET
  goal = excluded.goal,
  scope_paths = excluded.scope_paths,
  started_at = excluded.started_at
`, repo.ID, task.Goal, string(scopeJSON), startedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("save active task: %w", err)
	}
	return nil
}

func (s *Store) ActiveTask() (ActiveTask, error) {
	repo, err := s.Repo()
	if err != nil {
		return ActiveTask{}, err
	}

	row := s.conn.QueryRow(`SELECT goal, scope_paths, started_at FROM active_tasks WHERE repo_id = ?`, repo.ID)
	var task ActiveTask
	var scopeJSON string
	var startedAt string
	if err := row.Scan(&task.Goal, &scopeJSON, &startedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ActiveTask{}, sql.ErrNoRows
		}
		return ActiveTask{}, fmt.Errorf("read active task: %w", err)
	}

	if err := json.Unmarshal([]byte(scopeJSON), &task.ScopePaths); err != nil {
		return ActiveTask{}, fmt.Errorf("unmarshal task scope: %w", err)
	}
	task.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	return task, nil
}

func (s *Store) ClearActiveTask() error {
	repo, err := s.Repo()
	if err != nil {
		return err
	}
	if _, err := s.conn.Exec(`DELETE FROM active_tasks WHERE repo_id = ?`, repo.ID); err != nil {
		return fmt.Errorf("clear active task: %w", err)
	}
	return nil
}

func (s *Store) SaveVerificationRun(run VerificationRun) (VerificationRun, error) {
	repo, err := s.Repo()
	if err != nil {
		return VerificationRun{}, err
	}
	changedJSON, err := json.Marshal(run.ChangedFiles)
	if err != nil {
		return VerificationRun{}, fmt.Errorf("marshal changed files: %w", err)
	}
	ranAt := run.RanAt
	if ranAt == "" {
		ranAt = time.Now().UTC().Format(time.RFC3339)
	}
	result, err := s.conn.Exec(`
INSERT INTO verification_runs (repo_id, tier, command, exit_code, duration_ms, changed_files_json, cwd, ran_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, repo.ID, run.Tier, run.Command, run.ExitCode, run.DurationMS, string(changedJSON), run.CWD, ranAt)
	if err != nil {
		return VerificationRun{}, fmt.Errorf("save verification run: %w", err)
	}
	run.ID, _ = result.LastInsertId()
	run.RanAt = ranAt
	return run, nil
}

func (s *Store) RecentVerificationRuns(limit int) ([]VerificationRun, error) {
	repo, err := s.Repo()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.conn.Query(`
SELECT id, tier, command, exit_code, duration_ms, changed_files_json, cwd, ran_at
FROM verification_runs
WHERE repo_id = ?
ORDER BY ran_at DESC, id DESC
LIMIT ?
`, repo.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("read verification runs: %w", err)
	}
	defer rows.Close()

	runs := make([]VerificationRun, 0)
	for rows.Next() {
		var run VerificationRun
		var changedJSON string
		if err := rows.Scan(&run.ID, &run.Tier, &run.Command, &run.ExitCode, &run.DurationMS, &changedJSON, &run.CWD, &run.RanAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(changedJSON), &run.ChangedFiles)
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
