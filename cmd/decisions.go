package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runDecisions(out io.Writer, args []string, jsonOutput bool) error {
	query, err := requireArg(args, "query")
	if err != nil {
		return err
	}
	repoPath := optionalRepoArg(args, 1)
	indexer, err := openIndexedStore(repoPath)
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	decisions, err := analysis.Decisions(indexer.Store, query, 10)
	if err != nil {
		return err
	}

	return output.Write(out, decisions, jsonOutput, func() string {
		if len(decisions) == 0 {
			return "No matching decision notes found."
		}

		lines := []string{"Matching decisions:"}
		for _, decision := range decisions {
			lines = append(lines, fmt.Sprintf("- %s (%s)", decision.Title, decision.SourceRef[:8]))
		}
		return strings.Join(lines, "\n")
	})
}
