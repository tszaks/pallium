package analysis

import (
	"errors"
	"time"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
)

type Freshness struct {
	IndexStatus          string   `json:"index_status"`
	IndexedAt            string   `json:"indexed_at"`
	IndexedBranch        string   `json:"indexed_branch"`
	LastIndexedCommit    string   `json:"last_indexed_commit"`
	CurrentBranch        string   `json:"current_branch"`
	CurrentCommit        string   `json:"current_commit"`
	WorkingTreeDirty     bool     `json:"working_tree_dirty"`
	WorkingTreeFileCount int      `json:"working_tree_file_count"`
	IsStale              bool     `json:"is_stale"`
	Reasons              []string `json:"reasons"`
}

type Evidence struct {
	Sources []string `json:"sources"`
	Notes   []string `json:"notes"`
}

func buildFreshness(store *db.Store) Freshness {
	currentBranch, _ := gitlog.CurrentBranch(store.RepoRoot)
	currentCommit, _ := gitlog.CurrentCommit(store.RepoRoot)
	workingTree, _ := gitlog.WorkingTreeChanges(store.RepoRoot)

	repo, err := store.Repo()
	if err != nil {
		if errors.Is(err, db.ErrRepoNotIndexed) {
			return Freshness{
				IndexStatus:          "missing",
				CurrentBranch:        currentBranch,
				CurrentCommit:        currentCommit,
				WorkingTreeDirty:     len(workingTree) > 0,
				WorkingTreeFileCount: len(workingTree),
				IsStale:              true,
				Reasons:              []string{"Repo has not been indexed yet. Run `pallium index` to enable history-backed guidance."},
			}
		}
		return Freshness{}
	}

	reasons := make([]string, 0, 4)
	isStale := false
	if repo.LastIndexedCommit != "" && currentCommit != "" && repo.LastIndexedCommit != currentCommit {
		isStale = true
		reasons = append(reasons, "Git HEAD moved since the last index.")
	}
	if !repo.IndexedAt.IsZero() && time.Since(repo.IndexedAt) > 24*time.Hour {
		isStale = true
		reasons = append(reasons, "The local index is more than a day old.")
	}
	if len(workingTree) > 0 {
		reasons = append(reasons, "Working tree changes exist outside the last indexed snapshot.")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "Index is aligned with the current branch and clean working tree.")
	}

	return Freshness{
		IndexStatus:          "indexed",
		IndexedAt:            formatReportTime(repo.IndexedAt),
		IndexedBranch:        repo.Branch,
		LastIndexedCommit:    repo.LastIndexedCommit,
		CurrentBranch:        currentBranch,
		CurrentCommit:        currentCommit,
		WorkingTreeDirty:     len(workingTree) > 0,
		WorkingTreeFileCount: len(workingTree),
		IsStale:              isStale,
		Reasons:              reasons,
	}
}

func buildEvidence(freshness Freshness, structuralLinks int, suggestedTests int, verification VerificationPlan, task TaskScopeReport, recentCommits int) Evidence {
	sources := make([]string, 0, 6)
	notes := make([]string, 0, 6)

	if recentCommits > 0 {
		sources = append(sources, "git-history")
		notes = append(notes, "Recent commits informed the report.")
	}
	if structuralLinks > 0 {
		sources = append(sources, "repo-structure")
		notes = append(notes, "Structural links shaped related-file guidance.")
	}
	if suggestedTests > 0 {
		sources = append(sources, "test-inference")
		notes = append(notes, "Likely test coverage was inferred from repo structure.")
	}
	if len(verification.Fast) > 0 || len(verification.Safe) > 0 || len(verification.Full) > 0 {
		sources = append(sources, "repo-config")
		notes = append(notes, "Verification guidance used repo-local config and command conventions.")
	}
	if freshness.WorkingTreeDirty {
		sources = append(sources, "working-tree")
		notes = append(notes, "Live working tree state affected freshness and caution signals.")
	}
	if task.Goal != "" {
		sources = append(sources, "task-scope")
		notes = append(notes, "Task scope data informed drift and handoff guidance.")
	}

	return Evidence{
		Sources: uniqueStrings(sources, 0),
		Notes:   uniqueStrings(notes, 0),
	}
}

func formatReportTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
