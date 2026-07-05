package workflow

import (
	"database/sql"
	"fmt"
	"strings"
)

type Gate struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	Name       string `json:"name"`
	Message    string `json:"message,omitempty"`
	Status     string `json:"status"`
	OpenedAt   string `json:"opened_at"`
	ApprovedAt string `json:"approved_at,omitempty"`
}

func (s *Store) EnsureGate(runID, name, message string) (Gate, error) {
	runID = strings.TrimSpace(runID)
	name = strings.TrimSpace(name)
	if runID == "" || name == "" {
		return Gate{}, fmt.Errorf("workflow gate requires run id and name")
	}
	existing, err := s.Gate(runID, name)
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return Gate{}, err
	}
	gate := Gate{ID: NewID("gate"), RunID: runID, Name: name, Message: strings.TrimSpace(message), Status: "open", OpenedAt: nowString()}
	_, err = s.db.Exec(`INSERT INTO workflow_gates(id,run_id,name,message,status,opened_at,approved_at) VALUES(?,?,?,?,?,?,?)`,
		gate.ID, gate.RunID, gate.Name, gate.Message, gate.Status, gate.OpenedAt, gate.ApprovedAt)
	return gate, err
}

func (s *Store) Gate(runID, name string) (Gate, error) {
	row := s.db.QueryRow(`SELECT id,run_id,name,COALESCE(message,''),status,opened_at,COALESCE(approved_at,'') FROM workflow_gates WHERE run_id=? AND name=?`, runID, name)
	var gate Gate
	err := row.Scan(&gate.ID, &gate.RunID, &gate.Name, &gate.Message, &gate.Status, &gate.OpenedAt, &gate.ApprovedAt)
	return gate, err
}

func (s *Store) ListGates(runID string) ([]Gate, error) {
	rows, err := s.db.Query(`SELECT id,run_id,name,COALESCE(message,''),status,opened_at,COALESCE(approved_at,'') FROM workflow_gates WHERE run_id=? ORDER BY opened_at DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gates []Gate
	for rows.Next() {
		var gate Gate
		if err := rows.Scan(&gate.ID, &gate.RunID, &gate.Name, &gate.Message, &gate.Status, &gate.OpenedAt, &gate.ApprovedAt); err != nil {
			return nil, err
		}
		gates = append(gates, gate)
	}
	return gates, rows.Err()
}

func (s *Store) ApproveGate(runID, name string) (Gate, error) {
	runID = strings.TrimSpace(runID)
	name = strings.TrimSpace(name)
	if runID == "" || name == "" {
		return Gate{}, fmt.Errorf("workflow gate requires run id and name")
	}
	gate, err := s.Gate(runID, name)
	if err != nil {
		if err == sql.ErrNoRows {
			return Gate{}, fmt.Errorf("workflow gate %q for run %q was not found", name, runID)
		}
		return Gate{}, err
	}
	gate.Status = "approved"
	gate.ApprovedAt = nowString()
	_, err = s.db.Exec(`UPDATE workflow_gates SET status='approved',approved_at=? WHERE run_id=? AND name=?`, gate.ApprovedAt, runID, name)
	return gate, err
}
