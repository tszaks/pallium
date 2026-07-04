package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/workflow"
)

func runWorkflow(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printWorkflowHelp(out)
		return nil
	}
	switch args[0] {
	case "run":
		return runWorkflowRun(out, args[1:], jsonOutput)
	case "list", "ls":
		return runWorkflowList(out, args[1:], jsonOutput)
	case "show":
		return runWorkflowShow(out, args[1:], jsonOutput)
	case "read":
		return runWorkflowRead(out, args[1:], jsonOutput)
	case "watch":
		return runWorkflowWatch(out, args[1:])
	case "stop":
		return runWorkflowStop(out, args[1:], jsonOutput)
	case "save":
		return runWorkflowSave(out, args[1:], jsonOutput)
	case "apply":
		return runWorkflowApply(out, args[1:], jsonOutput)
	default:
		printWorkflowHelp(out)
		return fmt.Errorf("unknown workflow subcommand: %s", args[0])
	}
}

func runWorkflowRun(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow run")
	dbPath := fs.String("db", "", "")
	cwd := fs.String("cwd", "", "")
	id := fs.String("id", "", "")
	scriptPath := fs.String("script", "", "")
	argsJSON := fs.String("args", "", "")
	codexBinary := fs.String("codex", "codex", "")
	maxAgents := fs.Int("max-agents", 1000, "")
	maxConcurrentAgents := fs.Int("max-concurrent-agents", 16, "")
	maxBudgetUSD := fs.String("max-budget-usd", "", "")
	background := fs.Bool("background", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "cwd": {}, "id": {}, "script": {}, "args": {}, "codex": {}, "max-agents": {}, "max-concurrent-agents": {}, "max-budget-usd": {}}, map[string]struct{}{"background": {}}); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" && *scriptPath == "" {
		return fmt.Errorf("workflow run requires a task or --script")
	}
	if *id == "" {
		*id = workflow.NewID("wf")
	}
	if err := workflow.ValidateID(*id); err != nil {
		return err
	}
	if *cwd == "" {
		var err error
		*cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	absCWD, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	if task == "" {
		task = "Run workflow script " + *scriptPath
	}

	script := ""
	if *scriptPath != "" {
		raw, err := os.ReadFile(*scriptPath)
		if err != nil {
			return err
		}
		script = string(raw)
	} else {
		script = workflow.DefaultScript(task)
	}
	runScriptPath, err := workflow.WriteRunScript(*id, absCWD, script)
	if err != nil {
		return err
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	run, err := store.UpsertRun(workflow.Run{
		ID:         *id,
		Task:       task,
		CWD:        absCWD,
		ScriptPath: runScriptPath,
		ArgsJSON:   *argsJSON,
		Status:     "queued",
	})
	_ = store.Close()
	if err != nil {
		return err
	}
	if *background {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		ownedID := "workflow-" + run.ID
		cmdArgs := []string{"--background", "--id", ownedID, "--cwd", absCWD}
		if *dbPath != "" {
			cmdArgs = append(cmdArgs, "--db", *dbPath)
		}
		cmdArgs = append(cmdArgs,
			"--",
			exe, "workflow", "run",
			"--id", run.ID,
			"--cwd", absCWD,
			"--script", runScriptPath,
			"--codex", *codexBinary,
			"--max-agents", fmt.Sprintf("%d", *maxAgents),
			"--max-concurrent-agents", fmt.Sprintf("%d", *maxConcurrentAgents),
		)
		if *dbPath != "" {
			cmdArgs = append(cmdArgs, "--db", *dbPath)
		}
		if *argsJSON != "" {
			cmdArgs = append(cmdArgs, "--args", *argsJSON)
		}
		if *maxBudgetUSD != "" {
			cmdArgs = append(cmdArgs, "--max-budget-usd", *maxBudgetUSD)
		}
		cmdArgs = append(cmdArgs, task)
		var buf strings.Builder
		if err := runConsoleRun(&buf, cmdArgs, true); err != nil {
			return err
		}
		store, err := workflow.Open(*dbPath)
		if err == nil {
			_ = store.SetRunOwnedID(run.ID, ownedID)
			_ = store.Close()
		}
		return output.Write(out, map[string]string{"id": run.ID, "owned_session_id": ownedID, "status": "background"}, jsonOutput, func() string {
			return fmt.Sprintf("Workflow started: %s\nOwned session: %s", run.ID, ownedID)
		})
	}

	inputArgs, err := workflow.ParseArgsJSON(*argsJSON)
	if err != nil {
		return err
	}
	store, err = workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	runner := workflow.Runner{
		Store:               store,
		Run:                 run,
		MaxAgents:           *maxAgents,
		MaxConcurrentAgents: *maxConcurrentAgents,
		MaxBudgetUSD:        *maxBudgetUSD,
		CodexBinary:         *codexBinary,
	}
	result, err := runner.Execute(context.Background(), script, inputArgs)
	if err != nil {
		return err
	}
	snapshot, err := store.Snapshot(run.ID)
	if err != nil {
		return err
	}
	return output.Write(out, snapshot, jsonOutput, func() string {
		return renderWorkflowResult(snapshot, result)
	})
}

func runWorkflowList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow list")
	dbPath := fs.String("db", "", "")
	limit := fs.Int("limit", 50, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "limit": {}}, nil); err != nil {
		return err
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	runs, err := store.ListRuns(*limit)
	if err != nil {
		return err
	}
	return output.Write(out, runs, jsonOutput, func() string {
		if len(runs) == 0 {
			return "No workflow runs found."
		}
		lines := []string{"Workflow runs:"}
		for _, run := range runs {
			lines = append(lines, fmt.Sprintf("- %s %s %s", run.ID, run.Status, run.Task))
		}
		return strings.Join(lines, "\n")
	})
}

func runWorkflowShow(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow show")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow show <run-id>")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	snapshot, err := store.Snapshot(fs.Arg(0))
	if err != nil {
		return err
	}
	return output.Write(out, snapshot, jsonOutput, func() string {
		return renderWorkflowSnapshot(snapshot)
	})
}

func runWorkflowRead(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow read")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow read <run-id>")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	run, err := store.Run(fs.Arg(0))
	if err != nil {
		return err
	}
	payload := map[string]string{"id": run.ID, "result": run.Result, "error": run.Error}
	return output.Write(out, payload, jsonOutput, func() string {
		if run.Error != "" {
			return run.Error
		}
		if run.Result == "" {
			return "No result recorded."
		}
		return run.Result
	})
}

func runWorkflowWatch(out io.Writer, args []string) error {
	fs := newSessionFlagSet("workflow watch")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow watch <run-id>")
	}
	for {
		store, err := workflow.Open(*dbPath)
		if err != nil {
			return err
		}
		snapshot, err := store.Snapshot(fs.Arg(0))
		_ = store.Close()
		if err != nil {
			return err
		}
		if _, ok := out.(interface{ Fd() uintptr }); ok {
			fmt.Fprint(out, "\033[H\033[2J")
		}
		fmt.Fprintln(out, renderWorkflowSnapshot(snapshot))
		if snapshot.Run.Status == "completed" || snapshot.Run.Status == "failed" || snapshot.Run.Status == "stopped" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

func runWorkflowStop(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow stop")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow stop <run-id>")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	run, err := store.Run(fs.Arg(0))
	_ = store.Close()
	if err != nil {
		return err
	}
	if run.OwnedID == "" {
		if err := storeWorkflowStatus(*dbPath, run.ID, run.Result, "stopped without owned session"); err != nil {
			return err
		}
		return output.Write(out, map[string]string{"id": run.ID, "status": "stopped"}, jsonOutput, func() string {
			return "Workflow marked stopped: " + run.ID
		})
	}
	var buf strings.Builder
	interruptArgs := []string{run.OwnedID}
	if *dbPath != "" {
		interruptArgs = append(interruptArgs, "--db", *dbPath)
	}
	if err := runConsoleInterrupt(&buf, interruptArgs, true); err != nil {
		return err
	}
	store, err = workflow.Open(*dbPath)
	if err == nil {
		_ = store.SetRunStatus(run.ID, "stopped", run.Result, "interrupted")
		_ = store.Close()
	}
	return output.Write(out, map[string]string{"id": run.ID, "status": "stopped"}, jsonOutput, func() string {
		return "Workflow stopped: " + run.ID
	})
}

func storeWorkflowStatus(dbPath, runID, result, errorText string) error {
	store, err := workflow.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.SetRunStatus(runID, "stopped", result, errorText)
}

func runWorkflowSave(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow save")
	dbPath := fs.String("db", "", "")
	name := fs.String("name", "", "")
	user := fs.Bool("user", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "name": {}}, map[string]struct{}{"user": {}}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow save <run-id> --name name")
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("workflow save requires --name")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	run, err := store.Run(fs.Arg(0))
	_ = store.Close()
	if err != nil {
		return err
	}
	destRoot := filepath.Join(run.CWD, ".pallium", "workflows")
	if *user {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		destRoot = filepath.Join(home, ".pallium", "workflows")
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(destRoot, *name+".js")
	raw, err := os.ReadFile(run.ScriptPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dest, raw, 0o644); err != nil {
		return err
	}
	return output.Write(out, map[string]string{"path": dest}, jsonOutput, func() string {
		return "Workflow saved: " + dest
	})
}

func runWorkflowApply(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow apply")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow apply <run-id>")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	snapshot, err := store.Snapshot(fs.Arg(0))
	_ = store.Close()
	if err != nil {
		return err
	}
	applied, err := workflow.ApplyPatches(context.Background(), snapshot)
	if err != nil {
		return err
	}
	return output.Write(out, map[string]any{"id": snapshot.Run.ID, "applied": applied}, jsonOutput, func() string {
		if len(applied) == 0 {
			return "No workflow patches to apply."
		}
		return "Applied workflow patches:\n- " + strings.Join(applied, "\n- ")
	})
}

func renderWorkflowResult(snapshot workflow.Snapshot, result string) string {
	text := renderWorkflowSnapshot(snapshot)
	if result != "" {
		text += "\n\nResult:\n" + result
	}
	return text
}

func renderWorkflowSnapshot(snapshot workflow.Snapshot) string {
	lines := []string{
		fmt.Sprintf("Workflow %s: %s", snapshot.Run.ID, snapshot.Run.Status),
		"Task: " + snapshot.Run.Task,
	}
	if snapshot.Run.OwnedID != "" {
		lines = append(lines, "Owned session: "+snapshot.Run.OwnedID)
	}
	if len(snapshot.Phases) > 0 {
		lines = append(lines, "Phases:")
		for _, phase := range snapshot.Phases {
			lines = append(lines, fmt.Sprintf("- %s %s agents=%d", phase.Name, phase.Status, phase.AgentCount))
		}
	}
	if len(snapshot.Agents) > 0 {
		lines = append(lines, "Agents:")
		for _, agent := range snapshot.Agents {
			label := agent.Label
			if label == "" {
				label = agent.ID
			}
			lines = append(lines, fmt.Sprintf("- %s %s mode=%s phase=%s", label, agent.Status, agent.Mode, agent.Phase))
			if agent.PatchPath != "" {
				lines = append(lines, "  patch: "+agent.PatchPath)
			}
			if agent.Error != "" {
				lines = append(lines, "  error: "+agent.Error)
			}
		}
	}
	if snapshot.Run.Error != "" {
		lines = append(lines, "Error: "+snapshot.Run.Error)
	}
	return strings.Join(lines, "\n")
}

func printWorkflowHelp(out io.Writer) {
	fmt.Fprintln(out, `pallium workflow

Usage:
  pallium workflow run "task" [--script path.js] [--background] [--max-concurrent-agents 16] [--json]
  pallium workflow list [--limit n] [--json]
  pallium workflow show <run-id> [--json]
  pallium workflow read <run-id> [--json]
  pallium workflow watch <run-id>
  pallium workflow stop <run-id> [--json]
  pallium workflow save <run-id> --name name [--user] [--json]
  pallium workflow apply <run-id> [--json]`)
}

func latestWorkflowRun(store *workflow.Store) (workflow.Run, error) {
	runs, err := store.ListRuns(1)
	if err != nil {
		return workflow.Run{}, err
	}
	if len(runs) == 0 {
		return workflow.Run{}, sql.ErrNoRows
	}
	return runs[0], nil
}
