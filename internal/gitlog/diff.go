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
	cmd := exec.Command("git", "-C", repoRoot, "diff", "--name-only", "-z", baseRef, headRef)
	output, err := cmd.Output()
	if err != nil {
		return nil, wrapGitError("failed to diff changed files", err)
	}

	entries := strings.Split(string(output), "\x00")
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		out = append(out, filepath.ToSlash(entry))
	}
	return out, nil
}

func WorkingTreeChanges(repoRoot string) ([]WorkingTreeFile, error) {
	cmd := exec.Command("git", "-C", repoRoot, "status", "--porcelain", "-z")
	output, err := cmd.Output()
	if err != nil {
		return nil, wrapGitError("failed to read working tree changes", err)
	}

	entries := strings.Split(string(output), "\x00")
	out := make([]WorkingTreeFile, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		status := strings.TrimSpace(entry[:2])
		path := filepath.ToSlash(entry[3:])
		if strings.ContainsAny(status, "RC") {
			// Rename/copy entries emit the new path first, then the
			// original path as a separate NUL-terminated entry. Keep the
			// new path and skip the original.
			i++
		}
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
