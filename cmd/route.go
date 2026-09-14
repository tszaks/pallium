package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/router"
)

func defaultRunRoutedCommand(args []string) (string, string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := NewApp(&stdout, &stderr).Run(args)
	return stdout.String(), stderr.String(), err
}

func runRoute(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) > 0 && args[0] == "models" {
		return runModelRoute(out, args[1:], jsonOutput)
	}
	return runRouteWithRunner(out, args, jsonOutput, defaultRunRoutedCommand)
}

func runRouteWithRunner(out io.Writer, args []string, jsonOutput bool, runner func([]string) (string, string, error)) error {
	if hasHelpArg(args) {
		printRouteHelp(out)
		return nil
	}
	if len(args) > 0 && args[0] == "capabilities" {
		if len(args) != 1 {
			return fmt.Errorf("usage: pallium route capabilities [--json]")
		}
		capabilities := router.Capabilities()
		return output.Write(out, capabilities, jsonOutput, func() string {
			lines := []string{"Pallium capabilities:"}
			for _, capability := range capabilities {
				lines = append(lines, fmt.Sprintf("- %s (%s, %s): %s", capability.ID, capability.Service, capability.RequiredAuthority, capability.Description))
			}
			return strings.Join(lines, "\n")
		})
	}
	fs := newSessionFlagSet("route")
	cwd := fs.String("cwd", ".", "")
	authority := fs.String("authority", router.AuthorityObserve, "")
	execute := fs.Bool("execute", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"cwd": {}, "authority": {}}, map[string]struct{}{"execute": {}}); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return fmt.Errorf("usage: pallium route <task> [--cwd path] [--authority observe|execute|edit|external] [--execute] [--json]")
	}
	report, err := router.Route(context.Background(), router.Options{Task: task, CWD: *cwd, Authority: *authority})
	if err != nil {
		return err
	}
	var executionErr error
	if *execute {
		execution := &router.Execution{ExitCode: 1}
		report.Execution = execution
		if !report.Allowed {
			execution.Error = report.BlockedReason
			executionErr = fmt.Errorf("route execution blocked: %s", report.BlockedReason)
		} else {
			execution.Attempted = true
			startedAt := time.Now().UTC()
			execution.StartedAt = &startedAt
			stdout, stderr, runErr := runner(report.CommandArgs)
			finishedAt := time.Now().UTC()
			execution.FinishedAt = &finishedAt
			execution.Stderr = strings.TrimSpace(stderr)
			if runErr != nil {
				execution.Error = runErr.Error()
				execution.Output = strings.TrimSpace(stdout)
				executionErr = fmt.Errorf("routed command failed: %w", runErr)
			} else {
				execution.ExitCode = 0
				execution.Success = true
				trimmed := strings.TrimSpace(stdout)
				var result any
				if trimmed != "" && json.Unmarshal([]byte(trimmed), &result) == nil {
					execution.Result = result
				} else {
					execution.Output = trimmed
				}
			}
		}
	}
	writeErr := output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			"Recommended: " + report.CapabilityID + " / " + report.Action,
			"Why: " + report.Why,
			"Decision confidence: " + report.DecisionConfidence,
			fmt.Sprintf("Authority: requires %s; ceiling %s; allowed=%t", report.RequiredAuthority, report.AuthorityCeiling, report.Allowed),
		}
		if report.Command != "" {
			lines = append(lines, "Command: "+report.Command)
		}
		if report.BlockedReason != "" {
			lines = append(lines, "Blocked: "+report.BlockedReason)
		}
		if report.Execution != nil {
			switch {
			case report.Execution.Success:
				lines = append(lines, "Execution: succeeded")
			case report.Execution.Attempted:
				lines = append(lines, "Execution: failed: "+report.Execution.Error)
			default:
				lines = append(lines, "Execution: not attempted: "+report.Execution.Error)
			}
			if report.Execution.Output != "" {
				lines = append(lines, "", report.Execution.Output)
			}
		}
		if len(report.Signals) > 0 {
			lines = append(lines, "Signals: "+strings.Join(report.Signals, "; "))
		}
		if len(report.Alternatives) > 0 {
			lines = append(lines, "", "Alternatives considered:")
			for _, alternative := range report.Alternatives {
				line := "- " + alternative.Service + ": " + alternative.WhyNot
				if alternative.Command != "" {
					line += " (" + alternative.Command + ")"
				}
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	})
	if writeErr != nil {
		return writeErr
	}
	return executionErr
}

func printRouteHelp(out io.Writer) {
	fmt.Fprintln(out, `pallium route

Choose and explain the best Pallium capability for a task. The authority ceiling
is supplied by the caller and defaults to read-only observation. --execute runs
the recommendation directly without a shell, but never widens that ceiling.

Usage:
  pallium route <task> [--cwd path] [--authority observe|execute|edit|external] [--execute] [--json]
  pallium route capabilities [--json]
  pallium route models <init|explain|catalog|history> [options]`)
}
