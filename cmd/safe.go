package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runSafe(out io.Writer, args []string, jsonOutput bool) error {
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

	report, err := analysis.Safe(indexer.Store, target)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			fmt.Sprintf("Path: %s", report.Path),
			fmt.Sprintf("Verdict: %s", report.Verdict),
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
		lines = append(lines, "", "Required checks:")
		for _, item := range report.RequiredChecks {
			lines = append(lines, "- "+item)
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
		if actionLines := renderActionGuidance(report.ActionGuidance); len(actionLines) > 0 {
			lines = append(lines, "", "Agent guidance:")
			lines = append(lines, actionLines...)
		}
		return strings.Join(lines, "\n")
	})
}
