package sessionmemory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const DefaultEmbeddingModel = "text-embedding-3-small"

const (
	maxStoredRawEventJSON        = 20_000
	maxStoredRawEventsPerSession = 100
	maxStoredMessageText         = 50_000
	maxStoredFirstUserText       = 4_000
	maxStoredCommandText         = 20_000
	maxStoredCommands            = 1_000
	maxStoredErrors              = 200
	maxSearchBlobText            = 1_000_000
	largeTranscriptHeadMessages  = 200
	largeTranscriptTailMessages  = 200
	activeRolloutQuietPeriod     = 2 * time.Minute
	defaultIndexSafetyBuffer     = 30 * time.Minute
)

var (
	largeTranscriptThresholdBytes int64 = 100 * 1024 * 1024
	errSessionTombstoned                = errors.New("session is tombstoned")
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis)://[^\s'"` + "`" + `]+`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passwd|authorization|bearer)\s*[:=]\s*['"]?([A-Za-z0-9_./+=:-]{12,})`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

var pathLikePattern = regexp.MustCompile(`(?:^|\s)(/[A-Za-z0-9._~+/@:-][^\s'"` + "`" + `<>]*)`)

var embedTexts = openAICompatibleEmbeddings

type rolloutSkipReason int

const (
	rolloutSkipNone rolloutSkipReason = iota
	rolloutSkipActive
	rolloutSkipUnchanged
)

type Options struct {
	DBPath         string
	CodexHome      string
	ClaudeHome     string
	Machine        string
	Provider       string
	Force          bool
	Since          time.Duration
	SafetyBuffer   time.Duration
	EmbeddingModel string
	StoreRawEvents bool
}

type Session struct {
	ID               string   `json:"id"`
	Machine          string   `json:"machine"`
	Title            string   `json:"title"`
	FirstUserMessage string   `json:"first_user_message"`
	LastAgentMessage string   `json:"last_agent_message"`
	CWD              string   `json:"cwd"`
	Source           string   `json:"source"`
	ModelProvider    string   `json:"model_provider"`
	Model            string   `json:"model"`
	CLIVersion       string   `json:"cli_version"`
	GitBranch        string   `json:"git_branch"`
	GitOriginURL     string   `json:"git_origin_url"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	TokensUsed       int64    `json:"tokens_used"`
	Status           string   `json:"status"`
	RolloutPath      string   `json:"rollout_path"`
	RolloutSHA256    string   `json:"rollout_sha256"`
	FilesTouched     []string `json:"files_touched"`
	Commands         []string `json:"commands"`
	ToolNames        []string `json:"tool_names"`
	Errors           []string `json:"errors"`
}

type Message struct {
	LineNo    int    `json:"line_no"`
	Timestamp string `json:"timestamp"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
}

type ParsedSession struct {
	Session     Session
	Messages    []Message
	RawEvents   []RawEvent
	EventCounts map[string]int
	SearchBlob  string
	Coverage    SessionCoverage

	messageCount int
	messageTail  []Message
	tailNext     int
	lastMessage  Message
	hasLast      bool
}

type SessionCoverage struct {
	Mode            string `json:"mode"`
	RawBytes        int64  `json:"raw_bytes"`
	MessagesSeen    int    `json:"messages_seen"`
	MessagesStored  int    `json:"messages_stored"`
	MessagesDropped int    `json:"messages_dropped"`
	Warning         string `json:"warning,omitempty"`
}

type RawEvent struct {
	LineNo      int
	Timestamp   string
	Type        string
	PayloadType string
	RawJSON     string
}

type SearchResult struct {
	Session
	Rank          float64         `json:"rank,omitempty"`
	Score         int             `json:"score,omitempty"`
	HybridScore   float64         `json:"hybrid_score,omitempty"`
	LexicalScore  float64         `json:"lexical_score,omitempty"`
	SemanticScore float64         `json:"semantic_score,omitempty"`
	Snippet       string          `json:"snippet,omitempty"`
	Signals       []string        `json:"signals,omitempty"`
	Citation      SearchCitation  `json:"citation"`
	Coverage      SessionCoverage `json:"coverage"`
	Warnings      []string        `json:"warnings"`
}

type SearchCitation struct {
	SessionID   string `json:"session_id"`
	LineNo      int    `json:"line_no,omitempty"`
	Source      string `json:"source"`
	UpdatedAt   string `json:"updated_at"`
	RolloutPath string `json:"rollout_path,omitempty"`
}

type SessionSearchOptions struct {
	DBPath               string
	Query                string
	Limit                int
	Hybrid               bool
	Model                string
	MinimumSemanticScore float64
	Source               string
	CWD                  string
	RepoRoot             string
	GitOriginURL         string
	Files                []string
	After                time.Time
	Before               time.Time
}

type SemanticResult struct {
	Score     float64 `json:"score"`
	SessionID string  `json:"session_id"`
	ChunkID   string  `json:"chunk_id"`
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	CWD       string  `json:"cwd"`
	UpdatedAt string  `json:"updated_at"`
	Snippet   string  `json:"snippet"`
}

type Stats struct {
	Sessions   int              `json:"sessions"`
	Events     int              `json:"events"`
	Messages   int              `json:"messages"`
	Chunks     int              `json:"chunks"`
	Embeddings int              `json:"embeddings"`
	Capsules   int              `json:"capsules"`
	Models     []EmbeddingModel `json:"models"`
}

type RelatedOptions struct {
	RepoRoot     string
	GitOriginURL string
	Files        []string
	Query        string
	Limit        int
}

type EmbeddingModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Dim      int    `json:"dim"`
	Count    int    `json:"count"`
}

type Store struct {
	db *sql.DB
}

func DefaultDBPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		current := filepath.Join(home, ".pallium", "codex-sessions.sqlite")
		legacy := filepath.Join(home, ".codex-memory", "codex-sessions.sqlite")
		if _, err := os.Stat(current); err == nil {
			return current
		}
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
		return current
	}
	return ".codex-sessions.sqlite"
}

func DefaultCodexHome() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex")
	}
	return ".codex"
}

func DefaultClaudeHome() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude")
	}
	return ".claude"
}

func Open(path string) (*Store, error) {
	useDefaultPath := path == ""
	if path == "" {
		path = DefaultDBPath()
	}
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	directoryCreated := os.IsNotExist(statErr)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if useDefaultPath || filepath.Clean(path) == filepath.Clean(DefaultDBPath()) || directoryCreated {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("secure session database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := hardenDatabaseFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func hardenDatabaseFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(candidate); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat session database file %s: %w", candidate, err)
		}
		if err := os.Chmod(candidate, 0o600); err != nil {
			return fmt.Errorf("secure session database file %s: %w", candidate, err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	for _, stmt := range []string{"PRAGMA busy_timeout=60000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS codex_sessions (
  id TEXT PRIMARY KEY,
  machine TEXT NOT NULL,
  title TEXT,
  first_user_message TEXT,
  last_agent_message TEXT,
  cwd TEXT,
  source TEXT,
  model_provider TEXT,
  model TEXT,
  cli_version TEXT,
  git_branch TEXT,
  git_origin_url TEXT,
  created_at TEXT,
  updated_at TEXT,
  indexed_at TEXT NOT NULL,
  tokens_used INTEGER DEFAULT 0,
  status TEXT,
  rollout_path TEXT,
  rollout_sha256 TEXT,
  event_counts_json TEXT,
  files_touched_json TEXT,
  commands_json TEXT,
  tool_names_json TEXT,
  errors_json TEXT,
  metadata_json TEXT
);
CREATE INDEX IF NOT EXISTS idx_codex_sessions_rollout_path ON codex_sessions(rollout_path);
CREATE TABLE IF NOT EXISTS codex_session_events (
  session_id TEXT NOT NULL,
  line_no INTEGER NOT NULL,
  timestamp TEXT,
  type TEXT,
  payload_type TEXT,
  raw_json TEXT NOT NULL,
  PRIMARY KEY(session_id, line_no)
);
CREATE TABLE IF NOT EXISTS codex_session_messages (
  session_id TEXT NOT NULL,
  line_no INTEGER NOT NULL,
  timestamp TEXT,
  role TEXT,
  kind TEXT,
  text TEXT,
  PRIMARY KEY(session_id, line_no)
);
CREATE VIRTUAL TABLE IF NOT EXISTS codex_session_fts USING fts5(
  session_id UNINDEXED,
  title,
  cwd,
  first_user_message,
  last_agent_message,
  files,
  commands,
  text
);
CREATE VIRTUAL TABLE IF NOT EXISTS codex_message_fts USING fts5(
  session_id UNINDEXED,
  line_no UNINDEXED,
  role UNINDEXED,
  kind UNINDEXED,
  text
);
CREATE TABLE IF NOT EXISTS codex_session_chunks (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  chunk_index INTEGER NOT NULL,
  kind TEXT NOT NULL,
  text TEXT NOT NULL,
  text_sha256 TEXT NOT NULL,
  token_estimate INTEGER DEFAULT 0,
  metadata_json TEXT
);
CREATE TABLE IF NOT EXISTS codex_session_embeddings (
  chunk_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  dim INTEGER NOT NULL,
  vector_blob BLOB NOT NULL,
  text_sha256 TEXT NOT NULL,
  embedded_at TEXT NOT NULL,
  PRIMARY KEY(chunk_id, provider, model)
);
CREATE TABLE IF NOT EXISTS codex_session_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS codex_session_capsules (
  session_id TEXT PRIMARY KEY,
  schema_version INTEGER NOT NULL,
  capsule_json TEXT NOT NULL,
  search_text TEXT NOT NULL,
  generated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS codex_session_tombstones (
  session_id TEXT PRIMARY KEY,
  source TEXT,
  rollout_path TEXT,
  deleted_at TEXT NOT NULL,
  reason TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_codex_session_tombstones_rollout_path
  ON codex_session_tombstones(rollout_path)
  WHERE rollout_path IS NOT NULL AND rollout_path != '';
`)
	return err
}

func Index(ctx context.Context, opts Options, include []string) (int, error) {
	if opts.CodexHome == "" {
		opts.CodexHome = DefaultCodexHome()
	}
	if opts.ClaudeHome == "" {
		opts.ClaudeHome = DefaultClaudeHome()
	}
	if opts.Machine == "" {
		host, _ := os.Hostname()
		opts.Machine = host
	}
	provider := strings.ToLower(strings.TrimSpace(opts.Provider))
	if provider == "" {
		provider = "all"
	}
	if provider != "all" && provider != "codex" && provider != "claude" {
		return 0, fmt.Errorf("unsupported session provider %q (want codex, claude, or all)", opts.Provider)
	}
	store, err := Open(opts.DBPath)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	count := 0
	cutoff := store.indexCutoff(opts)
	if provider == "all" || provider == "codex" {
		state := loadStateMetadata(filepath.Join(opts.CodexHome, "state_5.sqlite"))
		files := findRollouts(filepath.Join(opts.CodexHome, "sessions"), include, cutoff)
		archiveCutoff := time.Time{}
		if !opts.Force && opts.Since > 0 {
			archiveCutoff = time.Now().Add(-opts.Since)
		}
		files = mergeSessionPaths(files, findRollouts(filepath.Join(opts.CodexHome, "archived_sessions"), nil, archiveCutoff))
		for _, file := range files {
			select {
			case <-ctx.Done():
				return count, ctx.Err()
			default:
			}
			tombstoned, err := store.sessionTombstoned("", file)
			if err != nil {
				return count, err
			}
			if tombstoned {
				continue
			}
			skip, err := store.rolloutSkipReason(file, opts.Force)
			if err != nil {
				return count, err
			}
			if skip == rolloutSkipActive {
				continue
			}
			if skip == rolloutSkipUnchanged {
				stateFresh, err := store.codexStateFreshForRollout(file, state)
				if err != nil {
					return count, err
				}
				if stateFresh {
					continue
				}
			}
			parsed, err := parseRollout(file)
			if err != nil {
				return count, fmt.Errorf("parse %s: %w", file, err)
			}
			tombstoned, err = store.sessionTombstoned(parsed.Session.ID, file)
			if err != nil {
				return count, err
			}
			if tombstoned {
				continue
			}
			mergeState(&parsed.Session, state[parsed.Session.ID])
			parsed.Session.Machine = opts.Machine
			parsed.Session.Source = first(parsed.Session.Source, "codex")
			if !opts.StoreRawEvents {
				parsed.RawEvents = nil
			}
			if err := store.upsert(parsed, state[parsed.Session.ID]); err != nil {
				if errors.Is(err, errSessionTombstoned) {
					continue
				}
				return count, err
			}
			count++
		}
	}
	if provider == "all" || provider == "claude" {
		files := findClaudeTranscripts(filepath.Join(opts.ClaudeHome, "projects"), include, cutoff)
		for _, file := range files {
			select {
			case <-ctx.Done():
				return count, ctx.Err()
			default:
			}
			tombstoned, err := store.sessionTombstoned("", file)
			if err != nil {
				return count, err
			}
			if tombstoned {
				continue
			}
			skip, err := store.rolloutSkipReason(file, opts.Force)
			if err != nil {
				return count, err
			}
			if skip != rolloutSkipNone {
				continue
			}
			parsed, err := parseClaudeTranscript(file)
			if err != nil {
				return count, fmt.Errorf("parse %s: %w", file, err)
			}
			tombstoned, err = store.sessionTombstoned(parsed.Session.ID, file)
			if err != nil {
				return count, err
			}
			if tombstoned {
				continue
			}
			parsed.Session.Machine = opts.Machine
			if !opts.StoreRawEvents {
				parsed.RawEvents = nil
			}
			if err := store.upsert(parsed, map[string]any{"provider": "claude"}); err != nil {
				if errors.Is(err, errSessionTombstoned) {
					continue
				}
				return count, err
			}
			count++
		}
	}
	return count, nil
}

func (s *Store) sessionTombstoned(sessionID, rolloutPath string) (bool, error) {
	return querySessionTombstone(s.db, sessionID, rolloutPath)
}

type sessionTombstoneQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

type sessionSQLTransaction interface {
	sessionTombstoneQuerier
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

type immediateSessionTransaction struct {
	ctx  context.Context
	conn *sql.Conn
	done bool
}

func (s *Store) beginImmediateSessionTransaction() (*immediateSessionTransaction, error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &immediateSessionTransaction{ctx: ctx, conn: conn}, nil
}

func (tx *immediateSessionTransaction) Exec(query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(tx.ctx, query, args...)
}

func (tx *immediateSessionTransaction) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(tx.ctx, query, args...)
}

func (tx *immediateSessionTransaction) QueryRow(query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(tx.ctx, query, args...)
}

func (tx *immediateSessionTransaction) Commit() error {
	if tx.done {
		return sql.ErrTxDone
	}
	if _, err := tx.conn.ExecContext(tx.ctx, `COMMIT`); err != nil {
		return err
	}
	tx.done = true
	return tx.conn.Close()
}

func (tx *immediateSessionTransaction) Rollback() error {
	if tx.done {
		return nil
	}
	_, err := tx.conn.ExecContext(tx.ctx, `ROLLBACK`)
	tx.done = true
	closeErr := tx.conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func querySessionTombstone(querier sessionTombstoneQuerier, sessionID, rolloutPath string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	rolloutPath = strings.TrimSpace(rolloutPath)
	if rolloutPath != "" {
		rolloutPath = filepath.Clean(rolloutPath)
	}
	if sessionID == "" && rolloutPath == "" {
		return false, nil
	}
	var count int
	err := querier.QueryRow(`SELECT COUNT(*) FROM codex_session_tombstones
		WHERE (? != '' AND session_id=?) OR (? != '' AND rollout_path=?)`, sessionID, sessionID, rolloutPath, rolloutPath).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check session deletion tombstone: %w", err)
	}
	return count > 0, nil
}

func (s *Store) indexCutoff(opts Options) time.Time {
	if opts.Force {
		return time.Time{}
	}
	if opts.Since > 0 {
		return time.Now().Add(-opts.Since)
	}
	model := resolveEmbeddingModel(opts.EmbeddingModel)
	cursor := s.embeddingCursor(model)
	if cursor.IsZero() {
		return time.Time{}
	}
	buffer := opts.SafetyBuffer
	if buffer <= 0 {
		buffer = defaultIndexSafetyBuffer
	}
	return cursor.Add(-buffer)
}

func (s *Store) embeddingCursor(model string) time.Time {
	key := embeddingCursorKey(model)
	row := s.db.QueryRow(`SELECT value FROM codex_session_state WHERE key=?`, key)
	var raw string
	if err := row.Scan(&raw); err == nil {
		return parseSessionTime(raw)
	}
	row = s.db.QueryRow(`SELECT MAX(embedded_at) FROM codex_session_embeddings WHERE provider=? AND model=?`, embeddingProvider(), model)
	var latest sql.NullString
	if err := row.Scan(&latest); err == nil && latest.Valid {
		return parseSessionTime(latest.String)
	}
	return time.Time{}
}

func (s *Store) setEmbeddingCursor(model string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO codex_session_state(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		embeddingCursorKey(model), at.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func embeddingCursorKey(model string) string {
	return "last_successful_embed_at:" + embeddingProvider() + ":" + model
}

func (s *Store) rolloutSkipReason(path string, force bool) (rolloutSkipReason, error) {
	if force {
		return rolloutSkipNone, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return rolloutSkipNone, fmt.Errorf("stat %s: %w", path, err)
	}
	if time.Since(info.ModTime()) < activeRolloutQuietPeriod {
		return rolloutSkipActive, nil
	}
	row := s.db.QueryRow(`SELECT rollout_sha256, indexed_at FROM codex_sessions WHERE rollout_path=? LIMIT 1`, path)
	var storedSHA string
	var indexedAt string
	switch err := row.Scan(&storedSHA, &indexedAt); err {
	case nil:
	case sql.ErrNoRows:
		return rolloutSkipNone, nil
	default:
		return rolloutSkipNone, fmt.Errorf("lookup indexed rollout %s: %w", path, err)
	}

	indexedTime := parseSessionTime(indexedAt)
	if storedSHA != "" && !indexedTime.IsZero() && !info.ModTime().After(indexedTime) {
		return rolloutSkipUnchanged, nil
	}

	sha, err := fileSHA(path)
	if err != nil {
		return rolloutSkipNone, fmt.Errorf("hash %s: %w", path, err)
	}
	if storedSHA != "" && storedSHA == sha {
		return rolloutSkipUnchanged, nil
	}
	return rolloutSkipNone, nil
}

func (s *Store) codexStateFreshForRollout(path string, state map[string]map[string]any) (bool, error) {
	if len(state) == 0 {
		return true, nil
	}
	row := s.db.QueryRow(`SELECT id, indexed_at FROM codex_sessions WHERE rollout_path=? LIMIT 1`, path)
	var id string
	var indexedAt string
	switch err := row.Scan(&id, &indexedAt); err {
	case nil:
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, fmt.Errorf("lookup indexed rollout %s: %w", path, err)
	}
	metadata := state[id]
	if len(metadata) == 0 {
		return true, nil
	}
	stateUpdatedAt := parseSessionTime(isoAny(metadata["updated_at_ms"]))
	indexedTime := parseSessionTime(indexedAt)
	if stateUpdatedAt.IsZero() || indexedTime.IsZero() {
		return true, nil
	}
	return !stateUpdatedAt.After(indexedTime), nil
}

func parseSessionTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		value, _ = time.Parse(time.RFC3339, raw)
	}
	return value
}

func (s *Store) upsert(parsed ParsedSession, metadata map[string]any) error {
	sess := parsed.Session
	if sess.Title == "" {
		sess.Title = short(sess.FirstUserMessage, 240)
	}
	if sess.Status == "" {
		sess.Status = "seen"
	}
	sess = sanitizeSession(sess)
	parsed.Session = sess
	parsed.SearchBlob = redact(parsed.SearchBlob)
	capsule := buildSessionCapsule(parsed)
	chunks := buildContinuityChunks(parsed, capsule)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.beginImmediateSessionTransaction()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tombstoned, err := querySessionTombstone(tx, sess.ID, sess.RolloutPath)
	if err != nil {
		return err
	}
	if tombstoned {
		return errSessionTombstoned
	}
	if err := removeChangedSessionEmbeddings(tx, sess.ID, chunks); err != nil {
		return err
	}
	for _, stmt := range []string{
		"DELETE FROM codex_session_events WHERE session_id=?",
		"DELETE FROM codex_session_messages WHERE session_id=?",
		"DELETE FROM codex_session_chunks WHERE session_id=?",
		"DELETE FROM codex_session_fts WHERE session_id=?",
		"DELETE FROM codex_message_fts WHERE session_id=?",
		"DELETE FROM codex_session_capsules WHERE session_id=?",
	} {
		if _, err := tx.Exec(stmt, sess.ID); err != nil {
			return err
		}
	}
	j := func(v any) string { b, _ := json.Marshal(redactObj(v)); return string(b) }
	_, err = tx.Exec(`INSERT INTO codex_sessions(id,machine,title,first_user_message,last_agent_message,cwd,source,model_provider,model,cli_version,git_branch,git_origin_url,created_at,updated_at,indexed_at,tokens_used,status,rollout_path,rollout_sha256,event_counts_json,files_touched_json,commands_json,tool_names_json,errors_json,metadata_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET machine=excluded.machine,title=excluded.title,first_user_message=excluded.first_user_message,last_agent_message=excluded.last_agent_message,cwd=excluded.cwd,source=excluded.source,model_provider=excluded.model_provider,model=excluded.model,cli_version=excluded.cli_version,git_branch=excluded.git_branch,git_origin_url=excluded.git_origin_url,created_at=excluded.created_at,updated_at=excluded.updated_at,indexed_at=excluded.indexed_at,tokens_used=excluded.tokens_used,status=excluded.status,rollout_path=excluded.rollout_path,rollout_sha256=excluded.rollout_sha256,event_counts_json=excluded.event_counts_json,files_touched_json=excluded.files_touched_json,commands_json=excluded.commands_json,tool_names_json=excluded.tool_names_json,errors_json=excluded.errors_json,metadata_json=excluded.metadata_json`,
		sess.ID, sess.Machine, sess.Title, sess.FirstUserMessage, sess.LastAgentMessage, sess.CWD, sess.Source, sess.ModelProvider, sess.Model, sess.CLIVersion, sess.GitBranch, sess.GitOriginURL, sess.CreatedAt, sess.UpdatedAt, now, sess.TokensUsed, sess.Status, sess.RolloutPath, sess.RolloutSHA256, j(parsed.EventCounts), j(sess.FilesTouched), j(sess.Commands), j(sess.ToolNames), j(sess.Errors), j(metadata))
	if err != nil {
		return err
	}
	for _, e := range parsed.RawEvents {
		if _, err := tx.Exec(`INSERT INTO codex_session_events(session_id,line_no,timestamp,type,payload_type,raw_json) VALUES(?,?,?,?,?,?)`, sess.ID, e.LineNo, e.Timestamp, e.Type, e.PayloadType, e.RawJSON); err != nil {
			return err
		}
	}
	for _, m := range parsed.Messages {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO codex_session_messages(session_id,line_no,timestamp,role,kind,text) VALUES(?,?,?,?,?,?)`, sess.ID, m.LineNo, m.Timestamp, m.Role, m.Kind, redact(m.Text)); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO codex_message_fts(session_id,line_no,role,kind,text) VALUES(?,?,?,?,?)`, sess.ID, m.LineNo, m.Role, m.Kind, redact(m.Text)); err != nil {
			return err
		}
	}
	searchText := strings.Join([]string{parsed.SearchBlob, capsuleSearchText(capsule)}, "\n")
	if _, err := tx.Exec(`INSERT INTO codex_session_fts(session_id,title,cwd,first_user_message,last_agent_message,files,commands,text) VALUES(?,?,?,?,?,?,?,?)`, sess.ID, sess.Title, sess.CWD, sess.FirstUserMessage, sess.LastAgentMessage, strings.Join(sess.FilesTouched, "\n"), strings.Join(sess.Commands, "\n"), searchText); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := tx.Exec(`INSERT INTO codex_session_chunks(id,session_id,chunk_index,kind,text,text_sha256,token_estimate,metadata_json) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.SessionID, c.Index, c.Kind, c.Text, c.TextSHA256, c.TokenEstimate, j(c.Metadata)); err != nil {
			return err
		}
	}
	capsuleJSON, err := json.Marshal(capsule)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO codex_session_capsules(session_id,schema_version,capsule_json,search_text,generated_at) VALUES(?,?,?,?,?)
ON CONFLICT(session_id) DO UPDATE SET schema_version=excluded.schema_version,capsule_json=excluded.capsule_json,search_text=excluded.search_text,generated_at=excluded.generated_at`, sess.ID, capsule.SchemaVersion, string(capsuleJSON), capsuleSearchText(capsule), capsule.GeneratedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func removeChangedSessionEmbeddings(tx sessionSQLTransaction, sessionID string, chunks []chunkRecord) error {
	currentHashes := make(map[string]string, len(chunks))
	for _, chunk := range chunks {
		currentHashes[chunk.ID] = chunk.TextSHA256
	}
	rows, err := tx.Query(`SELECT e.chunk_id,e.provider,e.model,e.text_sha256,c.text_sha256
		FROM codex_session_embeddings e
		JOIN codex_session_chunks c ON c.id=e.chunk_id
		WHERE c.session_id=?`, sessionID)
	if err != nil {
		return err
	}
	type embeddingKey struct {
		chunkID  string
		provider string
		model    string
	}
	var remove []embeddingKey
	for rows.Next() {
		var key embeddingKey
		var embeddingHash, oldChunkHash string
		if err := rows.Scan(&key.chunkID, &key.provider, &key.model, &embeddingHash, &oldChunkHash); err != nil {
			_ = rows.Close()
			return err
		}
		newHash, exists := currentHashes[key.chunkID]
		if !exists || embeddingHash != newHash || oldChunkHash != newHash {
			remove = append(remove, key)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, key := range remove {
		if _, err := tx.Exec(`DELETE FROM codex_session_embeddings WHERE chunk_id=? AND provider=? AND model=?`, key.chunkID, key.provider, key.model); err != nil {
			return err
		}
	}
	return nil
}

func List(limit int) ([]Session, error) {
	store, err := Open("")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.list(limit)
}

func (s *Store) list(limit int) ([]Session, error) {
	rows, err := s.db.Query(`SELECT id,machine,title,first_user_message,last_agent_message,cwd,source,model_provider,model,cli_version,git_branch,git_origin_url,created_at,updated_at,tokens_used,status,rollout_path,rollout_sha256,files_touched_json,commands_json,tool_names_json,errors_json FROM codex_sessions ORDER BY COALESCE(updated_at, created_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) listAll() ([]Session, error) {
	return s.list(1000000)
}

func Grep(query string, limit int) ([]map[string]any, error) {
	store, err := Open("")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	terms := uniqueStrings(searchTermPattern.FindAllString(strings.ToLower(query), -1), 24)
	if len(terms) == 0 {
		return []map[string]any{}, nil
	}
	results, err := store.grepMessages(quotedFTSTerms(terms, " AND "), limit)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 && len(terms) > 1 {
		return store.grepMessages(quotedFTSTerms(terms, " OR "), limit)
	}
	return results, nil
}

func (s *Store) grepMessages(expression string, limit int) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT m.session_id,m.line_no,m.role,m.kind,m.text,s.title FROM codex_message_fts f JOIN codex_session_messages m ON m.session_id=f.session_id AND m.line_no=f.line_no JOIN codex_sessions s ON s.id=m.session_id WHERE codex_message_fts MATCH ? ORDER BY bm25(codex_message_fts) LIMIT ?`, expression, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var sid, role, kind, text, title string
		var line int
		if err := rows.Scan(&sid, &line, &role, &kind, &text, &title); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"session_id": sid, "line_no": line, "role": role, "kind": kind, "title": title, "snippet": short(text, 500)})
	}
	return out, rows.Err()
}

func Show(id string, transcript bool) (Session, []Message, error) {
	return ShowPath("", id, transcript)
}

func ShowPath(dbPath, id string, transcript bool) (Session, []Message, error) {
	store, err := Open(dbPath)
	if err != nil {
		return Session{}, nil, err
	}
	defer store.Close()
	sid, err := store.resolveID(id)
	if err != nil {
		return Session{}, nil, err
	}
	row := store.db.QueryRow(`SELECT id,machine,title,first_user_message,last_agent_message,cwd,source,model_provider,model,cli_version,git_branch,git_origin_url,created_at,updated_at,tokens_used,status,rollout_path,rollout_sha256,files_touched_json,commands_json,tool_names_json,errors_json FROM codex_sessions WHERE id=?`, sid)
	sess, err := scanSession(row)
	if err != nil {
		return Session{}, nil, err
	}
	if !transcript {
		return sess, []Message{}, nil
	}
	rows, err := store.db.Query(`SELECT line_no,COALESCE(timestamp,''),COALESCE(role,''),COALESCE(kind,''),COALESCE(text,'') FROM codex_session_messages WHERE session_id=? ORDER BY line_no`, sid)
	if err != nil {
		return Session{}, nil, err
	}
	defer rows.Close()
	msgs := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.LineNo, &m.Timestamp, &m.Role, &m.Kind, &m.Text); err != nil {
			return Session{}, nil, err
		}
		msgs = append(msgs, m)
	}
	return sess, msgs, rows.Err()
}

func ReadMessages(dbPath, id string, fromLine, limit int) (Session, []Message, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	store, err := Open(dbPath)
	if err != nil {
		return Session{}, nil, err
	}
	defer store.Close()
	sessionID, err := store.resolveID(id)
	if err != nil {
		return Session{}, nil, err
	}
	sess, err := store.loadSession(sessionID)
	if err != nil {
		return Session{}, nil, err
	}
	rows, err := store.db.Query(`SELECT line_no,COALESCE(timestamp,''),COALESCE(role,''),COALESCE(kind,''),COALESCE(text,'') FROM codex_session_messages WHERE session_id=? AND line_no>=? ORDER BY line_no LIMIT ?`, sessionID, max(0, fromLine), limit)
	if err != nil {
		return Session{}, nil, err
	}
	defer rows.Close()
	messages := []Message{}
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.LineNo, &message.Timestamp, &message.Role, &message.Kind, &message.Text); err != nil {
			return Session{}, nil, err
		}
		messages = append(messages, message)
	}
	return sess, messages, rows.Err()
}

type SessionLocation struct {
	SessionID   string `json:"session_id"`
	Source      string `json:"source"`
	RolloutPath string `json:"rollout_path"`
	UpdatedAt   string `json:"updated_at"`
}

func LocateSession(dbPath, id string) (SessionLocation, error) {
	store, err := Open(dbPath)
	if err != nil {
		return SessionLocation{}, err
	}
	defer store.Close()
	sessionID, err := store.resolveID(id)
	if err != nil {
		return SessionLocation{}, err
	}
	var location SessionLocation
	if err := store.db.QueryRow(`SELECT id,COALESCE(source,''),COALESCE(rollout_path,''),COALESCE(NULLIF(updated_at,''),created_at,'') FROM codex_sessions WHERE id=?`, sessionID).Scan(&location.SessionID, &location.Source, &location.RolloutPath, &location.UpdatedAt); err != nil {
		return SessionLocation{}, err
	}
	return location, nil
}

func StatsRead() (Stats, error) {
	return StatsReadPath("")
}

func StatsReadPath(path string) (Stats, error) {
	store, err := Open(path)
	if err != nil {
		return Stats{}, err
	}
	defer store.Close()
	var st Stats
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_sessions`).Scan(&st.Sessions)
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_events`).Scan(&st.Events)
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_messages`).Scan(&st.Messages)
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_chunks`).Scan(&st.Chunks)
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_embeddings`).Scan(&st.Embeddings)
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_capsules`).Scan(&st.Capsules)
	rows, err := store.db.Query(`SELECT provider, model, dim, COUNT(*) FROM codex_session_embeddings GROUP BY provider, model, dim ORDER BY COUNT(*) DESC`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var m EmbeddingModel
			_ = rows.Scan(&m.Provider, &m.Model, &m.Dim, &m.Count)
			st.Models = append(st.Models, m)
		}
	}
	return st, nil
}

func EmbeddingBacklogPath(path, model string) (int, error) {
	model = resolveEmbeddingModel(model)
	store, err := Open(path)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	return store.embeddingBacklog(model)
}

func (s *Store) embeddingBacklog(model string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*)
FROM codex_session_chunks c
LEFT JOIN codex_session_embeddings e
  ON e.chunk_id = c.id
 AND e.provider = ?
 AND e.model = ?
 AND e.text_sha256 = c.text_sha256
WHERE e.chunk_id IS NULL`, embeddingProvider(), model).Scan(&count)
	return count, err
}

func (s *Store) resolveID(prefix string) (string, error) {
	var id string
	if err := s.db.QueryRow(`SELECT id FROM codex_sessions WHERE id=?`, prefix).Scan(&id); err == nil {
		return id, nil
	}
	rows, err := s.db.Query(`SELECT id FROM codex_sessions WHERE id LIKE ? ORDER BY updated_at DESC`, "%"+prefix+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	if len(ids) == 0 {
		return "", sql.ErrNoRows
	}
	return "", fmt.Errorf("ambiguous session prefix %q matched %d sessions", prefix, len(ids))
}

func scanSession(scanner interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var files, commands, tools, errs string
	err := scanner.Scan(&s.ID, &s.Machine, &s.Title, &s.FirstUserMessage, &s.LastAgentMessage, &s.CWD, &s.Source, &s.ModelProvider, &s.Model, &s.CLIVersion, &s.GitBranch, &s.GitOriginURL, &s.CreatedAt, &s.UpdatedAt, &s.TokensUsed, &s.Status, &s.RolloutPath, &s.RolloutSHA256, &files, &commands, &tools, &errs)
	if err != nil {
		return s, err
	}
	_ = json.Unmarshal([]byte(files), &s.FilesTouched)
	_ = json.Unmarshal([]byte(commands), &s.Commands)
	_ = json.Unmarshal([]byte(tools), &s.ToolNames)
	_ = json.Unmarshal([]byte(errs), &s.Errors)
	return s, nil
}

func scanSessionRank(scanner interface{ Scan(...any) error }) (Session, float64, error) {
	var s Session
	var files, commands, tools, errs string
	var rank float64
	err := scanner.Scan(&s.ID, &s.Machine, &s.Title, &s.FirstUserMessage, &s.LastAgentMessage, &s.CWD, &s.Source, &s.ModelProvider, &s.Model, &s.CLIVersion, &s.GitBranch, &s.GitOriginURL, &s.CreatedAt, &s.UpdatedAt, &s.TokensUsed, &s.Status, &s.RolloutPath, &s.RolloutSHA256, &files, &commands, &tools, &errs, &rank)
	if err != nil {
		return s, 0, err
	}
	_ = json.Unmarshal([]byte(files), &s.FilesTouched)
	_ = json.Unmarshal([]byte(commands), &s.Commands)
	_ = json.Unmarshal([]byte(tools), &s.ToolNames)
	_ = json.Unmarshal([]byte(errs), &s.Errors)
	return s, rank, nil
}

func loadStateMetadata(path string) map[string]map[string]any {
	out := map[string]map[string]any{}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return out
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id,title,first_user_message,cwd,source,model_provider,model,cli_version,git_branch,git_origin_url,created_at_ms,updated_at_ms,tokens_used,preview FROM threads`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]sql.NullString, 14)
		ptr := make([]any, 14)
		for i := range vals {
			ptr[i] = &vals[i]
		}
		if rows.Scan(ptr...) == nil {
			m := map[string]any{}
			keys := []string{"id", "title", "first_user_message", "cwd", "source", "model_provider", "model", "cli_version", "git_branch", "git_origin_url", "created_at_ms", "updated_at_ms", "tokens_used", "preview"}
			for i, k := range keys {
				m[k] = vals[i].String
			}
			out[vals[0].String] = m
		}
	}
	return out
}
func mergeState(s *Session, m map[string]any) {
	if m == nil {
		return
	}
	s.Title = first(s.Title, normalizeUserText(str(m["title"])), normalizeUserText(str(m["preview"])))
	s.FirstUserMessage = first(s.FirstUserMessage, normalizeUserText(str(m["first_user_message"])))
	s.CWD = first(s.CWD, str(m["cwd"]))
	s.Source = first(s.Source, str(m["source"]))
	s.ModelProvider = first(s.ModelProvider, str(m["model_provider"]))
	s.Model = first(s.Model, str(m["model"]))
	s.CLIVersion = first(s.CLIVersion, str(m["cli_version"]))
	s.GitBranch = first(s.GitBranch, str(m["git_branch"]))
	s.GitOriginURL = first(s.GitOriginURL, str(m["git_origin_url"]))
	s.CreatedAt = first(isoAny(m["created_at_ms"]), s.CreatedAt)
	s.UpdatedAt = first(isoAny(m["updated_at_ms"]), s.UpdatedAt)
	if n, err := parseInt(str(m["tokens_used"])); err == nil {
		s.TokensUsed = n
	}
}

func findRollouts(root string, include []string, cutoff time.Time) []string {
	seen := map[string]bool{}
	var files []string
	roots := append([]string{root}, include...)
	for _, r := range roots {
		info, err := os.Stat(r)
		if err != nil {
			continue
		}
		if !info.IsDir() && isCodexRolloutPath(r) && fileIsRecentEnough(info, cutoff) {
			files = append(files, r)
			continue
		}
		filepath.WalkDir(r, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && isCodexRolloutPath(path) && !seen[path] {
				info, statErr := d.Info()
				if statErr != nil || !fileIsRecentEnough(info, cutoff) {
					return nil
				}
				seen[path] = true
				files = append(files, path)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files
}

func mergeSessionPaths(groups ...[]string) []string {
	seen := map[string]struct{}{}
	var paths []string
	for _, group := range groups {
		for _, path := range group {
			if _, exists := seen[path]; exists {
				continue
			}
			seen[path] = struct{}{}
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func isCodexRolloutPath(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
}

func findClaudeTranscripts(root string, include []string, cutoff time.Time) []string {
	seen := map[string]bool{}
	var files []string
	roots := append([]string{root}, include...)
	for _, r := range roots {
		info, err := os.Stat(r)
		if err != nil {
			continue
		}
		if !info.IsDir() && isClaudeTranscriptPath(r) && fileIsRecentEnough(info, cutoff) {
			files = append(files, r)
			continue
		}
		filepath.WalkDir(r, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && isClaudeTranscriptPath(path) && !seen[path] {
				info, statErr := d.Info()
				if statErr != nil || !fileIsRecentEnough(info, cutoff) {
					return nil
				}
				seen[path] = true
				files = append(files, path)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files
}

func fileIsRecentEnough(info os.FileInfo, cutoff time.Time) bool {
	return cutoff.IsZero() || !info.ModTime().Before(cutoff)
}

func isClaudeTranscriptPath(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".jsonl") && !strings.HasPrefix(base, "rollout-")
}
func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
func redact(s string) string {
	out := s
	for _, re := range secretPatterns {
		out = re.ReplaceAllString(out, "<REDACTED>")
	}
	return out
}

func sanitizeSession(s Session) Session {
	s.Title = redact(s.Title)
	s.FirstUserMessage = redact(s.FirstUserMessage)
	s.LastAgentMessage = redact(s.LastAgentMessage)
	s.CWD = redact(s.CWD)
	s.GitBranch = redact(s.GitBranch)
	s.GitOriginURL = redact(s.GitOriginURL)
	s.RolloutPath = redact(s.RolloutPath)
	s.RolloutSHA256 = redact(s.RolloutSHA256)
	s.FilesTouched = redactStrings(s.FilesTouched)
	s.Commands = redactStrings(s.Commands)
	s.ToolNames = redactStrings(s.ToolNames)
	s.Errors = redactStrings(s.Errors)
	return s
}

func redactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = redact(value)
	}
	return out
}

func redactObj(v any) any {
	switch x := v.(type) {
	case string:
		return redact(x)
	case []string:
		return redactStrings(x)
	case []any:
		for i := range x {
			x[i] = redactObj(x[i])
		}
		return x
	case map[string]any:
		for k, val := range x {
			if regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passwd|authorization|credential)`).MatchString(k) {
				x[k] = "<REDACTED>"
			} else {
				x[k] = redactObj(val)
			}
		}
		return x
	}
	return v
}
func str(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}
func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
func short(s string, n int) string { return truncate(strings.Join(strings.Fields(s), " "), n) }
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.ValidString(s[:end]) {
		end--
	}
	return s[:end]
}
func capMessageText(s string) string {
	if len(s) <= maxStoredMessageText {
		return s
	}
	return truncate(s, maxStoredMessageText) + fmt.Sprintf("\n...[truncated message from %d bytes]", len(s))
}
func isoAny(v any) string {
	raw := str(v)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "T") {
		return strings.ReplaceAll(raw, "Z", "+00:00")
	}
	n, err := parseInt(raw)
	if err != nil {
		return raw
	}
	if n > 10000000000 {
		n /= 1000
	}
	return time.Unix(n, 0).UTC().Format(time.RFC3339Nano)
}
func parseInt(s string) (int64, error) { var n int64; _, err := fmt.Sscan(s, &n); return n, err }
func isContextNoise(s string) bool {
	return normalizeUserText(s) == ""
}

var injectedContextBlocks = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<recommended_plugins>.*?</recommended_plugins>`),
	regexp.MustCompile(`(?is)<app-context>.*?</app-context>`),
	regexp.MustCompile(`(?is)<skills_instructions>.*?</skills_instructions>`),
	regexp.MustCompile(`(?is)<environment_context>.*?</environment_context>`),
	regexp.MustCompile(`(?is)<permissions instructions>.*?</permissions instructions>`),
	regexp.MustCompile(`(?is)<apps_instructions>.*?</apps_instructions>`),
	regexp.MustCompile(`(?is)<plugins_instructions>.*?</plugins_instructions>`),
	regexp.MustCompile(`(?is)<collaboration_mode>.*?</collaboration_mode>`),
	regexp.MustCompile(`(?is)<INSTRUCTIONS>.*?</INSTRUCTIONS>`),
	regexp.MustCompile(`(?is)<!--\s*pallium:agents:begin\s*-->.*?<!--\s*pallium:agents:end\s*-->`),
}

func normalizeUserText(s string) string {
	t := strings.TrimSpace(s)
	lower := strings.ToLower(t)
	startsWithInjectedContext := false
	for _, prefix := range []string{
		"<recommended_plugins>",
		"<app-context>",
		"<skills_instructions>",
		"<environment_context>",
		"<permissions instructions>",
		"<apps_instructions>",
		"<plugins_instructions>",
		"<collaboration_mode>",
		"<instructions>",
		"# agents.md instructions",
		"<!-- pallium:agents:begin -->",
		"the following is the codex agent history whose request action you are assessing.",
	} {
		if strings.HasPrefix(lower, prefix) {
			startsWithInjectedContext = true
			break
		}
	}
	if !startsWithInjectedContext {
		return t
	}
	for _, pattern := range injectedContextBlocks {
		t = pattern.ReplaceAllString(t, "\n")
	}
	var lines []string
	for _, line := range strings.Split(t, "\n") {
		trimmed := strings.ToLower(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(trimmed, "# agents.md instructions"), trimmed == "# codex loader note":
			continue
		}
		lines = append(lines, line)
	}
	t = strings.TrimSpace(strings.Join(lines, "\n"))
	for _, prefix := range []string{"<recommended_plugins>", "<environment_context>", "<permissions instructions>", "<apps_instructions>", "<instructions>", "<!-- pallium:agents:begin -->", "the following is the codex agent history whose request action you are assessing."} {
		if strings.HasPrefix(strings.ToLower(t), prefix) {
			return ""
		}
	}
	return t
}
func contentText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, it := range x {
			if m, ok := it.(map[string]any); ok {
				parts = append(parts, first(str(m["text"]), str(m["input_text"]), str(m["output_text"])))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return str(v)
	}
}
func parseArgs(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(str(v)), &m)
	if m == nil {
		m = map[string]any{"raw": str(v)}
	}
	return m
}
func addPaths(files map[string]bool, text string) {
	for _, m := range pathLikePattern.FindAllStringSubmatch(text, -1) {
		files[strings.TrimRight(m[1], ".,);]")] = true
	}
}
func addPatchPaths(files map[string]bool, text string) {
	re := regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$`)
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		files[strings.TrimSpace(m[1])] = true
	}
}
func messagesText(ms []Message) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Role == "user" || m.Role == "assistant" {
			parts = append(parts, m.Text)
		}
	}
	return strings.Join(parts, "\n")
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
