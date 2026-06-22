package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
	"github.com/tszaks/pallium/internal/sessionmemory"
)

type SafeReport struct {
	Path           string           `json:"path"`
	Verdict        string           `json:"verdict"`
	Summary        string           `json:"summary"`
	Freshness      Freshness        `json:"freshness"`
	Evidence       Evidence         `json:"evidence"`
	RequiredChecks []string         `json:"required_checks"`
	SuggestedTests []string         `json:"suggested_tests"`
	TestCommands   []string         `json:"test_commands"`
	Verification   VerificationPlan `json:"verification"`
	BlastRadius    []string         `json:"blast_radius"`
	Confidence     Confidence       `json:"confidence"`
	ActionGuidance ActionGuidance   `json:"action_guidance"`
	Risk           RiskReport       `json:"risk"`
}

type PlanReport struct {
	Path           string           `json:"path"`
	Goal           string           `json:"goal"`
	Freshness      Freshness        `json:"freshness"`
	Evidence       Evidence         `json:"evidence"`
	Steps          []string         `json:"steps"`
	FilesToInspect []string         `json:"files_to_inspect"`
	TestsToRun     []string         `json:"tests_to_run"`
	TestCommands   []string         `json:"test_commands"`
	Verification   VerificationPlan `json:"verification"`
	Confidence     Confidence       `json:"confidence"`
	ActionGuidance ActionGuidance   `json:"action_guidance"`
	Risk           RiskReport       `json:"risk"`
}

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
	BaseRef          string                       `json:"base_ref"`
	HeadRef          string                       `json:"head_ref"`
	Summary          string                       `json:"summary"`
	Freshness        Freshness                    `json:"freshness"`
	Evidence         Evidence                     `json:"evidence"`
	ChangedFiles     []ReviewedFile               `json:"changed_files"`
	RequiredTests    []string                     `json:"required_tests"`
	TestCommands     []string                     `json:"test_commands"`
	Verification     VerificationPlan             `json:"verification"`
	Confidence       Confidence                   `json:"confidence"`
	ActionGuidance   ActionGuidance               `json:"action_guidance"`
	Task             TaskScopeReport              `json:"task"`
	BoundaryWarnings []BoundaryWarning            `json:"boundary_warnings"`
	RelatedSessions  []sessionmemory.SearchResult `json:"related_sessions"`
	Notes            []string                     `json:"notes"`
}

type ChangedNowFile struct {
	Path              string   `json:"path"`
	WorkingTreeStatus string   `json:"working_tree_status"`
	RiskLevel         string   `json:"risk_level"`
	SuggestedTests    []string `json:"suggested_tests"`
	BlastRadius       []string `json:"blast_radius"`
}

type ChangedNowReport struct {
	Summary                string           `json:"summary"`
	IndexStatus            string           `json:"index_status"`
	RecommendedNextCommand string           `json:"recommended_next_command,omitempty"`
	Freshness              Freshness        `json:"freshness"`
	Evidence               Evidence         `json:"evidence"`
	Files                  []ChangedNowFile `json:"files"`
	Task                   TaskScopeReport  `json:"task"`
}

type HandoffReport struct {
	Summary         string                       `json:"summary"`
	Freshness       Freshness                    `json:"freshness"`
	Evidence        Evidence                     `json:"evidence"`
	Review          ReviewReport                 `json:"review"`
	ChangedNow      ChangedNowReport             `json:"changed_now"`
	NextActions     []string                     `json:"next_actions"`
	Task            TaskScopeReport              `json:"task"`
	RelatedSessions []sessionmemory.SearchResult `json:"related_sessions"`
}

func Safe(store *db.Store, targetPath string) (SafeReport, error) {
	risk, err := Risk(store, targetPath)
	if err != nil {
		return SafeReport{}, err
	}
	tests, err := SuggestedTests(store, targetPath, 5)
	if err != nil {
		return SafeReport{}, err
	}
	blastRadius, err := BlastRadius(store, targetPath, 6)
	if err != nil {
		return SafeReport{}, err
	}
	testCommands, err := SuggestedTestCommands(store, targetPath, 5)
	if err != nil {
		return SafeReport{}, err
	}
	verification, err := SuggestedVerificationPlan(store, targetPath)
	if err != nil {
		return SafeReport{}, err
	}
	structuralLinks, err := StructuralLinks(store, targetPath, 6)
	if err != nil {
		return SafeReport{}, err
	}

	verdict := "safe_with_normal_review"
	summary := "Looks reasonably safe for an agent to edit with a normal review pass."
	switch risk.Level {
	case "high":
		verdict = "inspect_context_first"
		summary = "High-risk edit. An agent should inspect neighbors and recent history before changing this file."
	case "medium":
		verdict = "review_neighbors_first"
		summary = "Medium-risk edit. An agent should inspect related files and run the suggested tests."
	}

	checks := []string{
		"Read the explain report before editing.",
	}
	if len(blastRadius) > 0 {
		checks = append(checks, fmt.Sprintf("Inspect likely impact files: %s.", strings.Join(blastRadius[:min(len(blastRadius), 3)], ", ")))
	}
	if len(tests) > 0 {
		checks = append(checks, fmt.Sprintf("Run focused tests after editing: %s.", strings.Join(tests, ", ")))
	}

	confidence := buildConfidence(true, len(structuralLinks), len(tests), len(blastRadius))
	freshness := buildFreshness(store)
	evidence := buildEvidence(freshness, len(structuralLinks), len(tests), verification, TaskScopeReport{}, 1)

	return SafeReport{
		Path:           risk.Path,
		Verdict:        verdict,
		Summary:        summary,
		Freshness:      freshness,
		Evidence:       evidence,
		RequiredChecks: checks,
		SuggestedTests: tests,
		TestCommands:   testCommands,
		Verification:   verification,
		BlastRadius:    blastRadius,
		Confidence:     confidence,
		ActionGuidance: buildActionGuidance(risk.Path, risk, confidence, structuralLinks, blastRadius, verification.Fast),
		Risk:           risk,
	}, nil
}

func Plan(store *db.Store, targetPath string) (PlanReport, error) {
	safe, err := Safe(store, targetPath)
	if err != nil {
		return PlanReport{}, err
	}

	filesToInspect := uniqueStrings(append([]string{safe.Path}, safe.BlastRadius...), 5)
	steps := []string{
		fmt.Sprintf("Read `pallium explain %s` and inspect the recent decisions.", safe.Path),
		"Open the highest-signal related files before editing.",
		"Make the minimal change needed for the task.",
		"Run the focused tests suggested by pallium.",
		"Re-run explain or review if the blast radius grew during the change.",
	}

	return PlanReport{
		Path:           safe.Path,
		Goal:           "Help an agent make a low-surprise change with the right context loaded first.",
		Freshness:      safe.Freshness,
		Evidence:       safe.Evidence,
		Steps:          steps,
		FilesToInspect: filesToInspect,
		TestsToRun:     safe.SuggestedTests,
		TestCommands:   safe.TestCommands,
		Verification:   safe.Verification,
		Confidence:     safe.Confidence,
		ActionGuidance: safe.ActionGuidance,
		Risk:           safe.Risk,
	}, nil
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
	reviewFast := make([]string, 0)
	reviewSafe := make([]string, 0)
	reviewFull := make([]string, 0)
	notes := make([]string, 0)
	allBoundaries := make([]BoundaryWarning, 0)
	highRiskCount := 0

	for _, path := range changed {
		risk, err := Risk(store, path)
		if err != nil {
			notes = append(notes, fmt.Sprintf("No indexed risk data for %s yet.", path))
			reviewed = append(reviewed, ReviewedFile{
				Path:           path,
				ChangeSource:   changeSources[path],
				RiskLevel:      "unknown",
				TopReasons:     []string{"This file is new or outside indexed history, so risk is unknown."},
				SuggestedTests: []string{},
				BlastRadius:    []string{},
				NeedsReview:    true,
				NeedsTests:     true,
			})
			continue
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
		boundaries := detectBoundaryWarnings(append([]string{path}, blastRadius...))
		boundaryLabels := make([]string, 0, len(boundaries))
		for _, boundary := range boundaries {
			boundaryLabels = append(boundaryLabels, boundary.Label)
		}

		requiredTests = append(requiredTests, tests...)
		testCommands = append(testCommands, commands...)
		reviewFast = append(reviewFast, verification.Fast...)
		reviewSafe = append(reviewSafe, verification.Safe...)
		reviewFull = append(reviewFull, verification.Full...)
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
	confidence := buildConfidence(len(reviewed) > 0, 1, len(requiredTests), len(reviewed))
	freshness := buildFreshness(store)
	verification := VerificationPlan{
		Fast: uniqueStrings(reviewFast, 5),
		Safe: uniqueStrings(reviewSafe, 6),
		Full: uniqueStrings(reviewFull, 6),
	}
	actionGuidance := buildActionGuidance("working-tree", RiskReport{Level: "medium"}, confidence, nil, pathsFromReviewed(reviewed), verification.Fast)
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
	if len(reviewFast) == 0 {
		actionGuidance.StopSignals = uniqueStrings(append(actionGuidance.StopSignals, "No focused verification command was inferred for this review: pick a manual verification plan before handoff."), 5)
		actionGuidance.MustVerify = true
	}
	if len(reviewFast) > 0 || len(reviewSafe) > 0 || len(reviewFull) > 0 {
		actionGuidance.MustVerify = true
	}
	if actionGuidance.RecommendedNextCommand == "" && len(verification.Fast) > 0 {
		actionGuidance.RecommendedNextCommand = verification.Fast[0]
	}
	sortReviewedFiles(reviewed, task)
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

	return ReviewReport{
		BaseRef:          baseRef,
		HeadRef:          "HEAD",
		Summary:          summary,
		Freshness:        freshness,
		Evidence:         evidence,
		ChangedFiles:     reviewed,
		RequiredTests:    uniqueStrings(requiredTests, 10),
		TestCommands:     uniqueStrings(testCommands, 5),
		Verification:     verification,
		Confidence:       confidence,
		ActionGuidance:   actionGuidance,
		Task:             task,
		BoundaryWarnings: uniqueBoundaryWarnings(allBoundaries),
		RelatedSessions:  relatedSessions,
		Notes:            uniqueStrings(notes, 10),
	}, nil
}

func ChangedNow(store *db.Store) (ChangedNowReport, error) {
	workingTree, err := gitlog.WorkingTreeChanges(store.RepoRoot)
	if err != nil {
		return ChangedNowReport{}, err
	}

	files := make([]ChangedNowFile, 0, len(workingTree))
	for _, item := range workingTree {
		risk, err := Risk(store, item.Path)
		if err != nil {
			files = append(files, ChangedNowFile{
				Path:              item.Path,
				WorkingTreeStatus: item.Status,
				RiskLevel:         "unknown",
				SuggestedTests:    []string{},
				BlastRadius:       []string{},
			})
			continue
		}
		tests, err := SuggestedTests(store, item.Path, 4)
		if err != nil {
			return ChangedNowReport{}, err
		}
		blastRadius, err := BlastRadius(store, item.Path, 4)
		if err != nil {
			return ChangedNowReport{}, err
		}
		files = append(files, ChangedNowFile{
			Path:              item.Path,
			WorkingTreeStatus: item.Status,
			RiskLevel:         risk.Level,
			SuggestedTests:    tests,
			BlastRadius:       blastRadius,
		})
	}

	changedPaths := make([]string, 0, len(files))
	for _, file := range files {
		changedPaths = append(changedPaths, file.Path)
	}
	task, err := activeTaskScope(store, changedPaths)
	if err != nil {
		return ChangedNowReport{}, err
	}
	freshness := buildFreshness(store)
	evidence := buildEvidence(freshness, len(files), 0, VerificationPlan{}, task, 0)
	indexStatus := firstNonEmptyString(freshness.IndexStatus, "unknown")
	recommendedNextCommand := ""
	if indexStatus == "missing" {
		recommendedNextCommand = "pallium index"
	}

	return ChangedNowReport{
		Summary:                fmt.Sprintf("Working tree currently touches %d file(s).", len(files)),
		IndexStatus:            indexStatus,
		RecommendedNextCommand: recommendedNextCommand,
		Freshness:              freshness,
		Evidence:               evidence,
		Files:                  files,
		Task:                   task,
	}, nil
}

func Handoff(store *db.Store, baseRef string) (HandoffReport, error) {
	review, err := Review(store, baseRef)
	if err != nil {
		return HandoffReport{}, err
	}
	changedNow, err := ChangedNow(store)
	if err != nil {
		return HandoffReport{}, err
	}

	nextActions := make([]string, 0, 3)
	if len(review.RequiredTests) > 0 {
		nextActions = append(nextActions, fmt.Sprintf("Run focused tests: %s.", strings.Join(review.RequiredTests, ", ")))
	}
	if len(changedNow.Files) > 0 {
		nextActions = append(nextActions, "Review unstaged and untracked files before handing work back.")
	}
	if len(review.ChangedFiles) > 0 {
		nextActions = append(nextActions, "Open the highest-risk changed files and scan their blast radius.")
	}
	if review.Task.HasScopeDrift {
		nextActions = append(nextActions, fmt.Sprintf("Resolve task scope drift before handoff: %s.", strings.Join(review.Task.OutOfScopeChanged, ", ")))
	}
	if len(nextActions) == 0 {
		nextActions = append(nextActions, "No extra handoff actions suggested.")
	}

	freshness := buildFreshness(store)
	evidence := buildEvidence(freshness, len(review.ChangedFiles), len(review.RequiredTests), review.Verification, review.Task, len(review.ChangedFiles))

	return HandoffReport{
		Summary:         "Use this report to hand work from an agent back to a human or another agent with less surprise.",
		Freshness:       freshness,
		Evidence:        evidence,
		Review:          review,
		ChangedNow:      changedNow,
		NextActions:     nextActions,
		Task:            review.Task,
		RelatedSessions: review.RelatedSessions,
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

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
