package workflow

import "fmt"

type ToolInfo struct {
	Name        string   `json:"name"`
	Signature   string   `json:"signature"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Returns     string   `json:"returns"`
	Example     string   `json:"example"`
	Notes       []string `json:"notes,omitempty"`
}

type TemplateInfo struct {
	Name                string   `json:"name"`
	Aliases             []string `json:"aliases,omitempty"`
	Style               string   `json:"style"`
	Description         string   `json:"description"`
	Phases              []string `json:"phases"`
	RequiresTestCommand bool     `json:"requires_test_command,omitempty"`
	Example             string   `json:"example"`
}

type PackInfo struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	InstallsAs  string   `json:"installs_as"`
	Phases      []string `json:"phases"`
}

func WorkflowTools() []ToolInfo {
	return []ToolInfo{
		{
			Name:        "phase",
			Signature:   `phase(name, callback?)`,
			Kind:        "control",
			Description: "Marks the active workflow phase and records progress.",
			Returns:     "Callback result when a callback is supplied, otherwise void.",
			Example:     `await phase("inspect", async () => agent("inspect auth", { label: "auth" }))`,
			Notes:       []string{"Async callback phases remain open until the callback promise settles."},
		},
		{
			Name:        "agent",
			Signature:   `await agent(prompt, options)`,
			Kind:        "agent",
			Description: "Spawns one provider-backed subagent with read-only, test, or edit behavior.",
			Returns:     "Parsed JSON when possible, otherwise text.",
			Example:     `await agent("Review auth routes for missing checks", { label: "auth-review", mode: "read-only" })`,
			Notes: []string{
				"Edit agents run in isolated worktrees and auto-apply patches after successful workflow completion.",
				"Completed matching agents are reused when the same run id is relaunched.",
				"Use provider: \"codex\" for the native worker or configure another provider with PALLIUM_WORKFLOW_PROVIDER_<NAME>_COMMAND.",
				"Configured provider commands receive PALLIUM_WORKFLOW_PROMPT_FILE, PALLIUM_WORKFLOW_OUTPUT_FILE, PALLIUM_WORKFLOW_SCHEMA_FILE, and workflow metadata environment variables.",
				"Use repo: \"/path/to/repo\" to run a worker against another checkout.",
			},
		},
		{
			Name:        "parallel",
			Signature:   `await parallel(items, item => agent(...))`,
			Kind:        "control",
			Description: "Runs independent agent callbacks concurrently, capped by --max-concurrent-agents.",
			Returns:     "Array of callback results in item order.",
			Example:     `await parallel(files, file => agent("Review " + file, { label: file, mode: "read-only" }))`,
			Notes:       []string{"Default concurrency is 16 agents. Total agents per run default to 1000."},
		},
		{
			Name:        "pipeline",
			Signature:   `await pipeline(items, item => agent(...))`,
			Kind:        "control",
			Description: "Runs a stage over items with concurrent per-item agents and returns ordered results.",
			Returns:     "Array of stage results in item order.",
			Example:     `await pipeline(angles, angle => agent("Inspect from " + angle, { label: angle }))`,
		},
		{
			Name:        "check",
			Signature:   `await check(command, options)`,
			Kind:        "verification",
			Description: "Spawns a test-mode subagent to run a command, parse failures, and return objective status.",
			Returns:     `{ ok, command, summary, output_tail, failures }`,
			Example:     `const result = await check("go test ./...", { label: "baseline-check" })`,
			Notes:       []string{"The orchestration script does not run shell commands directly. The test subagent does."},
		},
		{
			Name:        "verify.untilGreen",
			Signature:   `await verify.untilGreen(command, { maxRounds, label })`,
			Kind:        "verification",
			Description: "Runs an objective check/fix loop until the command passes, max rounds are reached, or failures stop changing.",
			Returns:     `{ ok, command, rounds, stalled }`,
			Example:     `const result = await verify.untilGreen("go test ./...", { maxRounds: 3, label: "tests" })`,
			Notes:       []string{"Fix workers run in edit/worktree mode and do not weaken or skip tests."},
		},
		{
			Name:        "workflow",
			Signature:   `await workflow(name, args)`,
			Kind:        "control",
			Description: "Runs one saved workflow by name from .pallium/workflows, .claude/workflows, or user workflow folders.",
			Returns:     "Parsed JSON result when possible, otherwise text.",
			Example:     `const review = await workflow("review-branch", { base: "origin/main" })`,
			Notes:       []string{"Composition is intentionally limited to one nested level."},
		},
		{
			Name:        "gate",
			Signature:   `await gate(name, message?)`,
			Kind:        "control",
			Description: "Pauses the workflow until a human approves the named gate.",
			Returns:     "Approved gate JSON after resume.",
			Example:     `await gate("approve-patches", "Review generated patches before applying")`,
			Notes:       []string{"Approve with pallium workflow gate approve <run-id> <name>, then resume the run."},
		},
		{
			Name:        "pallium.verify",
			Signature:   `await pallium.verify("fast")`,
			Kind:        "pallium",
			Description: "Runs Pallium verification as a workflow primitive.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.verify("fast")`,
		},
		{
			Name:        "pallium.review",
			Signature:   `await pallium.review(baseRef)`,
			Kind:        "pallium",
			Description: "Runs Pallium drift/review context scoped to the workflow repo.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.review("origin/main")`,
		},
		{
			Name:        "pallium.handoff",
			Signature:   `await pallium.handoff(baseRef)`,
			Kind:        "pallium",
			Description: "Builds a Pallium handoff with workflow verification context.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.handoff("origin/main")`,
		},
		{
			Name:        "pallium.changedNow",
			Signature:   `await pallium.changedNow()`,
			Kind:        "pallium",
			Description: "Returns current changed-file context for the repo.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `const changed = await pallium.changedNow()`,
		},
		{
			Name:        "pallium.preflight",
			Signature:   `await pallium.preflight(task, ...scopePaths)`,
			Kind:        "pallium",
			Description: "Builds repo-native workflow scope, risk, inspection, and verification guidance before spawning workers.",
			Returns:     "Workflow preflight JSON with scope_paths, safe reports, test_commands, and agent_instructions.",
			Example:     `const preflight = await pallium.preflight("fix auth tests", "cmd/workflow.go")`,
		},
		{
			Name:        "pallium.decisions.record",
			Signature:   `await pallium.decisions.record(title, body, ...tags)`,
			Kind:        "pallium",
			Description: "Records a durable workflow decision for future runs and reports.",
			Returns:     "Recorded decision JSON.",
			Example:     `await pallium.decisions.record("Use worktrees", "Parallel edit agents must isolate patches.", "workflow")`,
		},
		{
			Name:        "pallium.decisions.search",
			Signature:   `await pallium.decisions.search(query, limit?)`,
			Kind:        "pallium",
			Description: "Searches prior workflow decisions stored in the local workflow database.",
			Returns:     "Array of matching decisions.",
			Example:     `const prior = await pallium.decisions.search("auth", 5)`,
		},
		{
			Name:        "pallium.explain",
			Signature:   `await pallium.explain(path)`,
			Kind:        "pallium",
			Description: "Retrieves Pallium explanation/context for a path.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.explain("cmd/workflow.go")`,
		},
		{
			Name:        "pallium.safe",
			Signature:   `await pallium.safe(path)`,
			Kind:        "pallium",
			Description: "Retrieves risk/safety context before editing a path.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.safe("internal/workflow/runtime.go")`,
		},
		{
			Name:        "pallium.plan",
			Signature:   `await pallium.plan(path)`,
			Kind:        "pallium",
			Description: "Retrieves Pallium edit-plan context for a path.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.plan("internal/workflow/runtime.go")`,
		},
		{
			Name:        "pallium.task.start",
			Signature:   `await pallium.task.start(goal, ...scopes)`,
			Kind:        "pallium",
			Description: "Starts workflow-scoped task context for drift detection and handoffs.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.task.start("fix auth tests", "cmd", "internal/auth")`,
		},
		{
			Name:        "pallium.task.show",
			Signature:   `await pallium.task.show()`,
			Kind:        "pallium",
			Description: "Shows the current Pallium task context.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.task.show()`,
		},
		{
			Name:        "pallium.task.clear",
			Signature:   `await pallium.task.clear()`,
			Kind:        "pallium",
			Description: "Clears the current Pallium task context.",
			Returns:     "Pallium CLI JSON when available.",
			Example:     `await pallium.task.clear()`,
		},
	}
}

func WorkflowTemplates() []TemplateInfo {
	return []TemplateInfo{
		{
			Name:        "review",
			Style:       "review",
			Description: "Parallel repo-grounded review with correctness, regression-risk, and verification-plan workers.",
			Phases:      []string{"scope", "inspect", "synthesize"},
			Example:     `pallium workflow generate "review this branch" --style review --output review.workflow.js`,
		},
		{
			Name:                "test-fix",
			Aliases:             []string{"fix-until-green"},
			Style:               "test-fix",
			Description:         "Claude-style implement, verify, fix loop using check() as the objective oracle.",
			Phases:              []string{"scope", "baseline", "fix-loop", "finalize"},
			RequiresTestCommand: true,
			Example:             `pallium workflow generate "fix tests until green" --style test-fix --test-command "go test ./..." --output fix.workflow.js`,
		},
		{
			Name:        "research",
			Style:       "research",
			Description: "Parallel research, cross-check, and synthesis workflow.",
			Phases:      []string{"research", "verify", "synthesize"},
			Example:     `pallium workflow generate "research migration risk" --style research --output research.workflow.js`,
		},
	}
}

func WorkflowTemplate(name string) (TemplateInfo, bool) {
	for _, tmpl := range WorkflowTemplates() {
		if tmpl.Name == name || tmpl.Style == name {
			return tmpl, true
		}
		for _, alias := range tmpl.Aliases {
			if alias == name {
				return tmpl, true
			}
		}
	}
	return TemplateInfo{}, false
}

func TemplateNames() []string {
	templates := WorkflowTemplates()
	names := make([]string, 0, len(templates))
	for _, tmpl := range templates {
		names = append(names, tmpl.Name)
	}
	return names
}

func WorkflowPacks() []PackInfo {
	return []PackInfo{
		{
			Name:        "security-audit",
			Version:     "1.0.0",
			Description: "Repo-grounded security audit with parallel auth, data, dependency, and secret-review workers.",
			Tags:        []string{"security", "review", "parallel"},
			InstallsAs:  "security-audit",
			Phases:      []string{"scope", "audit", "synthesize"},
		},
		{
			Name:        "migration-assistant",
			Version:     "1.0.0",
			Description: "Migration planner that maps blast radius, ports work in isolated edit agents, then runs a verification loop.",
			Tags:        []string{"migration", "edit", "verification"},
			InstallsAs:  "migration-assistant",
			Phases:      []string{"scope", "plan", "implement", "verify"},
		},
		{
			Name:        "test-gap-finder",
			Version:     "1.0.0",
			Description: "Finds missing or weak tests from changed files, risk context, and parallel coverage-review workers.",
			Tags:        []string{"tests", "coverage", "review"},
			InstallsAs:  "test-gap-finder",
			Phases:      []string{"scope", "inspect", "report"},
		},
	}
}

func WorkflowPack(name string) (PackInfo, bool) {
	for _, pack := range WorkflowPacks() {
		if pack.Name == name || pack.InstallsAs == name {
			return pack, true
		}
	}
	return PackInfo{}, false
}

func WorkflowPackScript(name string) (string, error) {
	pack, ok := WorkflowPack(name)
	if !ok {
		return "", fmt.Errorf("unknown workflow pack %q", name)
	}
	switch pack.Name {
	case "security-audit":
		return `export const meta = {
  name: "security-audit",
  description: "Repo-grounded security audit workflow",
  phases: ["scope", "audit", "synthesize"]
};

const task = args && args.task ? args.task : "Audit this repo for security risk";

phase("scope");
const preflight = await pallium.preflight(task);
const changed = await pallium.changedNow();

phase("audit");
const angles = ["auth-and-access-control", "data-and-secrets", "dependency-and-supply-chain", "test-and-regression-risk"];
const reports = await parallel(angles, angle =>
  agent("Security audit angle: " + angle + "\nTask: " + task + "\nPreflight: " + JSON.stringify(preflight) + "\nChanged files: " + JSON.stringify(changed) + "\nReturn JSON with findings, risks, evidence, next_steps.", {
    label: "security-" + angle,
    mode: "read-only",
    schema: {
      type: "object",
      properties: {
        findings: { type: "array", items: { type: "string" } },
        risks: { type: "array", items: { type: "string" } },
        evidence: { type: "array", items: { type: "string" } },
        next_steps: { type: "array", items: { type: "string" } }
      },
      required: ["findings", "risks", "evidence", "next_steps"]
    }
  })
);

phase("synthesize");
const summary = await agent("Synthesize security audit reports into ranked findings with exact evidence and smallest safe next actions. Reports: " + JSON.stringify(reports), {
  label: "security-synthesis",
  mode: "read-only"
});
return { task, preflight, changed, reports, summary };
`, nil
	case "migration-assistant":
		return `export const meta = {
  name: "migration-assistant",
  description: "Repo-grounded migration workflow",
  phases: ["scope", "plan", "implement", "verify"]
};

const task = args && args.task ? args.task : "Plan and execute the migration safely";
const testCommand = args && args.testCommand ? args.testCommand : "pallium verify fast";

phase("scope");
await pallium.task.start(task);
const preflight = await pallium.preflight(task);
await pallium.decisions.record("Migration scope", "Use Pallium preflight and isolated edit worktrees before applying patches.", "migration", "workflow");

phase("plan");
const plan = await agent("Create a migration plan from this Pallium preflight context. Task: " + task + "\nPreflight: " + JSON.stringify(preflight), {
  label: "migration-plan",
  mode: "read-only"
});

phase("implement");
const workers = ["mechanical-changes", "tests-and-fixtures", "docs-and-handoff"];
const edits = await parallel(workers, worker =>
  agent("Implement migration worker " + worker + ". Task: " + task + "\nPlan: " + JSON.stringify(plan) + "\nKeep the change focused and safe.", {
    label: "migration-" + worker,
    mode: worker === "docs-and-handoff" ? "read-only" : "edit",
    isolation: worker === "docs-and-handoff" ? "" : "worktree"
  })
);

phase("verify");
const verification = await verify.untilGreen(testCommand, { label: "migration-verify", maxRounds: 3 });
const handoff = await pallium.handoff("HEAD~1");
return { task, preflight, plan, edits, verification, handoff };
`, nil
	case "test-gap-finder":
		return `export const meta = {
  name: "test-gap-finder",
  description: "Repo-grounded test gap finder",
  phases: ["scope", "inspect", "report"]
};

const task = args && args.task ? args.task : "Find missing or weak tests";

phase("scope");
const preflight = await pallium.preflight(task);
const changed = await pallium.changedNow();

phase("inspect");
const angles = ["changed-files", "risky-neighbors", "missing-assertions", "verification-plan"];
const reports = await parallel(angles, angle =>
  agent("Find test gaps from angle " + angle + ". Task: " + task + "\nPreflight: " + JSON.stringify(preflight) + "\nChanged: " + JSON.stringify(changed) + "\nReturn JSON with gaps, suggested_tests, confidence.", {
    label: "test-gap-" + angle,
    mode: "read-only"
  })
);

phase("report");
const synthesis = await agent("Rank the test gaps by regression risk and give exact test files or commands to add. Reports: " + JSON.stringify(reports), {
  label: "test-gap-synthesis",
  mode: "read-only"
});
return { task, preflight, changed, reports, synthesis };
`, nil
	default:
		return "", fmt.Errorf("unknown workflow pack %q", name)
	}
}

func UnknownTemplateError(name string) error {
	return fmt.Errorf("unknown workflow template %q; available templates: %v", name, TemplateNames())
}
