package workflow

import (
	"bytes"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
)

type Trigger struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Task            string `json:"task"`
	CWD             string `json:"cwd"`
	WorkflowName    string `json:"workflow_name,omitempty"`
	ScriptPath      string `json:"script_path,omitempty"`
	ArgsJSON        string `json:"args_json,omitempty"`
	Enabled         bool   `json:"enabled"`
	LastRunID       string `json:"last_run_id,omitempty"`
	LastRanAt       string `json:"last_ran_at,omitempty"`
	LastFingerprint string `json:"last_fingerprint,omitempty"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func (s *Store) UpsertTrigger(trigger Trigger) (Trigger, error) {
	trigger.Name = strings.TrimSpace(trigger.Name)
	if err := ValidateID(trigger.Name); err != nil {
		return Trigger{}, err
	}
	trigger.Task = strings.TrimSpace(trigger.Task)
	if trigger.Task == "" {
		return Trigger{}, fmt.Errorf("workflow trigger task is required")
	}
	trigger.Kind = strings.TrimSpace(trigger.Kind)
	if trigger.Kind == "" {
		trigger.Kind = "manual"
	}
	trigger.CWD = strings.TrimSpace(trigger.CWD)
	if trigger.CWD == "" {
		trigger.CWD = "."
	}
	now := nowString()
	existing, err := s.Trigger(trigger.Name)
	if err != nil && err != sql.ErrNoRows {
		return Trigger{}, err
	}
	if err == sql.ErrNoRows {
		trigger.CreatedAt = now
		trigger.UpdatedAt = now
		_, err = s.db.Exec(`INSERT INTO workflow_triggers(name,kind,task,cwd,workflow_name,script_path,args_json,enabled,last_run_id,last_ran_at,last_fingerprint,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, trigger.Name, trigger.Kind, trigger.Task, trigger.CWD, trigger.WorkflowName, trigger.ScriptPath, trigger.ArgsJSON, boolInt(trigger.Enabled), trigger.LastRunID, trigger.LastRanAt, trigger.LastFingerprint, trigger.CreatedAt, trigger.UpdatedAt)
		return trigger, err
	}
	trigger.CreatedAt = existing.CreatedAt
	trigger.UpdatedAt = now
	if trigger.LastRunID == "" {
		trigger.LastRunID = existing.LastRunID
	}
	if trigger.LastRanAt == "" {
		trigger.LastRanAt = existing.LastRanAt
	}
	if trigger.LastFingerprint == "" {
		trigger.LastFingerprint = existing.LastFingerprint
	}
	_, err = s.db.Exec(`UPDATE workflow_triggers SET kind=?,task=?,cwd=?,workflow_name=?,script_path=?,args_json=?,enabled=?,last_run_id=?,last_ran_at=?,last_fingerprint=?,updated_at=? WHERE name=?`,
		trigger.Kind, trigger.Task, trigger.CWD, trigger.WorkflowName, trigger.ScriptPath, trigger.ArgsJSON, boolInt(trigger.Enabled), trigger.LastRunID, trigger.LastRanAt, trigger.LastFingerprint, trigger.UpdatedAt, trigger.Name)
	return trigger, err
}

func (s *Store) Trigger(name string) (Trigger, error) {
	row := s.db.QueryRow(`SELECT name,kind,task,cwd,COALESCE(workflow_name,''),COALESCE(script_path,''),COALESCE(args_json,''),enabled,COALESCE(last_run_id,''),COALESCE(last_ran_at,''),COALESCE(last_fingerprint,''),created_at,updated_at FROM workflow_triggers WHERE name=?`, name)
	var trigger Trigger
	var enabled int
	err := row.Scan(&trigger.Name, &trigger.Kind, &trigger.Task, &trigger.CWD, &trigger.WorkflowName, &trigger.ScriptPath, &trigger.ArgsJSON, &enabled, &trigger.LastRunID, &trigger.LastRanAt, &trigger.LastFingerprint, &trigger.CreatedAt, &trigger.UpdatedAt)
	trigger.Enabled = enabled != 0
	return trigger, err
}

func (s *Store) ListTriggers() ([]Trigger, error) {
	rows, err := s.db.Query(`SELECT name,kind,task,cwd,COALESCE(workflow_name,''),COALESCE(script_path,''),COALESCE(args_json,''),enabled,COALESCE(last_run_id,''),COALESCE(last_ran_at,''),COALESCE(last_fingerprint,''),created_at,updated_at FROM workflow_triggers ORDER BY updated_at DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var triggers []Trigger
	for rows.Next() {
		var trigger Trigger
		var enabled int
		if err := rows.Scan(&trigger.Name, &trigger.Kind, &trigger.Task, &trigger.CWD, &trigger.WorkflowName, &trigger.ScriptPath, &trigger.ArgsJSON, &enabled, &trigger.LastRunID, &trigger.LastRanAt, &trigger.LastFingerprint, &trigger.CreatedAt, &trigger.UpdatedAt); err != nil {
			return nil, err
		}
		trigger.Enabled = enabled != 0
		triggers = append(triggers, trigger)
	}
	return triggers, rows.Err()
}

func (s *Store) SetTriggerRun(name, runID, fingerprint string) error {
	_, err := s.db.Exec(`UPDATE workflow_triggers SET last_run_id=?,last_ran_at=?,last_fingerprint=?,updated_at=? WHERE name=?`, runID, nowString(), fingerprint, nowString(), name)
	return err
}

func RepoFingerprint(cwd string) (string, error) {
	head, err := gitOutput(cwd, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	status, err := gitOutput(cwd, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(head) + "\n" + strings.TrimRight(status, "\n"), nil
}

func gitOutput(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(raw), nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
