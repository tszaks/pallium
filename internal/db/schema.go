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
  repo_id INTEGER NOT NULL,
  branch TEXT NOT NULL DEFAULT '',
  goal TEXT NOT NULL,
  scope_paths TEXT NOT NULL,
  started_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, branch)
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

-- Content index. Everything above is derived from git history; these tables
-- are derived from the files themselves, so a question like "what symbols
-- live here" or "who imports this" is a SQL lookup instead of a filesystem
-- scan. Keyed by content_sha so reindexing only reparses what changed.
CREATE TABLE IF NOT EXISTS code_files (
  repo_id INTEGER NOT NULL,
  path TEXT NOT NULL,
  lang TEXT NOT NULL,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  content_sha TEXT NOT NULL,
  symbol_count INTEGER NOT NULL DEFAULT 0,
  parser TEXT NOT NULL DEFAULT '',
  indexed_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, path)
);

CREATE TABLE IF NOT EXISTS code_symbols (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_id INTEGER NOT NULL,
  path TEXT NOT NULL,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  receiver TEXT NOT NULL DEFAULT '',
  signature TEXT NOT NULL DEFAULT '',
  doc TEXT NOT NULL DEFAULT '',
  start_line INTEGER NOT NULL DEFAULT 0,
  end_line INTEGER NOT NULL DEFAULT 0,
  exported INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_code_symbols_path ON code_symbols(repo_id, path);
CREATE INDEX IF NOT EXISTS idx_code_symbols_name ON code_symbols(repo_id, name);

-- External-content FTS over the symbol table. Triggers keep it in sync so a
-- per-file reindex never has to hunt down stale rows by hand.
CREATE VIRTUAL TABLE IF NOT EXISTS code_symbol_fts USING fts5(
  name,
  receiver,
  signature,
  doc,
  content='code_symbols',
  content_rowid='id'
);
CREATE TRIGGER IF NOT EXISTS code_symbols_fts_insert AFTER INSERT ON code_symbols BEGIN
  INSERT INTO code_symbol_fts(rowid, name, receiver, signature, doc)
  VALUES (new.id, new.name, new.receiver, new.signature, new.doc);
END;
CREATE TRIGGER IF NOT EXISTS code_symbols_fts_delete AFTER DELETE ON code_symbols BEGIN
  INSERT INTO code_symbol_fts(code_symbol_fts, rowid, name, receiver, signature, doc)
  VALUES ('delete', old.id, old.name, old.receiver, old.signature, old.doc);
END;

CREATE TABLE IF NOT EXISTS code_imports (
  repo_id INTEGER NOT NULL,
  from_path TEXT NOT NULL,
  raw_spec TEXT NOT NULL,
  to_path TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  external INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (repo_id, from_path, raw_spec)
);
CREATE INDEX IF NOT EXISTS idx_code_imports_to ON code_imports(repo_id, to_path);

-- Identifiers a file calls but does not define. Powers the callers command
-- and the stem-matching heuristic explain has always used, without rereading
-- the repo to answer either.
CREATE TABLE IF NOT EXISTS code_refs (
  repo_id INTEGER NOT NULL,
  path TEXT NOT NULL,
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL,
  PRIMARY KEY (repo_id, path, name)
);
CREATE INDEX IF NOT EXISTS idx_code_refs_name ON code_refs(repo_id, name);
CREATE INDEX IF NOT EXISTS idx_code_refs_name_lower ON code_refs(repo_id, name_lower);

-- Knowledge base. A doc is a synthesized description of one module, or a
-- deterministic one (the incident list) derived from history. cited_paths and
-- cited_symbols are not decoration: a doc is only marked verified when every
-- citation in it resolves against code_files and code_symbols, and claims
-- whose citations do not resolve are dropped into dropped_claims_json rather
-- than published.
CREATE TABLE IF NOT EXISTS knowledge_docs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_id INTEGER NOT NULL,
  slug TEXT NOT NULL,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  body_md TEXT NOT NULL DEFAULT '',
  cited_paths_json TEXT NOT NULL DEFAULT '[]',
  cited_symbols_json TEXT NOT NULL DEFAULT '[]',
  dropped_claims_json TEXT NOT NULL DEFAULT '[]',
  source_commit TEXT NOT NULL DEFAULT '',
  fingerprint TEXT NOT NULL DEFAULT '',
  generator TEXT NOT NULL DEFAULT '',
  verified INTEGER NOT NULL DEFAULT 0,
  generated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_knowledge_docs_slug ON knowledge_docs(repo_id, slug);

CREATE VIRTUAL TABLE IF NOT EXISTS knowledge_fts USING fts5(
  title,
  summary,
  body_md,
  content='knowledge_docs',
  content_rowid='id'
);
CREATE TRIGGER IF NOT EXISTS knowledge_docs_fts_insert AFTER INSERT ON knowledge_docs BEGIN
  INSERT INTO knowledge_fts(rowid, title, summary, body_md)
  VALUES (new.id, new.title, new.summary, new.body_md);
END;
CREATE TRIGGER IF NOT EXISTS knowledge_docs_fts_delete AFTER DELETE ON knowledge_docs BEGIN
  INSERT INTO knowledge_fts(knowledge_fts, rowid, title, summary, body_md)
  VALUES ('delete', old.id, old.title, old.summary, old.body_md);
END;
`
