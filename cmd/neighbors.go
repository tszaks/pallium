package cmd

import (
	"io"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runNeighbors(out io.Writer, args []string, jsonOutput bool) error {
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

	neighbors, err := analysis.Neighbors(indexer.Store, target, 10)
	if err != nil {
		return err
	}

	return output.Write(out, neighbors, jsonOutput, func() string {
		return renderNeighbors(neighbors)
	})
}
