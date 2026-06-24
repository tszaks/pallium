package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/verification"
)

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

	report, err := verification.Run(indexer.Store, tier)
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
