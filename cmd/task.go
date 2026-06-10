package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/output"
)

type taskStatus struct {
	Goal       string   `json:"goal"`
	ScopePaths []string `json:"scope_paths"`
	StartedAt  string   `json:"started_at"`
}

func runTask(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return fmt.Errorf("missing task subcommand: use start, show, or clear")
	}

	indexer, err := openIndexedStore(".")
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	switch args[0] {
	case "start":
		goal, err := requireArg(args[1:], "goal")
		if err != nil {
			return err
		}
		scopeArgs := make([]string, 0)
		if len(args) > 2 {
			scopeArgs = args[2:]
		}
		if len(scopeArgs) == 0 {
			scopeArgs = []string{"."}
		}
		scopePaths := parseScopeArgs(scopeArgs)
		normalizedScope := make([]string, 0, len(scopePaths))
		for _, scope := range scopePaths {
			normalized, err := normalizeScopePath(indexer.Store.RepoRoot, scope)
			if err != nil {
				return err
			}
			normalizedScope = append(normalizedScope, normalized)
		}
		task := db.ActiveTask{
			Goal:       goal,
			ScopePaths: normalizedScope,
			StartedAt:  time.Now().UTC(),
		}
		if err := indexer.Store.SaveActiveTask(task); err != nil {
			return err
		}
		return output.Write(out, taskStatus{
			Goal:       task.Goal,
			ScopePaths: task.ScopePaths,
			StartedAt:  task.StartedAt.Format(time.RFC3339),
		}, jsonOutput, func() string {
			return fmt.Sprintf("Active task set: %s\nScope:\n- %s", task.Goal, strings.Join(task.ScopePaths, "\n- "))
		})
	case "show":
		task, err := indexer.Store.ActiveTask()
		if err != nil {
			if err == sql.ErrNoRows {
				return output.Write(out, taskStatus{}, jsonOutput, func() string {
					return "No active task."
				})
			}
			return err
		}
		return output.Write(out, taskStatus{
			Goal:       task.Goal,
			ScopePaths: task.ScopePaths,
			StartedAt:  task.StartedAt.Format(time.RFC3339),
		}, jsonOutput, func() string {
			lines := []string{fmt.Sprintf("Active task: %s", task.Goal)}
			if len(task.ScopePaths) > 0 {
				lines = append(lines, "Scope:")
				for _, scope := range task.ScopePaths {
					lines = append(lines, "- "+scope)
				}
			}
			return strings.Join(lines, "\n")
		})
	case "clear":
		if err := indexer.Store.ClearActiveTask(); err != nil {
			return err
		}
		return output.Write(out, map[string]string{"status": "cleared"}, jsonOutput, func() string {
			return "Active task cleared."
		})
	default:
		return fmt.Errorf("unknown task subcommand: %s", args[0])
	}
}

func normalizeScopePath(repoRoot, path string) (string, error) {
	if path == "." {
		return ".", nil
	}
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return "", err
		}
		path = rel
	}
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	path = strings.TrimPrefix(path, "./")
	if path == "" {
		path = "."
	}
	return path, nil
}

func parseScopeArgs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return uniqueArgStrings(out)
}

func uniqueArgStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
