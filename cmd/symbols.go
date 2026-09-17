package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
)

func runSymbols(out io.Writer, args []string, jsonOutput bool) error {
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

	report, err := analysis.Symbols(indexer.Store, target)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		return renderSymbols(report)
	})
}

func renderSymbols(report analysis.SymbolsReport) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s (%s, %d symbols)\n", report.Path, report.Lang, len(report.Symbols))
	if len(report.Symbols) == 0 {
		builder.WriteString("No symbols indexed for this file.\n")
		if report.Note != "" {
			fmt.Fprintf(&builder, "%s\n", report.Note)
		}
		return builder.String()
	}
	for _, symbol := range report.Symbols {
		marker := " "
		if symbol.Exported {
			marker = "*"
		}
		fmt.Fprintf(&builder, "%s %5d  %-9s %s\n", marker, symbol.StartLine, symbol.Kind, symbolLabel(symbol.Name, symbol.Receiver))
		if symbol.Doc != "" {
			fmt.Fprintf(&builder, "            %s\n", truncateLine(symbol.Doc, 96))
		}
	}
	if len(report.Imports) > 0 {
		builder.WriteString("\nImports in this repo:\n")
		for _, imp := range report.Imports {
			fmt.Fprintf(&builder, "  %s\n", imp)
		}
	}
	if len(report.Dependents) > 0 {
		builder.WriteString("\nImported by:\n")
		for _, path := range report.Dependents {
			fmt.Fprintf(&builder, "  %s\n", path)
		}
	}
	return builder.String()
}

func runCallers(out io.Writer, args []string, jsonOutput bool) error {
	name, err := requireArg(args, "symbol")
	if err != nil {
		return err
	}
	repoPath := optionalRepoArg(args, 1)
	indexer, err := openIndexedStore(repoPath)
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	report, err := analysis.Callers(indexer.Store, name)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		return renderCallers(report)
	})
}

func renderCallers(report analysis.CallersReport) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s\n", report.Name)
	if len(report.DeclaredIn) == 0 {
		builder.WriteString("Not declared anywhere in the index.\n")
	} else {
		builder.WriteString("Declared in:\n")
		for _, symbol := range report.DeclaredIn {
			fmt.Fprintf(&builder, "  %s:%d  %s %s\n", symbol.Path, symbol.StartLine, symbol.Kind, symbolLabel(symbol.Name, symbol.Receiver))
		}
	}
	if len(report.ReferencedBy) == 0 {
		builder.WriteString("No other indexed file references it.\n")
		return builder.String()
	}
	fmt.Fprintf(&builder, "Referenced by %d file(s):\n", len(report.ReferencedBy))
	for _, path := range report.ReferencedBy {
		fmt.Fprintf(&builder, "  %s\n", path)
	}
	return builder.String()
}

func runSearch(out io.Writer, args []string, jsonOutput bool) error {
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

	report, err := analysis.SearchCode(indexer.Store, query, 25)
	if err != nil {
		return err
	}

	return output.Write(out, report, jsonOutput, func() string {
		return renderCodeSearch(report)
	})
}

func renderCodeSearch(report analysis.CodeSearchReport) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d match(es) for %q\n", len(report.Matches), report.Query)
	for _, match := range report.Matches {
		fmt.Fprintf(&builder, "  %s:%d  %-9s %s\n", match.Path, match.StartLine, match.Kind, symbolLabel(match.Name, match.Receiver))
		if match.Doc != "" {
			fmt.Fprintf(&builder, "      %s\n", truncateLine(match.Doc, 96))
		}
	}
	if len(report.Matches) == 0 {
		builder.WriteString("Nothing indexed matches. Run `pallium index` if the repo changed.\n")
	}
	return builder.String()
}

func symbolLabel(name, receiver string) string {
	if receiver == "" {
		return name
	}
	return receiver + "." + name
}

func truncateLine(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if len(value) <= limit {
		return value
	}
	return value[:limit-1] + "…"
}
