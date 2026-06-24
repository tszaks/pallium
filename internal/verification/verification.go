package verification

import (
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/db"
)

type Report struct {
	Tier         string             `json:"tier"`
	Command      string             `json:"command"`
	ExitCode     int                `json:"exit_code"`
	DurationMS   int64              `json:"duration_ms"`
	ChangedFiles []string           `json:"changed_files"`
	Output       string             `json:"output"`
	Run          db.VerificationRun `json:"run"`
}

func Run(store *db.Store, tier string) (Report, error) {
	if _, err := store.Repo(); err != nil {
		return Report{}, err
	}

	review, err := analysis.Review(store, "HEAD~1")
	if err != nil {
		return Report{}, err
	}
	command, err := CommandForTier(review.Verification, tier)
	if err != nil {
		return Report{}, err
	}

	changedFiles := reviewedFilePaths(review.ChangedFiles)
	start := time.Now()
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = store.RepoRoot
	outputBytes, commandErr := cmd.CombinedOutput()
	duration := time.Since(start)

	exitCode := 0
	if commandErr != nil {
		var exitErr *exec.ExitError
		if errors.As(commandErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	run, err := store.SaveVerificationRun(db.VerificationRun{
		Tier:         tier,
		Command:      command,
		ExitCode:     exitCode,
		DurationMS:   duration.Milliseconds(),
		ChangedFiles: changedFiles,
		CWD:          store.RepoRoot,
		RanAt:        time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return Report{}, err
	}

	return Report{
		Tier:         tier,
		Command:      command,
		ExitCode:     exitCode,
		DurationMS:   duration.Milliseconds(),
		ChangedFiles: changedFiles,
		Output:       string(outputBytes),
		Run:          run,
	}, nil
}

func CommandForTier(plan analysis.VerificationPlan, tier string) (string, error) {
	var commands []string
	switch tier {
	case "fast":
		commands = plan.Fast
	case "safe":
		commands = plan.Safe
	case "full":
		commands = plan.Full
	default:
		return "", fmt.Errorf("invalid verification tier %q: use fast, safe, or full", tier)
	}
	if len(commands) == 0 {
		return "", fmt.Errorf("no %s verification command inferred", tier)
	}
	return commands[0], nil
}

func reviewedFilePaths(files []analysis.ReviewedFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
}
