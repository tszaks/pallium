package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runPlan(out io.Writer, args []string, jsonOutput bool) error {
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

	report, err := analysis.Plan(indexer.Store, target)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			fmt.Sprintf("Path: %s", report.Path),
			fmt.Sprintf("Goal: %s", report.Goal),
			fmt.Sprintf("Confidence: %s (%d)", report.Confidence.Level, report.Confidence.Score),
		}
		if freshnessLines := renderFreshness(report.Freshness); len(freshnessLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, freshnessLines...)
		}
		if evidenceLines := renderEvidence(report.Evidence); len(evidenceLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, evidenceLines...)
		}
		lines = append(lines, "", "Files to inspect:")
		for _, file := range report.FilesToInspect {
			lines = append(lines, "- "+file)
		}
		lines = append(lines, "", "Suggested steps:")
		for _, step := range report.Steps {
			lines = append(lines, "- "+step)
		}
		if len(report.TestsToRun) > 0 {
			lines = append(lines, "", "Tests to run:")
			for _, test := range report.TestsToRun {
				lines = append(lines, "- "+test)
			}
		}
		if len(report.TestCommands) > 0 {
			lines = append(lines, "", "Test commands:")
			for _, command := range report.TestCommands {
				lines = append(lines, "- "+command)
			}
		}
		if verificationLines := renderVerificationPlan(report.Verification); len(verificationLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, verificationLines...)
		}
		if actionLines := renderActionGuidance(report.ActionGuidance); len(actionLines) > 0 {
			lines = append(lines, "", "Agent guidance:")
			lines = append(lines, actionLines...)
		}
		return strings.Join(lines, "\n")
	})
}
