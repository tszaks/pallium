package gitlog

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

type WorkingTreeFile struct {
	Path   string
	Status string
}

func ChangedFilesBetween(repoRoot, baseRef, headRef string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "diff", "--name-only", baseRef, headRef)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to diff changed files: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, filepath.ToSlash(line))
	}
	return out, nil
}

func WorkingTreeChanges(repoRoot string) ([]WorkingTreeFile, error) {
	cmd := exec.Command("git", "-C", repoRoot, "status", "--short")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to read working tree changes: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	out := make([]WorkingTreeFile, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" || len(line) < 4 {
			continue
		}
		status := strings.TrimSpace(line[:2])
		path := strings.TrimSpace(line[3:])
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		path = filepath.ToSlash(path)
		if path == ".pallium.db" || path == ".pallium" || strings.HasPrefix(path, ".pallium/") ||
			path == ".codex-memory.db" || path == ".codex-memory" || strings.HasPrefix(path, ".codex-memory/") {
			continue
		}
		out = append(out, WorkingTreeFile{
			Path:   path,
			Status: status,
		})
	}
	return out, nil
}
