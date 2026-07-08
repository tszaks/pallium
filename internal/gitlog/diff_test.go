package gitlog

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestChangedFilesBetween(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-b", "main")
	run(t, repo, "git", "config", "user.name", "Test User")
	run(t, repo, "git", "config", "user.email", "test@example.com")

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "feat: add main")

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(repo, "main_test.go"), "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {\n\tmain()\n}\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "test: add coverage")

	files, err := ChangedFilesBetween(repo, "HEAD~1", "HEAD")
	if err != nil {
		t.Fatalf("ChangedFilesBetween failed: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 changed files, got %#v", files)
	}
}

func TestWorkingTreeChanges(t *testing.T) {
	repo := initTempRepo(t)

	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(repo, "extra_test.go"), "package main\n\nimport \"testing\"\n\nfunc TestExtra(t *testing.T) {}\n")

	files, err := WorkingTreeChanges(repo)
	if err != nil {
		t.Fatalf("WorkingTreeChanges failed: %v", err)
	}

	if len(files) < 2 {
		t.Fatalf("expected working tree changes, got %#v", files)
	}
}

func TestChangedFilesBetweenUnescapesNonASCIIPaths(t *testing.T) {
	repo := initTempRepo(t)

	writeFile(t, filepath.Join(repo, "naïve.txt"), "naïve\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "feat: add naïve file")

	files, err := ChangedFilesBetween(repo, "HEAD~1", "HEAD")
	if err != nil {
		t.Fatalf("ChangedFilesBetween failed: %v", err)
	}

	if len(files) != 1 || files[0] != "naïve.txt" {
		t.Fatalf("expected exactly [naïve.txt], got %#v", files)
	}
}

func TestWorkingTreeChangesUnquotesPathsWithSpaces(t *testing.T) {
	repo := initTempRepo(t)

	writeFile(t, filepath.Join(repo, "my file.txt"), "hello\n")

	files, err := WorkingTreeChanges(repo)
	if err != nil {
		t.Fatalf("WorkingTreeChanges failed: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 working tree change, got %#v", files)
	}
	if files[0].Path != "my file.txt" {
		t.Fatalf("expected path %q, got %q", "my file.txt", files[0].Path)
	}
	if files[0].Status != "??" {
		t.Fatalf("expected status ??, got %q", files[0].Status)
	}
}

func TestWorkingTreeChangesHandlesRenames(t *testing.T) {
	repo := initTempRepo(t)

	writeFile(t, filepath.Join(repo, "old.txt"), "some stable content for rename detection\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "feat: add old file")
	run(t, repo, "git", "mv", "old.txt", "new file.txt")

	files, err := WorkingTreeChanges(repo)
	if err != nil {
		t.Fatalf("WorkingTreeChanges failed: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 working tree change, got %#v", files)
	}
	if files[0].Path != "new file.txt" {
		t.Fatalf("expected renamed path %q, got %q", "new file.txt", files[0].Path)
	}
	if files[0].Status != "R" {
		t.Fatalf("expected status R, got %q", files[0].Status)
	}
}

func TestWorkingTreeChangesExpandsUntrackedDirectories(t *testing.T) {
	repo := initTempRepo(t)

	if err := os.MkdirAll(filepath.Join(repo, "cmd"), 0o755); err != nil {
		t.Fatalf("mkdir cmd: %v", err)
	}
	writeFile(t, filepath.Join(repo, "cmd", "newtool.go"), "package cmd\n")
	writeFile(t, filepath.Join(repo, "cmd", "newtool_test.go"), "package cmd\n")

	files, err := WorkingTreeChanges(repo)
	if err != nil {
		t.Fatalf("WorkingTreeChanges failed: %v", err)
	}

	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	if !slices.Contains(paths, "cmd/newtool.go") || !slices.Contains(paths, "cmd/newtool_test.go") {
		t.Fatalf("expected untracked directory files, got %#v", files)
	}
}
