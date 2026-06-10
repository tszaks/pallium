package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runChangedNow(out io.Writer, args []string, jsonOutput bool) error {
	repoPath := optionalRepoArg(args, 0)
	indexer, err := openIndexedStore(repoPath)
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	report, err := analysis.ChangedNow(indexer.Store)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{report.Summary}
		if freshnessLines := renderFreshness(report.Freshness); len(freshnessLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, freshnessLines...)
		}
		if evidenceLines := renderEvidence(report.Evidence); len(evidenceLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, evidenceLines...)
		}
		for _, file := range report.Files {
			lines = append(lines, fmt.Sprintf("- %s (%s, %s)", file.Path, file.RiskLevel, file.WorkingTreeStatus))
		}
		if taskLines := renderTaskScope(report.Task); len(taskLines) > 0 {
			lines = append(lines, "")
			lines = append(lines, taskLines...)
		}
		return strings.Join(lines, "\n")
	})
}
