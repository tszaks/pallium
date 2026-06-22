package db

const schema = `
CREATE TABLE IF NOT EXISTS repos (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  root TEXT NOT NULL UNIQUE,
  branch TEXT NOT NULL,
  last_indexed_commit TEXT,
  indexed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
  repo_id INTEGER NOT NULL,
  path TEXT NOT NULL,
  extension TEXT NOT NULL,
  churn_score INTEGER NOT NULL DEFAULT 0,
  recent_touch_count INTEGER NOT NULL DEFAULT 0,
  author_count INTEGER NOT NULL DEFAULT 0,
  last_touched_at TEXT NOT NULL DEFAULT '',
  exists_on_disk INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (repo_id, path)
);

CREATE TABLE IF NOT EXISTS commits (
  repo_id INTEGER NOT NULL,
  sha TEXT NOT NULL,
  author_name TEXT NOT NULL,
  author_email TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  subject TEXT NOT NULL,
  body TEXT NOT NULL,
  PRIMARY KEY (repo_id, sha)
);

CREATE TABLE IF NOT EXISTS file_commits (
  repo_id INTEGER NOT NULL,
  file_path TEXT NOT NULL,
  commit_sha TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, file_path, commit_sha)
);

CREATE TABLE IF NOT EXISTS cochange_edges (
  repo_id INTEGER NOT NULL,
  source_path TEXT NOT NULL,
  related_path TEXT NOT NULL,
  cochange_count INTEGER NOT NULL,
  recency_weight REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (repo_id, source_path, related_path)
);

CREATE TABLE IF NOT EXISTS decision_notes (
  repo_id INTEGER NOT NULL,
  source_type TEXT NOT NULL,
  source_ref TEXT NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, source_type, source_ref)
);

CREATE TABLE IF NOT EXISTS active_tasks (
  repo_id INTEGER PRIMARY KEY,
  goal TEXT NOT NULL,
  scope_paths TEXT NOT NULL,
  started_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS verification_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_id INTEGER NOT NULL,
  tier TEXT NOT NULL,
  command TEXT NOT NULL,
  exit_code INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  changed_files_json TEXT NOT NULL,
  cwd TEXT NOT NULL,
  ran_at TEXT NOT NULL
);
`
