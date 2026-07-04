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

	"github.com/tszaks/pallium/internal/analysis"
	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/workflow"
)

func runWorkflow(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printWorkflowHelp(out)
		return nil
	}
	switch args[0] {
	case "generate":
		return runWorkflowGenerate(out, args[1:], jsonOutput)
	case "preflight":
		return runWorkflowPreflight(out, args[1:], jsonOutput)
	case "validate":
		return runWorkflowValidate(out, args[1:], jsonOutput)
	case "tools":
		return runWorkflowTools(out, args[1:], jsonOutput)
	case "template", "templates":
		return runWorkflowTemplates(out, args[1:], jsonOutput)
	case "trigger", "triggers":
		return runWorkflowTrigger(out, args[1:], jsonOutput)
	case "fleet":
		return runWorkflowFleet(out, args[1:], jsonOutput)
	case "gate", "gates":
		return runWorkflowGate(out, args[1:], jsonOutput)
	case "serve":
		return runWorkflowServe(out, args[1:], jsonOutput)
	case "run":
		return runWorkflowRun(out, args[1:], jsonOutput)
	case "list", "ls":
		return runWorkflowList(out, args[1:], jsonOutput)
	case "status":
		return runWorkflowStatus(out, args[1:], jsonOutput)
	case "inspect":
		return runWorkflowInspect(out, args[1:], jsonOutput)
	case "show":
		return runWorkflowShow(out, args[1:], jsonOutput)
	case "read":
		return runWorkflowRead(out, args[1:], jsonOutput)
	case "report":
		return runWorkflowReport(out, args[1:], jsonOutput)
	case "watch":
		return runWorkflowWatch(out, args[1:])
	case "pause":
		return runWorkflowPause(out, args[1:], jsonOutput)
	case "resume":
		return runWorkflowResume(out, args[1:], jsonOutput)
	case "stop":
		return runWorkflowStop(out, args[1:], jsonOutput)
	case "save":
		return runWorkflowSave(out, args[1:], jsonOutput)
	case "apply":
		return runWorkflowApply(out, args[1:], jsonOutput)
	case "revert":
		return runWorkflowRevert(out, args[1:], jsonOutput)
	default:
		printWorkflowHelp(out)
		return fmt.Errorf("unknown workflow subcommand: %s", args[0])
	}
}

func runWorkflowPreflight(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow preflight")
	repoPath := fs.String("cwd", "", "")
	var scopes multiStringFlag
	fs.Var(&scopes, "scope", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"cwd": {}, "scope": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: pallium workflow preflight <task> [--scope path] [--cwd repo-path]")
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	indexer, err := openIndexedStore(firstNonEmpty(*repoPath, "."))
	if err != nil {
		return err
	}
	defer indexer.Store.Close()
	report, err := analysis.WorkflowPreflight(indexer.Store, task, scopes)
	if err != nil {
		return err
	}
	return output.Write(out, report, jsonOutput, func() string {
		return renderWorkflowPreflight(report)
	})
}

func runWorkflowTools(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		return runWorkflowToolsList(out, nil, jsonOutput)
	}
	switch args[0] {
	case "list", "ls":
		return runWorkflowToolsList(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown workflow tools subcommand: %s", args[0])
	}
}

func runWorkflowToolsList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow tools list")
	kind := fs.String("kind", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"kind": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: pallium workflow tools list [--kind control|agent|verification|pallium]")
	}
	tools := workflow.WorkflowTools()
	if strings.TrimSpace(*kind) != "" {
		filtered := tools[:0]
		for _, tool := range tools {
			if tool.Kind == *kind {
				filtered = append(filtered, tool)
			}
		}
		tools = filtered
	}
	return output.Write(out, tools, jsonOutput, func() string {
		if len(tools) == 0 {
			return "No workflow tools found."
		}
		lines := []string{"Workflow tools:"}
		for _, tool := range tools {
			lines = append(lines, fmt.Sprintf("- %s %s [%s]: %s", tool.Name, tool.Signature, tool.Kind, tool.Description))
		}
		return strings.Join(lines, "\n")
	})
}

func runWorkflowTemplates(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		return runWorkflowTemplateList(out, nil, jsonOutput)
	}
	switch args[0] {
	case "list", "ls":
		return runWorkflowTemplateList(out, args[1:], jsonOutput)
	case "show":
		return runWorkflowTemplateShow(out, args[1:], jsonOutput)
	default:
		if tmpl, ok := workflow.WorkflowTemplate(args[0]); ok {
			return output.Write(out, tmpl, jsonOutput, func() string {
				return renderWorkflowTemplate(tmpl)
			})
		}
		return workflow.UnknownTemplateError(args[0])
	}
}

func runWorkflowTemplateList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow template list")
	if err := parseSessionFlags(fs, args, nil, nil); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: pallium workflow template list")
	}
	templates := workflow.WorkflowTemplates()
	return output.Write(out, templates, jsonOutput, func() string {
		lines := []string{"Workflow templates:"}
		for _, tmpl := range templates {
			line := fmt.Sprintf("- %s [%s]: %s", tmpl.Name, tmpl.Style, tmpl.Description)
			if tmpl.RequiresTestCommand {
				line += " Requires --test-command."
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	})
}

func runWorkflowTemplateShow(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow template show")
	if err := parseSessionFlags(fs, args, nil, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow template show <name>")
	}
	tmpl, ok := workflow.WorkflowTemplate(fs.Arg(0))
	if !ok {
		return workflow.UnknownTemplateError(fs.Arg(0))
	}
	return output.Write(out, tmpl, jsonOutput, func() string {
		return renderWorkflowTemplate(tmpl)
	})
}

func runWorkflowTrigger(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		return fmt.Errorf("usage: pallium workflow trigger <add|list|show|run>")
	}
	switch args[0] {
	case "add", "set":
		return runWorkflowTriggerAdd(out, args[1:], jsonOutput)
	case "list", "ls":
		return runWorkflowTriggerList(out, args[1:], jsonOutput)
	case "show":
		return runWorkflowTriggerShow(out, args[1:], jsonOutput)
	case "run":
		return runWorkflowTriggerRun(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown workflow trigger subcommand: %s", args[0])
	}
}

func runWorkflowTriggerAdd(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow trigger add")
	dbPath := fs.String("db", "", "")
	cwd := fs.String("cwd", "", "")
	kind := fs.String("kind", "manual", "")
	workflowName := fs.String("workflow", "", "")
	scriptPath := fs.String("script", "", "")
	argsJSON := fs.String("args", "", "")
	disabled := fs.Bool("disabled", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "cwd": {}, "kind": {}, "workflow": {}, "script": {}, "args": {}}, map[string]struct{}{"disabled": {}}); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: pallium workflow trigger add <name> <task> [--workflow name|--script path] [--cwd repo-path]")
	}
	if *workflowName != "" && *scriptPath != "" {
		return fmt.Errorf("use either --workflow or --script, not both")
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
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	trigger, err := store.UpsertTrigger(workflow.Trigger{
		Name:         fs.Arg(0),
		Kind:         *kind,
		Task:         strings.TrimSpace(strings.Join(fs.Args()[1:], " ")),
		CWD:          absCWD,
		WorkflowName: *workflowName,
		ScriptPath:   *scriptPath,
		ArgsJSON:     *argsJSON,
		Enabled:      !*disabled,
	})
	if err != nil {
		return err
	}
	return output.Write(out, trigger, jsonOutput, func() string {
		return renderWorkflowTrigger(trigger)
	})
}

func runWorkflowTriggerList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow trigger list")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	triggers, err := store.ListTriggers()
	if err != nil {
		return err
	}
	return output.Write(out, triggers, jsonOutput, func() string {
		if len(triggers) == 0 {
			return "No workflow triggers found."
		}
		lines := []string{"Workflow triggers:"}
		for _, trigger := range triggers {
			lines = append(lines, fmt.Sprintf("- %s [%s] enabled=%t task=%s", trigger.Name, trigger.Kind, trigger.Enabled, trigger.Task))
		}
		return strings.Join(lines, "\n")
	})
}

func runWorkflowTriggerShow(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow trigger show")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow trigger show <name>")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	trigger, err := store.Trigger(fs.Arg(0))
	if err != nil {
		return err
	}
	return output.Write(out, trigger, jsonOutput, func() string {
		return renderWorkflowTrigger(trigger)
	})
}

func runWorkflowTriggerRun(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow trigger run")
	dbPath := fs.String("db", "", "")
	runID := fs.String("id", "", "")
	background := fs.Bool("background", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "id": {}}, map[string]struct{}{"background": {}}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow trigger run <name> [--background]")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	trigger, err := store.Trigger(fs.Arg(0))
	if err != nil {
		_ = store.Close()
		return err
	}
	if !trigger.Enabled {
		_ = store.Close()
		return fmt.Errorf("workflow trigger %q is disabled", trigger.Name)
	}
	if *runID == "" {
		*runID = workflow.NewID("wf-" + trigger.Name)
	}
	if err := workflow.ValidateID(*runID); err != nil {
		_ = store.Close()
		return err
	}
	if err := store.SetTriggerRun(trigger.Name, *runID); err != nil {
		_ = store.Close()
		return err
	}
	_ = store.Close()

	runArgs := []string{"run", "--id", *runID, "--db", *dbPath, "--cwd", trigger.CWD}
	if *background {
		runArgs = append(runArgs, "--background")
	}
	if trigger.WorkflowName != "" {
		runArgs = append(runArgs, "--workflow", trigger.WorkflowName)
	}
	if trigger.ScriptPath != "" {
		runArgs = append(runArgs, "--script", trigger.ScriptPath)
	}
	if trigger.ArgsJSON != "" {
		runArgs = append(runArgs, "--args", trigger.ArgsJSON)
	}
	runArgs = append(runArgs, trigger.Task)
	return runWorkflowRun(out, runArgs[1:], jsonOutput)
}

func runWorkflowFleet(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		return runWorkflowFleetStatus(out, nil, jsonOutput)
	}
	switch args[0] {
	case "status":
		return runWorkflowFleetStatus(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown workflow fleet subcommand: %s", args[0])
	}
}

type workflowFleetStatus struct {
	RunsTotal       int              `json:"runs_total"`
	RunsByStatus    map[string]int   `json:"runs_by_status"`
	ActiveRuns      []workflow.Run   `json:"active_runs,omitempty"`
	TriggersTotal   int              `json:"triggers_total"`
	EnabledTriggers int              `json:"enabled_triggers"`
	RunningAgents   int              `json:"running_agents"`
	PausedAgents    int              `json:"paused_agents"`
	FailedAgents    int              `json:"failed_agents"`
	UpdatedAt       string           `json:"updated_at"`
	RecentRuns      []workflowStatus `json:"recent_runs,omitempty"`
}

func runWorkflowFleetStatus(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow fleet status")
	dbPath := fs.String("db", "", "")
	limit := fs.Int("limit", 50, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "limit": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: pallium workflow fleet status [--limit n]")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	status, err := buildWorkflowFleetStatus(store, *limit)
	if err != nil {
		return err
	}
	return output.Write(out, status, jsonOutput, func() string {
		return renderWorkflowFleetStatus(status)
	})
}

func buildWorkflowFleetStatus(store *workflow.Store, limit int) (workflowFleetStatus, error) {
	runs, err := store.ListRuns(limit)
	if err != nil {
		return workflowFleetStatus{}, err
	}
	triggers, err := store.ListTriggers()
	if err != nil {
		return workflowFleetStatus{}, err
	}
	status := workflowFleetStatus{
		RunsTotal:     len(runs),
		RunsByStatus:  map[string]int{},
		TriggersTotal: len(triggers),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	for _, trigger := range triggers {
		if trigger.Enabled {
			status.EnabledTriggers++
		}
	}
	for _, run := range runs {
		status.RunsByStatus[run.Status]++
		if run.Status == "queued" || run.Status == "running" || run.Status == "paused" {
			status.ActiveRuns = append(status.ActiveRuns, run)
		}
		snapshot, err := store.Snapshot(run.ID)
		if err != nil {
			continue
		}
		summary := workflowStatusSummary(snapshot)
		status.RecentRuns = append(status.RecentRuns, summary)
		status.RunningAgents += summary.AgentsRunning
		status.PausedAgents += summary.AgentsPaused
		status.FailedAgents += summary.AgentsFailed
	}
	return status, nil
}

func runWorkflowGate(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		return fmt.Errorf("usage: pallium workflow gate <list|approve>")
	}
	switch args[0] {
	case "list", "ls":
		return runWorkflowGateList(out, args[1:], jsonOutput)
	case "approve":
		return runWorkflowGateApprove(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown workflow gate subcommand: %s", args[0])
	}
}

func runWorkflowGateList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow gate list")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow gate list <run-id>")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	gates, err := store.ListGates(fs.Arg(0))
	if err != nil {
		return err
	}
	return output.Write(out, gates, jsonOutput, func() string {
		if len(gates) == 0 {
			return "No workflow gates found."
		}
		lines := []string{"Workflow gates:"}
		for _, gate := range gates {
			lines = append(lines, fmt.Sprintf("- %s %s %s", gate.Name, gate.Status, gate.Message))
		}
		return strings.Join(lines, "\n")
	})
}

func runWorkflowGateApprove(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow gate approve")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: pallium workflow gate approve <run-id> <name>")
	}
	store, err := workflow.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	gate, err := store.ApproveGate(fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	return output.Write(out, gate, jsonOutput, func() string {
		return fmt.Sprintf("Approved workflow gate %s for %s", gate.Name, gate.RunID)
	})
}

func runWorkflowRun(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow run")
	dbPath := fs.String("db", "", "")
	cwd := fs.String("cwd", "", "")
	id := fs.String("id", "", "")
	scriptPath := fs.String("script", "", "")
	workflowName := fs.String("workflow", "", "")
	argsJSON := fs.String("args", "", "")
	codexBinary := fs.String("codex", "codex", "")
	maxAgents := fs.Int("max-agents", 1000, "")
	maxConcurrentAgents := fs.Int("max-concurrent-agents", 16, "")
	maxBudgetUSD := fs.String("max-budget-usd", "", "")
	background := fs.Bool("background", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "cwd": {}, "id": {}, "script": {}, "workflow": {}, "args": {}, "codex": {}, "max-agents": {}, "max-concurrent-agents": {}, "max-budget-usd": {}}, map[string]struct{}{"background": {}}); err != nil {
		return err
	}
	positionals := fs.Args()
	if *scriptPath != "" && *workflowName != "" {
		return fmt.Errorf("use either --script or --workflow, not both")
	}
	if *scriptPath == "" && *workflowName == "" && len(positionals) > 0 && strings.HasPrefix(positionals[0], "/") && len(positionals[0]) > 1 {
		*workflowName = strings.TrimPrefix(positionals[0], "/")
		positionals = positionals[1:]
	}
	task := strings.TrimSpace(strings.Join(positionals, " "))
	if task == "" && *scriptPath == "" && *workflowName == "" {
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
		if *workflowName != "" {
			task = "Run saved workflow " + *workflowName
		} else {
			task = "Run workflow script " + *scriptPath
		}
	}

	script := ""
	if *workflowName != "" {
		resolved, err := workflow.ResolveSavedWorkflow(absCWD, *workflowName)
		if err != nil {
			return err
		}
		*scriptPath = resolved
	}
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

func runWorkflowGenerate(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow generate")
	outputPath := fs.String("output", "", "")
	style := fs.String("style", "auto", "")
	testCommand := fs.String("test-command", "", "")
	maxRounds := fs.Int("max-rounds", 3, "")
	saveName := fs.String("save", "", "")
	user := fs.Bool("user", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"output": {}, "style": {}, "test-command": {}, "max-rounds": {}, "save": {}}, map[string]struct{}{"user": {}}); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return fmt.Errorf("workflow generate requires a task")
	}
	script, err := workflow.GenerateScript(workflow.GenerateOptions{
		Task:        task,
		Style:       *style,
		TestCommand: *testCommand,
		MaxRounds:   *maxRounds,
	})
	if err != nil {
		return err
	}
	if validation := workflow.ValidateScript(script); !validation.Valid {
		return fmt.Errorf("generated workflow is invalid: %s", validation.Error)
	}
	dest := strings.TrimSpace(*outputPath)
	if strings.TrimSpace(*saveName) != "" {
		if err := workflow.ValidateID(*saveName); err != nil {
			return err
		}
		root := filepath.Join(".pallium", "workflows")
		if *user {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			root = filepath.Join(home, ".pallium", "workflows")
		}
		dest = filepath.Join(root, *saveName+".js")
	}
	if dest != "" {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, []byte(script), 0o644); err != nil {
			return err
		}
	}
	payload := map[string]string{
		"task":   task,
		"style":  *style,
		"script": script,
		"path":   dest,
	}
	return output.Write(out, payload, jsonOutput, func() string {
		if dest != "" {
			return "Workflow generated: " + dest
		}
		return script
	})
}

func runWorkflowValidate(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow validate")
	scriptPath := fs.String("script", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"script": {}}, nil); err != nil {
		return err
	}
	if *scriptPath == "" && fs.NArg() == 1 {
		*scriptPath = fs.Arg(0)
	}
	if strings.TrimSpace(*scriptPath) == "" {
		return fmt.Errorf("usage: pallium workflow validate <path.js>")
	}
	raw, err := os.ReadFile(*scriptPath)
	if err != nil {
		return err
	}
	result := workflow.ValidateScript(string(raw))
	if !result.Valid && !jsonOutput {
		return fmt.Errorf("workflow script is invalid: %s", result.Error)
	}
	return output.Write(out, result, jsonOutput, func() string {
		if result.Valid {
			return "Workflow script valid: " + *scriptPath
		}
		return "Workflow script invalid: " + result.Error
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

func runWorkflowStatus(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow status")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow status <run-id>")
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
	status := workflowStatusSummary(snapshot)
	return output.Write(out, status, jsonOutput, func() string {
		return renderWorkflowStatus(status)
	})
}

func runWorkflowInspect(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow inspect")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow inspect <run-id>")
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
	inspection := workflowInspection(snapshot)
	return output.Write(out, inspection, jsonOutput, func() string {
		return renderWorkflowInspection(inspection)
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

func runWorkflowReport(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow report")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow report <run-id>")
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
	report := workflow.BuildReport(snapshot)
	return output.Write(out, report, jsonOutput, func() string {
		return renderWorkflowReport(report)
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
		if isWorkflowTerminalOrPaused(snapshot.Run.Status) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

func runWorkflowPause(out io.Writer, args []string, jsonOutput bool) error {
	return runWorkflowInterruptStatus(out, args, jsonOutput, "pause", "paused")
}

func runWorkflowStop(out io.Writer, args []string, jsonOutput bool) error {
	return runWorkflowInterruptStatus(out, args, jsonOutput, "stop", "stopped")
}

func runWorkflowInterruptStatus(out io.Writer, args []string, jsonOutput bool, commandName, status string) error {
	fs := newSessionFlagSet("workflow " + commandName)
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow %s <run-id>", commandName)
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
		message := status + " without owned session"
		if err := storeWorkflowStatus(*dbPath, run.ID, status, run.Result, message); err != nil {
			return err
		}
		return output.Write(out, map[string]string{"id": run.ID, "status": status}, jsonOutput, func() string {
			return fmt.Sprintf("Workflow marked %s: %s", status, run.ID)
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
		_ = store.SetRunStatus(run.ID, status, run.Result, "interrupted")
		_ = store.Close()
	}
	return output.Write(out, map[string]string{"id": run.ID, "status": status}, jsonOutput, func() string {
		return fmt.Sprintf("Workflow %s: %s", status, run.ID)
	})
}

func storeWorkflowStatus(dbPath, runID, status, result, errorText string) error {
	store, err := workflow.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.SetRunStatus(runID, status, result, errorText)
}

func runWorkflowResume(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow resume")
	dbPath := fs.String("db", "", "")
	codexBinary := fs.String("codex", "codex", "")
	maxAgents := fs.Int("max-agents", 1000, "")
	maxConcurrentAgents := fs.Int("max-concurrent-agents", 16, "")
	maxBudgetUSD := fs.String("max-budget-usd", "", "")
	background := fs.Bool("background", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "codex": {}, "max-agents": {}, "max-concurrent-agents": {}, "max-budget-usd": {}}, map[string]struct{}{"background": {}}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow resume <run-id>")
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
	runArgs := []string{"run", "--id", run.ID, "--cwd", run.CWD, "--script", run.ScriptPath, "--codex", *codexBinary, "--max-agents", fmt.Sprintf("%d", *maxAgents), "--max-concurrent-agents", fmt.Sprintf("%d", *maxConcurrentAgents)}
	if *dbPath != "" {
		runArgs = append(runArgs, "--db", *dbPath)
	}
	if run.ArgsJSON != "" {
		runArgs = append(runArgs, "--args", run.ArgsJSON)
	}
	if *maxBudgetUSD != "" {
		runArgs = append(runArgs, "--max-budget-usd", *maxBudgetUSD)
	}
	if *background {
		runArgs = append(runArgs, "--background")
	}
	runArgs = append(runArgs, run.Task)
	return runWorkflowRun(out, runArgs[1:], jsonOutput)
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

func runWorkflowRevert(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow revert")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium workflow revert <run-id>")
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
	reverted, err := workflow.RevertPatches(context.Background(), snapshot)
	if err != nil {
		return err
	}
	return output.Write(out, map[string]any{"id": snapshot.Run.ID, "reverted": reverted}, jsonOutput, func() string {
		if len(reverted) == 0 {
			return "No workflow patches to revert."
		}
		return "Reverted workflow patches:\n- " + strings.Join(reverted, "\n- ")
	})
}

func renderWorkflowResult(snapshot workflow.Snapshot, result string) string {
	text := renderWorkflowSnapshot(snapshot)
	if result != "" {
		text += "\n\nResult:\n" + result
	}
	return text
}

func isWorkflowTerminalOrPaused(status string) bool {
	switch status {
	case "completed", "failed", "stopped", "paused":
		return true
	default:
		return false
	}
}

type workflowStatus struct {
	ID              string `json:"id"`
	Task            string `json:"task"`
	Status          string `json:"status"`
	PhasesTotal     int    `json:"phases_total"`
	PhasesCompleted int    `json:"phases_completed"`
	AgentsTotal     int    `json:"agents_total"`
	AgentsRunning   int    `json:"agents_running"`
	AgentsCompleted int    `json:"agents_completed"`
	AgentsFailed    int    `json:"agents_failed"`
	AgentsStopped   int    `json:"agents_stopped"`
	AgentsPaused    int    `json:"agents_paused"`
	UpdatedAt       string `json:"updated_at"`
	CompletedAt     string `json:"completed_at,omitempty"`
	Error           string `json:"error,omitempty"`
}

type workflowInspectionReport struct {
	Status       workflowStatus        `json:"status"`
	ScriptPath   string                `json:"script_path"`
	Result       string                `json:"result,omitempty"`
	Phases       []workflow.Phase      `json:"phases"`
	Agents       []workflow.Agent      `json:"agents"`
	Patches      []string              `json:"patches"`
	FailedAgents []workflow.Agent      `json:"failed_agents"`
	ByPhase      map[string]phaseStats `json:"by_phase"`
}

type phaseStats struct {
	AgentsCompleted int `json:"agents_completed"`
	AgentsRunning   int `json:"agents_running"`
	AgentsFailed    int `json:"agents_failed"`
	AgentsStopped   int `json:"agents_stopped"`
	AgentsPaused    int `json:"agents_paused"`
}

func workflowStatusSummary(snapshot workflow.Snapshot) workflowStatus {
	status := workflowStatus{
		ID:          snapshot.Run.ID,
		Task:        snapshot.Run.Task,
		Status:      snapshot.Run.Status,
		UpdatedAt:   snapshot.Run.UpdatedAt,
		CompletedAt: snapshot.Run.CompletedAt,
		Error:       snapshot.Run.Error,
		PhasesTotal: len(snapshot.Phases),
		AgentsTotal: len(snapshot.Agents),
	}
	for _, phase := range snapshot.Phases {
		if phase.Status == "completed" {
			status.PhasesCompleted++
		}
	}
	for _, agent := range snapshot.Agents {
		switch agent.Status {
		case "completed":
			status.AgentsCompleted++
		case "failed":
			status.AgentsFailed++
		case "running":
			status.AgentsRunning++
		case "stopped":
			status.AgentsStopped++
		case "paused":
			status.AgentsPaused++
		}
	}
	return status
}

func workflowInspection(snapshot workflow.Snapshot) workflowInspectionReport {
	report := workflowInspectionReport{
		Status:     workflowStatusSummary(snapshot),
		ScriptPath: snapshot.Run.ScriptPath,
		Result:     snapshot.Run.Result,
		Phases:     snapshot.Phases,
		Agents:     snapshot.Agents,
		ByPhase:    map[string]phaseStats{},
	}
	for _, agent := range snapshot.Agents {
		if agent.PatchPath != "" {
			report.Patches = append(report.Patches, agent.PatchPath)
		}
		if agent.Status == "failed" {
			report.FailedAgents = append(report.FailedAgents, agent)
		}
		stats := report.ByPhase[agent.Phase]
		switch agent.Status {
		case "completed":
			stats.AgentsCompleted++
		case "failed":
			stats.AgentsFailed++
		case "running":
			stats.AgentsRunning++
		case "stopped":
			stats.AgentsStopped++
		case "paused":
			stats.AgentsPaused++
		}
		report.ByPhase[agent.Phase] = stats
	}
	return report
}

func renderWorkflowStatus(status workflowStatus) string {
	lines := []string{
		fmt.Sprintf("Workflow %s: %s", status.ID, status.Status),
		"Task: " + status.Task,
		fmt.Sprintf("Phases: %d/%d completed", status.PhasesCompleted, status.PhasesTotal),
		fmt.Sprintf("Agents: %d completed, %d running, %d failed, %d paused, %d stopped, %d total", status.AgentsCompleted, status.AgentsRunning, status.AgentsFailed, status.AgentsPaused, status.AgentsStopped, status.AgentsTotal),
		"Updated: " + status.UpdatedAt,
	}
	if status.CompletedAt != "" {
		lines = append(lines, "Completed: "+status.CompletedAt)
	}
	if status.Error != "" {
		lines = append(lines, "Error: "+status.Error)
	}
	return strings.Join(lines, "\n")
}

func renderWorkflowInspection(report workflowInspectionReport) string {
	lines := []string{
		renderWorkflowStatus(report.Status),
		"Script: " + report.ScriptPath,
	}
	if len(report.ByPhase) > 0 {
		lines = append(lines, "Phase stats:")
		for _, phase := range report.Phases {
			stats := report.ByPhase[phase.Name]
			lines = append(lines, fmt.Sprintf("- %s: %d completed, %d running, %d failed, %d paused, %d stopped", phase.Name, stats.AgentsCompleted, stats.AgentsRunning, stats.AgentsFailed, stats.AgentsPaused, stats.AgentsStopped))
		}
	}
	if len(report.Patches) > 0 {
		lines = append(lines, "Patches:")
		for _, patch := range report.Patches {
			lines = append(lines, "- "+patch)
		}
	}
	if len(report.FailedAgents) > 0 {
		lines = append(lines, "Failed agents:")
		for _, agent := range report.FailedAgents {
			label := firstNonEmpty(agent.Label, agent.ID)
			lines = append(lines, fmt.Sprintf("- %s phase=%s error=%s", label, agent.Phase, agent.Error))
		}
	}
	if report.Result != "" {
		lines = append(lines, "Result recorded: yes")
	}
	return strings.Join(lines, "\n")
}

func renderWorkflowTemplate(tmpl workflow.TemplateInfo) string {
	lines := []string{
		fmt.Sprintf("Workflow template %s [%s]", tmpl.Name, tmpl.Style),
		tmpl.Description,
		"Phases: " + strings.Join(tmpl.Phases, ", "),
	}
	if len(tmpl.Aliases) > 0 {
		lines = append(lines, "Aliases: "+strings.Join(tmpl.Aliases, ", "))
	}
	if tmpl.RequiresTestCommand {
		lines = append(lines, "Requires: --test-command")
	}
	if tmpl.Example != "" {
		lines = append(lines, "Example: "+tmpl.Example)
	}
	return strings.Join(lines, "\n")
}

func renderWorkflowPreflight(report analysis.WorkflowPreflightReport) string {
	lines := []string{
		"Workflow preflight",
		"Task: " + report.Task,
		"Summary: " + report.Summary,
	}
	if len(report.ScopePaths) > 0 {
		lines = append(lines, "Scope:")
		for _, path := range report.ScopePaths {
			lines = append(lines, "- "+path)
		}
	}
	if len(report.RiskSummary) > 0 {
		lines = append(lines, "Risk:")
		for _, risk := range report.RiskSummary {
			lines = append(lines, "- "+risk)
		}
	}
	if len(report.FilesToInspect) > 0 {
		lines = append(lines, "Inspect:")
		for _, path := range report.FilesToInspect {
			lines = append(lines, "- "+path)
		}
	}
	if len(report.TestCommands) > 0 {
		lines = append(lines, "Verification:")
		for _, command := range report.TestCommands {
			lines = append(lines, "- "+command)
		}
	}
	if len(report.AgentInstructions) > 0 {
		lines = append(lines, "Agent instructions:")
		for _, item := range report.AgentInstructions {
			lines = append(lines, "- "+item)
		}
	}
	if len(report.NextActions) > 0 {
		lines = append(lines, "Next actions:")
		for _, item := range report.NextActions {
			lines = append(lines, "- "+item)
		}
	}
	return strings.Join(lines, "\n")
}

func renderWorkflowTrigger(trigger workflow.Trigger) string {
	lines := []string{
		fmt.Sprintf("Workflow trigger %s [%s]", trigger.Name, trigger.Kind),
		"Enabled: " + fmt.Sprintf("%t", trigger.Enabled),
		"Task: " + trigger.Task,
		"CWD: " + trigger.CWD,
	}
	if trigger.WorkflowName != "" {
		lines = append(lines, "Workflow: "+trigger.WorkflowName)
	}
	if trigger.ScriptPath != "" {
		lines = append(lines, "Script: "+trigger.ScriptPath)
	}
	if trigger.ArgsJSON != "" {
		lines = append(lines, "Args: "+trigger.ArgsJSON)
	}
	if trigger.LastRunID != "" {
		lines = append(lines, "Last run: "+trigger.LastRunID)
	}
	if trigger.LastRanAt != "" {
		lines = append(lines, "Last ran: "+trigger.LastRanAt)
	}
	return strings.Join(lines, "\n")
}

func renderWorkflowFleetStatus(status workflowFleetStatus) string {
	lines := []string{
		"Workflow fleet status",
		fmt.Sprintf("Runs: %d", status.RunsTotal),
		fmt.Sprintf("Triggers: %d enabled / %d total", status.EnabledTriggers, status.TriggersTotal),
		fmt.Sprintf("Agents: %d running, %d paused, %d failed", status.RunningAgents, status.PausedAgents, status.FailedAgents),
	}
	if len(status.RunsByStatus) > 0 {
		lines = append(lines, "Runs by status:")
		for _, key := range []string{"queued", "running", "paused", "completed", "failed", "stopped"} {
			if count := status.RunsByStatus[key]; count > 0 {
				lines = append(lines, fmt.Sprintf("- %s: %d", key, count))
			}
		}
	}
	if len(status.ActiveRuns) > 0 {
		lines = append(lines, "Active runs:")
		for _, run := range status.ActiveRuns {
			lines = append(lines, fmt.Sprintf("- %s %s %s", run.ID, run.Status, run.Task))
		}
	}
	return strings.Join(lines, "\n")
}

func renderWorkflowReport(report workflow.Report) string {
	lines := []string{
		fmt.Sprintf("Workflow report %s: %s", report.ID, report.Status),
		"Task: " + report.Task,
		"Summary: " + report.Summary,
	}
	if len(report.Findings) > 0 {
		lines = append(lines, "Findings:")
		for _, finding := range report.Findings {
			lines = append(lines, "- "+finding)
		}
	}
	if len(report.Risks) > 0 {
		lines = append(lines, "Risks:")
		for _, risk := range report.Risks {
			lines = append(lines, "- "+risk)
		}
	}
	if len(report.NextSteps) > 0 {
		lines = append(lines, "Next steps:")
		for _, next := range report.NextSteps {
			lines = append(lines, "- "+next)
		}
	}
	if len(report.Patches) > 0 {
		lines = append(lines, "Patches:")
		for _, patch := range report.Patches {
			lines = append(lines, "- "+patch)
		}
	}
	if len(report.Agents) > 0 {
		lines = append(lines, "Agents:")
		for _, agent := range report.Agents {
			provider := firstNonEmpty(agent.Provider, "codex")
			lines = append(lines, fmt.Sprintf("- %s %s provider=%s mode=%s phase=%s", agent.Label, agent.Status, provider, agent.Mode, agent.Phase))
			if agent.Summary != "" {
				lines = append(lines, "  summary: "+agent.Summary)
			}
			if agent.Error != "" {
				lines = append(lines, "  error: "+agent.Error)
			}
		}
	}
	if report.Error != "" {
		lines = append(lines, "Error: "+report.Error)
	}
	return strings.Join(lines, "\n")
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
  pallium workflow preflight "task" [--scope path] [--cwd repo-path] [--json]
  pallium workflow generate "task" [--style review|test-fix|research] [--output path.js] [--save name] [--json]
  pallium workflow validate <path.js> [--json]
  pallium workflow tools list [--kind control|agent|verification|pallium] [--json]
  pallium workflow template list [--json]
  pallium workflow template show <name> [--json]
  pallium workflow trigger add <name> "task" [--workflow name|--script path] [--cwd repo-path] [--json]
  pallium workflow trigger list [--json]
  pallium workflow trigger show <name> [--json]
  pallium workflow trigger run <name> [--background] [--json]
  pallium workflow fleet status [--limit n] [--json]
  pallium workflow gate list <run-id> [--json]
  pallium workflow gate approve <run-id> <name> [--json]
  pallium workflow serve [--addr 127.0.0.1:8765]
  pallium workflow run "task" [--script path.js] [--workflow name] [--background] [--max-concurrent-agents 16] [--json]
  pallium workflow run /saved-name "task input"
  pallium workflow list [--limit n] [--json]
  pallium workflow status <run-id> [--json]
  pallium workflow inspect <run-id> [--json]
  pallium workflow show <run-id> [--json]
  pallium workflow read <run-id> [--json]
  pallium workflow report <run-id> [--json]
  pallium workflow watch <run-id>
  pallium workflow pause <run-id> [--json]
  pallium workflow resume <run-id> [--background] [--json]
  pallium workflow stop <run-id> [--json]
  pallium workflow save <run-id> --name name [--user] [--json]
  pallium workflow apply <run-id> [--json]
  pallium workflow revert <run-id> [--json]`)
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
