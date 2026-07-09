package gitlog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	commitDelimiter = "\x1e"
	fieldDelimiter  = "\x1f"
	filesDelimiter  = "\x1d"
)

type Commit struct {
	SHA          string
	AuthorName   string
	AuthorEmail  string
	CommittedAt  time.Time
	Subject      string
	Body         string
	ChangedFiles []string
}

// wrapGitError wraps a git command failure, including stderr when available
// so failures are diagnosable.
func wrapGitError(msg string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			return fmt.Errorf("%s: %w: %s", msg, err, stderr)
		}
	}
	return fmt.Errorf("%s: %w", msg, err)
}

func RepoRoot(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", wrapGitError("failed to resolve git repo root", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// CanonicalRepoRoot resolves the repo root shared by every worktree of the
// same repository. A linked worktree has its own toplevel (RepoRoot), but
// its git-common-dir always points back at the primary checkout's .git, so
// all worktrees of a repo resolve to the same canonical root here. For a
// normal checkout (or the primary worktree) this equals RepoRoot.
func CanonicalRepoRoot(path string) (string, error) {
	// Reuse RepoRoot's error handling so bare repos and non-git directories
	// fail the same way they do today: --git-common-dir alone would succeed
	// in a bare repo, where --show-toplevel does not.
	if _, err := RepoRoot(path); err != nil {
		return "", err
	}

	cmd := exec.Command("git", "-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return "", wrapGitError("failed to resolve canonical git repo root", err)
	}

	return filepath.Dir(strings.TrimSpace(string(output))), nil
}

func CurrentBranch(repoRoot string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "", wrapGitError("failed to read git branch", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func CurrentCommit(repoRoot string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "", wrapGitError("failed to read git head commit", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func OriginURL(repoRoot string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "config", "--get", "remote.origin.url")
	output, err := cmd.Output()
	if err != nil {
		return "", wrapGitError("failed to read git origin url", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func ReadHistory(repoRoot string) ([]Commit, error) {
	cmd := exec.Command(
		"git",
		"-C",
		repoRoot,
		"-c",
		"core.quotepath=false",
		"log",
		"--date=iso-strict",
		"--name-only",
		"--pretty=format:"+commitDelimiter+"%H"+fieldDelimiter+"%an"+fieldDelimiter+"%ae"+fieldDelimiter+"%ad"+fieldDelimiter+"%s"+fieldDelimiter+"%b"+filesDelimiter,
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, wrapGitError("failed to read git history", err)
	}

	commits := make([]Commit, 0)
	chunks := strings.Split(string(output), commitDelimiter)
	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}

		parts := strings.SplitN(chunk, filesDelimiter, 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("unexpected git history record: missing file delimiter")
		}

		fields := strings.SplitN(strings.TrimRight(parts[0], "\n"), fieldDelimiter, 6)
		if len(fields) < 6 {
			return nil, fmt.Errorf("unexpected git history header: %q", parts[0])
		}

		committedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[3]))
		if err != nil {
			return nil, fmt.Errorf("failed to parse commit timestamp %q: %w", fields[3], err)
		}

		files := make([]string, 0)
		reader := bufio.NewReader(bytes.NewBufferString(parts[1]))
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			path := strings.TrimSpace(scanner.Text())
			if path == "" {
				continue
			}
			// Even with core.quotepath=false, git C-quotes paths containing
			// double quotes, backslashes, or control characters.
			if len(path) >= 2 && strings.HasPrefix(path, `"`) && strings.HasSuffix(path, `"`) {
				if unquoted, err := strconv.Unquote(path); err == nil {
					path = unquoted
				}
			}
			files = append(files, filepath.ToSlash(path))
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("failed to parse commit file list: %w", err)
		}

		commits = append(commits, Commit{
			SHA:          strings.TrimSpace(fields[0]),
			AuthorName:   strings.TrimSpace(fields[1]),
			AuthorEmail:  strings.TrimSpace(fields[2]),
			CommittedAt:  committedAt,
			Subject:      strings.TrimSpace(fields[4]),
			Body:         strings.TrimSpace(fields[5]),
			ChangedFiles: files,
		})
	}

	return commits, nil
}
