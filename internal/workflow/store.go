package workflow

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

type Run struct {
	ID          string `json:"id"`
	Task        string `json:"task"`
	CWD         string `json:"cwd"`
	ScriptPath  string `json:"script_path"`
	ArgsJSON    string `json:"args_json,omitempty"`
	OwnedID     string `json:"owned_session_id,omitempty"`
	Status      string `json:"status"`
	Result      string `json:"result,omitempty"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	CompletedAt string `json:"completed_at,omitempty"`
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
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	Phase       string `json:"phase,omitempty"`
	Label       string `json:"label,omitempty"`
	Prompt      string `json:"prompt"`
	Mode        string `json:"mode"`
	Isolation   string `json:"isolation,omitempty"`
	Status      string `json:"status"`
	Output      string `json:"output,omitempty"`
	Error       string `json:"error,omitempty"`
	PatchPath   string `json:"patch_path,omitempty"`
	Worktree    string `json:"worktree,omitempty"`
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
  status TEXT NOT NULL,
  result TEXT,
  error TEXT,
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
  phase TEXT,
  label TEXT,
  prompt TEXT NOT NULL,
  mode TEXT NOT NULL,
  isolation TEXT,
  status TEXT NOT NULL,
  output TEXT,
  error TEXT,
  patch_path TEXT,
  worktree TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_workflow_agents_run ON workflow_agents(run_id, created_at);
`)
	return err
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
	_, err := s.db.Exec(`INSERT INTO workflow_runs(id,task,cwd,script_path,args_json,owned_session_id,status,result,error,created_at,updated_at,completed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.Task, run.CWD, run.ScriptPath, run.ArgsJSON, run.OwnedID, run.Status, run.Result, run.Error, run.CreatedAt, run.UpdatedAt, run.CompletedAt)
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
		if run.Status == "" {
			run.Status = existing.Status
		}
		run.CreatedAt = existing.CreatedAt
		run.UpdatedAt = nowString()
		_, err := s.db.Exec(`UPDATE workflow_runs SET task=?,cwd=?,script_path=?,args_json=?,owned_session_id=?,status=?,result=?,error=?,updated_at=?,completed_at=? WHERE id=?`,
			run.Task, run.CWD, run.ScriptPath, run.ArgsJSON, run.OwnedID, run.Status, run.Result, run.Error, run.UpdatedAt, run.CompletedAt, run.ID)
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

func (s *Store) Run(id string) (Run, error) {
	row := s.db.QueryRow(`SELECT id,task,cwd,script_path,COALESCE(args_json,''),COALESCE(owned_session_id,''),status,COALESCE(result,''),COALESCE(error,''),created_at,updated_at,COALESCE(completed_at,'') FROM workflow_runs WHERE id=?`, id)
	var run Run
	err := row.Scan(&run.ID, &run.Task, &run.CWD, &run.ScriptPath, &run.ArgsJSON, &run.OwnedID, &run.Status, &run.Result, &run.Error, &run.CreatedAt, &run.UpdatedAt, &run.CompletedAt)
	return run, err
}

func (s *Store) ListRuns(limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,task,cwd,script_path,COALESCE(args_json,''),COALESCE(owned_session_id,''),status,COALESCE(result,''),COALESCE(error,''),created_at,updated_at,COALESCE(completed_at,'') FROM workflow_runs ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var run Run
		if err := rows.Scan(&run.ID, &run.Task, &run.CWD, &run.ScriptPath, &run.ArgsJSON, &run.OwnedID, &run.Status, &run.Result, &run.Error, &run.CreatedAt, &run.UpdatedAt, &run.CompletedAt); err != nil {
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
	phase := Phase{ID: NewID("phase"), RunID: runID, Name: name, Status: "running", CreatedAt: now, UpdatedAt: now}
	_, err := s.db.Exec(`INSERT INTO workflow_phases(id,run_id,name,status,agent_count,created_at,updated_at,completed_at) VALUES(?,?,?,?,?,?,?,?)`,
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
	_, err := s.db.Exec(`INSERT INTO workflow_agents(id,run_id,phase,label,prompt,mode,isolation,status,output,error,patch_path,worktree,created_at,updated_at,completed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, agent.ID, agent.RunID, agent.Phase, agent.Label, agent.Prompt, agent.Mode, agent.Isolation, agent.Status, agent.Output, agent.Error, agent.PatchPath, agent.Worktree, agent.CreatedAt, agent.UpdatedAt, agent.CompletedAt)
	return agent, err
}

func (s *Store) FinishAgent(agent Agent, outputText, errorText string) error {
	status := "completed"
	if errorText != "" {
		status = "failed"
	}
	_, err := s.db.Exec(`UPDATE workflow_agents SET status=?,output=?,error=?,patch_path=?,worktree=?,updated_at=?,completed_at=? WHERE id=?`,
		status, outputText, errorText, agent.PatchPath, agent.Worktree, nowString(), nowString(), agent.ID)
	return err
}

func (s *Store) ListAgents(runID string) ([]Agent, error) {
	rows, err := s.db.Query(`SELECT id,run_id,COALESCE(phase,''),COALESCE(label,''),prompt,mode,COALESCE(isolation,''),status,COALESCE(output,''),COALESCE(error,''),COALESCE(patch_path,''),COALESCE(worktree,''),created_at,updated_at,COALESCE(completed_at,'') FROM workflow_agents WHERE run_id=? ORDER BY created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []Agent
	for rows.Next() {
		var agent Agent
		if err := rows.Scan(&agent.ID, &agent.RunID, &agent.Phase, &agent.Label, &agent.Prompt, &agent.Mode, &agent.Isolation, &agent.Status, &agent.Output, &agent.Error, &agent.PatchPath, &agent.Worktree, &agent.CreatedAt, &agent.UpdatedAt, &agent.CompletedAt); err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
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

func NewID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
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
