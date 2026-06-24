package analysis

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/db"
)

type BoundaryWarning struct {
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

type ActionGuidance struct {
	InspectFirst           []string          `json:"inspect_first"`
	RunNext                []string          `json:"run_next"`
	RecommendedNextCommand string            `json:"recommended_next_command"`
	SafeToEditAlone        bool              `json:"safe_to_edit_alone"`
	MustReview             bool              `json:"must_review"`
	MustVerify             bool              `json:"must_verify"`
	AskForReviewIf         []string          `json:"ask_for_review_if"`
	StopSignals            []string          `json:"stop_signals"`
	ConfidenceGaps         []string          `json:"confidence_gaps"`
	BoundaryWarnings       []BoundaryWarning `json:"boundary_warnings"`
}

type TaskScopeReport struct {
	Goal              string   `json:"goal"`
	ScopePaths        []string `json:"scope_paths"`
	StartedAt         string   `json:"started_at"`
	InScopeChanged    []string `json:"in_scope_changed"`
	OutOfScopeChanged []string `json:"out_of_scope_changed"`
	HasScopeDrift     bool     `json:"has_scope_drift"`
}

func buildActionGuidance(path string, risk RiskReport, confidence Confidence, structuralLinks []StructuralLink, blastRadius []string, testCommands []string) ActionGuidance {
	boundaries := detectBoundaryWarnings(append([]string{path}, blastRadius...))
	inspectFirst := make([]string, 0, 4)
	inspectFirst = append(inspectFirst, blastRadius...)
	for _, link := range structuralLinks {
		inspectFirst = append(inspectFirst, link.Path)
	}

	runNext := make([]string, 0, 2)
	runNext = append(runNext, testCommands...)
	runNext = append(runNext, fmt.Sprintf("pallium explain %s", path))

	askForReviewIf := make([]string, 0, 4)
	if risk.Level == "high" {
		askForReviewIf = append(askForReviewIf, "risk stays high after the change")
	}
	for _, boundary := range boundaries {
		askForReviewIf = append(askForReviewIf, "the change crosses the "+boundary.Label+" boundary")
	}
	if len(blastRadius) >= 4 {
		askForReviewIf = append(askForReviewIf, "the blast radius grows beyond a few files")
	}

	confidenceGaps := make([]string, 0, 3)
	for _, reason := range confidence.Reasons {
		if strings.Contains(strings.ToLower(reason), "new") || strings.Contains(strings.ToLower(reason), "outside indexed history") {
			confidenceGaps = append(confidenceGaps, "No commit history for this file yet.")
		}
	}
	if len(structuralLinks) == 0 {
		confidenceGaps = append(confidenceGaps, "No structural links were found.")
	}
	if len(testCommands) == 0 {
		confidenceGaps = append(confidenceGaps, "No test command could be inferred.")
	}

	safeToEditAlone := risk.Level == "low" && len(boundaries) == 0 && len(blastRadius) <= 2
	mustReview := risk.Level == "high" || len(boundaries) > 0 || len(blastRadius) >= 4
	mustVerify := len(testCommands) > 0 || risk.Level != "low" || len(boundaries) > 0

	stopSignals := make([]string, 0, 4)
	if risk.Level == "high" {
		stopSignals = append(stopSignals, "High-risk file: inspect recent history and related files before editing.")
	}
	if len(boundaries) > 0 {
		stopSignals = append(stopSignals, "Sensitive boundary touched: get an extra review pass before handoff.")
	}
	if len(blastRadius) >= 4 {
		stopSignals = append(stopSignals, "Blast radius is broad: review likely impact files before widening the change.")
	}
	if confidence.Level == "low" {
		stopSignals = append(stopSignals, "Confidence is low: treat the guidance as a hint, not a guarantee.")
	}
	if len(testCommands) == 0 {
		stopSignals = append(stopSignals, "No focused test command was inferred: verify manually before handoff.")
	}

	recommendedNextCommand := ""
	if len(runNext) > 0 {
		recommendedNextCommand = runNext[0]
	}

	return ActionGuidance{
		InspectFirst:           uniqueStrings(inspectFirst, 4),
		RunNext:                uniqueStrings(runNext, 3),
		RecommendedNextCommand: recommendedNextCommand,
		SafeToEditAlone:        safeToEditAlone,
		MustReview:             mustReview,
		MustVerify:             mustVerify,
		AskForReviewIf:         uniqueStrings(askForReviewIf, 4),
		StopSignals:            uniqueStrings(stopSignals, 4),
		ConfidenceGaps:         uniqueStrings(confidenceGaps, 4),
		BoundaryWarnings:       boundaries,
	}
}

func activeTaskScope(store *db.Store, changed []string) (TaskScopeReport, error) {
	task, err := store.ActiveTask()
	if err != nil {
		if err == sql.ErrNoRows {
			return TaskScopeReport{}, nil
		}
		if errors.Is(err, db.ErrRepoNotIndexed) {
			return TaskScopeReport{}, nil
		}
		return TaskScopeReport{}, err
	}

	inScope := make([]string, 0, len(changed))
	outOfScope := make([]string, 0, len(changed))
	for _, path := range changed {
		if matchesTaskScope(path, task.ScopePaths) {
			inScope = append(inScope, path)
			continue
		}
		outOfScope = append(outOfScope, path)
	}

	return TaskScopeReport{
		Goal:              task.Goal,
		ScopePaths:        task.ScopePaths,
		StartedAt:         formatTaskStartedAt(task.StartedAt),
		InScopeChanged:    inScope,
		OutOfScopeChanged: outOfScope,
		HasScopeDrift:     len(outOfScope) > 0 && len(changed) > 0,
	}, nil
}

func matchesTaskScope(path string, scopePaths []string) bool {
	normalized := filepath.ToSlash(filepath.Clean(path))
	for _, scope := range scopePaths {
		scope = filepath.ToSlash(filepath.Clean(scope))
		if normalized == scope || strings.HasPrefix(normalized, strings.TrimSuffix(scope, "/")+"/") {
			return true
		}
	}
	return false
}

func detectBoundaryWarnings(paths []string) []BoundaryWarning {
	type boundaryRule struct {
		label   string
		matches []string
		reason  string
	}
	rules := []boundaryRule{
		{label: "auth", matches: []string{"auth", "login", "session", "token", "permission"}, reason: "Auth changes can widen access or session bugs quickly."},
		{label: "config", matches: []string{"config", ".env", "settings", "yaml", "yml", "json"}, reason: "Config changes can affect many flows at once."},
		{label: "db", matches: []string{"db", "database", "migration", "schema", "sql", "supabase", "prisma"}, reason: "Database changes can break persistence or existing data assumptions."},
		{label: "api", matches: []string{"api", "route", "routes", "handler", "controller", "endpoint"}, reason: "API boundary changes can break callers and clients."},
		{label: "payments", matches: []string{"payment", "billing", "invoice", "stripe", "subscription"}, reason: "Payment code needs an extra review pass because mistakes are expensive."},
		{label: "jobs", matches: []string{"job", "queue", "worker", "cron", "background"}, reason: "Background work can fail later and be harder to notice."},
	}

	out := make([]BoundaryWarning, 0, 4)
	seen := map[string]struct{}{}
	for _, path := range paths {
		lower := strings.ToLower(path)
		for _, rule := range rules {
			if _, ok := seen[rule.label]; ok {
				continue
			}
			for _, needle := range rule.matches {
				if strings.Contains(lower, needle) {
					seen[rule.label] = struct{}{}
					out = append(out, BoundaryWarning{Label: rule.label, Reason: rule.reason})
					break
				}
			}
		}
	}
	return out
}

func formatTaskStartedAt(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
