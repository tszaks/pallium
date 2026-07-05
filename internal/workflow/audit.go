package workflow

// VersionRequirement documents one capability that must be present for a
// workflow release version to be considered complete.
type VersionRequirement struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// VersionRequirements returns the canonical v1-v7 completion checklist.
func VersionRequirements() []VersionRequirement {
	return []VersionRequirement{
		{ID: "v1-runner", Version: "v1", Name: "Core runner", Description: "run, list, show, read, watch, stop, pause, resume with SQLite persistence"},
		{ID: "v1-parallel", Version: "v1", Name: "Parallel workers", Description: "parallel() and pipeline() fan out concurrently with a default cap of 16"},
		{ID: "v1-edits", Version: "v1", Name: "Safe edits", Description: "edit workers use isolated worktrees and auto-apply patches on successful completion"},
		{ID: "v1-discovery", Version: "v1", Name: "Discovery", Description: "workflow tools list and template list/show for agent discovery"},
		{ID: "v2-validate", Version: "v2", Name: "Script validation", Description: "workflow validate compiles Claude-shaped async scripts before execution"},
		{ID: "v2-report", Version: "v2", Name: "Structured reports", Description: "workflow report extracts findings, risks, next steps, patches, and per-agent summaries"},
		{ID: "v2-generate", Version: "v2", Name: "Workflow generation", Description: "workflow generate supports deterministic templates and optional --llm output validation"},
		{ID: "v2-compose", Version: "v2", Name: "Composition", Description: "await workflow(name, args) composes one saved workflow from .pallium/workflows"},
		{ID: "v2-budget", Version: "v2", Name: "Budget controls", Description: "--max-budget-usd and PALLIUM_WORKFLOW_AGENT_COST_USD enforce per-run spend limits"},
		{ID: "v2-sessions", Version: "v2", Name: "Session cross-link", Description: "workflow report surfaces owned_session_id, related sessions, and run decisions"},
		{ID: "v3-preflight", Version: "v3", Name: "Repo preflight", Description: "workflow preflight and pallium.preflight() scope risk, files, and verification before workers fan out"},
		{ID: "v3-until-green", Version: "v3", Name: "Objective loops", Description: "verify.untilGreen() owns check, fix, and re-check loops inside workflow scripts"},
		{ID: "v3-decisions", Version: "v3", Name: "Decision memory", Description: "pallium.decisions.record/search persists durable choices across workflow runs"},
		{ID: "v3-multirepo", Version: "v3", Name: "Multi-repo agents", Description: "agent(..., { repo }) targets another checkout while keeping workflow state centralized"},
		{ID: "v3-revert", Version: "v3", Name: "Patch revert", Description: "workflow revert rolls back auto-applied workflow patches"},
		{ID: "v4-triggers", Version: "v4", Name: "Triggers", Description: "workflow trigger add/list/show/run stores reusable automations in SQLite"},
		{ID: "v4-on-changed", Version: "v4", Name: "On-changed triggers", Description: "trigger --kind on-changed skips until repo HEAD or dirty state changes"},
		{ID: "v4-watch", Version: "v4", Name: "Trigger watch", Description: "workflow trigger watch --once polls enabled triggers for autonomous execution"},
		{ID: "v4-gates", Version: "v4", Name: "Approval gates", Description: "gate() pauses until pallium workflow gate approve and resume continue the run"},
		{ID: "v4-library", Version: "v4", Name: "Workflow library", Description: "workflow library list/show/install exposes reusable packs such as security-audit"},
		{ID: "v5-providers", Version: "v5", Name: "Providers", Description: "agent provider options route through codex or PALLIUM_WORKFLOW_PROVIDER_<NAME>_COMMAND"},
		{ID: "v5-fleet", Version: "v5", Name: "Fleet status", Description: "workflow fleet status summarizes active runs, triggers, and worker health"},
		{ID: "v5-fleet-limit", Version: "v5", Name: "Fleet guard", Description: "--max-active-runs and PALLIUM_WORKFLOW_MAX_ACTIVE_RUNS cap concurrent active workflow runs"},
		{ID: "v5-coordinator", Version: "v5", Name: "Coordinator replan", Description: "coordinator.replan() adapts remaining work from the current run snapshot"},
		{ID: "v6-api", Version: "v6", Name: "HTTP API", Description: "workflow serve exposes health, fleet, analytics, library, run, and snapshot endpoints"},
		{ID: "v6-mcp", Version: "v6", Name: "MCP surface", Description: "workflow mcp exposes stdio JSON-RPC tools for run, status, fleet, analytics, and library packs"},
		{ID: "v6-sdk", Version: "v6", Name: "Go SDK", Description: "pkg/workflowclient wraps the local workflow HTTP API"},
		{ID: "v6-analytics", Version: "v6", Name: "Analytics", Description: "workflow analytics summarizes completion rate, provider mix, patches, and estimated cost"},
		{ID: "v7-acceptance", Version: "v7", Name: "Acceptance gate", Description: "scripts/workflow-acceptance.sh proves v1-v7 end to end through the installed CLI"},
		{ID: "v7-audit", Version: "v7", Name: "Audit command", Description: "workflow audit documents the v1-v7 requirement checklist and can run the acceptance gate"},
	}
}