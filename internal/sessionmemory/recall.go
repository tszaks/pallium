package sessionmemory

import (
	"context"
	"fmt"
	"math"
	"strings"
)

type RecallOptions struct {
	Search      SessionSearchOptions
	LexicalOnly bool
}

type RecallScope struct {
	RepoRoot string   `json:"repo_root,omitempty"`
	Source   string   `json:"source,omitempty"`
	Files    []string `json:"files"`
}

type RecallConfidence struct {
	Level   string   `json:"level"`
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

type RecallEvidence struct {
	Citation SearchCitation `json:"citation"`
	Role     string         `json:"role"`
	Kind     string         `json:"kind"`
	Summary  string         `json:"summary"`
}

type RecallMatch struct {
	SessionID string   `json:"session_id"`
	Title     string   `json:"title"`
	Score     float64  `json:"score"`
	Signals   []string `json:"signals"`
}

type RecallReport struct {
	Question         string           `json:"question"`
	Scope            RecallScope      `json:"scope"`
	Confidence       RecallConfidence `json:"confidence"`
	SessionID        string           `json:"session_id,omitempty"`
	Title            string           `json:"title,omitempty"`
	StoppedAt        string           `json:"stopped_at"`
	Completed        []string         `json:"completed"`
	Remaining        []string         `json:"remaining"`
	Blockers         []string         `json:"blockers"`
	NextAction       string           `json:"next_action"`
	Evidence         []RecallEvidence `json:"evidence"`
	CoverageWarnings []string         `json:"coverage_warnings"`
	Matches          []RecallMatch    `json:"matches"`
}

const minimumRecallSemanticSimilarity = 0.35

func Recall(ctx context.Context, opts RecallOptions) (RecallReport, error) {
	search := opts.Search
	if search.Limit <= 0 {
		search.Limit = 5
	}
	if !opts.LexicalOnly {
		search.Hybrid = true
		search.MinimumSemanticScore = minimumRecallSemanticSimilarity
	}
	report := RecallReport{
		Question: strings.TrimSpace(search.Query),
		Scope: RecallScope{
			RepoRoot: search.RepoRoot,
			Source:   search.Source,
			Files:    append([]string{}, search.Files...),
		},
		Confidence:       RecallConfidence{Level: "low", Score: 0, Reasons: []string{}},
		Completed:        []string{},
		Remaining:        []string{},
		Blockers:         []string{},
		Evidence:         []RecallEvidence{},
		CoverageWarnings: []string{},
		Matches:          []RecallMatch{},
	}
	results, err := SearchWithOptions(ctx, search)
	if err != nil {
		return report, err
	}
	results = credibleRecallResults(results)
	if len(results) == 0 {
		report.Confidence.Reasons = append(report.Confidence.Reasons, "No matching indexed sessions were found.")
		report.CoverageWarnings = append(report.CoverageWarnings, "Recall has no evidence for this question. Run `pallium sessions sync` or broaden the filters.")
		return report, nil
	}
	for _, result := range results {
		score := result.HybridScore
		if score == 0 {
			score = math.Max(result.LexicalScore, result.SemanticScore)
		}
		report.Matches = append(report.Matches, RecallMatch{SessionID: result.ID, Title: result.Title, Score: score, Signals: append([]string{}, result.Signals...)})
		report.CoverageWarnings = append(report.CoverageWarnings, result.Warnings...)
	}
	primary := results[0]
	report.SessionID = primary.ID
	report.Title = primary.Title
	capsule, err := ReadCapsule(search.DBPath, primary.ID)
	if err != nil {
		report.StoppedAt = primary.Snippet
		report.CoverageWarnings = append(report.CoverageWarnings, "The strongest match has no readable continuity capsule: "+short(err.Error(), 180))
	} else {
		report.StoppedAt = capsule.StoppedAt
		report.Completed = append(report.Completed, capsule.Completed...)
		report.Remaining = append(report.Remaining, capsule.Remaining...)
		report.Blockers = append(report.Blockers, capsule.Blockers...)
		report.NextAction = capsule.NextAction
		for _, evidence := range capsule.Evidence {
			report.Evidence = append(report.Evidence, RecallEvidence{
				Citation: SearchCitation{
					SessionID:   primary.ID,
					LineNo:      evidence.LineNo,
					Source:      primary.Citation.Source,
					UpdatedAt:   primary.Citation.UpdatedAt,
					RolloutPath: primary.Citation.RolloutPath,
				},
				Role:    evidence.Role,
				Kind:    evidence.Kind,
				Summary: evidence.Summary,
			})
		}
		if capsule.Coverage.Warning != "" {
			report.CoverageWarnings = append(report.CoverageWarnings, capsule.Coverage.Warning)
		}
	}
	report.CoverageWarnings = uniqueStrings(report.CoverageWarnings, 0)
	report.Confidence = recallConfidence(primary, report)
	return report, nil
}

func recallConfidence(primary SearchResult, report RecallReport) RecallConfidence {
	score := 0.35
	reasons := []string{"A matching indexed session was found."}
	lexicalMatch := containsString(primary.Signals, "lexical")
	semanticMatch := containsString(primary.Signals, "semantic") && primary.SemanticScore >= minimumRecallSemanticSimilarity
	if lexicalMatch && semanticMatch {
		score += 0.25
		reasons = append(reasons, "Lexical and semantic retrieval independently matched the same session.")
	} else if lexicalMatch || semanticMatch {
		score += 0.1
		reasons = append(reasons, "One retrieval method matched the session.")
	}
	if len(report.Evidence) >= 2 {
		score += 0.15
		reasons = append(reasons, "The continuity capsule includes both request and outcome evidence.")
	} else if len(report.Evidence) == 1 {
		score += 0.08
		reasons = append(reasons, "The continuity capsule includes one direct evidence reference.")
	}
	if primary.Coverage.Mode == "full" {
		score += 0.1
		reasons = append(reasons, "The primary transcript has full indexed message coverage.")
	} else if primary.Coverage.Mode == "sampled" {
		score -= 0.08
		reasons = append(reasons, "The primary transcript is sampled because it is very large.")
	}
	if len(report.CoverageWarnings) > 0 {
		score -= 0.05
	}
	score = math.Min(0.95, math.Max(0.05, score))
	level := "low"
	if score >= 0.75 {
		level = "high"
	} else if score >= 0.5 {
		level = "medium"
	}
	return RecallConfidence{Level: level, Score: score, Reasons: reasons}
}

func credibleRecallResults(results []SearchResult) []SearchResult {
	credible := make([]SearchResult, 0, len(results))
	for _, result := range results {
		if containsString(result.Signals, "lexical") || (containsString(result.Signals, "semantic") && result.SemanticScore >= minimumRecallSemanticSimilarity) {
			credible = append(credible, result)
		}
	}
	return credible
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func FormatRecallCitation(citation SearchCitation) string {
	if citation.SessionID == "" {
		return ""
	}
	line := ""
	if citation.LineNo > 0 {
		line = fmt.Sprintf(":%d", citation.LineNo)
	}
	return fmt.Sprintf("%s:%s%s", first(citation.Source, "session"), citation.SessionID, line)
}
