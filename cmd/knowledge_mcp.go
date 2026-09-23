package cmd

import (
	"bytes"
	"fmt"
	"strings"
)

// Knowledge and code-intelligence tools for the MCP server.
//
// Until now the MCP surface was workflow-only: an agent connected to Pallium
// could start a run and check on it, but could not ask a single question about
// the repo without shelling out to the CLI and parsing text. These tools close
// that, and they are the reason the index exists — a coding agent that can ask
// "what is this module for" and "who calls this" does not have to read the
// repo into its context window to find out.
func knowledgeMCPTools() []mcpTool {
	return []mcpTool{
		{Name: "pallium_knowledge_context", Description: "Get a bounded v2 task context pack with freshness, evidence and next source paths. Start here for a task; stale results are explicitly historical.", InputSchema: objectSchema(map[string]any{"query": stringSchema(), "cwd": stringSchema()})},
		{
			Name:        "pallium_knowledge_search",
			Description: "Search project knowledge. Version 2 returns at most five excerpts within 8 KB, with freshness and evidence states. Use full only when you need the legacy complete payload.",
			InputSchema: objectSchema(map[string]any{"query": stringSchema(), "cwd": stringSchema(), "full": boolSchema()}),
		},
		{
			Name:        "pallium_knowledge_get",
			Description: "Read a document or one section by slug. Freshness is separate from citation/audit evidence; an audit does not prove truth. The full option includes raw structured claims.",
			InputSchema: objectSchema(map[string]any{"slug": stringSchema(), "cwd": stringSchema(), "section": stringSchema(), "full": boolSchema()}),
		},
		{
			Name:        "pallium_knowledge_map",
			Description: "List the repo's modules with their sizes and dependency edges. Use to orient in an unfamiliar codebase before deciding what to read.",
			InputSchema: objectSchema(map[string]any{"cwd": stringSchema()}),
		},
		{
			Name:        "pallium_explain",
			Description: "Explain one file before editing it: what it declares, how risky it is, what usually changes with it, its tests, and the verification to run. Use before any non-trivial edit.",
			InputSchema: objectSchema(map[string]any{"path": stringSchema(), "cwd": stringSchema()}),
		},
		{
			Name:        "pallium_symbols",
			Description: "List what a file declares, with kinds, line numbers, signatures and doc comments, plus the files it imports and the files that import it. Cheaper and more precise than reading the file.",
			InputSchema: objectSchema(map[string]any{"path": stringSchema(), "cwd": stringSchema()}),
		},
		{
			Name:        "pallium_callers",
			Description: "Find where a symbol is declared and which other files reference it. Use to size the blast radius of a rename or signature change.",
			InputSchema: objectSchema(map[string]any{"symbol": stringSchema(), "cwd": stringSchema()}),
		},
		{
			Name:        "pallium_search_code",
			Description: "Search indexed symbol names, signatures and doc comments. Use to locate the right declaration by concept when you do not know its exact name.",
			InputSchema: objectSchema(map[string]any{"query": stringSchema(), "cwd": stringSchema()}),
		},
	}
}

// callKnowledgeTool handles the knowledge tools and reports whether it
// recognized the name, so the workflow dispatcher can fall through cleanly.
func callKnowledgeTool(name string, args map[string]any) (string, bool, error) {
	cwd := stringArg(args, "cwd")

	switch name {
	case "pallium_knowledge_context":
		query := stringArg(args, "query")
		if query == "" {
			return "", true, fmt.Errorf("query is required")
		}
		text, err := captureCommand(func(out *bytes.Buffer) error {
			return runKnowledgeContext(out, withOptionalRepo([]string{query}, cwd), true)
		})
		return text, true, err
	case "pallium_knowledge_search":
		query := stringArg(args, "query")
		if query == "" {
			return "", true, fmt.Errorf("query is required")
		}
		text, err := captureCommand(func(out *bytes.Buffer) error {
			argv := []string{"search", query}
			if boolArg(args, "full") {
				argv = append(argv, "--full")
			}
			return runKnowledge(out, withOptionalRepo(argv, cwd), true)
		})
		return text, true, err
	case "pallium_knowledge_get":
		slug := stringArg(args, "slug")
		if slug == "" {
			return "", true, fmt.Errorf("slug is required")
		}
		text, err := captureCommand(func(out *bytes.Buffer) error {
			argv := []string{"get", slug}
			if section := stringArg(args, "section"); section != "" {
				argv = append(argv, "--section", section)
			}
			if boolArg(args, "full") {
				argv = append(argv, "--full")
			}
			return runKnowledge(out, withOptionalRepo(argv, cwd), true)
		})
		return text, true, err
	case "pallium_knowledge_map":
		text, err := captureCommand(func(out *bytes.Buffer) error {
			return runKnowledge(out, withOptionalRepo([]string{"map"}, cwd), true)
		})
		return text, true, err
	case "pallium_explain":
		path := stringArg(args, "path")
		if path == "" {
			return "", true, fmt.Errorf("path is required")
		}
		text, err := captureCommand(func(out *bytes.Buffer) error {
			return runExplain(out, withOptionalRepo([]string{path}, cwd), true)
		})
		return text, true, err
	case "pallium_symbols":
		path := stringArg(args, "path")
		if path == "" {
			return "", true, fmt.Errorf("path is required")
		}
		text, err := captureCommand(func(out *bytes.Buffer) error {
			return runSymbols(out, withOptionalRepo([]string{path}, cwd), true)
		})
		return text, true, err
	case "pallium_callers":
		symbol := stringArg(args, "symbol")
		if symbol == "" {
			return "", true, fmt.Errorf("symbol is required")
		}
		text, err := captureCommand(func(out *bytes.Buffer) error {
			return runCallers(out, withOptionalRepo([]string{symbol}, cwd), true)
		})
		return text, true, err
	case "pallium_search_code":
		query := stringArg(args, "query")
		if query == "" {
			return "", true, fmt.Errorf("query is required")
		}
		text, err := captureCommand(func(out *bytes.Buffer) error {
			return runSearch(out, withOptionalRepo([]string{query}, cwd), true)
		})
		return text, true, err
	default:
		return "", false, nil
	}
}

// withOptionalRepo appends the repo path only when the caller supplied one,
// because these commands read the second positional as the repo and an empty
// string would resolve to the process working directory by a different route.
func withOptionalRepo(args []string, cwd string) []string {
	if strings.TrimSpace(cwd) == "" {
		return args
	}
	return append(args, cwd)
}

func captureCommand(fn func(out *bytes.Buffer) error) (string, error) {
	var out bytes.Buffer
	if err := fn(&out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}
