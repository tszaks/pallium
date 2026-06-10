package analysis

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tszaks/pallium/internal/db"
)

type RiskReport struct {
	Path             string     `json:"path"`
	Score            int        `json:"score"`
	Level            string     `json:"level"`
	ChurnScore       int        `json:"churn_score"`
	RecentTouchCount int        `json:"recent_touch_count"`
	NeighborCount    int        `json:"neighbor_count"`
	AuthorCount      int        `json:"author_count"`
	LastTouchedAt    string     `json:"last_touched_at"`
	Reasons          []string   `json:"reasons"`
	TopNeighbors     []Neighbor `json:"top_neighbors"`
}

func Risk(store *db.Store, targetPath string) (RiskReport, error) {
	repo, err := store.Repo()
	if err != nil {
		return RiskReport{}, err
	}
	normalized, err := normalizeRepoPath(store.RepoRoot, targetPath)
	if err != nil {
		return RiskReport{}, err
	}

	row := store.DB().QueryRow(`
SELECT churn_score, recent_touch_count
     , author_count, last_touched_at
FROM files
WHERE repo_id = ? AND path = ?
`, repo.ID, normalized)

	var churnScore, recentTouchCount, authorCount int
	var lastTouchedAt string
	if err := row.Scan(&churnScore, &recentTouchCount, &authorCount, &lastTouchedAt); err != nil {
		if err == sql.ErrNoRows {
			return inferredRisk(store, normalized)
		}
		return RiskReport{}, fmt.Errorf("read file risk data: %w", err)
	}

	neighborCount, err := NeighborCount(store, normalized)
	if err != nil {
		return RiskReport{}, err
	}

	neighbors, err := Neighbors(store, normalized, 5)
	if err != nil {
		return RiskReport{}, err
	}

	score := churnScore + (recentTouchCount * 3) + min(neighborCount, 10)*2 + min(authorCount, 5)
	lastTouched, _ := time.Parse(time.RFC3339, lastTouchedAt)
	if !lastTouched.IsZero() && time.Since(lastTouched) <= 14*24*time.Hour {
		score += 2
	}

	reasons := riskReasons(churnScore, recentTouchCount, neighborCount, authorCount, lastTouched)
	level := "low"
	switch {
	case score >= 20:
		level = "high"
	case score >= 10:
		level = "medium"
	}

	return RiskReport{
		Path:             normalized,
		Score:            score,
		Level:            level,
		ChurnScore:       churnScore,
		RecentTouchCount: recentTouchCount,
		NeighborCount:    neighborCount,
		AuthorCount:      authorCount,
		LastTouchedAt:    lastTouchedAt,
		Reasons:          reasons,
		TopNeighbors:     neighbors,
	}, nil
}

func riskReasons(churnScore, recentTouchCount, neighborCount, authorCount int, lastTouched time.Time) []string {
	reasons := make([]string, 0, 4)

	switch {
	case recentTouchCount >= 2:
		reasons = append(reasons, "This file changed repeatedly in the last 30 days.")
	case churnScore >= 3:
		reasons = append(reasons, "This file has a higher-than-usual change history.")
	}

	if neighborCount >= 2 {
		reasons = append(reasons, "Changes here often fan out into other files.")
	}

	if authorCount >= 2 {
		reasons = append(reasons, "More than one author has touched this file, so shared context matters.")
	}

	if !lastTouched.IsZero() && time.Since(lastTouched) <= 14*24*time.Hour {
		reasons = append(reasons, "This file was touched recently, so nearby work may still be in motion.")
	}

	if len(reasons) == 0 {
		reasons = append(reasons, "This file looks relatively stable based on current history.")
	}

	return reasons
}

func inferredRisk(store *db.Store, normalizedPath string) (RiskReport, error) {
	if _, err := os.Stat(filepath.Join(store.RepoRoot, filepath.FromSlash(normalizedPath))); err != nil {
		return RiskReport{}, fmt.Errorf("read file risk data: %w", sql.ErrNoRows)
	}

	links, err := StructuralLinks(store, normalizedPath, 6)
	if err != nil {
		return RiskReport{}, err
	}
	tests, err := SuggestedTests(store, normalizedPath, 4)
	if err != nil {
		return RiskReport{}, err
	}

	reasons := []string{
		"This file is new to indexed history, so advice is based on repo structure instead of past commits.",
	}
	if len(links) > 0 {
		reasons = append(reasons, "The tool found related files based on naming or directory structure.")
	}
	if len(tests) > 0 {
		reasons = append(reasons, "The tool inferred likely tests for this new file.")
	}
	if len(links) == 0 && len(tests) == 0 {
		reasons = append(reasons, "This file currently looks isolated, so change risk is mostly about local correctness.")
	}

	score := min(len(links), 3)*2 + min(len(tests), 2)*2
	level := "low"
	if len(links) >= 2 || len(tests) > 0 {
		level = "medium"
	}

	return RiskReport{
		Path:          normalizedPath,
		Score:         score,
		Level:         level,
		Reasons:       reasons,
		TopNeighbors:  structuralNeighbors(links, 5),
		NeighborCount: len(links),
	}, nil
}

func structuralNeighbors(links []StructuralLink, limit int) []Neighbor {
	out := make([]Neighbor, 0, min(len(links), limit))
	for _, link := range links {
		out = append(out, Neighbor{Path: link.Path})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
