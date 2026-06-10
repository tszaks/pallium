package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runExplain(out io.Writer, args []string, jsonOutput bool) error {
	target, err := requireArg(args, "path")
	if err != nil {
		return err
	}
	repoPath := optionalRepoArg(args, 1)
	indexer, err := openIndexedStore(repoPath)
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	report, err := analysis.Explain(indexer.Store, target)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			fmt.Sprintf("Path: %s", report.Path),
			fmt.Sprintf("Risk: %s (%d)", report.Risk.Level, report.Risk.Score),
			fmt.Sprintf("Confidence: %s (%d)", report.Confidence.Level, report.Confidence.Score),
			fmt.Sprintf("Summary: %s", report.Summary),
		}
		if freshnessLines := renderFreshness(report.Freshness); len(freshnessLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, freshnessLines...)
		}
		if evidenceLines := renderEvidence(report.Evidence); len(evidenceLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, evidenceLines...)
		}
		lines = append(lines, "", "Before you edit:")
		for _, item := range report.EditChecklist {
			lines = append(lines, "- "+item)
		}
		if len(report.RecentCommits) > 0 {
			lines = append(lines, "", "Recent commits:")
			for _, commit := range report.RecentCommits {
				lines = append(lines, fmt.Sprintf("- %s %s", commit.SHA[:8], commit.Subject))
			}
		} else {
			lines = append(lines, "", "Recent commits:", "- No indexed commits for this file yet.")
		}
		lines = append(lines, "", "Why it matters:")
		for _, reason := range report.Risk.Reasons {
			lines = append(lines, "- "+reason)
		}
		if len(report.SuggestedTests) > 0 {
			lines = append(lines, "", "Suggested tests:")
			for _, test := range report.SuggestedTests {
				lines = append(lines, "- "+test)
			}
		}
		if len(report.TestCommands) > 0 {
			lines = append(lines, "", "Suggested test commands:")
			for _, command := range report.TestCommands {
				lines = append(lines, "- "+command)
			}
		}
		if verificationLines := renderVerificationPlan(report.Verification); len(verificationLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, verificationLines...)
		}
		if len(report.BlastRadius) > 0 {
			lines = append(lines, "", "Blast radius:")
			for _, path := range report.BlastRadius {
				lines = append(lines, "- "+path)
			}
		}
		if len(report.StructuralLinks) > 0 {
			lines = append(lines, "", "Structural links:")
			for _, link := range report.StructuralLinks {
				lines = append(lines, fmt.Sprintf("- %s (%s)", link.Path, link.Kind))
			}
		}
		if actionLines := renderActionGuidance(report.ActionGuidance); len(actionLines) > 0 {
			lines = append(lines, "", "Agent guidance:")
			lines = append(lines, actionLines...)
		}
		lines = append(lines, "", renderNeighbors(report.Neighbors))
		if len(report.Decisions) > 0 {
			lines = append(lines, "", "Decision notes:")
			for _, decision := range report.Decisions {
				lines = append(lines, fmt.Sprintf("- %s", decision.Title))
			}
		}
		return strings.Join(lines, "\n")
	})
}
