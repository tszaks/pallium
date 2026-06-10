package analysis

import (
	"fmt"
	"strings"

	"github.com/tszaks/pallium/internal/db"
)

type Decision struct {
	SourceType  string `json:"source_type"`
	SourceRef   string `json:"source_ref"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	CommittedAt string `json:"committed_at"`
}

func Decisions(store *db.Store, query string, limit int) ([]Decision, error) {
	repo, err := store.Repo()
	if err != nil {
		return nil, err
	}
	needle := "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
	rows, err := store.DB().Query(`
SELECT source_type, source_ref, title, body, committed_at
FROM decision_notes
WHERE repo_id = ? AND (
  lower(title) LIKE ? OR lower(body) LIKE ?
)
ORDER BY committed_at DESC
LIMIT ?
`, repo.ID, needle, needle, limit)
	if err != nil {
		return nil, fmt.Errorf("query decisions: %w", err)
	}
	defer rows.Close()

	out := make([]Decision, 0)
	for rows.Next() {
		var item Decision
		if err := rows.Scan(&item.SourceType, &item.SourceRef, &item.Title, &item.Body, &item.CommittedAt); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		out = append(out, item)
	}

	return out, rows.Err()
}

func DecisionsByRefs(store *db.Store, refs []string, limit int) ([]Decision, error) {
	if len(refs) == 0 {
		return []Decision{}, nil
	}

	repo, err := store.Repo()
	if err != nil {
		return nil, err
	}

	placeholders := make([]string, 0, len(refs))
	args := make([]any, 0, len(refs)+2)
	args = append(args, repo.ID)
	for _, ref := range refs {
		placeholders = append(placeholders, "?")
		args = append(args, ref)
	}
	args = append(args, limit)

	query := fmt.Sprintf(`
SELECT source_type, source_ref, title, body, committed_at
FROM decision_notes
WHERE repo_id = ? AND source_ref IN (%s)
ORDER BY committed_at DESC
LIMIT ?
`, strings.Join(placeholders, ","))

	rows, err := store.DB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query decisions by refs: %w", err)
	}
	defer rows.Close()

	out := make([]Decision, 0)
	for rows.Next() {
		var item Decision
		if err := rows.Scan(&item.SourceType, &item.SourceRef, &item.Title, &item.Body, &item.CommittedAt); err != nil {
			return nil, fmt.Errorf("scan decision by ref: %w", err)
		}
		out = append(out, item)
	}

	return out, rows.Err()
}
