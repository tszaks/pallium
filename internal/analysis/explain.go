package analysis

import (
	"fmt"
	"strings"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
	"github.com/tszaks/pallium/internal/knowledge"
	"github.com/tszaks/pallium/internal/sessionmemory"
)

type CommitSummary struct {
	SHA         string `json:"sha"`
	Subject     string `json:"subject"`
	CommittedAt string `json:"committed_at"`
}

// ModuleRef points an explanation at the knowledge doc covering the file.
type ModuleRef struct {
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Verified bool   `json:"verified"`
}

type ExplainReport struct {
	Path            string                       `json:"path"`
	Summary         string                       `json:"summary"`
	Declares        []db.CodeSymbol              `json:"declares"`
	Module          *ModuleRef                   `json:"module,omitempty"`
	Freshness       Freshness                    `json:"freshness"`
	Evidence        Evidence                     `json:"evidence"`
	EditChecklist   []string                     `json:"edit_checklist"`
	SuggestedTests  []string                     `json:"suggested_tests"`
	TestCommands    []string                     `json:"test_commands"`
	Verification    VerificationPlan             `json:"verification"`
	BlastRadius     []string                     `json:"blast_radius"`
	StructuralLinks []StructuralLink             `json:"structural_links"`
	Confidence      Confidence                   `json:"confidence"`
	ActionGuidance  ActionGuidance               `json:"action_guidance"`
	Risk            RiskReport                   `json:"risk"`
	RecentCommits   []CommitSummary              `json:"recent_commits"`
	Decisions       []Decision                   `json:"decisions"`
	Neighbors       []Neighbor                   `json:"neighbors"`
	RelatedSessions []sessionmemory.SearchResult `json:"related_sessions"`
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
	origin, _ := gitlog.OriginURL(store.RepoRoot)
	relatedSessions, _ := sessionmemory.Related(sessionmemory.RelatedOptions{
		RepoRoot:     store.RepoRoot,
		GitOriginURL: origin,
		Files:        []string{risk.Path},
		Limit:        3,
	})

	declares, module := fileIdentity(store, repo.ID, risk.Path)

	return ExplainReport{
		Path:            risk.Path,
		Summary:         explainSummary(risk, commits, decisions, risk.Path, declares, module),
		Declares:        declares,
		Module:          module,
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
		RelatedSessions: relatedSessions,
	}, nil
}

// fileIdentity gathers what the file declares and which module it belongs to.
// Both are optional: a repo with no content index, or one where the knowledge
// base has not been built, still gets the risk half of an explanation.
func fileIdentity(store *db.Store, repoID int64, path string) ([]db.CodeSymbol, *ModuleRef) {
	if !store.HasCodeIndex(repoID) {
		return []db.CodeSymbol{}, nil
	}

	symbols, err := store.SymbolsInFile(repoID, path)
	if err != nil {
		symbols = []db.CodeSymbol{}
	}
	exported := make([]db.CodeSymbol, 0, len(symbols))
	for _, symbol := range symbols {
		if symbol.Exported {
			exported = append(exported, symbol)
		}
	}
	if len(exported) == 0 {
		exported = symbols
	}
	if len(exported) > 8 {
		exported = exported[:8]
	}

	doc, found, err := knowledge.DocForPath(store, repoID, path)
	if err != nil || !found {
		return exported, nil
	}
	return exported, &ModuleRef{
		Slug:     doc.Slug,
		Title:    doc.Title,
		Summary:  doc.Summary,
		Verified: doc.Verified,
	}
}

// explainSummary now leads with what the file is before saying how dangerous
// it is. It used to return one of three fixed strings chosen by risk level,
// which meant explain could tell you a file was risky but never what it did.
func explainSummary(risk RiskReport, commits []CommitSummary, decisions []Decision, path string, declares []db.CodeSymbol, module *ModuleRef) string {
	parts := make([]string, 0, 3)

	if len(declares) > 0 {
		names := make([]string, 0, 3)
		for _, symbol := range declares {
			if len(names) == 3 {
				break
			}
			names = append(names, symbol.Name)
		}
		lead := fmt.Sprintf("Declares %s", strings.Join(names, ", "))
		if extra := len(declares) - len(names); extra > 0 {
			lead += fmt.Sprintf(" and %d more", extra)
		}
		parts = append(parts, lead+".")
	}

	if module != nil && strings.TrimSpace(module.Summary) != "" {
		note := fmt.Sprintf("Part of %s: %s", module.Title, strings.TrimSpace(module.Summary))
		if !module.Verified {
			note += " (module doc not fully verified)"
		}
		parts = append(parts, note)
	}

	switch risk.Level {
	case "high":
		parts = append(parts, "High-risk file. Check recent commits and related files before making changes.")
	case "medium":
		parts = append(parts, "Medium-risk file. A quick scan of recent history and neighbors will lower surprise regressions.")
	default:
		if len(decisions) > 0 {
			parts = append(parts, "Lower-risk file, but there is some history worth reading before you edit.")
		} else {
			parts = append(parts, "Lower-risk file with limited recent churn.")
		}
	}

	return strings.Join(parts, " ")
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
