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
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			_, err := fmt.Fprintln(out, "pallium knowledge <map|build|audit|list|status|get|search|context|serve|maintain|decisions> [arguments] [repo]\nReads include freshness and evidence. search/context return bounded v2 JSON; --full returns legacy full documents. get <slug> --section <heading> reads a section.")
			return err
		}
	}
	action := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		action = args[0]
		args = args[1:]
	}

	switch action {
	case "decisions":
		return runKnowledgeDecisions(out, args, jsonOutput)
	case "serve":
		return runKnowledgeServe(out, args)
	case "maintain":
		return runKnowledgeMaintain(out, args, jsonOutput)
	case "map":
		return runKnowledgeMap(out, args, jsonOutput)
	case "build":
		return runKnowledgeBuild(out, args, jsonOutput)
	case "audit":
		return runKnowledgeAudit(out, args, jsonOutput)
	case "status":
		return runKnowledgeStatus(out, args, jsonOutput)
	case "list":
		return runKnowledgeList(out, args, jsonOutput)
	case "get":
		return runKnowledgeGet(out, args, jsonOutput)
	case "context":
		return runKnowledgeContext(out, args, jsonOutput)
	case "search":
		return runKnowledgeSearch(out, args, jsonOutput)
	default:
		return fmt.Errorf("unknown knowledge action: %s (want map, build, audit, list, get, or search)", action)
	}
}

func runKnowledgeMap(out io.Writer, args []string, jsonOutput bool) error {
	args, full, _, err := knowledgeReadArgs(args)
	if err != nil {
		return err
	}
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

	var value any = map[string]any{"version": knowledge.ContractVersion, "modules": knowledge.CompactMap(modules)}
	if full {
		value = modules
	}
	return output.Write(out, value, jsonOutput, func() string {
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
	opts, useModel, positional, err := parseKnowledgeBuildArgs(args)
	if err != nil {
		return err
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

	if useModel {
		audit, err := knowledge.Audit(indexer.Store, repo.ID, indexer.Store.RepoRoot, knowledge.AuditOptions{Synth: opts.Synth, Only: opts.Only, Concurrency: opts.Concurrency, Materialize: opts.Materialize})
		if err != nil {
			return err
		}
		report.Audit = &audit
	}
	return output.Write(out, report, jsonOutput, func() string {
		return renderKnowledgeBuild(report)
	})
}

func parseKnowledgeBuildArgs(args []string) (knowledge.BuildOptions, bool, []string, error) {
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
		case "--allow-stale":
			opts.AllowStale = true
		case "--concurrency":
			if index+1 >= len(args) {
				return knowledge.BuildOptions{}, false, nil, fmt.Errorf("--concurrency needs a number")
			}
			index++
			value, convErr := strconv.Atoi(args[index])
			if convErr != nil || value < 1 {
				return knowledge.BuildOptions{}, false, nil, fmt.Errorf("--concurrency needs a positive number, got %q", args[index])
			}
			opts.Concurrency = value
		case "--only":
			if index+1 >= len(args) {
				return knowledge.BuildOptions{}, false, nil, fmt.Errorf("--only needs a module slug")
			}
			index++
			opts.Only = append(opts.Only, strings.Split(args[index], ",")...)
		default:
			positional = append(positional, args[index])
		}
	}
	return opts, useModel, positional, nil
}

func renderKnowledgeBuild(report knowledge.BuildReport) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Built %d doc(s) across %d module(s) using %s.\n", report.Written, report.Modules, report.Generator)
	fmt.Fprintf(&builder, "%d citation-resolved, %d with unresolved claims, %d unchanged, %d claim(s) dropped.\n",
		report.Verified, report.Unverified, report.Unchanged, report.Dropped)
	if report.NeedsReindex > 0 {
		fmt.Fprintf(&builder, "%d module(s) changed since indexing and were SKIPPED: a doc built from stale line numbers describes code that moved. Run `pallium index`, or pass --allow-stale.\n", report.NeedsReindex)
	}
	if report.Audit != nil {
		fmt.Fprintf(&builder, "Audit: %d supported, %d unclear, %d removed; %d failures.\n", report.Audit.Supported, report.Audit.Unclear, report.Audit.Removed, len(report.Audit.Failures))
	}
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

	if err := knowledge.Assess(indexer.Store, docs); err != nil {
		return err
	}
	summaries := []knowledge.Hit{}
	for _, d := range docs {
		summaries = append(summaries, knowledge.Hit{Slug: d.Slug, Title: d.Title, Kind: d.Kind, Snippet: truncateLine(d.Summary, 240), Freshness: d.Freshness, State: d.Evidence.State, SourceCommit: d.SourceCommit})
	}
	return output.Write(out, map[string]any{"version": knowledge.ContractVersion, "documents": summaries}, jsonOutput, func() string {
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
	args, full, section, err := knowledgeReadArgs(args)
	if err != nil {
		return err
	}
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

	assessed := []db.KnowledgeDoc{doc}
	if err := knowledge.Assess(indexer.Store, assessed); err != nil {
		return err
	}
	doc = assessed[0]
	if section != "" {
		doc.Body = knowledge.Section(doc.Body, section)
		if doc.Body == "" {
			return fmt.Errorf("section %q not found", section)
		}
	}
	if !full {
		doc.Claims = ""
	}
	return output.Write(out, doc, jsonOutput, func() string {
		return renderKnowledgeDoc(doc)
	})
}

func renderKnowledgeDoc(doc db.KnowledgeDoc) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Freshness: %s · Evidence: %s\n\n", doc.Freshness, doc.Evidence.State)
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
	args, full, _, err := knowledgeReadArgs(args)
	if err != nil {
		return err
	}
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
	if full {
		docs, err := indexer.Store.SearchKnowledge(repo.ID, query, 10)
		if err != nil {
			return err
		}
		if err := knowledge.Assess(indexer.Store, docs); err != nil {
			return err
		}
		return output.Write(out, docs, jsonOutput, func() string {
			var b strings.Builder
			for _, d := range docs {
				b.WriteString(renderKnowledgeDoc(d))
			}
			return b.String()
		})
	}
	result, err := knowledge.Search(indexer.Store, repo.ID, query, 5, 8192)
	if err != nil {
		return err
	}
	return output.Write(out, result, jsonOutput, func() string {
		var b strings.Builder
		for _, hit := range result.Results {
			fmt.Fprintf(&b, "%s [%s; %s] %s\n", hit.Slug, hit.Freshness, hit.State, hit.Snippet)
		}
		if len(result.Results) == 0 {
			b.WriteString("No matching knowledge. Index and build this workspace first.\n")
		}
		return b.String()
	})
}

// runKnowledgeAudit re-checks stored claims against the source. Build proves a
// citation resolves; this records a model assessment of bounded source evidence.
func runKnowledgeAudit(out io.Writer, args []string, jsonOutput bool) error {
	opts := knowledge.AuditOptions{Materialize: true}
	positional := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		switch args[index] {
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
	opts.Synth = knowledge.ProviderSynthesizer{RepoRoot: indexer.Store.RepoRoot}

	report, err := knowledge.Audit(indexer.Store, repo.ID, indexer.Store.RepoRoot, opts)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		var builder strings.Builder
		fmt.Fprintf(&builder, "Audited %d of %d module doc(s), %d skipped as structural-only.\n", report.Audited, report.Docs, report.Skipped)
		if report.NeedsReindex > 0 {
			fmt.Fprintf(&builder, "%d module(s) changed since indexing and were NOT audited: their stored line numbers point at the wrong code. Run `pallium index` first.\n", report.NeedsReindex)
		}
		if report.NeedsRebuild > 0 {
			fmt.Fprintf(&builder, "%d doc(s) were written by a model before claims were stored and cannot be audited: run `pallium knowledge build --force` first.\n", report.NeedsRebuild)
		}
		fmt.Fprintf(&builder, "%d claim(s) checked: %d supported, %d unclear, %d REMOVED.\n", report.Claims, report.Supported, report.Unclear, report.Removed)
		for _, finding := range report.Findings {
			fmt.Fprintf(&builder, "- %s: %s\n    %s\n", finding.Slug, finding.Claim, finding.Reason)
		}
		for _, failure := range report.Failures {
			fmt.Fprintf(&builder, "! %s: %s\n", failure.Slug, failure.Reason)
		}
		return builder.String()
	})
}
