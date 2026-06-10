package analysis

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/db"
)

type Neighbor struct {
	Path          string  `json:"path"`
	CochangeCount int     `json:"cochange_count"`
	RecencyWeight float64 `json:"recency_weight"`
}

func Neighbors(store *db.Store, targetPath string, limit int) ([]Neighbor, error) {
	repo, err := store.Repo()
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeRepoPath(store.RepoRoot, targetPath)
	if err != nil {
		return nil, err
	}

	rows, err := store.DB().Query(`
SELECT related_path, cochange_count, recency_weight
FROM cochange_edges
WHERE repo_id = ? AND source_path = ?
ORDER BY cochange_count DESC, recency_weight DESC, related_path ASC
LIMIT ?
`, repo.ID, normalized, limit)
	if err != nil {
		return nil, fmt.Errorf("query neighbors: %w", err)
	}
	defer rows.Close()

	out := make([]Neighbor, 0)
	for rows.Next() {
		var item Neighbor
		if err := rows.Scan(&item.Path, &item.CochangeCount, &item.RecencyWeight); err != nil {
			return nil, fmt.Errorf("scan neighbor: %w", err)
		}
		out = append(out, item)
	}

	return out, rows.Err()
}

func NeighborCount(store *db.Store, normalizedPath string) (int, error) {
	repo, err := store.Repo()
	if err != nil {
		return 0, err
	}
	row := store.DB().QueryRow(`
SELECT COUNT(*) FROM cochange_edges WHERE repo_id = ? AND source_path = ?
`, repo.ID, normalizedPath)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count neighbors: %w", err)
	}
	return count, nil
}

func normalizeRepoPath(repoRoot, targetPath string) (string, error) {
	if targetPath == "" {
		return "", fmt.Errorf("path is required")
	}

	if filepath.IsAbs(targetPath) {
		rel, err := filepath.Rel(repoRoot, targetPath)
		if err != nil {
			return "", fmt.Errorf("normalize path: %w", err)
		}
		targetPath = rel
	}

	targetPath = filepath.ToSlash(filepath.Clean(strings.TrimSpace(targetPath)))
	targetPath = strings.TrimPrefix(targetPath, "./")
	return targetPath, nil
}

func topNeighborPaths(neighbors []Neighbor) []string {
	paths := make([]string, 0, len(neighbors))
	for _, neighbor := range neighbors {
		paths = append(paths, neighbor.Path)
	}
	sort.Strings(paths)
	return paths
}

func hasFileStats(store *db.Store, normalizedPath string) (bool, error) {
	repo, err := store.Repo()
	if err != nil {
		return false, err
	}
	row := store.DB().QueryRow(`SELECT 1 FROM files WHERE repo_id = ? AND path = ? LIMIT 1`, repo.ID, normalizedPath)
	var value int
	switch err := row.Scan(&value); err {
	case nil:
		return true, nil
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, fmt.Errorf("lookup file stats: %w", err)
	}
}
