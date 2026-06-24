package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

type App struct {
	stdout io.Writer
	stderr io.Writer
}

func NewApp(stdout, stderr io.Writer) *App {
	return &App{stdout: stdout, stderr: stderr}
}

func (a *App) Run(args []string) error {
	jsonOutput := false
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			jsonOutput = true
			continue
		}
		filtered = append(filtered, arg)
	}

	if len(filtered) == 0 {
		a.printHelp()
		return nil
	}

	switch filtered[0] {
	case "help", "-h", "--help":
		a.printHelp()
		return nil
	case "index":
		return runIndex(a.stdout, filtered[1:], jsonOutput)
	case "doctor":
		return runDoctor(a.stdout, filtered[1:], jsonOutput)
	case "version":
		return runVersion(a.stdout, jsonOutput)
	case "explain":
		return runExplain(a.stdout, filtered[1:], jsonOutput)
	case "risk":
		return runRisk(a.stdout, filtered[1:], jsonOutput)
	case "neighbors":
		return runNeighbors(a.stdout, filtered[1:], jsonOutput)
	case "decisions":
		return runDecisions(a.stdout, filtered[1:], jsonOutput)
	case "safe":
		return runSafe(a.stdout, filtered[1:], jsonOutput)
	case "plan":
		return runPlan(a.stdout, filtered[1:], jsonOutput)
	case "review":
		return runReview(a.stdout, filtered[1:], jsonOutput)
	case "verify":
		return runVerify(a.stdout, filtered[1:], jsonOutput)
	case "changed-now":
		return runChangedNow(a.stdout, filtered[1:], jsonOutput)
	case "handoff":
		return runHandoff(a.stdout, filtered[1:], jsonOutput)
	case "task":
		return runTask(a.stdout, filtered[1:], jsonOutput)
	case "sessions":
		return runSessions(a.stdout, filtered[1:], jsonOutput)
	default:
		a.printHelp()
		return fmt.Errorf("unknown command: %s", filtered[0])
	}
}

func (a *App) printHelp() {
	fmt.Fprintln(a.stdout, `pallium

Usage:
  pallium index [repo-path] [--json]
  pallium doctor [repo-path] [--json]
  pallium version [--json]
  pallium explain <path> [repo-path] [--json]
  pallium risk <path> [repo-path] [--json]
  pallium neighbors <path> [repo-path] [--json]
  pallium decisions <query> [repo-path] [--json]
  pallium safe <path> [repo-path] [--json]
  pallium plan <path> [repo-path] [--json]
  pallium review [base-ref] [repo-path] [--json]
  pallium verify <fast|safe|full> [repo-path] [--json]
  pallium changed-now [repo-path] [--json]
  pallium handoff [base-ref] [repo-path] [--json]
  pallium task start <goal> [scope-paths...] [--json]
  pallium task show [--json]
  pallium task clear [--json]
  pallium sessions <live|watch|index|list|search|related|grep|show|embed|semantic|stats> [--json]`)
}

func requireArg(args []string, field string) (string, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return "", fmt.Errorf("missing required argument: %s", field)
	}
	return strings.TrimSpace(args[0]), nil
}

func optionalRepoArg(args []string, index int) string {
	if len(args) <= index {
		return "."
	}
	if strings.TrimSpace(args[index]) == "" {
		return "."
	}
	return strings.TrimSpace(args[index])
}

var errNotImplemented = errors.New("not implemented")
