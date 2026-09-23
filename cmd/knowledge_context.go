package cmd

import (
	"fmt"
	"github.com/tszaks/pallium/internal/knowledge"
	"github.com/tszaks/pallium/internal/output"
	"io"
	"strings"
)

func runKnowledgeContext(out io.Writer, args []string, jsonOutput bool) error {
	query, err := requireArg(args, "task")
	if err != nil {
		return err
	}
	idx, err := openIndexedStore(optionalRepoArg(args, 1))
	if err != nil {
		return err
	}
	defer idx.Store.Close()
	repo, err := idx.Store.Repo()
	if err != nil {
		return err
	}
	pack, err := knowledge.Context(idx.Store, repo.ID, query)
	if err != nil {
		return err
	}
	if m, err := knowledge.OpenMaintenance(); err == nil {
		var slugs []string
		for _, h := range pack.Results {
			slugs = append(slugs, h.Slug)
		}
		_ = m.Prioritize(idx.Store.RepoRoot, slugs)
		m.Close()
	}
	return output.Write(out, pack, jsonOutput, func() string {
		return fmt.Sprintf("%s\nRead next: %s\nRelated tests: %s\n", pack.Guidance, strings.Join(pack.ReadNext, ", "), strings.Join(pack.Tests, ", "))
	})
}
func knowledgeReadArgs(args []string) ([]string, bool, string, error) {
	var positional []string
	full := false
	section := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--full":
			full = true
		case "--section":
			i++
			if i >= len(args) {
				return nil, false, "", fmt.Errorf("--section needs a heading")
			}
			section = args[i]
		default:
			if strings.HasPrefix(args[i], "--") {
				return nil, false, "", fmt.Errorf("unknown flag %s", args[i])
			}
			positional = append(positional, args[i])
		}
	}
	return positional, full, section, nil
}

func runKnowledgeStatus(out io.Writer, args []string, jsonOutput bool) error {
	idx, err := openIndexedStore(optionalRepoArg(args, 0))
	if err != nil {
		return err
	}
	defer idx.Store.Close()
	repo, err := idx.Store.Repo()
	if err != nil {
		return err
	}
	h, err := knowledge.HealthReport(idx.Store, repo.ID)
	if err != nil {
		return err
	}
	return output.Write(out, h, jsonOutput, func() string {
		return fmt.Sprintf("%d knowledge documents: %d current, %d stale, %d need rebuilding; %d audited. %d indexed files (%d heuristic, %d partial, %d outdated parsers).\n", h.Documents, h.Current, h.Stale, h.NeedsRebuild, h.Audited, h.IndexedFiles, h.HeuristicFiles, h.PartialFiles, h.OutdatedParsers)
	})
}
