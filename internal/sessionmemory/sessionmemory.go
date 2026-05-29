package sessionmemory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const DefaultEmbeddingModel = "text-embedding-3-small"

const maxStoredRawEventJSON = 100_000

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passwd|authorization|bearer)\s*[:=]\s*['"]?([A-Za-z0-9_./+=:-]{12,})`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

var pathLikePattern = regexp.MustCompile(`(?:^|\s)(/[A-Za-z0-9._~+/@:-][^\s'"` + "`" + `<>]*)`)

type Options struct {
	DBPath    string
	CodexHome string
	Machine   string
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
	Rank float64 `json:"rank,omitempty"`
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
	Models     []EmbeddingModel `json:"models"`
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
		return filepath.Join(home, ".codex-memory", "codex-sessions.sqlite")
	}
	return ".codex-sessions.sqlite"
}

func DefaultCodexHome() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex")
	}
	return ".codex"
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultDBPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
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
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	for _, stmt := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
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
`)
	return err
}

func Index(ctx context.Context, opts Options, include []string) (int, error) {
	if opts.CodexHome == "" {
		opts.CodexHome = DefaultCodexHome()
	}
	if opts.Machine == "" {
		host, _ := os.Hostname()
		opts.Machine = host
	}
	store, err := Open(opts.DBPath)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	state := loadStateMetadata(filepath.Join(opts.CodexHome, "state_5.sqlite"))
	files := findRollouts(filepath.Join(opts.CodexHome, "sessions"), include)
	count := 0
	for _, file := range files {
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		default:
		}
		parsed, err := parseRollout(file)
		if err != nil {
			return count, fmt.Errorf("parse %s: %w", file, err)
		}
		mergeState(&parsed.Session, state[parsed.Session.ID])
		parsed.Session.Machine = opts.Machine
		if err := store.upsert(parsed, state[parsed.Session.ID]); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Store) upsert(parsed ParsedSession, metadata map[string]any) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sess := parsed.Session
	if sess.Title == "" {
		sess.Title = short(sess.FirstUserMessage, 240)
	}
	if sess.Status == "" {
		sess.Status = "seen"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, stmt := range []string{
		"DELETE FROM codex_session_events WHERE session_id=?",
		"DELETE FROM codex_session_messages WHERE session_id=?",
		"DELETE FROM codex_session_chunks WHERE session_id=?",
		"DELETE FROM codex_session_fts WHERE session_id=?",
		"DELETE FROM codex_message_fts WHERE session_id=?",
	} {
		if _, err := tx.Exec(stmt, sess.ID); err != nil {
			return err
		}
	}
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
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
	if _, err := tx.Exec(`INSERT INTO codex_session_fts(session_id,title,cwd,first_user_message,last_agent_message,files,commands,text) VALUES(?,?,?,?,?,?,?,?)`, sess.ID, sess.Title, sess.CWD, sess.FirstUserMessage, sess.LastAgentMessage, strings.Join(sess.FilesTouched, "\n"), strings.Join(sess.Commands, "\n"), parsed.SearchBlob); err != nil {
		return err
	}
	for _, c := range buildChunks(parsed) {
		if _, err := tx.Exec(`INSERT INTO codex_session_chunks(id,session_id,chunk_index,kind,text,text_sha256,token_estimate,metadata_json) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.SessionID, c.Index, c.Kind, c.Text, c.TextSHA256, c.TokenEstimate, j(c.Metadata)); err != nil {
			return err
		}
	}
	return tx.Commit()
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

func Search(query string, limit int) ([]SearchResult, error) {
	store, err := Open("")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.search(query, limit)
}

func (s *Store) search(query string, limit int) ([]SearchResult, error) {
	rows, err := s.db.Query(`SELECT cs.id,cs.machine,cs.title,cs.first_user_message,cs.last_agent_message,cs.cwd,cs.source,cs.model_provider,cs.model,cs.cli_version,cs.git_branch,cs.git_origin_url,cs.created_at,cs.updated_at,cs.tokens_used,cs.status,cs.rollout_path,cs.rollout_sha256,cs.files_touched_json,cs.commands_json,cs.tool_names_json,cs.errors_json, bm25(codex_session_fts) AS rank FROM codex_session_fts JOIN codex_sessions cs ON cs.id=codex_session_fts.session_id WHERE codex_session_fts MATCH ? ORDER BY rank LIMIT ?`, query, limit)
	if err != nil {
		quoted := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
		rows, err = s.db.Query(`SELECT cs.id,cs.machine,cs.title,cs.first_user_message,cs.last_agent_message,cs.cwd,cs.source,cs.model_provider,cs.model,cs.cli_version,cs.git_branch,cs.git_origin_url,cs.created_at,cs.updated_at,cs.tokens_used,cs.status,cs.rollout_path,cs.rollout_sha256,cs.files_touched_json,cs.commands_json,cs.tool_names_json,cs.errors_json, bm25(codex_session_fts) AS rank FROM codex_session_fts JOIN codex_sessions cs ON cs.id=codex_session_fts.session_id WHERE codex_session_fts MATCH ? ORDER BY rank LIMIT ?`, quoted, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		sess, rank, err := scanSessionRank(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, SearchResult{Session: sess, Rank: rank})
	}
	return out, rows.Err()
}

func Grep(query string, limit int) ([]map[string]any, error) {
	store, err := Open("")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	rows, err := store.db.Query(`SELECT m.session_id,m.line_no,m.role,m.kind,m.text,s.title FROM codex_message_fts f JOIN codex_session_messages m ON m.session_id=f.session_id AND m.line_no=f.line_no JOIN codex_sessions s ON s.id=m.session_id WHERE codex_message_fts MATCH ? ORDER BY bm25(codex_message_fts) LIMIT ?`, query, limit)
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
	store, err := Open("")
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
		return sess, nil, nil
	}
	rows, err := store.db.Query(`SELECT line_no,timestamp,role,kind,text FROM codex_session_messages WHERE session_id=? ORDER BY line_no`, sid)
	if err != nil {
		return Session{}, nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.LineNo, &m.Timestamp, &m.Role, &m.Kind, &m.Text); err != nil {
			return Session{}, nil, err
		}
		msgs = append(msgs, m)
	}
	return sess, msgs, rows.Err()
}

func StatsRead() (Stats, error) {
	store, err := Open("")
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

// Embedding and semantic search.
func Embed(ctx context.Context, model string, limit, batchSize int) (int, error) {
	if model == "" {
		model = DefaultEmbeddingModel
	}
	if batchSize <= 0 {
		batchSize = 64
	}
	if limit <= 0 {
		limit = 1000000
	}
	store, err := Open("")
	if err != nil {
		return 0, err
	}
	defer store.Close()
	rows, err := store.db.Query(`SELECT c.id,c.text,c.text_sha256 FROM codex_session_chunks c LEFT JOIN codex_session_embeddings e ON e.chunk_id=c.id AND e.provider='openai' AND e.model=? AND e.text_sha256=c.text_sha256 WHERE e.chunk_id IS NULL ORDER BY c.session_id,c.chunk_index LIMIT ?`, model, limit)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type chunk struct{ id, text, sha string }
	var chunks []chunk
	for rows.Next() {
		var c chunk
		if err := rows.Scan(&c.id, &c.text, &c.sha); err != nil {
			return 0, err
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	total := 0
	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, c.text)
		}
		vecs, err := openAIEmbeddings(ctx, model, texts)
		if err != nil {
			return total, err
		}
		for j, vec := range vecs {
			c := chunks[i+j]
			if _, err := store.db.Exec(`INSERT OR REPLACE INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, c.id, "openai", model, len(vec), packVector(vec), c.sha, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return total, err
			}
			total++
		}
	}
	return total, nil
}

func Semantic(ctx context.Context, query, model string, limit int, sessionsOnly bool) ([]SemanticResult, error) {
	if model == "" {
		model = DefaultEmbeddingModel
	}
	if limit <= 0 {
		limit = 10
	}
	qvecs, err := openAIEmbeddings(ctx, model, []string{query})
	if err != nil {
		return nil, err
	}
	store, err := Open("")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	rows, err := store.db.Query(`SELECT e.vector_blob,c.id,c.session_id,c.kind,c.text,s.title,s.cwd,s.updated_at FROM codex_session_embeddings e JOIN codex_session_chunks c ON c.id=e.chunk_id JOIN codex_sessions s ON s.id=c.session_id WHERE e.provider='openai' AND e.model=?`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scored []SemanticResult
	for rows.Next() {
		var blob []byte
		var r SemanticResult
		var text string
		if err := rows.Scan(&blob, &r.ChunkID, &r.SessionID, &r.Kind, &text, &r.Title, &r.CWD, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Score = cosine(qvecs[0], unpackVector(blob))
		r.Snippet = short(text, 600)
		scored = append(scored, r)
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	seen := map[string]bool{}
	out := []SemanticResult{}
	for _, r := range scored {
		if sessionsOnly && seen[r.SessionID] {
			continue
		}
		seen[r.SessionID] = true
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func openAIEmbeddings(ctx context.Context, model string, texts []string) ([][]float64, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_ADMIN_API_KEY")
	}
	if key == "" {
		return nil, errors.New("OPENAI_API_KEY is required for embeddings")
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai embeddings failed: %s: %s", resp.Status, short(string(payload), 500))
	}
	var decoded struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	out := make([][]float64, len(decoded.Data))
	for i, item := range decoded.Data {
		out[i] = item.Embedding
	}
	return out, nil
}

func packVector(vec []float64) []byte {
	buf := new(bytes.Buffer)
	for _, v := range vec {
		_ = binary.Write(buf, binary.LittleEndian, float32(v))
	}
	return buf.Bytes()
}
func unpackVector(blob []byte) []float64 {
	out := make([]float64, len(blob)/4)
	for i := range out {
		out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:])))
	}
	return out
}
func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

type chunkRecord struct {
	ID, SessionID, Kind, Text, TextSHA256 string
	Index, TokenEstimate                  int
	Metadata                              map[string]any
}

func buildChunks(p ParsedSession) []chunkRecord {
	overview := strings.Join([]string{"Title: " + p.Session.Title, "CWD: " + p.Session.CWD, "Git: " + p.Session.GitOriginURL + " " + p.Session.GitBranch, "First ask: " + p.Session.FirstUserMessage, "Last agent message: " + p.Session.LastAgentMessage, "Files touched:\n" + strings.Join(p.Session.FilesTouched, "\n"), "Commands:\n" + strings.Join(p.Session.Commands, "\n"), "Errors:\n" + strings.Join(p.Session.Errors, "\n")}, "\n")
	var transcriptParts []string
	for _, m := range p.Messages {
		if m.Text != "" {
			transcriptParts = append(transcriptParts, fmt.Sprintf("[%s/%s line %d]\n%s", m.Role, m.Kind, m.LineNo, m.Text))
		}
	}
	var out []chunkRecord
	idx := 0
	for _, piece := range []struct{ kind, text string }{{"overview", overview}, {"transcript", strings.Join(transcriptParts, "\n\n")}} {
		for _, text := range chunkText(piece.text, 6000, 600) {
			sha := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
			out = append(out, chunkRecord{ID: fmt.Sprintf("%s:%04d", p.Session.ID, idx), SessionID: p.Session.ID, Index: idx, Kind: piece.kind, Text: text, TextSHA256: sha, TokenEstimate: max(1, len(text)/4), Metadata: map[string]any{"session_title": p.Session.Title, "cwd": p.Session.CWD}})
			idx++
		}
	}
	return out
}
func chunkText(text string, maxChars, overlap int) []string {
	text = strings.TrimSpace(redact(text))
	if text == "" {
		return nil
	}
	if len(text) <= maxChars {
		return []string{text}
	}
	var chunks []string
	for start := 0; start < len(text); {
		end := min(len(text), start+maxChars)
		chunks = append(chunks, strings.TrimSpace(text[start:end]))
		if end >= len(text) {
			break
		}
		start = max(0, end-overlap)
	}
	return chunks
}

func parseRollout(path string) (ParsedSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return ParsedSession{}, err
	}
	defer f.Close()
	sha, _ := fileSHA(path)
	p := ParsedSession{EventCounts: map[string]int{}}
	p.Session.RolloutPath = path
	p.Session.RolloutSHA256 = sha
	files := map[string]bool{}
	tools := map[string]bool{}
	reader := bufio.NewReader(f)
	lineNo := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			var obj map[string]any
			if json.Unmarshal(line, &obj) == nil {
				obj = redactObj(obj).(map[string]any)
				typ := str(obj["type"])
				if typ == "" {
					typ = "unknown"
				}
				p.EventCounts[typ]++
				ts := isoAny(obj["timestamp"])
				if p.Session.CreatedAt == "" {
					p.Session.CreatedAt = ts
				}
				if ts != "" {
					p.Session.UpdatedAt = ts
				}
				payload, _ := obj["payload"].(map[string]any)
				ptype := str(payload["type"])
				raw, _ := json.Marshal(obj)
				rawJSON := string(raw)
				if len(rawJSON) > maxStoredRawEventJSON {
					rawJSON = rawJSON[:maxStoredRawEventJSON] + fmt.Sprintf("\n...[truncated raw event from %d bytes]", len(rawJSON))
				}
				p.RawEvents = append(p.RawEvents, RawEvent{lineNo, ts, typ, ptype, rawJSON})
				switch typ {
				case "session_meta":
					p.Session.ID = str(payload["id"])
					p.Session.CWD = first(p.Session.CWD, str(payload["cwd"]))
					p.Session.Source = first(p.Session.Source, str(payload["source"]), str(payload["originator"]))
					p.Session.ModelProvider = first(p.Session.ModelProvider, str(payload["model_provider"]))
					p.Session.Model = first(p.Session.Model, str(payload["model"]))
					p.Session.CLIVersion = first(p.Session.CLIVersion, str(payload["cli_version"]))
				case "turn_context":
					p.Session.CWD = first(p.Session.CWD, str(payload["cwd"]))
				case "event_msg":
					handleEventMessage(&p, payload, lineNo, ts, files)
				case "response_item":
					handleResponseItem(&p, payload, lineNo, ts, files, tools)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return p, err
		}
	}
	if p.Session.ID == "" {
		p.Session.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	for f := range files {
		p.Session.FilesTouched = append(p.Session.FilesTouched, f)
	}
	sort.Strings(p.Session.FilesTouched)
	for t := range tools {
		p.Session.ToolNames = append(p.Session.ToolNames, t)
	}
	sort.Strings(p.Session.ToolNames)
	p.Session.Title = short(first(p.Session.FirstUserMessage, p.Session.Title), 240)
	p.SearchBlob = strings.Join([]string{p.Session.Title, p.Session.CWD, strings.Join(p.Session.Commands, "\n"), strings.Join(p.Session.FilesTouched, "\n"), messagesText(p.Messages)}, "\n")
	return p, nil
}

func handleEventMessage(p *ParsedSession, payload map[string]any, lineNo int, ts string, files map[string]bool) {
	ptype := str(payload["type"])
	switch ptype {
	case "user_message":
		text := str(payload["message"])
		if text != "" && !isContextNoise(text) {
			if p.Session.FirstUserMessage == "" {
				p.Session.FirstUserMessage = text
			}
			p.Messages = append(p.Messages, Message{lineNo, ts, "user", "message", text})
		}
	case "agent_message":
		text := str(payload["message"])
		if text != "" {
			p.Session.LastAgentMessage = text
			p.Messages = append(p.Messages, Message{lineNo, ts, "assistant", "message", text})
		}
	case "task_complete":
		p.Session.Status = "complete"
		text := str(payload["last_agent_message"])
		if text != "" {
			p.Session.LastAgentMessage = text
			p.Messages = append(p.Messages, Message{lineNo, ts, "assistant", "task_complete", text})
		}
	case "turn_aborted":
		p.Session.Status = "aborted"
		if r := str(payload["reason"]); r != "" {
			p.Session.Errors = append(p.Session.Errors, "aborted: "+r)
		}
	case "patch_apply_end":
		addPaths(files, str(payload["stdout"]))
		if changes, ok := payload["changes"].(map[string]any); ok {
			for k := range changes {
				files[k] = true
			}
		}
	}
}
func handleResponseItem(p *ParsedSession, payload map[string]any, lineNo int, ts string, files, mapTools map[string]bool) {
	ptype := str(payload["type"])
	switch ptype {
	case "message":
		role := str(payload["role"])
		text := contentText(payload["content"])
		if (role == "user" || role == "assistant") && text != "" && !isContextNoise(text) {
			if role == "user" && p.Session.FirstUserMessage == "" {
				p.Session.FirstUserMessage = text
			}
			if role == "assistant" {
				p.Session.LastAgentMessage = text
			}
			p.Messages = append(p.Messages, Message{lineNo, ts, role, "message", text})
		}
	case "function_call":
		name := str(payload["name"])
		mapTools[name] = true
		args := parseArgs(payload["arguments"])
		cmd := str(args["cmd"])
		if cmd == "" {
			cmd = str(args["command"])
		}
		if cmd != "" {
			p.Session.Commands = append(p.Session.Commands, cmd)
			p.Messages = append(p.Messages, Message{lineNo, ts, "tool", name, cmd})
		}
	case "custom_tool_call":
		name := str(payload["name"])
		mapTools[name] = true
		input := str(payload["input"])
		addPatchPaths(files, input)
		if input != "" {
			p.Messages = append(p.Messages, Message{lineNo, ts, "tool", name, input})
		}
	case "function_call_output", "custom_tool_call_output":
		out := str(payload["output"])
		addPaths(files, out)
		if regexp.MustCompile(`(?i)(error|traceback|failed|exception|permission denied)`).MatchString(out) {
			p.Session.Errors = append(p.Session.Errors, short(out, 500))
		}
	}
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
	s.Title = first(s.Title, str(m["title"]), str(m["preview"]))
	s.FirstUserMessage = first(s.FirstUserMessage, str(m["first_user_message"]))
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

func findRollouts(root string, include []string) []string {
	seen := map[string]bool{}
	var files []string
	roots := append([]string{root}, include...)
	for _, r := range roots {
		info, err := os.Stat(r)
		if err != nil {
			continue
		}
		if !info.IsDir() && strings.HasSuffix(r, ".jsonl") {
			files = append(files, r)
			continue
		}
		filepath.WalkDir(r, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasPrefix(filepath.Base(path), "rollout-") && strings.HasSuffix(path, ".jsonl") && !seen[path] {
				seen[path] = true
				files = append(files, path)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files
}
func fileSHA(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}
func redact(s string) string {
	out := s
	for _, re := range secretPatterns {
		out = re.ReplaceAllString(out, "<REDACTED>")
	}
	return out
}
func redactObj(v any) any {
	switch x := v.(type) {
	case string:
		return redact(x)
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
	return s[:n]
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
	t := strings.TrimSpace(s)
	for _, p := range []string{"# AGENTS.md instructions", "<environment_context>", "<permissions instructions>", "<apps_instructions>", "<INSTRUCTIONS>"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
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
