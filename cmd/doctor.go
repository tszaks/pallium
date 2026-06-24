package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/sessionmemory"
)

type DoctorReport struct {
	RepoRoot               string              `json:"repo_root"`
	RepoDBPath             string              `json:"repo_db_path"`
	RepoDBExists           bool                `json:"repo_db_exists"`
	IndexStatus            string              `json:"index_status"`
	IndexedBranch          string              `json:"indexed_branch,omitempty"`
	LastIndexedCommit      string              `json:"last_indexed_commit,omitempty"`
	IndexedAt              string              `json:"indexed_at,omitempty"`
	CurrentBranch          string              `json:"current_branch,omitempty"`
	CurrentCommit          string              `json:"current_commit,omitempty"`
	WorkingTreeDirty       bool                `json:"working_tree_dirty"`
	WorkingTreeFileCount   int                 `json:"working_tree_file_count"`
	SessionDBPath          string              `json:"session_db_path"`
	SessionDBExists        bool                `json:"session_db_exists"`
	SessionStats           sessionmemory.Stats `json:"session_stats"`
	EmbeddingModel         string              `json:"embedding_model"`
	EmbeddingBacklog       int                 `json:"embedding_backlog"`
	OpenAIKeyAvailable     bool                `json:"openai_key_available"`
	ExecutablePath         string              `json:"executable_path,omitempty"`
	RecommendedNextCommand string              `json:"recommended_next_command,omitempty"`
	Notes                  []string            `json:"notes"`
}

func runDoctor(out io.Writer, args []string, jsonOutput bool) error {
	repoPath := optionalRepoArg(args, 0)
	repoRoot, err := gitlog.RepoRoot(repoPath)
	if err != nil {
		return err
	}

	report := DoctorReport{
		RepoRoot:           repoRoot,
		RepoDBPath:         db.DefaultDBPath(repoRoot),
		IndexStatus:        "missing",
		SessionDBPath:      sessionmemory.DefaultDBPath(),
		EmbeddingModel:     sessionmemory.DefaultEmbeddingModel,
		OpenAIKeyAvailable: os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("OPENAI_ADMIN_API_KEY") != "",
	}
	report.ExecutablePath, _ = os.Executable()

	currentBranch, _ := gitlog.CurrentBranch(repoRoot)
	currentCommit, _ := gitlog.CurrentCommit(repoRoot)
	workingTree, _ := gitlog.WorkingTreeChanges(repoRoot)
	report.CurrentBranch = currentBranch
	report.CurrentCommit = currentCommit
	report.WorkingTreeDirty = len(workingTree) > 0
	report.WorkingTreeFileCount = len(workingTree)

	if _, err := os.Stat(report.RepoDBPath); err == nil {
		report.RepoDBExists = true
		store, err := db.OpenPath(repoRoot, report.RepoDBPath)
		if err != nil {
			return err
		}
		repo, err := store.Repo()
		closeErr := store.Close()
		if closeErr != nil && err == nil {
			err = closeErr
		}
		if err == nil {
			report.IndexStatus = "indexed"
			report.IndexedBranch = repo.Branch
			report.LastIndexedCommit = repo.LastIndexedCommit
			report.IndexedAt = repo.IndexedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		} else if !errors.Is(err, db.ErrRepoNotIndexed) {
			return err
		}
	}

	if report.IndexStatus == "missing" {
		report.RecommendedNextCommand = "pallium index"
		report.Notes = append(report.Notes, "Repo has not been indexed yet, so history-backed guidance is unavailable.")
	} else if report.LastIndexedCommit != "" && report.CurrentCommit != "" && report.LastIndexedCommit != report.CurrentCommit {
		report.IndexStatus = "stale"
		report.RecommendedNextCommand = "pallium index"
		report.Notes = append(report.Notes, "Git HEAD moved since the last repo index.")
	}
	if report.WorkingTreeDirty {
		report.Notes = append(report.Notes, "Working tree has changes outside the last indexed snapshot.")
	}

	if _, err := os.Stat(report.SessionDBPath); err == nil {
		report.SessionDBExists = true
		stats, err := sessionmemory.StatsReadPath(report.SessionDBPath)
		if err != nil {
			return err
		}
		report.SessionStats = stats
		backlog, err := sessionmemory.EmbeddingBacklogPath(report.SessionDBPath, report.EmbeddingModel)
		if err != nil {
			return err
		}
		report.EmbeddingBacklog = backlog
	} else {
		report.Notes = append(report.Notes, "Session memory database does not exist yet.")
	}
	if report.EmbeddingBacklog > 0 {
		report.Notes = append(report.Notes, "Session embeddings are not fully caught up.")
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			"Pallium doctor",
			"Repo: " + report.RepoRoot,
			"Repo DB: " + report.RepoDBPath,
			"Index status: " + report.IndexStatus,
			fmt.Sprintf("Working tree files: %d", report.WorkingTreeFileCount),
			"Session DB: " + report.SessionDBPath,
			fmt.Sprintf("Sessions: %d chunks: %d embeddings: %d backlog: %d", report.SessionStats.Sessions, report.SessionStats.Chunks, report.SessionStats.Embeddings, report.EmbeddingBacklog),
			fmt.Sprintf("OpenAI key available: %t", report.OpenAIKeyAvailable),
		}
		if report.RecommendedNextCommand != "" {
			lines = append(lines, "Recommended next command: "+report.RecommendedNextCommand)
		}
		if len(report.Notes) > 0 {
			lines = append(lines, "", "Notes:")
			for _, note := range report.Notes {
				lines = append(lines, "- "+note)
			}
		}
		return strings.Join(lines, "\n")
	})
}
