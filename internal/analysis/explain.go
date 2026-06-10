package analysis

import (
	"fmt"

	"github.com/tszaks/pallium/internal/db"
)

type CommitSummary struct {
	SHA         string `json:"sha"`
	Subject     string `json:"subject"`
	CommittedAt string `json:"committed_at"`
}

type ExplainReport struct {
	Path            string           `json:"path"`
	Summary         string           `json:"summary"`
	Freshness       Freshness        `json:"freshness"`
	Evidence        Evidence         `json:"evidence"`
	EditChecklist   []string         `json:"edit_checklist"`
	SuggestedTests  []string         `json:"suggested_tests"`
	TestCommands    []string         `json:"test_commands"`
	Verification    VerificationPlan `json:"verification"`
	BlastRadius     []string         `json:"blast_radius"`
	StructuralLinks []StructuralLink `json:"structural_links"`
	Confidence      Confidence       `json:"confidence"`
	ActionGuidance  ActionGuidance   `json:"action_guidance"`
	Risk            RiskReport       `json:"risk"`
	RecentCommits   []CommitSummary  `json:"recent_commits"`
	Decisions       []Decision       `json:"decisions"`
	Neighbors       []Neighbor       `json:"neighbors"`
}

func Explain(store *db.Store, targetPath string) (ExplainReport, error) {
	risk, err := Risk(store, targetPath)
	if err != nil {
		return ExplainReport{}, err
	}

	repo, err := store.Repo()
	if err != nil {
		return ExplainReport{}, err
	}

	rows, err := store.DB().Query(`
SELECT c.sha, c.subject, c.committed_at
FROM file_commits fc
JOIN commits c
  ON c.repo_id = fc.repo_id AND c.sha = fc.commit_sha
WHERE fc.repo_id = ? AND fc.file_path = ?
ORDER BY c.committed_at DESC
LIMIT 5
`, repo.ID, risk.Path)
	if err != nil {
		return ExplainReport{}, fmt.Errorf("query recent commits: %w", err)
	}
	defer rows.Close()

	commits := make([]CommitSummary, 0)
	commitRefs := make([]string, 0, 5)
	for rows.Next() {
		var item CommitSummary
		if err := rows.Scan(&item.SHA, &item.Subject, &item.CommittedAt); err != nil {
			return ExplainReport{}, fmt.Errorf("scan recent commit: %w", err)
		}
		commits = append(commits, item)
		commitRefs = append(commitRefs, item.SHA)
	}

	decisions, err := DecisionsByRefs(store, commitRefs, 3)
	if err != nil {
		return ExplainReport{}, err
	}
	suggestedTests, err := SuggestedTests(store, risk.Path, 5)
	if err != nil {
		return ExplainReport{}, err
	}
	blastRadius, err := BlastRadius(store, risk.Path, 6)
	if err != nil {
		return ExplainReport{}, err
	}
	structuralLinks, err := StructuralLinks(store, risk.Path, 6)
	if err != nil {
		return ExplainReport{}, err
	}
	testCommands, err := SuggestedTestCommands(store, risk.Path, 5)
	if err != nil {
		return ExplainReport{}, err
	}
	verification, err := SuggestedVerificationPlan(store, risk.Path)
	if err != nil {
		return ExplainReport{}, err
	}
	confidence := buildConfidence(true, len(structuralLinks), len(suggestedTests), len(blastRadius))
	freshness := buildFreshness(store)
	actionGuidance := buildActionGuidance(risk.Path, risk, confidence, structuralLinks, blastRadius, verification.Fast)
	evidence := buildEvidence(freshness, len(structuralLinks), len(suggestedTests), verification, TaskScopeReport{}, len(commits))

	return ExplainReport{
		Path:            risk.Path,
		Summary:         explainSummary(risk, commits, decisions),
		Freshness:       freshness,
		Evidence:        evidence,
		EditChecklist:   editChecklist(risk, commits),
		SuggestedTests:  suggestedTests,
		TestCommands:    testCommands,
		Verification:    verification,
		BlastRadius:     blastRadius,
		StructuralLinks: structuralLinks,
		Confidence:      confidence,
		ActionGuidance:  actionGuidance,
		Risk:            risk,
		RecentCommits:   commits,
		Decisions:       decisions,
		Neighbors:       risk.TopNeighbors,
	}, nil
}

func explainSummary(risk RiskReport, commits []CommitSummary, decisions []Decision) string {
	switch risk.Level {
	case "high":
		return "High-risk file. Check recent commits and related files before making changes."
	case "medium":
		return "Medium-risk file. A quick scan of recent history and neighbors will lower surprise regressions."
	default:
		if len(decisions) > 0 {
			return "Lower-risk file, but there is some history worth reading before you edit."
		}
		return "Lower-risk file with limited recent churn."
	}
}

func editChecklist(risk RiskReport, commits []CommitSummary) []string {
	checklist := make([]string, 0, 4)

	if len(risk.TopNeighbors) > 0 {
		checklist = append(checklist, fmt.Sprintf("Review related file %s before editing alone.", risk.TopNeighbors[0].Path))
	}
	if len(commits) > 0 {
		checklist = append(checklist, fmt.Sprintf("Read the latest commit touching this file: %s.", commits[0].SHA[:8]))
	}
	if risk.AuthorCount >= 2 {
		checklist = append(checklist, "Expect shared ownership context because multiple authors have touched this file.")
	}
	if risk.RecentTouchCount >= 2 {
		checklist = append(checklist, "Double-check nearby work because this file changed several times recently.")
	}

	if len(checklist) == 0 {
		checklist = append(checklist, "This file looks isolated enough to change with a normal review pass.")
	}

	return checklist
}
