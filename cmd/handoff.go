package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runHandoff(out io.Writer, args []string, jsonOutput bool) error {
	baseRef, repoPath := parseReviewArgs(args)
	indexer, err := openIndexedStore(repoPath)
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	report, err := analysis.Handoff(indexer.Store, baseRef)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			fmt.Sprintf("Summary: %s", report.Summary),
			fmt.Sprintf("Review confidence: %s (%d)", report.Review.Confidence.Level, report.Review.Confidence.Score),
			fmt.Sprintf("Review: %s", report.Review.Summary),
			fmt.Sprintf("Changed now: %s", report.ChangedNow.Summary),
		}
		if freshnessLines := renderFreshness(report.Freshness); len(freshnessLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, freshnessLines...)
		}
		if evidenceLines := renderEvidence(report.Evidence); len(evidenceLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, evidenceLines...)
		}
		if len(report.NextActions) > 0 {
			lines = append(lines, "", "Next actions:")
			for _, action := range report.NextActions {
				lines = append(lines, "- "+action)
			}
		}
		if sessionLines := renderRelatedSessions(report.RelatedSessions); len(sessionLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, sessionLines...)
		}
		if historyLines := renderVerificationHistory(report.VerificationHistory); len(historyLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, historyLines...)
		}
		if taskLines := renderTaskScope(report.Task); len(taskLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, taskLines...)
		}
		return strings.Join(lines, "\n")
	})
}
