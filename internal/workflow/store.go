package workflow

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tszaks/pallium/internal/sessionmemory"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Run struct {
	ID           string `json:"id"`
	Task         string `json:"task"`
	CWD          string `json:"cwd"`
	ScriptPath   string `json:"script_path"`
	ArgsJSON     string `json:"args_json,omitempty"`
	OwnedID      string `json:"owned_session_id,omitempty"`
	MaxAgents    int    `json:"max_agents,omitempty"`
	MaxBudgetUSD string `json:"max_budget_usd,omitempty"`
	AgentTimeout int    `json:"agent_timeout_seconds,omitempty"`
	// AgentTimeoutExplicit records whether AgentTimeout was ever explicitly
	// configured for this run (via --agent-timeout, including 0 to disable
	// timeouts). Callers must check this flag before trusting AgentTimeout:
	// the field's own zero value is ambiguous between "not configured" and
	// "explicitly disabled", so the flag is the source of truth.
	AgentTimeoutExplicit bool `json:"agent_timeout_explicit,omitempty"`
	// AllowNetwork is the run-level ceiling for worker network egress, set by
	// --allow-network. An agent's network: true request is only honored when
	// this is true. Default false keeps every worker sandboxed unless the
	// operator explicitly opted the run in.
	AllowNetwork  bool         `json:"allow_network,omitempty"`
	Status        string       `json:"status"`
	Result        string       `json:"result,omitempty"`
	Error         string       `json:"error,omitempty"`
	Failures      []RunFailure `json:"failures,omitempty"`
	ScriptHash    string       `json:"script_hash,omitempty"`
	ScriptChanged bool         `json:"script_changed,omitempty"`
	CreatedAt     string       `json:"created_at"`
	UpdatedAt     string       `json:"updated_at"`
	CompletedAt   string       `json:"completed_at,omitempty"`
}

// RunFailure records an item that was dropped to null inside parallel or
// pipeline instead of failing the run.
type RunFailure struct {
	Label string `json:"label,omitempty"`
	Phase string `json:"phase,omitempty"`
	Error string `json:"error"`
}

type Phase struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	AgentCount  int    `json:"agent_count"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

type Agent struct {
	ID               string  `json:"id"`
	RunID            string  `json:"run_id"`
	CallIndex        int     `json:"call_index,omitempty"`
	Phase            string  `json:"phase,omitempty"`
	Label            string  `json:"label,omitempty"`
	Prompt           string  `json:"prompt"`
	Provider         string  `json:"provider,omitempty"`
	Repo             string  `json:"repo,omitempty"`
	Mode             string  `json:"mode"`
	Isolation        string  `json:"isolation,omitempty"`
	Model            string  `json:"model,omitempty"`
	SchemaHash       string  `json:"schema_hash,omitempty"`
	ScriptHash       string  `json:"script_hash,omitempty"`
	ArgsHash         string  `json:"args_hash,omitempty"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd,omitempty"`
	UsageJSON        string  `json:"usage_json,omitempty"`
	Status           string  `json:"status"`
	Output           string  `json:"output,omitempty"`
	Error            string  `json:"error,omitempty"`
	PatchPath        string  `json:"patch_path,omitempty"`
	Worktree         string  `json:"worktree,omitempty"`
	// Networked records whether the agent actually ran with network egress
	// (its network: true request AND the run's --allow-network ceiling). It is
	// part of the completed-agent cache identity so a network-off rerun of the
	// same run-id never reuses output produced by a networked run.
	Networked   bool   `json:"networked,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

type Snapshot struct {
	Run    Run     `json:"run"`
	Phases []Phase `json:"phases"`
	Agents []Agent `json:"agents"`
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
CREATE TABLE IF NOT EXISTS workflow_runs (
  id TEXT PRIMARY KEY,
  task TEXT NOT NULL,
  cwd TEXT NOT NULL,
  script_path TEXT NOT NULL,
  args_json TEXT,
  owned_session_id TEXT,
  max_agents INTEGER DEFAULT 0,
  max_budget_usd TEXT,
  agent_timeout_seconds INTEGER DEFAULT 0,
  agent_timeout_explicit INTEGER DEFAULT 0,
  allow_network INTEGER DEFAULT 0,
  status TEXT NOT NULL,
  result TEXT,
  error TEXT,
  failures TEXT,
  script_hash TEXT,
  script_changed INTEGER DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_updated ON workflow_runs(updated_at DESC);
CREATE TABLE IF NOT EXISTS workflow_phases (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  agent_count INTEGER DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_workflow_phases_run ON workflow_phases(run_id, created_at);
CREATE TABLE IF NOT EXISTS workflow_agents (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  call_index INTEGER DEFAULT 0,
  phase TEXT,
  label TEXT,
  prompt TEXT NOT NULL,
  provider TEXT,
  repo TEXT,
  mode TEXT NOT NULL,
  isolation TEXT,
  model TEXT,
  schema_hash TEXT,
  script_hash TEXT,
  args_hash TEXT,
  estimated_cost_usd REAL DEFAULT 0,
  usage_json TEXT,
  status TEXT NOT NULL,
  output TEXT,
  error TEXT,
  patch_path TEXT,
  worktree TEXT,
  networked INTEGER DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_workflow_agents_run ON workflow_agents(run_id, created_at);
CREATE TABLE IF NOT EXISTS workflow_triggers (
  name TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  task TEXT NOT NULL,
  cwd TEXT NOT NULL,
  workflow_name TEXT,
  script_path TEXT,
  args_json TEXT,
  enabled INTEGER NOT NULL DEFAULT 1,
  last_run_id TEXT,
  last_ran_at TEXT,
  last_fingerprint TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_triggers_updated ON workflow_triggers(updated_at DESC);
CREATE TABLE IF NOT EXISTS workflow_decisions (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  title TEXT NOT NULL,
  body TEXT,
  tags TEXT,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_decisions_created ON workflow_decisions(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_workflow_decisions_run ON workflow_decisions(run_id, created_at DESC);
CREATE TABLE IF NOT EXISTS workflow_gates (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  name TEXT NOT NULL,
  message TEXT,
  status TEXT NOT NULL,
  opened_at TEXT NOT NULL,
  approved_at TEXT,
  UNIQUE(run_id, name)
);
CREATE INDEX IF NOT EXISTS idx_workflow_gates_run ON workflow_gates(run_id, opened_at DESC);
CREATE TABLE IF NOT EXISTS workflow_repo_locks (
  repo_root TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`)
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		"ALTER TABLE workflow_agents ADD COLUMN model TEXT",
		"ALTER TABLE workflow_agents ADD COLUMN provider TEXT",
		"ALTER TABLE workflow_agents ADD COLUMN repo TEXT",
		"ALTER TABLE workflow_agents ADD COLUMN call_index INTEGER DEFAULT 0",
		"ALTER TABLE workflow_agents ADD COLUMN schema_hash TEXT",
		"ALTER TABLE workflow_agents ADD COLUMN script_hash TEXT",
		"ALTER TABLE workflow_agents ADD COLUMN args_hash TEXT",
		"ALTER TABLE workflow_agents ADD COLUMN estimated_cost_usd REAL DEFAULT 0",
		"ALTER TABLE workflow_agents ADD COLUMN usage_json TEXT",
		"ALTER TABLE workflow_agents ADD COLUMN networked INTEGER DEFAULT 0",
		"ALTER TABLE workflow_runs ADD COLUMN max_agents INTEGER DEFAULT 0",
		"ALTER TABLE workflow_runs ADD COLUMN max_budget_usd TEXT",
		"ALTER TABLE workflow_runs ADD COLUMN agent_timeout_seconds INTEGER DEFAULT 0",
		"ALTER TABLE workflow_runs ADD COLUMN agent_timeout_explicit INTEGER DEFAULT 0",
		"ALTER TABLE workflow_runs ADD COLUMN allow_network INTEGER DEFAULT 0",
		"ALTER TABLE workflow_runs ADD COLUMN failures TEXT",
		"ALTER TABLE workflow_runs ADD COLUMN script_hash TEXT",
		"ALTER TABLE workflow_runs ADD COLUMN script_changed INTEGER DEFAULT 0",
		"ALTER TABLE workflow_triggers ADD COLUMN last_fingerprint TEXT",
	} {
		if _, alterErr := s.db.Exec(stmt); alterErr != nil && !strings.Contains(alterErr.Error(), "duplicate column name") {
			return alterErr
		}
	}
	if _, err := s.db.Exec(`
WITH ordered AS (
  SELECT id, ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY created_at, id) AS rn
  FROM workflow_agents
  WHERE COALESCE(call_index,0)=0
)
UPDATE workflow_agents
SET call_index=(SELECT rn FROM ordered WHERE ordered.id=workflow_agents.id)
WHERE id IN (SELECT id FROM ordered);
`); err != nil {
		return err
	}
	// Backfill agent_timeout_explicit for rows written before that column
	// existed. Only rows from before this change can have a positive stored
	// timeout with explicit=0 (every write path since always sets both
	// together), so this only ever touches genuinely pre-upgrade rows and is
	// a no-op once applied. Without it, a pre-upgrade run with a custom
	// timeout would silently stop honoring it on resume and fall back to the
	// 600s flag default, since resume only forwards the stored value when
	// AgentTimeoutExplicit is true.
	if _, err := s.db.Exec(`UPDATE workflow_runs SET agent_timeout_explicit=1 WHERE agent_timeout_seconds>0 AND COALESCE(agent_timeout_explicit,0)=0`); err != nil {
		return err
	}
	return nil
}

func (s *Store) CreateRun(run Run) (Run, error) {
	if strings.TrimSpace(run.ID) == "" {
		run.ID = NewID("wf")
	}
	if err := ValidateID(run.ID); err != nil {
		return Run{}, err
	}
	if strings.TrimSpace(run.Task) == "" {
		return Run{}, fmt.Errorf("workflow task is required")
	}
	if strings.TrimSpace(run.CWD) == "" || strings.TrimSpace(run.ScriptPath) == "" {
		return Run{}, fmt.Errorf("workflow cwd and script path are required")
	}
	if run.Status == "" {
		run.Status = "queued"
	}
	if run.CreatedAt == "" {
		run.CreatedAt = nowString()
	}
	if run.UpdatedAt == "" {
		run.UpdatedAt = run.CreatedAt
	}
	_, err := s.db.Exec(`INSERT INTO workflow_runs(id,task,cwd,script_path,args_json,owned_session_id,max_agents,max_budget_usd,agent_timeout_seconds,agent_timeout_explicit,allow_network,status,result,error,created_at,updated_at,completed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.Task, run.CWD, run.ScriptPath, run.ArgsJSON, run.OwnedID, run.MaxAgents, run.MaxBudgetUSD, run.AgentTimeout, run.AgentTimeoutExplicit, run.AllowNetwork, run.Status, run.Result, run.Error, run.CreatedAt, run.UpdatedAt, run.CompletedAt)
	return run, err
}

func (s *Store) UpsertRun(run Run) (Run, error) {
	if existing, err := s.Run(run.ID); err == nil {
		if run.Task == "" {
			run.Task = existing.Task
		}
		if run.CWD == "" {
			run.CWD = existing.CWD
		}
		if run.ScriptPath == "" {
			run.ScriptPath = existing.ScriptPath
		}
		if run.ArgsJSON == "" {
			run.ArgsJSON = existing.ArgsJSON
		}
		if run.OwnedID == "" {
			run.OwnedID = existing.OwnedID
		}
		if run.MaxAgents == 0 {
			run.MaxAgents = existing.MaxAgents
		}
		if run.MaxBudgetUSD == "" {
			run.MaxBudgetUSD = existing.MaxBudgetUSD
		}
		// AgentTimeout's zero value is a legitimate explicit value ("disable
		// timeouts"), so it cannot double as the "caller didn't set this"
		// sentinel the other fields use. AgentTimeoutExplicit is the actual
		// signal: only fall back to the existing stored value when this call
		// did not explicitly configure a timeout at all.
		if !run.AgentTimeoutExplicit {
			run.AgentTimeout = existing.AgentTimeout
			run.AgentTimeoutExplicit = existing.AgentTimeoutExplicit
		}
		// AllowNetwork is NOT OR-folded with the existing stored value: a fresh
		// `workflow run` must reflect only THIS invocation's --allow-network
		// flag (default false), so reusing a run-id that once had the ceiling
		// does not silently keep egress on. `workflow resume` preserves the
		// stored ceiling by explicitly reading it and forwarding
		// --allow-network to the nested run, so persistence does not depend on
		// folding here.
		if run.Status == "" {
			run.Status = existing.Status
		}
		run.CreatedAt = existing.CreatedAt
		run.UpdatedAt = nowString()
		_, err := s.db.Exec(`UPDATE workflow_runs SET task=?,cwd=?,script_path=?,args_json=?,owned_session_id=?,max_agents=?,max_budget_usd=?,agent_timeout_seconds=?,agent_timeout_explicit=?,allow_network=?,status=?,result=?,error=?,updated_at=?,completed_at=? WHERE id=?`,
			run.Task, run.CWD, run.ScriptPath, run.ArgsJSON, run.OwnedID, run.MaxAgents, run.MaxBudgetUSD, run.AgentTimeout, run.AgentTimeoutExplicit, run.AllowNetwork, run.Status, run.Result, run.Error, run.UpdatedAt, run.CompletedAt, run.ID)
		return run, err
	}
	return s.CreateRun(run)
}

func (s *Store) SetRunStatus(id, status, resultText, errorText string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("workflow run id is required")
	}
	completedAt := ""
	if status == "completed" || status == "failed" || status == "stopped" {
		completedAt = nowString()
	}
	_, err := s.db.Exec(`UPDATE workflow_runs SET status=?,result=?,error=?,updated_at=?,completed_at=? WHERE id=?`,
		status, resultText, errorText, nowString(), completedAt, id)
	return err
}

func (s *Store) SetRunOwnedID(id, ownedID string) error {
	_, err := s.db.Exec(`UPDATE workflow_runs SET owned_session_id=?,updated_at=? WHERE id=?`, ownedID, nowString(), id)
	return err
}

const runSelectColumns = `id,task,cwd,script_path,COALESCE(args_json,''),COALESCE(owned_session_id,''),COALESCE(max_agents,0),COALESCE(max_budget_usd,''),COALESCE(agent_timeout_seconds,0),COALESCE(agent_timeout_explicit,0),COALESCE(allow_network,0),status,COALESCE(result,''),COALESCE(error,''),COALESCE(failures,''),COALESCE(script_hash,''),COALESCE(script_changed,0),created_at,updated_at,COALESCE(completed_at,'')`

func scanRun(row interface{ Scan(dest ...any) error }) (Run, error) {
	var run Run
	var failuresJSON string
	var scriptChanged int
	var agentTimeoutExplicit int
	var allowNetwork int
	if err := row.Scan(&run.ID, &run.Task, &run.CWD, &run.ScriptPath, &run.ArgsJSON, &run.OwnedID, &run.MaxAgents, &run.MaxBudgetUSD, &run.AgentTimeout, &agentTimeoutExplicit, &allowNetwork, &run.Status, &run.Result, &run.Error, &failuresJSON, &run.ScriptHash, &scriptChanged, &run.CreatedAt, &run.UpdatedAt, &run.CompletedAt); err != nil {
		return Run{}, err
	}
	run.AgentTimeoutExplicit = agentTimeoutExplicit != 0
	run.AllowNetwork = allowNetwork != 0
	run.ScriptChanged = scriptChanged != 0
	if failuresJSON != "" {
		_ = json.Unmarshal([]byte(failuresJSON), &run.Failures)
	}
	return run, nil
}

func (s *Store) Run(id string) (Run, error) {
	row := s.db.QueryRow(`SELECT `+runSelectColumns+` FROM workflow_runs WHERE id=?`, id)
	return scanRun(row)
}

func (s *Store) SetRunFailures(id string, failures []RunFailure) error {
	raw := ""
	if len(failures) > 0 {
		encoded, err := json.Marshal(failures)
		if err != nil {
			return err
		}
		raw = string(encoded)
	}
	_, err := s.db.Exec(`UPDATE workflow_runs SET failures=?,updated_at=? WHERE id=?`, raw, nowString(), id)
	return err
}

func (s *Store) SetRunScriptState(id, scriptHash string, scriptChanged bool) error {
	changed := 0
	if scriptChanged {
		changed = 1
	}
	_, err := s.db.Exec(`UPDATE workflow_runs SET script_hash=?,script_changed=?,updated_at=? WHERE id=?`, scriptHash, changed, nowString(), id)
	return err
}

func (s *Store) CountActiveRuns() (int, error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM workflow_runs WHERE status IN ('queued','running','paused')`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) ListRuns(limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+runSelectColumns+` FROM workflow_runs ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) Snapshot(id string) (Snapshot, error) {
	run, err := s.Run(id)
	if err != nil {
		return Snapshot{}, err
	}
	phases, err := s.ListPhases(id)
	if err != nil {
		return Snapshot{}, err
	}
	agents, err := s.ListAgents(id)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Run: run, Phases: phases, Agents: agents}, nil
}

func (s *Store) StartPhase(runID, name string) (Phase, error) {
	if strings.TrimSpace(name) == "" {
		name = "default"
	}
	now := nowString()
	var existing Phase
	err := s.db.QueryRow(`SELECT id,run_id,name,status,agent_count,created_at,updated_at,COALESCE(completed_at,'') FROM workflow_phases WHERE run_id=? AND name=? ORDER BY created_at DESC LIMIT 1`, runID, name).
		Scan(&existing.ID, &existing.RunID, &existing.Name, &existing.Status, &existing.AgentCount, &existing.CreatedAt, &existing.UpdatedAt, &existing.CompletedAt)
	if err == nil {
		existing.Status = "running"
		existing.AgentCount = 0
		existing.UpdatedAt = now
		existing.CompletedAt = ""
		_, err := s.db.Exec(`UPDATE workflow_phases SET status=?,agent_count=0,updated_at=?,completed_at='' WHERE id=?`, existing.Status, existing.UpdatedAt, existing.ID)
		return existing, err
	}
	if err != sql.ErrNoRows {
		return Phase{}, err
	}
	phase := Phase{ID: NewID("phase"), RunID: runID, Name: name, Status: "running", CreatedAt: now, UpdatedAt: now}
	_, err = s.db.Exec(`INSERT INTO workflow_phases(id,run_id,name,status,agent_count,created_at,updated_at,completed_at) VALUES(?,?,?,?,?,?,?,?)`,
		phase.ID, phase.RunID, phase.Name, phase.Status, phase.AgentCount, phase.CreatedAt, phase.UpdatedAt, phase.CompletedAt)
	return phase, err
}

func (s *Store) FinishPhase(runID, name string) error {
	_, err := s.db.Exec(`UPDATE workflow_phases SET status='completed',updated_at=?,completed_at=? WHERE run_id=? AND name=? AND status='running'`,
		nowString(), nowString(), runID, name)
	return err
}

func (s *Store) IncrementPhaseAgentCount(runID, name string) error {
	_, err := s.db.Exec(`UPDATE workflow_phases SET agent_count=agent_count+1,updated_at=? WHERE run_id=? AND name=? AND status='running'`,
		nowString(), runID, name)
	return err
}

func (s *Store) ListPhases(runID string) ([]Phase, error) {
	rows, err := s.db.Query(`SELECT id,run_id,name,status,agent_count,created_at,updated_at,COALESCE(completed_at,'') FROM workflow_phases WHERE run_id=? ORDER BY created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var phases []Phase
	for rows.Next() {
		var phase Phase
		if err := rows.Scan(&phase.ID, &phase.RunID, &phase.Name, &phase.Status, &phase.AgentCount, &phase.CreatedAt, &phase.UpdatedAt, &phase.CompletedAt); err != nil {
			return nil, err
		}
		phases = append(phases, phase)
	}
	return phases, rows.Err()
}

func (s *Store) CreateAgent(agent Agent) (Agent, error) {
	if agent.ID == "" {
		agent.ID = NewID("agent")
	}
	if err := ValidateID(agent.ID); err != nil {
		return Agent{}, err
	}
	if agent.Mode == "" {
		agent.Mode = "read-only"
	}
	if agent.Status == "" {
		agent.Status = "running"
	}
	if agent.CreatedAt == "" {
		agent.CreatedAt = nowString()
	}
	if agent.UpdatedAt == "" {
		agent.UpdatedAt = agent.CreatedAt
	}
	_, err := s.db.Exec(`INSERT INTO workflow_agents(id,run_id,call_index,phase,label,prompt,provider,repo,mode,isolation,model,schema_hash,script_hash,args_hash,estimated_cost_usd,usage_json,status,output,error,patch_path,worktree,networked,created_at,updated_at,completed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, agent.ID, agent.RunID, agent.CallIndex, agent.Phase, agent.Label, agent.Prompt, agent.Provider, agent.Repo, agent.Mode, agent.Isolation, agent.Model, agent.SchemaHash, agent.ScriptHash, agent.ArgsHash, agent.EstimatedCostUSD, agent.UsageJSON, agent.Status, agent.Output, agent.Error, agent.PatchPath, agent.Worktree, agent.Networked, agent.CreatedAt, agent.UpdatedAt, agent.CompletedAt)
	return agent, err
}

func (s *Store) FinishAgent(agent Agent, outputText, errorText string) error {
	status := "completed"
	if errorText != "" {
		status = "failed"
	}
	return s.FinishAgentStatus(agent, status, outputText, errorText)
}

func (s *Store) FinishAgentStatus(agent Agent, status, outputText, errorText string) error {
	_, err := s.db.Exec(`UPDATE workflow_agents SET status=?,output=?,error=?,patch_path=?,worktree=?,estimated_cost_usd=?,usage_json=?,updated_at=?,completed_at=? WHERE id=?`,
		status, outputText, errorText, agent.PatchPath, agent.Worktree, agent.EstimatedCostUSD, agent.UsageJSON, nowString(), nowString(), agent.ID)
	return err
}

func (s *Store) CompletedAgent(runID string, callIndex int, phase, label, prompt, provider, repo, mode, isolation, model, schemaHash, argsHash string, networked bool) (Agent, bool, error) {
	row := s.db.QueryRow(`SELECT id,run_id,COALESCE(call_index,0),COALESCE(phase,''),COALESCE(label,''),prompt,COALESCE(provider,''),COALESCE(repo,''),mode,COALESCE(isolation,''),COALESCE(model,''),COALESCE(schema_hash,''),COALESCE(script_hash,''),COALESCE(args_hash,''),COALESCE(estimated_cost_usd,0),COALESCE(usage_json,''),status,COALESCE(output,''),COALESCE(error,''),COALESCE(patch_path,''),COALESCE(worktree,''),COALESCE(networked,0),created_at,updated_at,COALESCE(completed_at,'') FROM workflow_agents WHERE run_id=? AND COALESCE(call_index,0)=? AND COALESCE(phase,'')=? AND COALESCE(label,'')=? AND prompt=? AND COALESCE(provider,'')=? AND COALESCE(repo,'')=? AND mode=? AND COALESCE(isolation,'')=? AND COALESCE(model,'')=? AND COALESCE(schema_hash,'')=? AND COALESCE(args_hash,'')=? AND COALESCE(networked,0)=? AND status='completed' ORDER BY completed_at DESC, updated_at DESC LIMIT 1`,
		runID, callIndex, phase, label, prompt, provider, repo, mode, isolation, model, schemaHash, argsHash, networked)
	var agent Agent
	var networkedInt int
	err := row.Scan(&agent.ID, &agent.RunID, &agent.CallIndex, &agent.Phase, &agent.Label, &agent.Prompt, &agent.Provider, &agent.Repo, &agent.Mode, &agent.Isolation, &agent.Model, &agent.SchemaHash, &agent.ScriptHash, &agent.ArgsHash, &agent.EstimatedCostUSD, &agent.UsageJSON, &agent.Status, &agent.Output, &agent.Error, &agent.PatchPath, &agent.Worktree, &networkedInt, &agent.CreatedAt, &agent.UpdatedAt, &agent.CompletedAt)
	if err == sql.ErrNoRows {
		return Agent{}, false, nil
	}
	if err != nil {
		return Agent{}, false, err
	}
	agent.Networked = networkedInt != 0
	return agent, true, nil
}

func (s *Store) ListAgents(runID string) ([]Agent, error) {
	rows, err := s.db.Query(`SELECT id,run_id,COALESCE(call_index,0),COALESCE(phase,''),COALESCE(label,''),prompt,COALESCE(provider,''),COALESCE(repo,''),mode,COALESCE(isolation,''),COALESCE(model,''),COALESCE(schema_hash,''),COALESCE(script_hash,''),COALESCE(args_hash,''),COALESCE(estimated_cost_usd,0),COALESCE(usage_json,''),status,COALESCE(output,''),COALESCE(error,''),COALESCE(patch_path,''),COALESCE(worktree,''),COALESCE(networked,0),created_at,updated_at,COALESCE(completed_at,'') FROM workflow_agents WHERE run_id=? ORDER BY COALESCE(call_index,0), created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []Agent
	for rows.Next() {
		var agent Agent
		var networkedInt int
		if err := rows.Scan(&agent.ID, &agent.RunID, &agent.CallIndex, &agent.Phase, &agent.Label, &agent.Prompt, &agent.Provider, &agent.Repo, &agent.Mode, &agent.Isolation, &agent.Model, &agent.SchemaHash, &agent.ScriptHash, &agent.ArgsHash, &agent.EstimatedCostUSD, &agent.UsageJSON, &agent.Status, &agent.Output, &agent.Error, &agent.PatchPath, &agent.Worktree, &networkedInt, &agent.CreatedAt, &agent.UpdatedAt, &agent.CompletedAt); err != nil {
			return nil, err
		}
		agent.Networked = networkedInt != 0
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// AgentUsage returns the run's spawned-agent count and total spend, used to
// seed the --max-agents and --max-budget-usd caps on resume. The count
// excludes provider="internal" rows (e.g. the untilGreen combined-patch
// bookkeeping row from registerUntilGreenPatch): those rows never spawned a
// worker, so counting them would let a resume trip --max-agents early even
// though no extra worker actually ran. Cost stays summed across all rows for
// audit history, though internal rows always report zero cost so this is a
// no-op for the budget total.
func (s *Store) AgentUsage(runID string) (int, float64, error) {
	row := s.db.QueryRow(`SELECT COUNT(CASE WHEN COALESCE(provider,'') != 'internal' THEN 1 END), COALESCE(SUM(estimated_cost_usd),0) FROM workflow_agents WHERE run_id=?`, runID)
	var count int
	var cost float64
	if err := row.Scan(&count, &cost); err != nil {
		return 0, 0, err
	}
	return count, cost, nil
}

func ValidateID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if len(id) > 120 {
		return fmt.Errorf("id is too long")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("id %q contains invalid character %q", id, r)
		}
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("id cannot contain '..'")
	}
	return nil
}

var idCounter uint64

func NewID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UTC().UnixNano(), atomic.AddUint64(&idCounter, 1))
}

func MarshalJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(raw)
}

func nowString() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
