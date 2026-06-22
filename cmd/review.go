package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runReview(out io.Writer, args []string, jsonOutput bool) error {
	baseRef, repoPath := parseReviewArgs(args)
	indexer, err := openIndexedStore(repoPath)
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	report, err := analysis.Review(indexer.Store, baseRef)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			fmt.Sprintf("Review base: %s", report.BaseRef),
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
		if len(report.RequiredTests) > 0 {
			lines = append(lines, "", "Required tests:")
			for _, test := range report.RequiredTests {
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
		if len(report.ChangedFiles) > 0 {
			lines = append(lines, "", "Changed files:")
			for _, file := range report.ChangedFiles {
				lines = append(lines, fmt.Sprintf("- %s (%s, %s)", file.Path, file.RiskLevel, file.ChangeSource))
				if len(file.BoundaryLabels) > 0 {
					lines = append(lines, "  boundaries: "+strings.Join(file.BoundaryLabels, ", "))
				}
				lines = append(lines, fmt.Sprintf("  needs review: %t", file.NeedsReview))
				lines = append(lines, fmt.Sprintf("  needs tests: %t", file.NeedsTests))
				for _, reason := range file.TopReasons {
					lines = append(lines, "  reason: "+reason)
				}
				for _, test := range file.SuggestedTests {
					lines = append(lines, "  test: "+test)
				}
				for _, path := range file.BlastRadius {
					lines = append(lines, "  blast: "+path)
				}
			}
		}
		if actionLines := renderActionGuidance(report.ActionGuidance); len(actionLines) > 0 {
			lines = append(lines, "", "Agent guidance:")
			lines = append(lines, actionLines...)
		}
		if taskLines := renderTaskScope(report.Task); len(taskLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, taskLines...)
		}
		if len(report.BoundaryWarnings) > 0 {
			lines = append(lines, "", "Boundary warnings:")
			for _, item := range report.BoundaryWarnings {
				lines = append(lines, fmt.Sprintf("- %s: %s", item.Label, item.Reason))
			}
		}
		if sessionLines := renderRelatedSessions(report.RelatedSessions); len(sessionLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, sessionLines...)
		}
		if len(report.Notes) > 0 {
			lines = append(lines, "", "Notes:")
			for _, note := range report.Notes {
				lines = append(lines, "- "+note)
			}
		}
		return strings.Join(lines, "\n")
	})
}

func parseReviewArgs(args []string) (string, string) {
	baseRef := "HEAD~1"
	repoPath := "."
	if len(args) == 0 {
		return baseRef, repoPath
	}

	first := strings.TrimSpace(args[0])
	if first == "" {
		return baseRef, repoPath
	}

	if looksLikePath(first) {
		repoPath = first
		if len(args) > 1 && strings.TrimSpace(args[1]) != "" {
			baseRef = strings.TrimSpace(args[1])
		}
		return baseRef, repoPath
	}

	baseRef = first
	if len(args) > 1 && strings.TrimSpace(args[1]) != "" {
		repoPath = strings.TrimSpace(args[1])
	}
	return baseRef, repoPath
}

func looksLikePath(value string) bool {
	if value == "." || value == ".." || strings.HasPrefix(value, "/") {
		return true
	}
	if info, err := os.Stat(value); err == nil && info.IsDir() {
		return true
	}
	return false
}
