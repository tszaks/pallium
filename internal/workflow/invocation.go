package workflow

import (
	"encoding/json"
	"os"
	"time"
)

// Invocation is an attempt, including failed and retried calls. Cost remains
// null when the provider supplied only tokens or no accounting at all.
type Invocation struct {
	ID                  string         `json:"id"`
	RunID               string         `json:"run_id"`
	AgentID             string         `json:"agent_id,omitempty"`
	Provider            string         `json:"provider"`
	Model               string         `json:"model,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	ConfigurationStatus string         `json:"configuration_status"`
	StartedAt           string         `json:"started_at"`
	DurationMS          int64          `json:"duration_ms"`
	Status              string         `json:"status"`
	CostUSD             *float64       `json:"cost_usd"`
	Usage               map[string]any `json:"usage"`
}

func (s *Store) recordInvocation(runID, agentID, provider, model, effort string, started time.Time, usage map[string]any, callErr error, dispatched ...bool) error {
	if s == nil {
		return nil
	}
	v := Invocation{ID: NewID("inv"), RunID: runID, AgentID: agentID, Provider: provider, Model: model, ReasoningEffort: effort, ConfigurationStatus: "sent_to_provider", StartedAt: started.UTC().Format(time.RFC3339Nano), DurationMS: time.Since(started).Milliseconds(), Status: "completed", Usage: usage}
	if len(dispatched) > 0 && !dispatched[0] {
		v.ConfigurationStatus = "not_dispatched"
	} else if model == "" || effort == "" {
		v.ConfigurationStatus = "provider_default_unresolved"
	}
	if callErr != nil {
		v.Status = "failed"
	}
	if n, ok := usage["cost_usd"].(float64); ok && n >= 0 {
		v.CostUSD = &n
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO provider_invocations(id,run_id,agent_id,record_json) VALUES(?,?,?,?)`, v.ID, runID, agentID, string(raw))
	return err
}

func usageFromFile(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var usage map[string]any
	if json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	return usage
}

func (s *Store) ListInvocations(runID string) ([]Invocation, error) {
	rows, err := s.db.Query(`SELECT record_json FROM provider_invocations WHERE run_id=? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Invocation{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var v Invocation
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
