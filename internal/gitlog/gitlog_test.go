package gitlog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadHistory(t *testing.T) {
	repo := initTempRepo(t)

	commits, err := ReadHistory(repo)
	if err != nil {
		t.Fatalf("ReadHistory failed: %v", err)
	}

	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}

	if commits[0].SHA == "" || commits[0].Subject == "" {
		t.Fatalf("expected parsed commit metadata, got %#v", commits[0])
	}

	if len(commits[0].ChangedFiles) == 0 {
		t.Fatalf("expected changed files in most recent commit")
	}
}

func TestReadHistoryDoesNotTreatMultilineBodyAsChangedFiles(t *testing.T) {
	repo := initTempRepo(t)

	writeFile(t, filepath.Join(repo, "feature.txt"), "feature\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit",
		"-m", "feat: add feature",
		"-m", "This explains why the feature exists.",
		"-m", "It is not a path and must not become a changed file.",
	)

	commits, err := ReadHistory(repo)
	if err != nil {
		t.Fatalf("ReadHistory failed: %v", err)
	}
	if len(commits) == 0 {
		t.Fatal("expected commits")
	}
	latest := commits[0]
	if latest.Subject != "feat: add feature" {
		t.Fatalf("unexpected latest subject: %q", latest.Subject)
	}
	if !strings.Contains(latest.Body, "must not become a changed file") {
		t.Fatalf("expected multiline body to be preserved, got %q", latest.Body)
	}
	if len(latest.ChangedFiles) != 1 || latest.ChangedFiles[0] != "feature.txt" {
		t.Fatalf("commit body leaked into changed files: %#v", latest.ChangedFiles)
	}
}

func initTempRepo(t *testing.T) string {
	t.Helper()

	repo := t.TempDir()
	run(t, repo, "git", "init", "-b", "main")
	run(t, repo, "git", "config", "user.name", "Test User")
	run(t, repo, "git", "config", "user.email", "test@example.com")

	writeFile(t, filepath.Join(repo, "README.md"), "# test\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "docs: add readme")

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "feat: add main")

	return repo
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(output))
	}
}
