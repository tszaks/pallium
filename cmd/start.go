package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/workflow"
)

// runStart is Pallium's golden-path command. From a natural-language task it
// generates a repo-scoped workflow using the RESOLVED provider (adopting
// whatever agent is steering Pallium — never a hardcoded model) and runs it.
// The generated workflow calls pallium.preflight() itself, so start is the
// single "describe what you want" entry point an agent can call first without
// knowing the lower-level preflight/generate/run commands.
//
// Provider generation is preferred; the deterministic styled template
// (workflow.GenerateScript) is the fallback when no provider can generate, so
// start always yields a runnable workflow without silently masking a broken
// provider — the fallback is announced.
func runStart(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("start")
	workflowName := fs.String("workflow", "", "")
	cwd := fs.String("cwd", "", "")
	style := fs.String("style", "auto", "")
	testCommand := fs.String("test-command", "", "")
	codexBinary := fs.String("codex", "codex", "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := parseSessionFlags(fs, args,
		map[string]struct{}{"workflow": {}, "cwd": {}, "style": {}, "test-command": {}, "codex": {}},
		map[string]struct{}{"dry-run": {}}); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" && strings.TrimSpace(*workflowName) == "" {
		return fmt.Errorf("pallium start requires a task, e.g. pallium start \"add a health endpoint\" (or --workflow <name>)")
	}
	if strings.TrimSpace(*cwd) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		*cwd = wd
	}
	absCWD, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}

	script := ""
	source := ""
	if name := strings.TrimSpace(*workflowName); name != "" {
		resolved, err := workflow.ResolveSavedWorkflow(absCWD, name)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(resolved)
		if err != nil {
			return err
		}
		script = string(raw)
		source = "saved:" + name
	} else {
		// Deterministic styled template: always available, needs no provider.
		base, err := workflow.GenerateScript(workflow.GenerateOptions{Task: task, Style: *style, TestCommand: *testCommand})
		if err != nil {
			return err
		}
		// Tailor it with the resolved provider (adopts the steering agent). Fall
		// back to the deterministic template when the provider can't generate OR
		// returns an invalid script (LLM output is non-deterministic) — start
		// must always yield a runnable workflow, and the fallback is announced.
		script = base
		source = "template"
		tailored, genErr := generateWorkflowWithLLM(task, *style, *testCommand, 3, *codexBinary, base)
		switch {
		case genErr != nil:
			if !jsonOutput {
				fmt.Fprintf(out, "note: provider generation unavailable (%v); running the deterministic template\n", genErr)
			}
		case !workflow.ValidateScript(tailored).Valid:
			if !jsonOutput {
				fmt.Fprintln(out, "note: provider produced an invalid script; running the deterministic template")
			}
		default:
			script = tailored
			source = "generated:" + workflow.ResolveProvider("", "")
		}
	}

	if validation := workflow.ValidateScript(script); !validation.Valid {
		return fmt.Errorf("resolved workflow is invalid: %s", validation.Error)
	}

	if *dryRun {
		return output.Write(out, map[string]string{"task": task, "source": source, "script": script}, jsonOutput, func() string {
			return "# start --dry-run\n# source: " + source + "\n\n" + script
		})
	}

	// Hand the resolved workflow to the normal run path via a unique temp file:
	// runWorkflowRun copies it into the per-run store (inspectable with
	// `pallium workflow inspect <id>`), so a fixed repo path would only invite
	// concurrent starts to clobber each other and litter the repo.
	tmp, err := os.CreateTemp("", "pallium-start-*.js")
	if err != nil {
		return err
	}
	scriptPath := tmp.Name()
	defer os.Remove(scriptPath)
	if _, err := tmp.WriteString(script); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !jsonOutput {
		fmt.Fprintf(out, "Starting workflow (source: %s)\n", source)
	}
	runArgs := []string{"--script", scriptPath, "--cwd", absCWD}
	if task != "" {
		runArgs = append(runArgs, task)
	}
	return runWorkflowRun(out, runArgs, jsonOutput)
}
