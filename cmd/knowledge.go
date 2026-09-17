package cmd

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/knowledge"
	"github.com/tszaks/pallium/internal/output"
)

func runKnowledge(out io.Writer, args []string, jsonOutput bool) error {
	action := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		action = args[0]
		args = args[1:]
	}

	switch action {
	case "map":
		return runKnowledgeMap(out, args, jsonOutput)
	case "build":
		return runKnowledgeBuild(out, args, jsonOutput)
	case "list", "status":
		return runKnowledgeList(out, args, jsonOutput)
	case "get":
		return runKnowledgeGet(out, args, jsonOutput)
	case "search":
		return runKnowledgeSearch(out, args, jsonOutput)
	default:
		return fmt.Errorf("unknown knowledge action: %s (want map, build, list, get, or search)", action)
	}
}

func runKnowledgeMap(out io.Writer, args []string, jsonOutput bool) error {
	indexer, err := openIndexedStore(optionalRepoArg(args, 0))
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	repo, err := indexer.Store.Repo()
	if err != nil {
		return err
	}
	modules, err := knowledge.Modules(indexer.Store, repo.ID, knowledge.ModuleOptions{})
	if err != nil {
		return err
	}

	return output.Write(out, modules, jsonOutput, func() string {
		var builder strings.Builder
		fmt.Fprintf(&builder, "%d module(s)\n", len(modules))
		for _, module := range modules {
			fmt.Fprintf(&builder, "  %-28s %4d files  %4d symbols", module.Slug, len(module.Files), module.SymbolCount)
			if len(module.DependsOn) > 0 {
				fmt.Fprintf(&builder, "  -> %s", strings.Join(module.DependsOn, ", "))
			}
			builder.WriteString("\n")
		}
		return builder.String()
	})
}

func runKnowledgeBuild(out io.Writer, args []string, jsonOutput bool) error {
	opts := knowledge.BuildOptions{Materialize: true}
	useModel := true
	positional := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--force":
			opts.Force = true
		case "--no-model", "--structural":
			useModel = false
		case "--no-materialize":
			opts.Materialize = false
		case "--concurrency":
			if index+1 >= len(args) {
				return fmt.Errorf("--concurrency needs a number")
			}
			index++
			value, convErr := strconv.Atoi(args[index])
			if convErr != nil || value < 1 {
				return fmt.Errorf("--concurrency needs a positive number, got %q", args[index])
			}
			opts.Concurrency = value
		case "--only":
			if index+1 >= len(args) {
				return fmt.Errorf("--only needs a module slug")
			}
			index++
			opts.Only = append(opts.Only, strings.Split(args[index], ",")...)
		default:
			positional = append(positional, args[index])
		}
	}

	indexer, err := openIndexedStore(optionalRepoArg(positional, 0))
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	repo, err := indexer.Store.Repo()
	if err != nil {
		return err
	}
	if !indexer.Store.HasCodeIndex(repo.ID) {
		return fmt.Errorf("no content index for this repo: run `pallium index` first")
	}

	if useModel {
		opts.Synth = knowledge.ProviderSynthesizer{RepoRoot: indexer.Store.RepoRoot}
	}

	report, err := knowledge.Build(indexer.Store, repo.ID, indexer.Store.RepoRoot, opts)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		return renderKnowledgeBuild(report)
	})
}

func renderKnowledgeBuild(report knowledge.BuildReport) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Built %d doc(s) across %d module(s) using %s.\n", report.Written, report.Modules, report.Generator)
	fmt.Fprintf(&builder, "%d verified, %d unverified, %d unchanged, %d claim(s) dropped.\n",
		report.Verified, report.Unverified, report.Unchanged, report.Dropped)
	if report.Materialized != "" {
		fmt.Fprintf(&builder, "Markdown written to %s\n", report.Materialized)
	}
	for _, doc := range report.Docs {
		marker := " "
		if !doc.Verified {
			marker = "!"
		}
		fmt.Fprintf(&builder, "%s %-28s %-9s %s", marker, doc.Slug, doc.Kind, doc.Status)
		if doc.Dropped > 0 {
			fmt.Fprintf(&builder, "  (%d dropped)", doc.Dropped)
		}
		builder.WriteString("\n")
	}
	for _, failure := range report.Failures {
		fmt.Fprintf(&builder, "! %s: %s\n", failure.Slug, failure.Reason)
	}
	return builder.String()
}

func runKnowledgeList(out io.Writer, args []string, jsonOutput bool) error {
	indexer, err := openIndexedStore(optionalRepoArg(args, 0))
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	repo, err := indexer.Store.Repo()
	if err != nil {
		return err
	}
	docs, err := indexer.Store.KnowledgeDocs(repo.ID)
	if err != nil {
		return err
	}

	return output.Write(out, docs, jsonOutput, func() string {
		if len(docs) == 0 {
			return "No knowledge docs yet. Run `pallium knowledge build`.\n"
		}
		var builder strings.Builder
		fmt.Fprintf(&builder, "%d doc(s)\n", len(docs))
		for _, doc := range docs {
			marker := " "
			if !doc.Verified {
				marker = "!"
			}
			fmt.Fprintf(&builder, "%s %-28s %-9s %s\n", marker, doc.Slug, doc.Kind, truncateLine(doc.Summary, 70))
		}
		return builder.String()
	})
}

func runKnowledgeGet(out io.Writer, args []string, jsonOutput bool) error {
	slug, err := requireArg(args, "slug")
	if err != nil {
		return err
	}
	indexer, err := openIndexedStore(optionalRepoArg(args, 1))
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	repo, err := indexer.Store.Repo()
	if err != nil {
		return err
	}
	doc, found, err := indexer.Store.KnowledgeDoc(repo.ID, slug)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no knowledge doc named %q", slug)
	}

	return output.Write(out, doc, jsonOutput, func() string {
		return renderKnowledgeDoc(doc)
	})
}

func renderKnowledgeDoc(doc db.KnowledgeDoc) string {
	var builder strings.Builder
	builder.WriteString(doc.Body)
	if !doc.Verified {
		builder.WriteString("\nNOT FULLY VERIFIED — some generated claims did not resolve against the index.\n")
	}
	if len(doc.DroppedClaims) > 0 {
		builder.WriteString("\nDropped claims:\n")
		for _, claim := range doc.DroppedClaims {
			fmt.Fprintf(&builder, "  - %s\n", claim)
		}
	}
	return builder.String()
}

func runKnowledgeSearch(out io.Writer, args []string, jsonOutput bool) error {
	query, err := requireArg(args, "query")
	if err != nil {
		return err
	}
	indexer, err := openIndexedStore(optionalRepoArg(args, 1))
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	repo, err := indexer.Store.Repo()
	if err != nil {
		return err
	}
	docs, err := indexer.Store.SearchKnowledge(repo.ID, query, 10)
	if err != nil {
		return err
	}

	return output.Write(out, docs, jsonOutput, func() string {
		var builder strings.Builder
		fmt.Fprintf(&builder, "%d match(es) for %q\n", len(docs), query)
		for _, doc := range docs {
			fmt.Fprintf(&builder, "  %-28s %s\n", doc.Slug, truncateLine(doc.Summary, 80))
		}
		if len(docs) == 0 {
			builder.WriteString("Nothing matched. Run `pallium knowledge build` if the base is empty.\n")
		}
		return builder.String()
	})
}
