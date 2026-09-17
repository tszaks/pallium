package knowledge

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/db"
)

// modulePrompt is the whole contract with the model. It hands over the facts
// the index already proved and asks for the one thing the index cannot know:
// why this module exists. The citation allowlist is stated twice, because the
// verification pass will silently delete anything outside it and a model that
// knows the rule produces more usable output than one that gets filtered.
func modulePrompt(module Module) string {
	var builder strings.Builder

	fmt.Fprintf(&builder, "You are documenting one module of a codebase for other engineers and coding agents.\n\n")
	fmt.Fprintf(&builder, "Module directory: %s\n", module.Dir)
	fmt.Fprintf(&builder, "Languages: %s\n", strings.Join(module.Languages, ", "))
	fmt.Fprintf(&builder, "Files (%d):\n", len(module.Files))
	for _, path := range capStrings(module.Files, 60) {
		fmt.Fprintf(&builder, "  %s\n", path)
	}
	if len(module.Files) > 60 {
		fmt.Fprintf(&builder, "  ...and %d more\n", len(module.Files)-60)
	}

	if len(module.EntryCandidates) > 0 {
		fmt.Fprintf(&builder, "\nLikely entry points: %s\n", strings.Join(module.EntryCandidates, ", "))
	}

	if len(module.KeySymbols) > 0 {
		builder.WriteString("\nMost-referenced symbols:\n")
		for _, symbol := range module.KeySymbols {
			fmt.Fprintf(&builder, "  %s (%s) in %s:%d", symbol.Name, symbol.Kind, symbol.Path, symbol.StartLine)
			if symbol.Signature != "" {
				fmt.Fprintf(&builder, " — %s", truncate(symbol.Signature, 120))
			}
			builder.WriteString("\n")
			if symbol.Doc != "" {
				fmt.Fprintf(&builder, "      doc: %s\n", truncate(symbol.Doc, 160))
			}
		}
	}

	if len(module.DependsOn) > 0 {
		fmt.Fprintf(&builder, "\nDepends on modules: %s\n", strings.Join(module.DependsOn, ", "))
	}
	if len(module.DependedOnBy) > 0 {
		fmt.Fprintf(&builder, "Depended on by modules: %s\n", strings.Join(module.DependedOnBy, ", "))
	}
	if len(module.ExternalDeps) > 0 {
		fmt.Fprintf(&builder, "External packages used: %s\n", strings.Join(module.ExternalDeps, ", "))
	}

	if len(module.RecentCommits) > 0 {
		builder.WriteString("\nRecent commits touching it:\n")
		for _, commit := range module.RecentCommits {
			fmt.Fprintf(&builder, "  %s %s %s\n", commit.Date, shortSHA(commit.SHA), commit.Subject)
		}
	}

	builder.WriteString(`
Return ONLY a JSON object with this shape and no prose around it:

{
  "summary": "one sentence: what this module is for",
  "purpose": "two to four sentences: what problem it solves and how it fits the rest of the repo",
  "entry_points": [{"claim": "why a reader should start here", "cited_paths": ["path/from/the/list/above"], "cited_symbols": []}],
  "key_symbols": [{"claim": "what this symbol does and why it matters", "cited_paths": [], "cited_symbols": ["SymbolName"]}],
  "invariants": [{"claim": "a rule this module relies on that is not obvious from a signature", "cited_paths": [], "cited_symbols": ["SymbolName"]}],
  "risks": [{"claim": "what breaks if someone changes this carelessly", "cited_paths": [], "cited_symbols": []}]
}

Rules, enforced automatically after you answer:
- Every claim must cite at least one path or symbol, and every citation must
  appear verbatim in the lists above. Uncited or unresolvable claims are
  deleted before storage and recorded as dropped, so a guess costs you the
  whole claim.
- Do not restate the file list, the dependency list, or the commit list. Those
  are already stored and rendered; your job is only what they do not say.
- Prefer four precise claims to twelve vague ones. Say nothing rather than
  something you cannot cite.
`)

	return builder.String()
}

func renderModuleDoc(module Module, result synthesis) string {
	var builder strings.Builder

	fmt.Fprintf(&builder, "# %s\n\n", module.Title)
	if summary := strings.TrimSpace(result.Summary); summary != "" {
		fmt.Fprintf(&builder, "%s\n\n", summary)
	} else {
		fmt.Fprintf(&builder, "%s\n\n", structuralSummary(module))
	}
	if purpose := strings.TrimSpace(result.Purpose); purpose != "" {
		fmt.Fprintf(&builder, "## Purpose\n\n%s\n\n", purpose)
	}

	writeClaims(&builder, "Where to start", result.EntryPoints)
	writeClaims(&builder, "Key symbols", result.KeySymbols)
	writeClaims(&builder, "Invariants", result.Invariants)
	writeClaims(&builder, "Risks", result.Risks)

	builder.WriteString("## Surface\n\n")
	if len(module.KeySymbols) == 0 {
		builder.WriteString("No symbols indexed.\n\n")
	} else {
		for _, symbol := range module.KeySymbols {
			fmt.Fprintf(&builder, "- `%s` (%s) — %s:%d\n", symbolName(symbol), symbol.Kind, symbol.Path, symbol.StartLine)
		}
		builder.WriteString("\n")
	}

	fmt.Fprintf(&builder, "## Files (%d)\n\n", len(module.Files))
	for _, path := range capStrings(module.Files, 50) {
		fmt.Fprintf(&builder, "- `%s`\n", path)
	}
	if len(module.Files) > 50 {
		fmt.Fprintf(&builder, "- ...and %d more\n", len(module.Files)-50)
	}
	builder.WriteString("\n")

	if len(module.DependsOn) > 0 || len(module.DependedOnBy) > 0 || len(module.ExternalDeps) > 0 {
		builder.WriteString("## Wiring\n\n")
		if len(module.DependsOn) > 0 {
			fmt.Fprintf(&builder, "- Depends on: %s\n", strings.Join(module.DependsOn, ", "))
		}
		if len(module.DependedOnBy) > 0 {
			fmt.Fprintf(&builder, "- Depended on by: %s\n", strings.Join(module.DependedOnBy, ", "))
		}
		if len(module.ExternalDeps) > 0 {
			fmt.Fprintf(&builder, "- External packages: %s\n", strings.Join(module.ExternalDeps, ", "))
		}
		builder.WriteString("\n")
	}

	if len(module.RecentCommits) > 0 {
		builder.WriteString("## Recent history\n\n")
		for _, commit := range module.RecentCommits {
			fmt.Fprintf(&builder, "- %s `%s` %s\n", commit.Date, shortSHA(commit.SHA), commit.Subject)
		}
		builder.WriteString("\n")
	}

	return builder.String()
}

func writeClaims(builder *strings.Builder, heading string, claims []citedClaim) {
	if len(claims) == 0 {
		return
	}
	fmt.Fprintf(builder, "## %s\n\n", heading)
	for _, claim := range claims {
		citations := append([]string{}, claim.CitedSymbols...)
		citations = append(citations, claim.CitedPaths...)
		if len(citations) == 0 {
			fmt.Fprintf(builder, "- %s\n", strings.TrimSpace(claim.Claim))
			continue
		}
		fmt.Fprintf(builder, "- %s (`%s`)\n", strings.TrimSpace(claim.Claim), strings.Join(citations, "`, `"))
	}
	builder.WriteString("\n")
}

func structuralSummary(module Module) string {
	languages := "mixed sources"
	if len(module.Languages) > 0 {
		languages = module.Languages[0]
	}
	return fmt.Sprintf("%d %s files declaring %d symbols.", len(module.Files), languages, module.SymbolCount)
}

func buildOverviewDoc(modules []Module, sourceCommit string) db.KnowledgeDoc {
	var builder strings.Builder
	builder.WriteString("# Repository map\n\n")
	fmt.Fprintf(&builder, "%d modules, clustered from the content index by directory.\n\n", len(modules))

	sorted := make([]Module, len(modules))
	copy(sorted, modules)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i].Files) > len(sorted[j].Files) })

	builder.WriteString("| Module | Files | Symbols | Depends on |\n")
	builder.WriteString("| --- | ---: | ---: | --- |\n")
	for _, module := range sorted {
		depends := strings.Join(capStrings(module.DependsOn, 4), ", ")
		if depends == "" {
			depends = "—"
		}
		fmt.Fprintf(&builder, "| `%s` | %d | %d | %s |\n", module.Slug, len(module.Files), module.SymbolCount, depends)
	}
	builder.WriteString("\n")

	paths := make([]string, 0, len(modules))
	for _, module := range modules {
		paths = append(paths, module.Dir)
	}

	return db.KnowledgeDoc{
		Slug:         "overview",
		Kind:         "overview",
		Title:        "Repository map",
		Summary:      fmt.Sprintf("%d modules derived from the content index.", len(modules)),
		Body:         builder.String(),
		CitedPaths:   sortedUnique(paths),
		CitedSymbols: []string{},
		SourceCommit: sourceCommit,
		Generator:    "structural",
		Verified:     true,
		GeneratedAt:  time.Now().UTC(),
	}
}

func symbolName(symbol db.CodeSymbol) string {
	if symbol.Receiver == "" {
		return symbol.Name
	}
	return symbol.Receiver + "." + symbol.Name
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func capStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

// Materialize writes the stored docs out as markdown. The database is the
// source of truth; these files exist so a person can read the knowledge base
// in an editor, and so a team that wants it can commit it and watch it change
// in review.
func Materialize(store *db.Store, repoID int64, dir string) error {
	docs, err := store.KnowledgeDocs(repoID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create knowledge directory: %w", err)
	}

	// Clear stale pages: a module that merged away should not leave its old
	// description sitting on disk looking current.
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return fmt.Errorf("clear stale knowledge page: %w", err)
			}
		}
	}

	for _, doc := range docs {
		var builder strings.Builder
		builder.WriteString("<!-- Generated by `pallium knowledge build`. Edits are overwritten.\n")
		fmt.Fprintf(&builder, "     slug: %s  kind: %s  verified: %t  generator: %s\n", doc.Slug, doc.Kind, doc.Verified, doc.Generator)
		fmt.Fprintf(&builder, "     generated: %s  source commit: %s -->\n\n", doc.GeneratedAt.Format(time.RFC3339), shortSHA(doc.SourceCommit))
		builder.WriteString(doc.Body)
		if len(doc.DroppedClaims) > 0 {
			builder.WriteString("\n## Dropped claims\n\n")
			builder.WriteString("These were generated but did not survive verification against the index.\n\n")
			for _, claim := range doc.DroppedClaims {
				fmt.Fprintf(&builder, "- %s\n", claim)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, doc.Slug+".md"), []byte(builder.String()), 0o644); err != nil {
			return fmt.Errorf("write knowledge page %s: %w", doc.Slug, err)
		}
	}
	return nil
}
