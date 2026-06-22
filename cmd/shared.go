package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/index"
	"github.com/tszaks/pallium/internal/sessionmemory"
)

func openIndexedStore(path string) (*index.Indexer, error) {
	store, err := index.OpenStore(path)
	if err != nil {
		return nil, err
	}
	return index.New(store), nil
}

func renderNeighbors(neighbors []analysis.Neighbor) string {
	if len(neighbors) == 0 {
		return "No related files found."
	}
	lines := make([]string, 0, len(neighbors)+1)
	lines = append(lines, "Related files:")
	for _, neighbor := range neighbors {
		lines = append(lines, fmt.Sprintf("- %s (%d co-changes)", neighbor.Path, neighbor.CochangeCount))
	}
	return strings.Join(lines, "\n")
}

func renderActionGuidance(guidance analysis.ActionGuidance) []string {
	lines := make([]string, 0)
	if len(guidance.InspectFirst) > 0 {
		lines = append(lines, "Inspect first:")
		for _, item := range guidance.InspectFirst {
			lines = append(lines, "- "+item)
		}
	}
	if len(guidance.RunNext) > 0 {
		lines = append(lines, "Run next:")
		for _, item := range guidance.RunNext {
			lines = append(lines, "- "+item)
		}
	}
	if guidance.RecommendedNextCommand != "" {
		lines = append(lines, "Recommended next command: "+guidance.RecommendedNextCommand)
	}
	lines = append(lines, fmt.Sprintf("Safe to edit alone: %t", guidance.SafeToEditAlone))
	lines = append(lines, fmt.Sprintf("Must review: %t", guidance.MustReview))
	lines = append(lines, fmt.Sprintf("Must verify: %t", guidance.MustVerify))
	if len(guidance.AskForReviewIf) > 0 {
		lines = append(lines, "Ask for review if:")
		for _, item := range guidance.AskForReviewIf {
			lines = append(lines, "- "+item)
		}
	}
	if len(guidance.StopSignals) > 0 {
		lines = append(lines, "Stop signals:")
		for _, item := range guidance.StopSignals {
			lines = append(lines, "- "+item)
		}
	}
	if len(guidance.ConfidenceGaps) > 0 {
		lines = append(lines, "Confidence gaps:")
		for _, item := range guidance.ConfidenceGaps {
			lines = append(lines, "- "+item)
		}
	}
	if len(guidance.BoundaryWarnings) > 0 {
		lines = append(lines, "Boundary warnings:")
		for _, item := range guidance.BoundaryWarnings {
			lines = append(lines, fmt.Sprintf("- %s: %s", item.Label, item.Reason))
		}
	}
	return lines
}

func renderFreshness(freshness analysis.Freshness) []string {
	if freshness.IndexedAt == "" && freshness.CurrentCommit == "" {
		return nil
	}
	lines := []string{fmt.Sprintf("Freshness: %s", freshnessSummary(freshness))}
	if len(freshness.Reasons) > 0 {
		lines = append(lines, "Freshness details:")
		for _, reason := range freshness.Reasons {
			lines = append(lines, "- "+reason)
		}
	}
	return lines
}

func renderEvidence(evidence analysis.Evidence) []string {
	lines := make([]string, 0)
	if len(evidence.Sources) > 0 {
		lines = append(lines, "Evidence sources:")
		for _, source := range evidence.Sources {
			lines = append(lines, "- "+source)
		}
	}
	if len(evidence.Notes) > 0 {
		lines = append(lines, "Evidence notes:")
		for _, note := range evidence.Notes {
			lines = append(lines, "- "+note)
		}
	}
	return lines
}

func freshnessSummary(freshness analysis.Freshness) string {
	state := "fresh"
	if freshness.IsStale {
		state = "stale"
	}
	parts := []string{state}
	if freshness.WorkingTreeDirty {
		parts = append(parts, "working tree dirty")
	}
	if freshness.IndexedAt != "" {
		parts = append(parts, "indexed at "+freshness.IndexedAt)
	}
	return strings.Join(parts, " | ")
}

func renderVerificationPlan(plan analysis.VerificationPlan) []string {
	lines := make([]string, 0)
	if len(plan.Fast) > 0 {
		lines = append(lines, "Fast verification:")
		for _, item := range plan.Fast {
			lines = append(lines, "- "+item)
		}
	}
	if len(plan.Safe) > 0 {
		lines = append(lines, "Safe verification:")
		for _, item := range plan.Safe {
			lines = append(lines, "- "+item)
		}
	}
	if len(plan.Full) > 0 {
		lines = append(lines, "Full verification:")
		for _, item := range plan.Full {
			lines = append(lines, "- "+item)
		}
	}
	return lines
}

func renderTaskScope(task analysis.TaskScopeReport) []string {
	if task.Goal == "" {
		return nil
	}
	lines := []string{fmt.Sprintf("Active task: %s", task.Goal)}
	if len(task.ScopePaths) > 0 {
		lines = append(lines, "Planned scope:")
		for _, scope := range task.ScopePaths {
			lines = append(lines, "- "+scope)
		}
	}
	if task.HasScopeDrift {
		lines = append(lines, "Scope drift:")
		for _, path := range task.OutOfScopeChanged {
			lines = append(lines, "- "+path)
		}
	}
	return lines
}

func renderRelatedSessions(results []sessionmemory.SearchResult) []string {
	if len(results) == 0 {
		return nil
	}
	lines := []string{"Related sessions:"}
	for _, result := range results {
		title := strings.Join(strings.Fields(result.Title), " ")
		if title == "" {
			title = result.ID
		}
		lines = append(lines, fmt.Sprintf("- score=%d %s %s", result.Score, shortID(result.ID), title))
		if len(result.Signals) > 0 {
			lines = append(lines, "  signals: "+strings.Join(result.Signals, ", "))
		}
	}
	return lines
}

func writeError(out io.Writer, err error) error {
	_, writeErr := fmt.Fprintln(out, err)
	if writeErr != nil {
		return writeErr
	}
	return err
}
