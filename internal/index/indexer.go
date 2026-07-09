package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
)

type Result struct {
	RepoRoot          string    `json:"repo_root"`
	Branch            string    `json:"branch"`
	CommitCount       int       `json:"commit_count"`
	FileCount         int       `json:"file_count"`
	CochangeEdgeCount int       `json:"cochange_edge_count"`
	IndexedAt         time.Time `json:"indexed_at"`
}

type Indexer struct {
	Store *db.Store
}

func New(store *db.Store) *Indexer {
	return &Indexer{Store: store}
}

func (i *Indexer) Run() (Result, error) {
	branch, err := gitlog.CurrentBranch(i.Store.RepoRoot)
	if err != nil {
		return Result{}, err
	}

	commits, err := gitlog.ReadHistory(i.Store.RepoRoot)
	if err != nil {
		return Result{}, err
	}

	lastIndexedCommit := ""
	if len(commits) > 0 {
		lastIndexedCommit = commits[0].SHA
	}

	indexedAt := time.Now().UTC()

	// A single transaction keeps reindexing atomic: last_indexed_commit is
	// only recorded when the repopulated data commits along with it.
	var result Result
	err = i.Store.WithTx(func(tx *db.Store) error {
		var txErr error
		result, txErr = repopulate(tx, branch, lastIndexedCommit, indexedAt, commits)
		return txErr
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

type edgeKey struct {
	source  string
	related string
}

func repopulate(store *db.Store, branch, lastIndexedCommit string, indexedAt time.Time, commits []gitlog.Commit) (Result, error) {
	repo, err := store.UpsertRepo(branch, lastIndexedCommit, indexedAt)
	if err != nil {
		return Result{}, err
	}

	if err := store.ResetRepoData(repo.ID); err != nil {
		return Result{}, err
	}

	churn := make(map[string]int)
	recent := make(map[string]int)
	authors := make(map[string]map[string]struct{})
	lastTouched := make(map[string]time.Time)
	threshold := indexedAt.AddDate(0, 0, -30)
	edges := make(map[edgeKey]db.CochangeEdge)

	for _, commit := range commits {
		if err := store.InsertCommit(repo.ID, db.CommitRecord{
			SHA:         commit.SHA,
			AuthorName:  commit.AuthorName,
			AuthorEmail: commit.AuthorEmail,
			CommittedAt: commit.CommittedAt,
			Subject:     commit.Subject,
			Body:        commit.Body,
		}); err != nil {
			return Result{}, err
		}

		if err := store.UpsertDecisionNote(repo.ID, db.DecisionNote{
			SourceType:  "git",
			SourceRef:   commit.SHA,
			Title:       commit.Subject,
			Body:        strings.TrimSpace(strings.Join([]string{commit.Subject, commit.Body}, "\n\n")),
			CommittedAt: commit.CommittedAt,
		}); err != nil {
			return Result{}, err
		}

		uniqueFiles := dedupe(commit.ChangedFiles)
		for _, filePath := range uniqueFiles {
			churn[filePath]++
			if commit.CommittedAt.After(threshold) {
				recent[filePath]++
			}
			if authors[filePath] == nil {
				authors[filePath] = make(map[string]struct{})
			}
			authors[filePath][commit.AuthorEmail] = struct{}{}
			if commit.CommittedAt.After(lastTouched[filePath]) {
				lastTouched[filePath] = commit.CommittedAt
			}

			if err := store.InsertFileCommit(repo.ID, filePath, commit.SHA, commit.CommittedAt); err != nil {
				return Result{}, err
			}
		}

		for _, source := range uniqueFiles {
			for _, related := range uniqueFiles {
				if source == related {
					continue
				}

				key := edgeKey{source: source, related: related}
				edge := edges[key]
				edge.SourcePath = source
				edge.RelatedPath = related
				edge.CochangeCount++
				daysAgo := indexedAt.Sub(commit.CommittedAt).Hours()/24 + 1
				if daysAgo < 1 {
					daysAgo = 1
				}
				edge.RecencyWeight += 1 / daysAgo
				edges[key] = edge
			}
		}
	}

	fileCount := 0
	for filePath, churnScore := range churn {
		absPath := filepath.Join(store.RepoRoot, filepath.FromSlash(filePath))
		_, statErr := os.Stat(absPath)
		exists := statErr == nil

		if err := store.UpsertFile(repo.ID, db.FileStat{
			Path:             filePath,
			Extension:        strings.TrimPrefix(filepath.Ext(filePath), "."),
			ChurnScore:       churnScore,
			RecentTouchCount: recent[filePath],
			AuthorCount:      len(authors[filePath]),
			LastTouchedAt:    lastTouched[filePath],
			ExistsOnDisk:     exists,
		}); err != nil {
			return Result{}, err
		}
		fileCount++
	}

	for _, edge := range edges {
		if err := store.UpsertEdge(repo.ID, edge); err != nil {
			return Result{}, err
		}
	}

	return Result{
		RepoRoot:          store.RepoRoot,
		Branch:            branch,
		CommitCount:       len(commits),
		FileCount:         fileCount,
		CochangeEdgeCount: len(edges),
		IndexedAt:         indexedAt,
	}, nil
}

func dedupe(values []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func OpenStore(path string) (*db.Store, error) {
	repoRoot, err := gitlog.RepoRoot(path)
	if err != nil {
		return nil, fmt.Errorf("resolve repo: %w", err)
	}
	canonicalRoot, err := gitlog.CanonicalRepoRoot(path)
	if err != nil {
		return nil, fmt.Errorf("resolve repo: %w", err)
	}
	return db.OpenCanonical(repoRoot, canonicalRoot)
}
