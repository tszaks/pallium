package gitlog

import (
	"fmt"
	"os"
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
		if ignoredWorkingTreePath(path) {
			continue
		}
		if status == "??" {
			expanded, err := expandUntrackedDirectory(repoRoot, path)
			if err != nil {
				return nil, err
			}
			if len(expanded) > 0 {
				out = append(out, expanded...)
				continue
			}
		}
		out = append(out, WorkingTreeFile{
			Path:   path,
			Status: status,
		})
	}
	return out, nil
}

func expandUntrackedDirectory(repoRoot, path string) ([]WorkingTreeFile, error) {
	info, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(path)))
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	files := make([]WorkingTreeFile, 0)
	err = filepath.WalkDir(filepath.Join(repoRoot, filepath.FromSlash(path)), func(absPath string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, absPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" || rel == "node_modules" || ignoredWorkingTreePath(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignoredWorkingTreePath(rel) {
			return nil
		}
		files = append(files, WorkingTreeFile{Path: rel, Status: "??"})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("expand untracked directory %s: %w", path, err)
	}
	return files, nil
}

func ignoredWorkingTreePath(path string) bool {
	return path == ".pallium.db" || path == ".pallium" || strings.HasPrefix(path, ".pallium/") ||
		path == ".codex-memory.db" || path == ".codex-memory" || strings.HasPrefix(path, ".codex-memory/")
}
