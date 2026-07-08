package index

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tszaks/pallium/internal/db"
)

func TestIndexerRun(t *testing.T) {
	repo := gitlogTestRepo(t)
	store, err := OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	indexer := New(store)
	result, err := indexer.Run()
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if result.CommitCount == 0 || result.FileCount == 0 {
		t.Fatalf("expected indexed data, got %+v", result)
	}
}

func TestIndexerRunPreservesActiveTask(t *testing.T) {
	repo := gitlogTestRepo(t)
	store, err := OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	indexer := New(store)
	if _, err := indexer.Run(); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	task := db.ActiveTask{
		Goal:       "Fix the indexer",
		ScopePaths: []string{"internal/index"},
		StartedAt:  time.Date(2026, 3, 13, 12, 30, 0, 0, time.UTC),
	}
	if err := store.SaveActiveTask(task); err != nil {
		t.Fatalf("SaveActiveTask failed: %v", err)
	}

	if _, err := indexer.Run(); err != nil {
		t.Fatalf("reindex failed: %v", err)
	}

	saved, err := store.ActiveTask()
	if err != nil {
		t.Fatalf("expected active task to survive reindex, got %v", err)
	}
	if saved.Goal != task.Goal {
		t.Fatalf("expected goal %q after reindex, got %q", task.Goal, saved.Goal)
	}
}

func TestIndexerRunFailureDoesNotUpdateLastIndexedCommit(t *testing.T) {
	repo := gitlogTestRepo(t)
	store, err := OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	indexer := New(store)
	if _, err := indexer.Run(); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	before, err := store.Repo()
	if err != nil {
		t.Fatalf("Repo failed: %v", err)
	}
	var commitsBefore int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, before.ID).Scan(&commitsBefore); err != nil {
		t.Fatalf("count commits: %v", err)
	}

	writeFile(t, filepath.Join(repo, "extra.go"), "package main\n\nfunc extra() {}\n")
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "feat: add extra")
	newHead := gitHeadSHA(t, repo)
	if newHead == before.LastIndexedCommit {
		t.Fatalf("expected HEAD to move after new commit")
	}

	if _, err := store.DB().Exec(`CREATE TRIGGER fail_edges BEFORE INSERT ON cochange_edges BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	if _, err := indexer.Run(); err == nil {
		t.Fatalf("expected reindex to fail with injected failure")
	}

	after, err := store.Repo()
	if err != nil {
		t.Fatalf("Repo after failed reindex: %v", err)
	}
	if after.LastIndexedCommit != before.LastIndexedCommit {
		t.Fatalf("expected last_indexed_commit %q after failed reindex, got %q", before.LastIndexedCommit, after.LastIndexedCommit)
	}
	var commitsAfter int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, before.ID).Scan(&commitsAfter); err != nil {
		t.Fatalf("count commits after failed reindex: %v", err)
	}
	if commitsAfter != commitsBefore {
		t.Fatalf("expected %d commits after rollback, got %d", commitsBefore, commitsAfter)
	}

	if _, err := store.DB().Exec(`DROP TRIGGER fail_edges`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if _, err := indexer.Run(); err != nil {
		t.Fatalf("reindex after removing failure: %v", err)
	}
	recovered, err := store.Repo()
	if err != nil {
		t.Fatalf("Repo after successful reindex: %v", err)
	}
	if recovered.LastIndexedCommit != newHead {
		t.Fatalf("expected last_indexed_commit %q after successful reindex, got %q", newHead, recovered.LastIndexedCommit)
	}
}

func TestIndexerRunKeepsCochangeEdgesWithColonPathsDistinct(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-b", "main")
	run(t, repo, "git", "config", "user.name", "Test User")
	run(t, repo, "git", "config", "user.email", "test@example.com")

	// ("a::b", "c") and ("a", "b::c") collide when edge keys are joined with "::".
	for _, name := range []string{"a::b", "c", "a", "b::c"} {
		writeFile(t, filepath.Join(repo, name), "content\n")
	}
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "-m", "feat: add colon files")

	store, err := OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	result, err := New(store).Run()
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// 4 files changed together produce 4*3 = 12 distinct directed edges.
	if result.CochangeEdgeCount != 12 {
		t.Fatalf("expected 12 cochange edges, got %d", result.CochangeEdgeCount)
	}
	var stored int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM cochange_edges`).Scan(&stored); err != nil {
		t.Fatalf("count cochange edges: %v", err)
	}
	if stored != 12 {
		t.Fatalf("expected 12 stored cochange edges, got %d", stored)
	}
}

func gitlogTestRepo(t *testing.T) string {
	t.Helper()
	return gitlogTestRepoHelper(t)
}

func gitHeadSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v", err)
	}
	return strings.TrimSpace(string(output))
}
