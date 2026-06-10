package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runRisk(out io.Writer, args []string, jsonOutput bool) error {
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

	report, err := analysis.Risk(indexer.Store, target)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			report.Path,
			fmt.Sprintf("Risk: %s (%d)", report.Level, report.Score),
			fmt.Sprintf("Churn: %d", report.ChurnScore),
			fmt.Sprintf("Recent touches: %d", report.RecentTouchCount),
			fmt.Sprintf("Authors: %d", report.AuthorCount),
			fmt.Sprintf("Neighbors: %d", report.NeighborCount),
		}
		if report.LastTouchedAt != "" {
			lines = append(lines, fmt.Sprintf("Last touched: %s", report.LastTouchedAt))
		}
		if len(report.Reasons) > 0 {
			lines = append(lines, "", "Why it matters:")
			for _, reason := range report.Reasons {
				lines = append(lines, "- "+reason)
			}
		}
		return strings.Join(lines, "\n")
	})
}
