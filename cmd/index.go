package cmd

import (
	"fmt"
	"io"

	"github.com/tszaks/pallium/internal/output"
)

func runIndex(out io.Writer, args []string, jsonOutput bool) error {
	repoPath := optionalRepoArg(args, 0)
	indexer, err := openIndexedStore(repoPath)
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	result, err := indexer.Run()
	if err != nil {
		return err
	}

	return output.Write(out, result, jsonOutput, func() string {
		return fmt.Sprintf("Indexed %d commits, %d files, and %d co-change edges in %s\nContent: %d symbols and %d imports across %d source files (%d reparsed, %d unchanged)",
			result.CommitCount, result.FileCount, result.CochangeEdgeCount, result.RepoRoot,
			result.Content.Symbols, result.Content.Imports, result.Content.Files,
			result.Content.Reparsed, result.Content.Unchanged)
	})
}
