package cmd

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/output"
)

type VerifyReport struct {
	Tier         string             `json:"tier"`
	Command      string             `json:"command"`
	ExitCode     int                `json:"exit_code"`
	DurationMS   int64              `json:"duration_ms"`
	ChangedFiles []string           `json:"changed_files"`
	Output       string             `json:"output"`
	Run          db.VerificationRun `json:"run"`
}

type verificationCommandError struct {
	exitCode int
}

func (e verificationCommandError) Error() string {
	return fmt.Sprintf("verification command failed with exit code %d", e.exitCode)
}

func runVerify(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		printVerifyHelp(out)
		return nil
	}
	tier, err := requireArg(args, "tier")
	if err != nil {
		return err
	}
	tier = strings.ToLower(tier)
	if tier != "fast" && tier != "safe" && tier != "full" {
		return fmt.Errorf("invalid verification tier %q: use fast, safe, or full", tier)
	}

	repoPath := optionalRepoArg(args, 1)
	indexer, err := openIndexedStore(repoPath)
	if err != nil {
		return err
	}
	defer indexer.Store.Close()

	report, err := runVerificationTier(indexer.Store, tier)
	if err != nil {
		return err
	}

	if err := output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			fmt.Sprintf("Verification tier: %s", report.Tier),
			fmt.Sprintf("Command: %s", report.Command),
			fmt.Sprintf("Exit code: %d", report.ExitCode),
			fmt.Sprintf("Duration: %dms", report.DurationMS),
		}
		if len(report.ChangedFiles) > 0 {
			lines = append(lines, "Changed files:")
			for _, path := range report.ChangedFiles {
				lines = append(lines, "- "+path)
			}
		}
		if strings.TrimSpace(report.Output) != "" {
			lines = append(lines, "", "Output:", strings.TrimRight(report.Output, "\n"))
		}
		return strings.Join(lines, "\n")
	}); err != nil {
		return err
	}

	if report.ExitCode != 0 {
		return verificationCommandError{exitCode: report.ExitCode}
	}
	return nil
}

func printVerifyHelp(out io.Writer) {
	fmt.Fprintln(out, `pallium verify

Usage:
  pallium verify <fast|safe|full> [repo-path] [--json]`)
}

func runVerificationTier(store *db.Store, tier string) (VerifyReport, error) {
	if _, err := store.Repo(); err != nil {
		return VerifyReport{}, err
	}

	review, err := analysis.Review(store, "HEAD~1")
	if err != nil {
		return VerifyReport{}, err
	}
	command, err := commandForVerificationTier(review.Verification, tier)
	if err != nil {
		return VerifyReport{}, err
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
		return VerifyReport{}, err
	}

	return VerifyReport{
		Tier:         tier,
		Command:      command,
		ExitCode:     exitCode,
		DurationMS:   duration.Milliseconds(),
		ChangedFiles: changedFiles,
		Output:       string(outputBytes),
		Run:          run,
	}, nil
}

func commandForVerificationTier(plan analysis.VerificationPlan, tier string) (string, error) {
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
