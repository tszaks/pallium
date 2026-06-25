package console

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/sessionmemory"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type SessionKey struct {
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
	Machine   string `json:"machine"`
}

type Manifest struct {
	SessionKey
	CWD         string   `json:"cwd,omitempty"`
	Goal        string   `json:"goal,omitempty"`
	CurrentStep string   `json:"current_step,omitempty"`
	Files       []string `json:"files,omitempty"`
	NextActions []string `json:"next_actions,omitempty"`
	Risks       []string `json:"risks,omitempty"`
	Blockers    []string `json:"blockers,omitempty"`
	Stop        string   `json:"stop_condition,omitempty"`
	SourcePID   int      `json:"source_pid,omitempty"`
	UpdatedAt   string   `json:"updated_at"`
}

type Handoff struct {
	ID             string   `json:"id"`
	FromProvider   string   `json:"from_provider"`
	FromSessionID  string   `json:"from_session_id"`
	FromMachine    string   `json:"from_machine"`
	ToProvider     string   `json:"to_provider,omitempty"`
	ToSessionID    string   `json:"to_session_id,omitempty"`
	ToMachine      string   `json:"to_machine,omitempty"`
	Summary        string   `json:"summary"`
	PendingActions []string `json:"pending_actions,omitempty"`
	Blockers       []string `json:"blockers,omitempty"`
	Status         string   `json:"status"`
	CreatedAt      string   `json:"created_at"`
	AcceptedAt     string   `json:"accepted_at,omitempty"`
}

type Claim struct {
	ID          string   `json:"id"`
	Provider    string   `json:"provider"`
	SessionID   string   `json:"session_id"`
	Machine     string   `json:"machine"`
	RepoRoot    string   `json:"repo_root"`
	TargetType  string   `json:"target_type"`
	Target      string   `json:"target"`
	Intent      string   `json:"intent,omitempty"`
	LeaseUntil  string   `json:"lease_until"`
	Status      string   `json:"status"`
	CreatedAt   string   `json:"created_at"`
	ReleasedAt  string   `json:"released_at,omitempty"`
	Conflict    bool     `json:"conflict,omitempty"`
	ConflictIDs []string `json:"conflict_ids,omitempty"`
}

type AuthorityEvent struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	SessionID      string `json:"session_id"`
	Machine        string `json:"machine"`
	Actor          string `json:"actor"`
	Level          string `json:"level"`
	Action         string `json:"action"`
	TargetRef      string `json:"target_ref,omitempty"`
	DetailsJSON    string `json:"details_json,omitempty"`
	Status         string `json:"status"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	CreatedAt      string `json:"created_at"`
	DecidedAt      string `json:"decided_at,omitempty"`
}

type RiskGate struct {
	ID                   string `json:"id"`
	AuthorityEventID     string `json:"authority_event_id"`
	GateType             string `json:"gate_type"`
	RequiredAttestations int    `json:"required_attestations"`
	Status               string `json:"status"`
	OpenedAt             string `json:"opened_at"`
	ClosedAt             string `json:"closed_at,omitempty"`
}

type Attestation struct {
	ID        string `json:"id"`
	GateID    string `json:"gate_id"`
	Provider  string `json:"provider,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Machine   string `json:"machine,omitempty"`
	Verdict   string `json:"verdict"`
	Evidence  string `json:"evidence_ref,omitempty"`
	CreatedAt string `json:"created_at"`
}

type ReviewCase struct {
	ID        string `json:"id"`
	Topic     string `json:"topic"`
	OpenedBy  string `json:"opened_by"`
	Reviewer  string `json:"reviewer,omitempty"`
	Status    string `json:"status"`
	Decision  string `json:"decision,omitempty"`
	CreatedAt string `json:"created_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

type OwnedSession struct {
	ID        string   `json:"id"`
	Command   []string `json:"command"`
	CWD       string   `json:"cwd"`
	LogPath   string   `json:"log_path"`
	RunnerPID int      `json:"runner_pid,omitempty"`
	ChildPID  int      `json:"child_pid,omitempty"`
	Status    string   `json:"status"`
	StartedAt string   `json:"started_at"`
	UpdatedAt string   `json:"updated_at"`
	ExitedAt  string   `json:"exited_at,omitempty"`
	ExitCode  int      `json:"exit_code,omitempty"`
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = sessionmemory.DefaultDBPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	for _, stmt := range []string{"PRAGMA busy_timeout=60000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS session_manifests (
  provider TEXT NOT NULL,
  session_id TEXT NOT NULL,
  machine TEXT NOT NULL,
  cwd TEXT,
  goal TEXT,
  current_step TEXT,
  files_json TEXT,
  next_actions_json TEXT,
  risks_json TEXT,
  blockers_json TEXT,
  stop_condition TEXT,
  source_pid INTEGER DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(provider, session_id, machine)
);
CREATE TABLE IF NOT EXISTS session_handoffs (
  id TEXT PRIMARY KEY,
  from_provider TEXT NOT NULL,
  from_session_id TEXT NOT NULL,
  from_machine TEXT NOT NULL,
  to_provider TEXT,
  to_session_id TEXT,
  to_machine TEXT,
  summary TEXT NOT NULL,
  pending_actions_json TEXT,
  blockers_json TEXT,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  accepted_at TEXT
);
CREATE TABLE IF NOT EXISTS session_claims (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  session_id TEXT NOT NULL,
  machine TEXT NOT NULL,
  repo_root TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target TEXT NOT NULL,
  intent TEXT,
  lease_until TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  released_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_session_claims_active_target ON session_claims(repo_root, target_type, target, status, lease_until);
CREATE TABLE IF NOT EXISTS authority_events (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  session_id TEXT NOT NULL,
  machine TEXT NOT NULL,
  actor TEXT NOT NULL,
  level TEXT NOT NULL,
  action TEXT NOT NULL,
  target_ref TEXT,
  details_json TEXT,
  status TEXT NOT NULL,
  idempotency_key TEXT,
  created_at TEXT NOT NULL,
  decided_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_authority_events_session ON authority_events(provider, session_id, machine, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_authority_events_idempotency ON authority_events(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '';
CREATE TABLE IF NOT EXISTS risk_gates (
  id TEXT PRIMARY KEY,
  authority_event_id TEXT NOT NULL,
  gate_type TEXT NOT NULL,
  required_attestations INTEGER NOT NULL,
  status TEXT NOT NULL,
  opened_at TEXT NOT NULL,
  closed_at TEXT
);
CREATE TABLE IF NOT EXISTS attestations (
  id TEXT PRIMARY KEY,
  gate_id TEXT NOT NULL,
  provider TEXT,
  session_id TEXT,
  machine TEXT,
  verdict TEXT NOT NULL,
  evidence_ref TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_reviews (
  id TEXT PRIMARY KEY,
  topic TEXT NOT NULL,
  opened_by TEXT NOT NULL,
  reviewer TEXT,
  status TEXT NOT NULL,
  decision TEXT,
  created_at TEXT NOT NULL,
  closed_at TEXT
);
CREATE TABLE IF NOT EXISTS owned_sessions (
  id TEXT PRIMARY KEY,
  command_json TEXT NOT NULL,
  cwd TEXT NOT NULL,
  log_path TEXT NOT NULL,
  runner_pid INTEGER DEFAULT 0,
  child_pid INTEGER DEFAULT 0,
  status TEXT NOT NULL,
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  exited_at TEXT,
  exit_code INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_owned_sessions_status ON owned_sessions(status, updated_at);
`)
	return err
}

func (s *Store) UpsertManifest(m Manifest) (Manifest, error) {
	if err := validateSessionKey(m.SessionKey); err != nil {
		return Manifest{}, err
	}
	m.Provider = normalizeToken(m.Provider)
	if m.UpdatedAt == "" {
		m.UpdatedAt = nowString()
	}
	files, err := encodeList(m.Files)
	if err != nil {
		return Manifest{}, err
	}
	next, err := encodeList(m.NextActions)
	if err != nil {
		return Manifest{}, err
	}
	risks, err := encodeList(m.Risks)
	if err != nil {
		return Manifest{}, err
	}
	blockers, err := encodeList(m.Blockers)
	if err != nil {
		return Manifest{}, err
	}
	_, err = s.db.Exec(`INSERT INTO session_manifests(provider,session_id,machine,cwd,goal,current_step,files_json,next_actions_json,risks_json,blockers_json,stop_condition,source_pid,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(provider,session_id,machine) DO UPDATE SET
cwd=excluded.cwd, goal=excluded.goal, current_step=excluded.current_step, files_json=excluded.files_json,
next_actions_json=excluded.next_actions_json, risks_json=excluded.risks_json, blockers_json=excluded.blockers_json,
stop_condition=excluded.stop_condition, source_pid=excluded.source_pid, updated_at=excluded.updated_at`,
		m.Provider, m.SessionID, m.Machine, m.CWD, m.Goal, m.CurrentStep, files, next, risks, blockers, m.Stop, m.SourcePID, m.UpdatedAt)
	if err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (s *Store) Manifest(key SessionKey) (Manifest, error) {
	row := s.db.QueryRow(`SELECT provider,session_id,machine,cwd,goal,current_step,files_json,next_actions_json,risks_json,blockers_json,stop_condition,source_pid,updated_at
FROM session_manifests WHERE provider=? AND session_id=? AND machine=?`, normalizeToken(key.Provider), key.SessionID, key.Machine)
	return scanManifest(row)
}

func (s *Store) ListManifests() ([]Manifest, error) {
	rows, err := s.db.Query(`SELECT provider,session_id,machine,cwd,goal,current_step,files_json,next_actions_json,risks_json,blockers_json,stop_condition,source_pid,updated_at FROM session_manifests ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Manifest, 0)
	for rows.Next() {
		m, err := scanManifest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) WriteHandoff(h Handoff) (Handoff, error) {
	if h.FromProvider == "" || h.FromSessionID == "" || h.FromMachine == "" {
		return Handoff{}, fmt.Errorf("handoff requires from provider, session, and machine")
	}
	if strings.TrimSpace(h.Summary) == "" {
		return Handoff{}, fmt.Errorf("handoff summary is required")
	}
	if h.ID == "" {
		h.ID = newID("handoff")
	}
	if h.Status == "" {
		h.Status = "open"
	}
	if h.CreatedAt == "" {
		h.CreatedAt = nowString()
	}
	pending, err := encodeList(h.PendingActions)
	if err != nil {
		return Handoff{}, err
	}
	blockers, err := encodeList(h.Blockers)
	if err != nil {
		return Handoff{}, err
	}
	_, err = s.db.Exec(`INSERT INTO session_handoffs(id,from_provider,from_session_id,from_machine,to_provider,to_session_id,to_machine,summary,pending_actions_json,blockers_json,status,created_at,accepted_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, h.ID, normalizeToken(h.FromProvider), h.FromSessionID, h.FromMachine, normalizeToken(h.ToProvider), h.ToSessionID, h.ToMachine, h.Summary, pending, blockers, h.Status, h.CreatedAt, h.AcceptedAt)
	if err != nil {
		return Handoff{}, err
	}
	return h, nil
}

func (s *Store) ListHandoffs(limit int) ([]Handoff, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,from_provider,from_session_id,from_machine,to_provider,to_session_id,to_machine,summary,pending_actions_json,blockers_json,status,created_at,COALESCE(accepted_at,'') FROM session_handoffs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Handoff, 0)
	for rows.Next() {
		h, err := scanHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) AcceptHandoff(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("handoff id is required")
	}
	_, err := s.db.Exec(`UPDATE session_handoffs SET status='accepted', accepted_at=? WHERE id=?`, nowString(), id)
	return err
}

func (s *Store) AcquireClaim(c Claim) (Claim, error) {
	if c.Provider == "" || c.SessionID == "" || c.Machine == "" {
		return Claim{}, fmt.Errorf("claim requires provider, session, and machine")
	}
	if strings.TrimSpace(c.RepoRoot) == "" || strings.TrimSpace(c.Target) == "" {
		return Claim{}, fmt.Errorf("claim requires repo root and target")
	}
	if c.ID == "" {
		c.ID = newID("claim")
	}
	if c.TargetType == "" {
		c.TargetType = "file"
	}
	if c.Status == "" {
		c.Status = "active"
	}
	if c.CreatedAt == "" {
		c.CreatedAt = nowString()
	}
	if c.LeaseUntil == "" {
		c.LeaseUntil = time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339Nano)
	}
	conflicts, err := s.conflictingClaimIDs(c)
	if err != nil {
		return Claim{}, err
	}
	c.ConflictIDs = conflicts
	c.Conflict = len(conflicts) > 0
	_, err = s.db.Exec(`INSERT INTO session_claims(id,provider,session_id,machine,repo_root,target_type,target,intent,lease_until,status,created_at,released_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, c.ID, normalizeToken(c.Provider), c.SessionID, c.Machine, c.RepoRoot, c.TargetType, c.Target, c.Intent, c.LeaseUntil, c.Status, c.CreatedAt, c.ReleasedAt)
	if err != nil {
		return Claim{}, err
	}
	return c, nil
}

func (s *Store) ReleaseClaim(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("claim id is required")
	}
	_, err := s.db.Exec(`UPDATE session_claims SET status='released', released_at=? WHERE id=?`, nowString(), id)
	return err
}

func (s *Store) ListClaims(repoRoot string, includeReleased bool) ([]Claim, error) {
	query := `SELECT id,provider,session_id,machine,repo_root,target_type,target,intent,lease_until,status,created_at,COALESCE(released_at,'') FROM session_claims`
	args := []any{}
	parts := []string{}
	if strings.TrimSpace(repoRoot) != "" {
		parts = append(parts, "repo_root=?")
		args = append(args, repoRoot)
	}
	if !includeReleased {
		parts = append(parts, "status!='released'")
	}
	if len(parts) > 0 {
		query += " WHERE " + strings.Join(parts, " AND ")
	}
	query += " ORDER BY created_at DESC"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Claim, 0)
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) RequestAuthority(e AuthorityEvent) (AuthorityEvent, error) {
	if e.Provider == "" || e.SessionID == "" || e.Machine == "" {
		return AuthorityEvent{}, fmt.Errorf("authority event requires provider, session, and machine")
	}
	if strings.TrimSpace(e.Level) == "" || strings.TrimSpace(e.Action) == "" {
		return AuthorityEvent{}, fmt.Errorf("authority event requires level and action")
	}
	if e.ID == "" {
		e.ID = newID("auth")
	}
	if e.Actor == "" {
		e.Actor = "agent"
	}
	if e.Status == "" {
		e.Status = "requested"
	}
	if e.CreatedAt == "" {
		e.CreatedAt = nowString()
	}
	_, err := s.db.Exec(`INSERT INTO authority_events(id,provider,session_id,machine,actor,level,action,target_ref,details_json,status,idempotency_key,created_at,decided_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, normalizeToken(e.Provider), e.SessionID, e.Machine, e.Actor, e.Level, e.Action, e.TargetRef, e.DetailsJSON, e.Status, e.IdempotencyKey, e.CreatedAt, e.DecidedAt)
	if err != nil {
		return AuthorityEvent{}, err
	}
	return e, nil
}

func (s *Store) DecideAuthority(id, status, actor string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(status) == "" {
		return fmt.Errorf("authority id and status are required")
	}
	if actor == "" {
		actor = "human"
	}
	_, err := s.db.Exec(`UPDATE authority_events SET status=?, actor=?, decided_at=? WHERE id=?`, status, actor, nowString(), id)
	return err
}

func (s *Store) ListAuthority(sessionID string, limit int) ([]AuthorityEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id,provider,session_id,machine,actor,level,action,COALESCE(target_ref,''),COALESCE(details_json,''),status,COALESCE(idempotency_key,''),created_at,COALESCE(decided_at,'') FROM authority_events`
	args := []any{}
	if strings.TrimSpace(sessionID) != "" {
		query += " WHERE session_id=?"
		args = append(args, sessionID)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuthorityEvent, 0)
	for rows.Next() {
		var e AuthorityEvent
		if err := rows.Scan(&e.ID, &e.Provider, &e.SessionID, &e.Machine, &e.Actor, &e.Level, &e.Action, &e.TargetRef, &e.DetailsJSON, &e.Status, &e.IdempotencyKey, &e.CreatedAt, &e.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) OpenGate(g RiskGate) (RiskGate, error) {
	if strings.TrimSpace(g.AuthorityEventID) == "" {
		return RiskGate{}, fmt.Errorf("authority event id is required")
	}
	if g.ID == "" {
		g.ID = newID("gate")
	}
	if g.GateType == "" {
		g.GateType = "human"
	}
	if g.RequiredAttestations <= 0 {
		g.RequiredAttestations = 1
	}
	if g.Status == "" {
		g.Status = "open"
	}
	if g.OpenedAt == "" {
		g.OpenedAt = nowString()
	}
	_, err := s.db.Exec(`INSERT INTO risk_gates(id,authority_event_id,gate_type,required_attestations,status,opened_at,closed_at) VALUES(?,?,?,?,?,?,?)`,
		g.ID, g.AuthorityEventID, g.GateType, g.RequiredAttestations, g.Status, g.OpenedAt, g.ClosedAt)
	if err != nil {
		return RiskGate{}, err
	}
	return g, nil
}

func (s *Store) ListGates(limit int) ([]RiskGate, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,authority_event_id,gate_type,required_attestations,status,opened_at,COALESCE(closed_at,'') FROM risk_gates ORDER BY opened_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RiskGate, 0)
	for rows.Next() {
		var g RiskGate
		if err := rows.Scan(&g.ID, &g.AuthorityEventID, &g.GateType, &g.RequiredAttestations, &g.Status, &g.OpenedAt, &g.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) AddAttestation(a Attestation) (Attestation, error) {
	if strings.TrimSpace(a.GateID) == "" || strings.TrimSpace(a.Verdict) == "" {
		return Attestation{}, fmt.Errorf("gate id and verdict are required")
	}
	if a.ID == "" {
		a.ID = newID("attest")
	}
	if a.CreatedAt == "" {
		a.CreatedAt = nowString()
	}
	_, err := s.db.Exec(`INSERT INTO attestations(id,gate_id,provider,session_id,machine,verdict,evidence_ref,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		a.ID, a.GateID, normalizeToken(a.Provider), a.SessionID, a.Machine, a.Verdict, a.Evidence, a.CreatedAt)
	if err != nil {
		return Attestation{}, err
	}
	if strings.EqualFold(a.Verdict, "allow") || strings.EqualFold(a.Verdict, "approve") {
		_ = s.closeSatisfiedGate(a.GateID)
	}
	return a, nil
}

func (s *Store) CreateReview(r ReviewCase) (ReviewCase, error) {
	if strings.TrimSpace(r.Topic) == "" {
		return ReviewCase{}, fmt.Errorf("review topic is required")
	}
	if r.ID == "" {
		r.ID = newID("review")
	}
	if r.OpenedBy == "" {
		r.OpenedBy = "human"
	}
	if r.Status == "" {
		r.Status = "open"
	}
	if r.CreatedAt == "" {
		r.CreatedAt = nowString()
	}
	_, err := s.db.Exec(`INSERT INTO agent_reviews(id,topic,opened_by,reviewer,status,decision,created_at,closed_at) VALUES(?,?,?,?,?,?,?,?)`,
		r.ID, r.Topic, r.OpenedBy, r.Reviewer, r.Status, r.Decision, r.CreatedAt, r.ClosedAt)
	if err != nil {
		return ReviewCase{}, err
	}
	return r, nil
}

func (s *Store) Review(id string) (ReviewCase, error) {
	row := s.db.QueryRow(`SELECT id,topic,opened_by,COALESCE(reviewer,''),status,COALESCE(decision,''),created_at,COALESCE(closed_at,'') FROM agent_reviews WHERE id=?`, id)
	var r ReviewCase
	err := row.Scan(&r.ID, &r.Topic, &r.OpenedBy, &r.Reviewer, &r.Status, &r.Decision, &r.CreatedAt, &r.ClosedAt)
	return r, err
}

func (s *Store) CloseReview(id, decision string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(decision) == "" {
		return fmt.Errorf("review id and decision are required")
	}
	_, err := s.db.Exec(`UPDATE agent_reviews SET status='closed', decision=?, closed_at=? WHERE id=?`, decision, nowString(), id)
	return err
}

func (s *Store) CreateOwnedSession(sess OwnedSession) (OwnedSession, error) {
	if len(sess.Command) == 0 {
		return OwnedSession{}, fmt.Errorf("owned session command is required")
	}
	if strings.TrimSpace(sess.CWD) == "" || strings.TrimSpace(sess.LogPath) == "" {
		return OwnedSession{}, fmt.Errorf("owned session cwd and log path are required")
	}
	if sess.ID == "" {
		sess.ID = newID("owned")
	}
	if sess.Status == "" {
		sess.Status = "starting"
	}
	if sess.StartedAt == "" {
		sess.StartedAt = nowString()
	}
	if sess.UpdatedAt == "" {
		sess.UpdatedAt = sess.StartedAt
	}
	commandJSON, err := encodeList(sess.Command)
	if err != nil {
		return OwnedSession{}, err
	}
	_, err = s.db.Exec(`INSERT INTO owned_sessions(id,command_json,cwd,log_path,runner_pid,child_pid,status,started_at,updated_at,exited_at,exit_code)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, sess.ID, commandJSON, sess.CWD, sess.LogPath, sess.RunnerPID, sess.ChildPID, sess.Status, sess.StartedAt, sess.UpdatedAt, sess.ExitedAt, sess.ExitCode)
	if err != nil {
		return OwnedSession{}, err
	}
	return sess, nil
}

func (s *Store) UpdateOwnedSessionStarted(id string, runnerPID, childPID int) error {
	return s.updateOwnedSession(id, "running", runnerPID, childPID, "", 0)
}

func (s *Store) FinishOwnedSession(id string, exitCode int) error {
	return s.updateOwnedSession(id, "exited", 0, 0, nowString(), exitCode)
}

func (s *Store) FailOwnedSession(id string, exitCode int) error {
	return s.updateOwnedSession(id, "failed", 0, 0, nowString(), exitCode)
}

func (s *Store) OwnedSession(id string) (OwnedSession, error) {
	row := s.db.QueryRow(`SELECT id,command_json,cwd,log_path,runner_pid,child_pid,status,started_at,updated_at,COALESCE(exited_at,''),exit_code FROM owned_sessions WHERE id=?`, id)
	return scanOwnedSession(row)
}

func (s *Store) ListOwnedSessions(limit int) ([]OwnedSession, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,command_json,cwd,log_path,runner_pid,child_pid,status,started_at,updated_at,COALESCE(exited_at,''),exit_code FROM owned_sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OwnedSession, 0)
	for rows.Next() {
		sess, err := scanOwnedSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) updateOwnedSession(id, status string, runnerPID, childPID int, exitedAt string, exitCode int) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("owned session id is required")
	}
	sets := []string{"status=?", "updated_at=?"}
	args := []any{status, nowString()}
	if runnerPID > 0 {
		sets = append(sets, "runner_pid=?")
		args = append(args, runnerPID)
	}
	if childPID > 0 {
		sets = append(sets, "child_pid=?")
		args = append(args, childPID)
	}
	if exitedAt != "" {
		sets = append(sets, "exited_at=?", "exit_code=?")
		args = append(args, exitedAt, exitCode)
	}
	args = append(args, id)
	_, err := s.db.Exec(`UPDATE owned_sessions SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	return err
}

func (s *Store) conflictingClaimIDs(c Claim) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM session_claims WHERE repo_root=? AND target_type=? AND target=? AND status='active' AND lease_until > ? AND NOT (provider=? AND session_id=? AND machine=?)`,
		c.RepoRoot, c.TargetType, c.Target, nowString(), normalizeToken(c.Provider), c.SessionID, c.Machine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) closeSatisfiedGate(gateID string) error {
	var required int
	var status string
	if err := s.db.QueryRow(`SELECT required_attestations,status FROM risk_gates WHERE id=?`, gateID).Scan(&required, &status); err != nil {
		return err
	}
	if status != "open" {
		return nil
	}
	var allowed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attestations WHERE gate_id=? AND lower(verdict) IN ('allow','approve')`, gateID).Scan(&allowed); err != nil {
		return err
	}
	if allowed >= required {
		_, err := s.db.Exec(`UPDATE risk_gates SET status='satisfied', closed_at=? WHERE id=?`, nowString(), gateID)
		return err
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanManifest(row scanner) (Manifest, error) {
	var m Manifest
	var files, next, risks, blockers string
	err := row.Scan(&m.Provider, &m.SessionID, &m.Machine, &m.CWD, &m.Goal, &m.CurrentStep, &files, &next, &risks, &blockers, &m.Stop, &m.SourcePID, &m.UpdatedAt)
	if err != nil {
		return Manifest{}, err
	}
	m.Files = decodeList(files)
	m.NextActions = decodeList(next)
	m.Risks = decodeList(risks)
	m.Blockers = decodeList(blockers)
	return m, nil
}

func scanHandoff(row scanner) (Handoff, error) {
	var h Handoff
	var pending, blockers string
	err := row.Scan(&h.ID, &h.FromProvider, &h.FromSessionID, &h.FromMachine, &h.ToProvider, &h.ToSessionID, &h.ToMachine, &h.Summary, &pending, &blockers, &h.Status, &h.CreatedAt, &h.AcceptedAt)
	if err != nil {
		return Handoff{}, err
	}
	h.PendingActions = decodeList(pending)
	h.Blockers = decodeList(blockers)
	return h, nil
}

func scanClaim(row scanner) (Claim, error) {
	var c Claim
	err := row.Scan(&c.ID, &c.Provider, &c.SessionID, &c.Machine, &c.RepoRoot, &c.TargetType, &c.Target, &c.Intent, &c.LeaseUntil, &c.Status, &c.CreatedAt, &c.ReleasedAt)
	return c, err
}

func scanOwnedSession(row scanner) (OwnedSession, error) {
	var sess OwnedSession
	var commandJSON string
	err := row.Scan(&sess.ID, &commandJSON, &sess.CWD, &sess.LogPath, &sess.RunnerPID, &sess.ChildPID, &sess.Status, &sess.StartedAt, &sess.UpdatedAt, &sess.ExitedAt, &sess.ExitCode)
	if err != nil {
		return OwnedSession{}, err
	}
	sess.Command = decodeList(commandJSON)
	return sess, nil
}

func validateSessionKey(key SessionKey) error {
	if strings.TrimSpace(key.Provider) == "" || strings.TrimSpace(key.SessionID) == "" || strings.TrimSpace(key.Machine) == "" {
		return fmt.Errorf("provider, session id, and machine are required")
	}
	return nil
}

func encodeList(values []string) (string, error) {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	raw, err := json.Marshal(cleaned)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func normalizeToken(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
}

func nowString() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
