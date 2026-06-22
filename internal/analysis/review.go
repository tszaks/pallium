package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
	"github.com/tszaks/pallium/internal/sessionmemory"
)

type ReviewedFile struct {
	Path           string   `json:"path"`
	ChangeSource   string   `json:"change_source"`
	RiskLevel      string   `json:"risk_level"`
	TopReasons     []string `json:"top_reasons"`
	SuggestedTests []string `json:"suggested_tests"`
	BlastRadius    []string `json:"blast_radius"`
	BoundaryLabels []string `json:"boundary_labels,omitempty"`
	NeedsReview    bool     `json:"needs_review"`
	NeedsTests     bool     `json:"needs_tests"`
}

type ReviewReport struct {
	BaseRef             string                       `json:"base_ref"`
	HeadRef             string                       `json:"head_ref"`
	Summary             string                       `json:"summary"`
	Freshness           Freshness                    `json:"freshness"`
	Evidence            Evidence                     `json:"evidence"`
	ChangedFiles        []ReviewedFile               `json:"changed_files"`
	RequiredTests       []string                     `json:"required_tests"`
	TestCommands        []string                     `json:"test_commands"`
	Verification        VerificationPlan             `json:"verification"`
	Confidence          Confidence                   `json:"confidence"`
	ActionGuidance      ActionGuidance               `json:"action_guidance"`
	Task                TaskScopeReport              `json:"task"`
	BoundaryWarnings    []BoundaryWarning            `json:"boundary_warnings"`
	RelatedSessions     []sessionmemory.SearchResult `json:"related_sessions"`
	VerificationHistory []db.VerificationRun         `json:"verification_history"`
	Notes               []string                     `json:"notes"`
}

func Review(store *db.Store, baseRef string) (ReviewReport, error) {
	if strings.TrimSpace(baseRef) == "" {
		baseRef = "HEAD~1"
	}

	changed, err := gitlog.ChangedFilesBetween(store.RepoRoot, baseRef, "HEAD")
	if err != nil {
		return ReviewReport{}, err
	}
	workingTree, err := gitlog.WorkingTreeChanges(store.RepoRoot)
	if err != nil {
		return ReviewReport{}, err
	}

	changeSources := make(map[string]string)
	for _, path := range changed {
		changeSources[path] = "committed"
	}
	for _, item := range workingTree {
		source := "working_tree"
		if item.Status == "??" {
			source = "untracked"
		}
		if _, ok := changeSources[item.Path]; ok {
			source = "committed+working_tree"
		}
		changeSources[item.Path] = source
	}
	changed = mapKeysSorted(changeSources)

	reviewed := make([]ReviewedFile, 0, len(changed))
	requiredTests := make([]string, 0)
	testCommands := make([]string, 0)
	notes := make([]string, 0)
	allBoundaries := make([]BoundaryWarning, 0)
	allStructuralLinks := make([]StructuralLink, 0)
	fileVerification := make(map[string]VerificationPlan, len(changed))
	highRiskCount := 0

	for _, path := range changed {
		risk, err := Risk(store, path)
		if err != nil {
			links, _ := StructuralLinks(store, path, 4)
			tests, _ := SuggestedTests(store, path, 4)
			commands, _ := SuggestedTestCommands(store, path, 3)
			verification, _ := SuggestedVerificationPlan(store, path)
			fileVerification[path] = verification
			allStructuralLinks = append(allStructuralLinks, links...)
			requiredTests = append(requiredTests, tests...)
			testCommands = append(testCommands, commands...)
			notes = append(notes, fmt.Sprintf("No indexed risk data for %s yet.", path))
			reviewed = append(reviewed, ReviewedFile{
				Path:           path,
				ChangeSource:   changeSources[path],
				RiskLevel:      "unknown",
				TopReasons:     []string{"This file is new or outside indexed history, so risk is unknown."},
				SuggestedTests: tests,
				BlastRadius:    []string{},
				NeedsReview:    true,
				NeedsTests:     len(commands) > 0,
			})
			continue
		}
		links, err := StructuralLinks(store, path, 4)
		if err != nil {
			return ReviewReport{}, err
		}
		tests, err := SuggestedTests(store, path, 4)
		if err != nil {
			return ReviewReport{}, err
		}
		commands, err := SuggestedTestCommands(store, path, 3)
		if err != nil {
			return ReviewReport{}, err
		}
		verification, err := SuggestedVerificationPlan(store, path)
		if err != nil {
			return ReviewReport{}, err
		}
		blastRadius, err := BlastRadius(store, path, 4)
		if err != nil {
			return ReviewReport{}, err
		}

		if risk.Level == "high" {
			highRiskCount++
		}
		fileVerification[path] = verification
		boundaries := detectBoundaryWarnings(append([]string{path}, blastRadius...))
		boundaryLabels := make([]string, 0, len(boundaries))
		for _, boundary := range boundaries {
			boundaryLabels = append(boundaryLabels, boundary.Label)
		}

		requiredTests = append(requiredTests, tests...)
		testCommands = append(testCommands, commands...)
		allStructuralLinks = append(allStructuralLinks, links...)
		allBoundaries = append(allBoundaries, boundaries...)
		reviewed = append(reviewed, ReviewedFile{
			Path:           path,
			ChangeSource:   changeSources[path],
			RiskLevel:      risk.Level,
			TopReasons:     risk.Reasons,
			SuggestedTests: tests,
			BlastRadius:    blastRadius,
			BoundaryLabels: boundaryLabels,
			NeedsReview:    risk.Level == "high" || len(boundaries) > 0 || len(blastRadius) >= 4,
			NeedsTests:     len(commands) > 0,
		})
	}

	task, err := activeTaskScope(store, changed)
	if err != nil {
		return ReviewReport{}, err
	}
	structuralLinks := uniqueStructuralLinks(allStructuralLinks, 0)
	confidence := buildConfidence(len(reviewed) > 0, len(structuralLinks), len(requiredTests), len(reviewed))
	freshness := buildFreshness(store)
	sortReviewedFiles(reviewed, task)
	verification := verificationPlanFromReviewed(reviewed, fileVerification)
	actionGuidance := buildActionGuidance("working-tree", RiskReport{Level: "medium"}, confidence, structuralLinks, pathsFromReviewed(reviewed), verification.Fast)
	if task.HasScopeDrift {
		notes = append(notes, fmt.Sprintf("Active task drifted outside planned scope: %s.", strings.Join(task.OutOfScopeChanged, ", ")))
		actionGuidance.AskForReviewIf = uniqueStrings(append(actionGuidance.AskForReviewIf, "the change drifted outside the active task scope"), 5)
		actionGuidance.StopSignals = uniqueStrings(append(actionGuidance.StopSignals, "Task scope drift detected: review out-of-scope files before handoff."), 5)
		actionGuidance.MustReview = true
	}
	if highRiskCount > 0 {
		actionGuidance.StopSignals = uniqueStrings(append(actionGuidance.StopSignals, fmt.Sprintf("%d high-risk changed file(s) need extra review before handoff.", highRiskCount)), 5)
		actionGuidance.MustReview = true
	}
	if len(uniqueBoundaryWarnings(allBoundaries)) > 1 {
		actionGuidance.StopSignals = uniqueStrings(append(actionGuidance.StopSignals, "Multiple sensitive boundaries changed together: slow down and review the full flow."), 5)
		actionGuidance.MustReview = true
	}
	if len(verification.Fast) == 0 {
		actionGuidance.StopSignals = uniqueStrings(append(actionGuidance.StopSignals, "No focused verification command was inferred for this review: pick a manual verification plan before handoff."), 5)
		actionGuidance.MustVerify = true
	}
	if len(verification.Fast) > 0 || len(verification.Safe) > 0 || len(verification.Full) > 0 {
		actionGuidance.MustVerify = true
	}
	if actionGuidance.RecommendedNextCommand == "" && len(verification.Fast) > 0 {
		actionGuidance.RecommendedNextCommand = verification.Fast[0]
	}
	evidence := buildEvidence(freshness, len(reviewed), len(requiredTests), verification, task, len(reviewed))

	summary := fmt.Sprintf("Review %d changed files before handing this branch back to an agent.", len(changed))
	if highRiskCount > 0 {
		summary = fmt.Sprintf("Review %d changed files carefully. %d high-risk file(s) need extra attention.", len(changed), highRiskCount)
	}
	origin, _ := gitlog.OriginURL(store.RepoRoot)
	relatedSessions, _ := sessionmemory.Related(sessionmemory.RelatedOptions{
		RepoRoot:     store.RepoRoot,
		GitOriginURL: origin,
		Files:        changed,
		Limit:        5,
	})
	verificationHistory, _ := store.RecentVerificationRuns(5)
	if failed, ok := latestFailedVerification(verificationHistory); ok {
		notes = append(notes, fmt.Sprintf("Most recent verification failed: `%s` exited %d.", failed.Command, failed.ExitCode))
		actionGuidance.StopSignals = uniqueStrings(append(actionGuidance.StopSignals, "Most recent verification failed: re-run verification before handoff."), 5)
		actionGuidance.MustVerify = true
		if actionGuidance.RecommendedNextCommand == "" {
			actionGuidance.RecommendedNextCommand = failed.Command
		}
	}

	return ReviewReport{
		BaseRef:             baseRef,
		HeadRef:             "HEAD",
		Summary:             summary,
		Freshness:           freshness,
		Evidence:            evidence,
		ChangedFiles:        reviewed,
		RequiredTests:       uniqueStrings(requiredTests, 10),
		TestCommands:        uniqueStrings(testCommands, 5),
		Verification:        verification,
		Confidence:          confidence,
		ActionGuidance:      actionGuidance,
		Task:                task,
		BoundaryWarnings:    uniqueBoundaryWarnings(allBoundaries),
		RelatedSessions:     relatedSessions,
		VerificationHistory: verificationHistory,
		Notes:               uniqueStrings(notes, 10),
	}, nil
}

func mapKeysSorted(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return uniqueStrings(keys, 0)
}

func pathsFromReviewed(files []ReviewedFile) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Path)
	}
	return out
}

func verificationPlanFromReviewed(files []ReviewedFile, perFile map[string]VerificationPlan) VerificationPlan {
	fast := make([]string, 0)
	safe := make([]string, 0)
	full := make([]string, 0)
	for _, file := range files {
		plan := perFile[file.Path]
		fast = append(fast, plan.Fast...)
		safe = append(safe, plan.Safe...)
		full = append(full, plan.Full...)
	}
	return VerificationPlan{
		Fast: uniqueStrings(fast, 5),
		Safe: uniqueStrings(safe, 6),
		Full: uniqueStrings(full, 6),
	}
}

func uniqueBoundaryWarnings(values []BoundaryWarning) []BoundaryWarning {
	seen := map[string]struct{}{}
	out := make([]BoundaryWarning, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value.Label]; ok {
			continue
		}
		seen[value.Label] = struct{}{}
		out = append(out, value)
	}
	return out
}

func sortReviewedFiles(files []ReviewedFile, task TaskScopeReport) {
	outOfScope := make(map[string]struct{}, len(task.OutOfScopeChanged))
	for _, path := range task.OutOfScopeChanged {
		outOfScope[path] = struct{}{}
	}

	sort.SliceStable(files, func(i, j int) bool {
		left := reviewPriority(files[i], outOfScope)
		right := reviewPriority(files[j], outOfScope)
		if left != right {
			return left > right
		}
		return files[i].Path < files[j].Path
	})
}

func reviewPriority(file ReviewedFile, outOfScope map[string]struct{}) int {
	score := 0
	if _, ok := outOfScope[file.Path]; ok {
		score += 100
	}
	if file.ChangeSource == "untracked" || strings.Contains(file.ChangeSource, "working_tree") {
		score += 100
	}
	switch file.RiskLevel {
	case "high":
		score += 50
	case "medium":
		score += 25
	case "unknown":
		score += 20
	}
	score += len(file.BoundaryLabels) * 15
	if file.NeedsReview {
		score += 10
	}
	if !file.NeedsTests {
		score += 8
	}
	score += min(len(file.BlastRadius), 5)
	return score
}

func latestFailedVerification(runs []db.VerificationRun) (db.VerificationRun, bool) {
	if len(runs) == 0 {
		return db.VerificationRun{}, false
	}
	latest := runs[0]
	if latest.ExitCode == 0 {
		return db.VerificationRun{}, false
	}
	return latest, true
}
