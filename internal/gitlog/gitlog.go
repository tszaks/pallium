package gitlog

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	commitDelimiter = "\x1e"
	fieldDelimiter  = "\x1f"
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

func RepoRoot(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to resolve git repo root: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func CurrentBranch(repoRoot string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to read git branch: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func CurrentCommit(repoRoot string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to read git head commit: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func OriginURL(repoRoot string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "config", "--get", "remote.origin.url")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to read git origin url: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func ReadHistory(repoRoot string) ([]Commit, error) {
	cmd := exec.Command(
		"git",
		"-C",
		repoRoot,
		"log",
		"--date=iso-strict",
		"--name-only",
		"--pretty=format:"+commitDelimiter+"%H"+fieldDelimiter+"%an"+fieldDelimiter+"%ae"+fieldDelimiter+"%ad"+fieldDelimiter+"%s"+fieldDelimiter+"%b",
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to read git history: %w", err)
	}

	commits := make([]Commit, 0)
	chunks := strings.Split(string(output), commitDelimiter)
	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}

		reader := bufio.NewReader(bytes.NewBufferString(chunk))
		header, err := reader.ReadString('\n')
		if err != nil && header == "" {
			return nil, fmt.Errorf("failed to parse git history header: %w", err)
		}

		header = strings.TrimRight(header, "\n")
		fields := strings.Split(header, fieldDelimiter)
		if len(fields) < 6 {
			return nil, fmt.Errorf("unexpected git history header: %q", header)
		}

		committedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[3]))
		if err != nil {
			return nil, fmt.Errorf("failed to parse commit timestamp %q: %w", fields[3], err)
		}

		files := make([]string, 0)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			path := strings.TrimSpace(scanner.Text())
			if path == "" {
				continue
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
