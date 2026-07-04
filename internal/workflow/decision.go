package workflow

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type Decision struct {
	ID        string   `json:"id"`
	RunID     string   `json:"run_id"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	CreatedAt string   `json:"created_at"`
}

func (s *Store) RecordDecision(runID, title, body string, tags []string) (Decision, error) {
	runID = strings.TrimSpace(runID)
	title = strings.TrimSpace(title)
	if runID == "" || title == "" {
		return Decision{}, fmt.Errorf("workflow decision requires run id and title")
	}
	tags = cleanTags(tags)
	rawTags, err := json.Marshal(tags)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{
		ID:        NewID("decision"),
		RunID:     runID,
		Title:     title,
		Body:      strings.TrimSpace(body),
		Tags:      tags,
		CreatedAt: nowString(),
	}
	_, err = s.db.Exec(`INSERT INTO workflow_decisions(id,run_id,title,body,tags,created_at) VALUES(?,?,?,?,?,?)`,
		decision.ID, decision.RunID, decision.Title, decision.Body, string(rawTags), decision.CreatedAt)
	return decision, err
}

func (s *Store) SearchDecisions(query string, limit int) ([]Decision, error) {
	if limit <= 0 {
		limit = 10
	}
	query = strings.TrimSpace(query)
	sqlQuery := `SELECT id,run_id,title,COALESCE(body,''),COALESCE(tags,'[]'),created_at FROM workflow_decisions`
	args := []any{}
	if query != "" {
		sqlQuery += ` WHERE title LIKE ? OR body LIKE ? OR tags LIKE ?`
		like := "%" + query + "%"
		args = append(args, like, like, like)
	}
	sqlQuery += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var decisions []Decision
	for rows.Next() {
		decision, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

type decisionScanner interface {
	Scan(dest ...any) error
}

func scanDecision(row decisionScanner) (Decision, error) {
	var decision Decision
	var rawTags string
	if err := row.Scan(&decision.ID, &decision.RunID, &decision.Title, &decision.Body, &rawTags, &decision.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return Decision{}, err
		}
		return Decision{}, err
	}
	_ = json.Unmarshal([]byte(rawTags), &decision.Tags)
	return decision, nil
}

func cleanTags(tags []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}
