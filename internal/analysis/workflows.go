package analysis

import (
	"fmt"
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
	Summary             string                       `json:"summary"`
	Freshness           Freshness                    `json:"freshness"`
	Evidence            Evidence                     `json:"evidence"`
	Review              ReviewReport                 `json:"review"`
	ChangedNow          ChangedNowReport             `json:"changed_now"`
	NextActions         []string                     `json:"next_actions"`
	Task                TaskScopeReport              `json:"task"`
	RelatedSessions     []sessionmemory.SearchResult `json:"related_sessions"`
	VerificationHistory []db.VerificationRun         `json:"verification_history"`
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
	if failed, ok := latestFailedVerification(review.VerificationHistory); ok {
		nextActions = append(nextActions, fmt.Sprintf("Re-run failed verification before handoff: %s.", failed.Command))
	}
	if len(nextActions) == 0 {
		nextActions = append(nextActions, "No extra handoff actions suggested.")
	}

	freshness := buildFreshness(store)
	evidence := buildEvidence(freshness, len(review.ChangedFiles), len(review.RequiredTests), review.Verification, review.Task, len(review.ChangedFiles))

	return HandoffReport{
		Summary:             "Use this report to hand work from an agent back to a human or another agent with less surprise.",
		Freshness:           freshness,
		Evidence:            evidence,
		Review:              review,
		ChangedNow:          changedNow,
		NextActions:         nextActions,
		Task:                review.Task,
		RelatedSessions:     review.RelatedSessions,
		VerificationHistory: review.VerificationHistory,
	}, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
