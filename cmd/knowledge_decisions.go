package cmd

import (
	"fmt"
	"github.com/tszaks/pallium/internal/knowledge"
	"github.com/tszaks/pallium/internal/output"
	"io"
	"os"
)

func runKnowledgeDecisions(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return runKnowledgeList(out, nil, jsonOutput)
	}
	if args[0] == "list" {
		return runKnowledgeList(out, args[1:], jsonOutput)
	}
	if args[0] != "link" {
		return fmt.Errorf("use decisions link --title TITLE --file FILE --source REFERENCE [--supersedes SLUG] [repo]")
	}
	values := map[string]string{}
	var positional []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--title", "--file", "--source", "--supersedes":
			key := args[i]
			i++
			if i >= len(args) {
				return fmt.Errorf("%s needs a value", key)
			}
			values[key] = args[i]
		default:
			positional = append(positional, args[i])
		}
	}
	file, err := os.Open(values["--file"])
	if err != nil {
		return err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 64001))
	if err != nil {
		return err
	}
	idx, err := openIndexedStore(optionalRepoArg(positional, 0))
	if err != nil {
		return err
	}
	defer idx.Store.Close()
	repo, err := idx.Store.Repo()
	if err != nil {
		return err
	}
	doc, err := knowledge.LinkDecision(idx.Store, repo.ID, values["--title"], string(body), values["--source"], values["--supersedes"])
	if err != nil {
		return err
	}
	return output.Write(out, doc, jsonOutput, func() string { return "Linked authored decision: " + doc.Slug + "\n" })
}
