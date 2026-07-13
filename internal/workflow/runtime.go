package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/google/uuid"
	"github.com/tszaks/pallium/internal/gitlog"
)

var (
	ErrWorkflowStopped           = errors.New("workflow stopped")
	ErrWorkflowPaused            = errors.New("workflow paused")
	ErrWorkflowBudgetExhausted   = errors.New("workflow budget exhausted")
	ErrWorkflowMaxAgentsExceeded = errors.New("workflow exceeded max agents")
	// ErrWorkflowRepoLockContended means another run already holds the
	// advisory edit lock on a repo this run needs to edit (see
	// acquireRepoLock). Distinguished from an ordinary per-item provider
	// failure so parallel()/pipeline() halt the run with one clear message
	// instead of silently dropping every contended agent call to null, which
	// would look like N unrelated failures rather than one root cause.
	ErrWorkflowRepoLockContended = errors.New("workflow repo edit lock contended")
)

type Runner struct {
	Store               *Store
	Run                 Run
	MaxAgents           int
	MaxBudgetUSD        string
	CodexBinary         string
	MaxConcurrentAgents int
	PalliumBinary       string
	AgentTimeoutSeconds int

	mu         sync.Mutex
	failuresMu sync.Mutex
	// teamApplyMu serializes `git apply` against a team's repo root across
	// this Runner's concurrently scheduled member turns (see RunTeamTurn in
	// team_runtime.go). Regular (non-team) workflow patch application is
	// unaffected — it is already sequential by construction.
	teamApplyMu    sync.Mutex
	currentPhase   string
	agentCount     int
	budgetLimit    float64
	budgetSpent    float64
	agentCostUSD   float64
	workflowDepth  int
	stubIndex      int
	agentCallIndex int
	pipelineIndex  int
	capture        *parallelCapture
	scriptHash     string
	argsHash       string
	failures       []RunFailure
	fatalErr       error

	// lockedRepos memoizes which canonical repo roots this run already holds
	// the advisory edit lock for (see acquireRepoLock), so repeated
	// edit-intent agents in the same run skip the DB round trip after the
	// first.
	lockMu      sync.Mutex
	lockedRepos map[string]bool

	// staging holds this run's lazily-created staging worktree per repo
	// root (see mergeIntoStaging), keyed by the same repoRoot value
	// runAgentCommand resolves (firstNonEmpty(agent.Repo, r.Run.CWD)) — not
	// the canonical root acquireRepoLock uses, since staging is a pure
	// in-process mirror with no cross-process identity to reconcile.
	stagingMu sync.Mutex
	staging   map[string]*stagingWorktree
}

type AgentOptions struct {
	Label          string         `json:"label,omitempty"`
	Provider       string         `json:"provider,omitempty"`
	Repo           string         `json:"repo,omitempty"`
	Mode           string         `json:"mode,omitempty"`
	Isolation      string         `json:"isolation,omitempty"`
	Schema         map[string]any `json:"schema,omitempty"`
	Model          string         `json:"model,omitempty"`
	TimeoutSeconds *int           `json:"timeout_seconds,omitempty"`
	// Network is the agent's opt-in request for network egress. It is only
	// honored when the run was launched with --allow-network (the operator
	// ceiling); otherwise the agent runs sandboxed. Default false.
	Network bool `json:"network,omitempty"`
}

type CheckOptions struct {
	Label    string         `json:"label,omitempty"`
	Provider string         `json:"provider,omitempty"`
	Model    string         `json:"model,omitempty"`
	Schema   map[string]any `json:"schema,omitempty"`
}

type GateOptions struct {
	Label      string `json:"label,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Criteria   string `json:"criteria,omitempty"`
	FailOnDeny *bool  `json:"fail_on_deny,omitempty"`
}

type PolicyFinding struct {
	Kind    string `json:"kind"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type parallelAgentCall struct {
	Prompt    string
	Opts      AgentOptions
	CallIndex int
}

type parallelCapture struct {
	Calls []parallelAgentCall
}

const parallelAgentMarkerKey = "__pallium_parallel_agent__"
const maxWorkflowCollectionItems = 4096
const pipelineCallIndexBase = 1_000_000_000_000
const pipelineIndexStride = 10_000_000_000
const pipelineStageStride = 10_000_000
const pipelineItemStride = 1_000

func (r *Runner) Execute(ctx context.Context, script string, args any) (string, error) {
	if r.Store == nil {
		return "", fmt.Errorf("workflow store is required")
	}
	if r.Run.ID == "" {
		return "", fmt.Errorf("workflow run is required")
	}
	// Unconditional regardless of how Execute returns: a run-scoped staging
	// worktree or a held repo lock must never outlive the Execute call that
	// created them. removeStagingWorktrees runs first (defers are LIFO, and
	// it is registered after releaseRepoLocks) so this run's own staging
	// artifacts are fully torn down before the repo lock is released for a
	// waiting run to take over.
	defer r.releaseRepoLocks()
	defer r.removeStagingWorktrees()
	if r.MaxAgents <= 0 && r.Run.MaxAgents > 0 {
		r.MaxAgents = r.Run.MaxAgents
	}
	if r.MaxAgents <= 0 {
		r.MaxAgents = 1000
	}
	if r.MaxConcurrentAgents <= 0 {
		r.MaxConcurrentAgents = 16
	}
	if r.CodexBinary == "" {
		r.CodexBinary = "codex"
	}
	if r.PalliumBinary == "" {
		r.PalliumBinary = "pallium"
	}
	r.agentCostUSD = workflowAgentCostUSD()
	maxBudgetUSD := strings.TrimSpace(firstNonEmpty(r.MaxBudgetUSD, r.Run.MaxBudgetUSD))
	if maxBudgetUSD != "" {
		limit, err := strconv.ParseFloat(maxBudgetUSD, 64)
		if err != nil || limit < 0 {
			return "", fmt.Errorf("invalid max budget usd %q", maxBudgetUSD)
		}
		r.budgetLimit = limit
	}
	usedAgents, usedBudget, err := r.Store.AgentUsage(r.Run.ID)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.agentCount = usedAgents
	r.budgetSpent = usedBudget
	r.agentCallIndex = 0
	r.pipelineIndex = 0
	r.failures = nil
	r.fatalErr = nil
	r.mu.Unlock()
	if err := r.Store.SetRunFailures(r.Run.ID, nil); err != nil {
		return "", err
	}
	r.checkScriptChanged(script)
	if err := r.Store.SetRunStatus(r.Run.ID, "running", "", ""); err != nil {
		return "", err
	}
	if err := r.Store.ClaimRunOwnership(r.Run.ID); err != nil {
		return "", err
	}
	return r.executeScript(ctx, script, args, true)
}

// checkScriptChanged records the script hash on the first execution and, on
// resume, warns when the script differs from the original run. The hash stays
// out of the resume cache key on purpose so an unchanged prefix replays from
// cache after a tail edit.
func (r *Runner) checkScriptChanged(script string) {
	stored, err := r.Store.Run(r.Run.ID)
	if err != nil {
		return
	}
	currentHash := StableHash(script)
	if stored.ScriptHash == "" {
		_ = r.Store.SetRunScriptState(r.Run.ID, currentHash, false)
		return
	}
	changed := stored.ScriptHash != currentHash
	if changed {
		fmt.Fprintf(os.Stderr, "[workflow:%s] script changed since original run; unchanged prefix will replay from cache\n", r.Run.ID)
	}
	_ = r.Store.SetRunScriptState(r.Run.ID, stored.ScriptHash, changed)
}

func (r *Runner) executeScript(ctx context.Context, script string, args any, topLevel bool) (string, error) {
	r.mu.Lock()
	previousScriptHash := r.scriptHash
	previousArgsHash := r.argsHash
	r.scriptHash = StableHash(script)
	r.argsHash = StableHash(args)
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.scriptHash = previousScriptHash
		r.argsHash = previousArgsHash
		r.mu.Unlock()
	}()
	if err := r.ensureNotStopped(ctx); err != nil {
		return "", err
	}

	body := stripMeta(script)
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	installDeterministicWorkflowGuards(vm)
	if err := vm.Set("args", args); err != nil {
		return "", err
	}
	if err := vm.Set("log", func(message ...any) goja.Value {
		parts := make([]string, 0, len(message))
		for _, part := range message {
			parts = append(parts, fmt.Sprint(part))
		}
		fmt.Fprintf(os.Stderr, "[workflow:%s] %s\n", r.Run.ID, strings.Join(parts, " "))
		return goja.Undefined()
	}); err != nil {
		return "", err
	}
	budgetTotal := any(nil)
	if r.budgetLimit > 0 {
		budgetTotal = r.budgetLimit
	}
	if err := vm.Set("budget", map[string]any{
		"total": budgetTotal,
		"spent": func() goja.Value {
			r.mu.Lock()
			defer r.mu.Unlock()
			return vm.ToValue(r.budgetSpent)
		},
		"remaining": func() goja.Value {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.budgetLimit > 0 {
				return vm.ToValue(r.budgetLimit - r.budgetSpent)
			}
			return goja.Null()
		},
	}); err != nil {
		return "", err
	}
	if err := vm.Set("phase", r.jsPhase(vm)); err != nil {
		return "", err
	}
	if err := vm.Set("agent", r.jsAgent(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("check", r.jsCheck(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("verify", r.jsVerify(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("pallium", r.jsPallium(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("parallel", r.jsParallel(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("pipeline", r.jsPipeline(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("workflow", func(call goja.FunctionCall) goja.Value {
		name := strings.TrimSpace(call.Argument(0).String())
		if name == "" {
			panic(vm.ToValue("workflow name is required"))
		}
		var input any
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) && !goja.IsNull(call.Argument(1)) {
			input = call.Argument(1).Export()
		}
		result, err := r.executeSavedWorkflow(ctx, name, input)
		if err != nil {
			panic(vm.ToValue(r.throwable(err)))
		}
		return vm.ToValue(parseAgentOutput(result))
	}); err != nil {
		return "", err
	}
	if err := vm.Set("coordinator", r.jsCoordinator(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("gate", r.jsGate(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("team", r.jsTeam(ctx, vm)); err != nil {
		return "", err
	}

	value, err := vm.RunString("(async function(){\n" + body + "\n})()")
	r.mu.Lock()
	openPhase := r.currentPhase
	r.currentPhase = ""
	r.mu.Unlock()
	if openPhase != "" {
		_ = r.Store.FinishPhase(r.Run.ID, openPhase)
	}
	if err != nil {
		err = r.classifyRunError(err)
		if isWorkflowStoppedError(err) {
			if topLevel {
				_ = r.Store.SetRunStatus(r.Run.ID, "stopped", "", ErrWorkflowStopped.Error())
			}
			return "", ErrWorkflowStopped
		}
		if isWorkflowPausedError(err) {
			if topLevel {
				_ = r.Store.SetRunStatus(r.Run.ID, "paused", "", ErrWorkflowPaused.Error())
			}
			return "", ErrWorkflowPaused
		}
		if topLevel {
			_ = r.Store.SetRunStatus(r.Run.ID, r.topLevelFailureStatus(), "", err.Error())
		}
		return "", err
	}
	value, err = awaitPromiseValue(value)
	if err != nil {
		err = r.classifyRunError(err)
		if isWorkflowStoppedError(err) {
			if topLevel {
				_ = r.Store.SetRunStatus(r.Run.ID, "stopped", "", ErrWorkflowStopped.Error())
			}
			return "", ErrWorkflowStopped
		}
		if isWorkflowPausedError(err) {
			if topLevel {
				_ = r.Store.SetRunStatus(r.Run.ID, "paused", "", ErrWorkflowPaused.Error())
			}
			return "", ErrWorkflowPaused
		}
		if topLevel {
			_ = r.Store.SetRunStatus(r.Run.ID, r.topLevelFailureStatus(), "", err.Error())
		}
		return "", err
	}
	if err := r.ensureNotStopped(ctx); err != nil {
		if topLevel {
			_ = r.Store.SetRunStatus(r.Run.ID, interruptedStatus(err), "", interruptedMessage(err))
		}
		return "", err
	}
	if topLevel {
		if _, err := r.ApplyPatches(ctx); err != nil {
			if isWorkflowInterruptedError(err) {
				_ = r.Store.SetRunStatus(r.Run.ID, interruptedStatus(err), "", interruptedMessage(err))
				return "", err
			}
			_ = r.Store.SetRunStatus(r.Run.ID, r.topLevelFailureStatus(), "", err.Error())
			return "", err
		}
	}
	if err := r.ensureNotStopped(ctx); err != nil {
		if topLevel {
			_ = r.Store.SetRunStatus(r.Run.ID, interruptedStatus(err), "", interruptedMessage(err))
		}
		return "", err
	}
	result := value.Export()
	resultText := stringifyResult(result)
	if topLevel {
		r.warnOnDropsAtCompletion(resultText)
		if err := r.Store.SetRunStatus(r.Run.ID, "completed", resultText, ""); err != nil {
			return "", err
		}
	}
	return resultText, nil
}

// topLevelFailureStatus decides the run-level status to persist when the
// top-level script itself failed (an uncaught rejection, or patch
// application after the script otherwise completed). A run with at least
// one completed agent already produced usable, report-able work — "failed"
// would contradict that. "completed_with_failures" names the honest middle
// ground; a run that never got a single agent past "completed" is a
// genuine flat failure.
func (r *Runner) topLevelFailureStatus() string {
	agents, err := r.Store.ListAgents(r.Run.ID)
	if err != nil {
		return "failed"
	}
	for _, agent := range agents {
		if agent.Status == "completed" {
			return "completed_with_failures"
		}
	}
	return "failed"
}

// warnOnDropsAtCompletion emits a consolidated stderr warning when a run
// finishes "completed" despite dropping items to null. Individual drops
// already log at the moment they happen, but those lines scroll away; this
// summary makes the partial (or total) failure loud at the end. When the
// result is also empty the run produced nothing usable, so the warning says
// so explicitly. The run status stays "completed" on purpose: a stage drop is
// documented to null only that item and continue, and flipping to "failed"
// would wrongly punish workflows that legitimately return empty/void or that
// succeeded on their surviving items — the failures list plus this warning are
// how the drop stays visible.
func (r *Runner) warnOnDropsAtCompletion(resultText string) {
	r.mu.Lock()
	count := len(r.failures)
	r.mu.Unlock()
	if count == 0 {
		return
	}
	if isEmptyResult(resultText) {
		fmt.Fprintf(os.Stderr, "[workflow:%s] WARNING: run completed with an EMPTY result after dropping %d item(s) to null; nothing usable was produced. See the failures list for reasons.\n", r.Run.ID, count)
		return
	}
	fmt.Fprintf(os.Stderr, "[workflow:%s] WARNING: run completed but dropped %d item(s) to null. See the failures list for reasons.\n", r.Run.ID, count)
}

// isEmptyResult reports whether a stringified workflow result carries no usable
// output: empty/void, a bare null, or an empty/all-null object or array (e.g.
// {"results":[null,null]} from a pipeline whose every item dropped).
func isEmptyResult(resultText string) bool {
	trimmed := strings.TrimSpace(resultText)
	switch trimmed {
	case "", "null", "undefined", "{}", "[]", `""`:
		return true
	}
	var parsed any
	if json.Unmarshal([]byte(trimmed), &parsed) != nil {
		return false
	}
	return isEmptyValue(parsed)
}

// isEmptyValue reports whether a decoded JSON value is null or a container in
// which every leaf is null/empty, so a pipeline result that is all-null counts
// as producing nothing.
func isEmptyValue(v any) bool {
	switch typed := v.(type) {
	case nil:
		return true
	case []any:
		for _, item := range typed {
			if !isEmptyValue(item) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, item := range typed {
			if !isEmptyValue(item) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (r *Runner) executeSavedWorkflow(ctx context.Context, name string, input any) (string, error) {
	if r.workflowDepth >= 1 {
		return "", fmt.Errorf("nested workflow depth exceeded while running %q", name)
	}
	path, err := ResolveSavedWorkflow(r.Run.CWD, name)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	r.workflowDepth++
	defer func() {
		r.workflowDepth--
	}()
	return r.executeScript(ctx, string(raw), input, false)
}

func (r *Runner) jsGate(ctx context.Context, vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		name := strings.TrimSpace(call.Argument(0).String())
		if name == "" {
			panic(vm.ToValue("gate name is required"))
		}
		message := ""
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) && !goja.IsNull(call.Argument(1)) {
			message = strings.TrimSpace(call.Argument(1).String())
		}
		opts := GateOptions{}
		if len(call.Arguments) > 2 && !goja.IsUndefined(call.Argument(2)) && !goja.IsNull(call.Argument(2)) {
			raw, err := json.Marshal(call.Argument(2).Export())
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			if err := json.Unmarshal(raw, &opts); err != nil {
				panic(vm.ToValue(err.Error()))
			}
		}
		result, err := r.runAgentGate(ctx, name, message, opts)
		if err != nil {
			panic(vm.ToValue(r.throwable(err)))
		}
		return vm.ToValue(result)
	}
}

func (r *Runner) runAgentGate(ctx context.Context, name, message string, opts GateOptions) (map[string]any, error) {
	gate, err := r.Store.EnsureGate(r.Run.ID, name, message)
	if err != nil {
		return nil, err
	}
	if gate.Status == "approved" {
		return map[string]any{"approved": true, "gate": gate, "cached": true}, nil
	}
	if gate.Status == "rejected" {
		return map[string]any{"approved": false, "gate": gate, "cached": true}, fmt.Errorf("workflow gate %q was already rejected", name)
	}

	failOnDeny := true
	if opts.FailOnDeny != nil {
		failOnDeny = *opts.FailOnDeny
	}
	mode := strings.TrimSpace(opts.Mode)
	if mode == "" {
		mode = "read-only"
	}
	agentOpts := AgentOptions{
		Label:    firstNonEmpty(opts.Label, "gate-"+name),
		Provider: opts.Provider,
		Mode:     mode,
		Model:    opts.Model,
		Schema:   defaultGateSchema(),
	}
	output, err := r.RunAgent(ctx, buildGatePrompt(name, message, opts.Criteria), agentOpts)
	if err != nil {
		return nil, err
	}
	verdict := parseAgentOutput(output)
	approved, reason := gateVerdict(verdict)
	if approved {
		gate, err = r.Store.ApproveGate(r.Run.ID, name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"approved": true, "reason": reason, "gate": gate, "verdict": verdict}, nil
	}
	gate, err = r.Store.RejectGate(r.Run.ID, name)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"approved": false, "reason": reason, "gate": gate, "verdict": verdict}
	if failOnDeny {
		if reason == "" {
			reason = "agent gate denied continuation"
		}
		return result, fmt.Errorf("workflow gate %q rejected by agent: %s", name, reason)
	}
	return result, nil
}

// jsTeam exposes team.* to workflow/loop scripts (M2 item 1): a loop tick or
// a workflow can now convene and drive an Agent Team programmatically. Every
// call below goes through the exact same Store methods (or the exact same
// RunTeam/CreateTeamTaskWithGate/CompleteTaskWithGate runtime helpers)
// `pallium team ...` uses — front-door composition per the kernel/services
// architecture ruling, nothing here reaches into team internals a CLI caller
// couldn't also reach.
//
// teamRunner constructs a FRESH *Runner for any call that dispatches real
// provider turns (wait) or a gate check (tasks.create/tasks.complete) —
// never r itself. r.Run.ID is THIS workflow run's id, already load-bearing
// for every agent()/check() call elsewhere in this same script; RunTeam sets
// r.Run.ID = teamID on whatever Runner it's given (see its own doc comment),
// which would silently corrupt this run's own agent-call bookkeeping if it
// ran on r directly.
func (r *Runner) jsTeam(ctx context.Context, vm *goja.Runtime) map[string]any {
	teamRunner := func() *Runner {
		return &Runner{CodexBinary: r.CodexBinary, PalliumBinary: r.PalliumBinary}
	}
	decodeOpts := func(raw any, out any) {
		if raw == nil {
			return
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		if err := json.Unmarshal(encoded, out); err != nil {
			panic(vm.ToValue(err.Error()))
		}
	}
	mintClaudeSessionIfNeeded := func(teamID, name, provider string) TeamMember {
		if provider == "claude" {
			if err := r.Store.PersistMemberSession(teamID, name, uuid.NewString()); err != nil {
				panic(vm.ToValue(err.Error()))
			}
		}
		member, err := r.Store.GetMember(teamID, name)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return member
	}

	return map[string]any{
		"create": func(goal string, rawOpts ...any) goja.Value {
			opts := struct {
				CWD       string  `json:"cwd"`
				BudgetUSD float64 `json:"budgetUsd"`
			}{}
			if len(rawOpts) > 0 {
				decodeOpts(rawOpts[0], &opts)
			}
			cwd := strings.TrimSpace(opts.CWD)
			switch {
			case cwd == "":
				cwd = r.Run.CWD
			case !filepath.IsAbs(cwd):
				// Resolve against the WORKFLOW's own cwd, not
				// filepath.Abs's implicit base (the Pallium process's own
				// OS working directory, which can easily differ from
				// r.Run.CWD and would silently resolve to the wrong repo).
				cwd = filepath.Join(r.Run.CWD, cwd)
			}
			// Final normalize/clean — a no-op once cwd is already absolute
			// (the common case), matching `pallium team start`'s own
			// filepath.Abs(--cwd) call. Found by review: this team
			// durably persists cwd for every future `pallium team
			// run/attach`; an unresolved relative path stored as-is
			// breaks or silently targets the wrong repo once resumed from
			// anywhere else.
			if absCWD, aerr := filepath.Abs(cwd); aerr == nil {
				cwd = absCWD
			}
			team, err := r.Store.CreateTeam(goal, cwd, opts.BudgetUSD)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(team)
		},
		"spawn": func(teamID, name string, rawOpts ...any) goja.Value {
			opts := struct {
				Provider     string `json:"provider"`
				Model        string `json:"model"`
				Role         string `json:"role"`
				Mode         string `json:"mode"`
				PlanRequired bool   `json:"planRequired"`
			}{Mode: "read-only"}
			if len(rawOpts) > 0 {
				decodeOpts(rawOpts[0], &opts)
			}
			provider := ResolveProvider("", opts.Provider)
			var err error
			if opts.PlanRequired {
				_, err = r.Store.SpawnPlanRequiredMember(teamID, name, provider, opts.Model, opts.Role)
			} else {
				_, err = r.Store.SpawnMember(teamID, name, provider, opts.Model, opts.Role, opts.Mode)
			}
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(mintClaudeSessionIfNeeded(teamID, name, provider))
		},
		"send": func(teamID, to, body string, from ...string) goja.Value {
			sender := firstNonEmpty(strings.Join(from, ""), "lead")
			msg, err := r.Store.SendTeamMessage(teamID, sender, to, body)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(msg)
		},
		"approve": func(teamID, name string) goja.Value {
			existing, err := r.Store.GetMember(teamID, name)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			var member TeamMember
			if existing.PlanRequired {
				member, err = r.Store.ApproveMemberPlan(teamID, name)
			} else {
				if err = r.Store.SetMemberMode(teamID, name, "edit"); err == nil {
					member, err = r.Store.GetMember(teamID, name)
				}
			}
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(member)
		},
		"reject": func(teamID, name, feedback string) goja.Value {
			member, err := r.Store.RejectMemberPlan(teamID, name, feedback)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(member)
		},
		"gate": func(teamID, prompt string, hooks []string) goja.Value {
			if err := r.Store.SetTeamGate(teamID, prompt, hooks); err != nil {
				panic(vm.ToValue(err.Error()))
			}
			team, err := r.Store.GetTeam(teamID)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(team)
		},
		"status": func(teamID string) goja.Value {
			team, err := r.Store.GetTeam(teamID)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			members, err := r.Store.ListMembers(teamID)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			tasks, err := r.Store.ListTeamTasks(teamID)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(map[string]any{"team": team, "members": members, "tasks": tasks})
		},
		// wait drives the team — the acceptance shape for "a loop tick
		// convening a team": create/spawn/tasks.create set the board up,
		// wait actually runs rounds of real teammate turns (same bounded
		// scheduler `pallium team run` uses: rounds until convergence,
		// budget, or the round cap, then returns — no daemon), then
		// status/tasks.list read back what happened.
		"wait": func(teamID string, rawOpts ...any) goja.Value {
			opts := struct {
				AgentTimeoutSeconds int `json:"agentTimeoutSeconds"`
				StaleAfterMinutes   int `json:"staleAfterMinutes"`
				MaxConcurrent       int `json:"maxConcurrent"`
			}{}
			if len(rawOpts) > 0 {
				decodeOpts(rawOpts[0], &opts)
			}
			if opts.AgentTimeoutSeconds <= 0 {
				// RunTeamTurn treats AgentTimeout<=0 as "no deadline at all"
				// (unlike StaleAfterMinutes, which already has its own
				// internal default via staleAfterString) — `pallium team
				// run`'s own --agent-timeout CLI flag defaults to 600s, so
				// team.wait needs the identical default when the script
				// didn't specify one, or a stuck teammate can hang the
				// whole workflow indefinitely. Found by review.
				opts.AgentTimeoutSeconds = 600
			}
			turnOpts := TeamTurnOptions{
				AgentTimeout:   time.Duration(opts.AgentTimeoutSeconds) * time.Second,
				StaleTurnAfter: time.Duration(opts.StaleAfterMinutes) * time.Minute,
				MaxConcurrent:  opts.MaxConcurrent,
			}
			summary, err := teamRunner().RunTeam(ctx, r.Store, teamID, turnOpts)
			if err != nil {
				panic(vm.ToValue(r.throwable(err)))
			}
			return vm.ToValue(summary)
		},
		"stop": func(teamID string) goja.Value {
			if err := r.Store.SetTeamStatus(teamID, "stopped"); err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(map[string]any{"team_id": teamID, "status": "stopped"})
		},
		"tasks": map[string]any{
			"create": func(teamID, title string, rawOpts ...any) goja.Value {
				opts := struct {
					Description string   `json:"description"`
					DependsOn   []string `json:"dependsOn"`
				}{}
				if len(rawOpts) > 0 {
					decodeOpts(rawOpts[0], &opts)
				}
				task, err := teamRunner().CreateTeamTaskWithGate(ctx, r.Store, teamID, title, opts.Description, opts.DependsOn)
				if err != nil {
					panic(vm.ToValue(err.Error()))
				}
				return vm.ToValue(task)
			},
			"list": func(teamID string) goja.Value {
				tasks, err := r.Store.ListTeamTasks(teamID)
				if err != nil {
					panic(vm.ToValue(err.Error()))
				}
				return vm.ToValue(tasks)
			},
			"claim": func(teamID, taskID, as string) goja.Value {
				task, err := r.Store.ClaimTask(teamID, taskID, as)
				if err != nil {
					panic(vm.ToValue(err.Error()))
				}
				return vm.ToValue(task)
			},
			"complete": func(teamID, taskID, as string, result ...string) goja.Value {
				task, approved, err := teamRunner().CompleteTaskWithGate(ctx, r.Store, teamID, taskID, as, strings.Join(result, ""))
				if err != nil {
					panic(vm.ToValue(err.Error()))
				}
				return vm.ToValue(map[string]any{"task": task, "approved": approved})
			},
		},
	}
}

func (r *Runner) jsPhase(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		name := strings.TrimSpace(call.Argument(0).String())
		if name == "" {
			name = "default"
		}
		r.mu.Lock()
		previous := r.currentPhase
		r.currentPhase = name
		r.mu.Unlock()
		if previous != "" {
			_ = r.Store.FinishPhase(r.Run.ID, previous)
		}
		_, err := r.Store.StartPhase(r.Run.ID, name)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		if len(call.Arguments) > 1 {
			fn, ok := goja.AssertFunction(call.Argument(1))
			if !ok {
				panic(vm.ToValue("phase callback must be a function"))
			}
			closePhase := func() {
				_ = r.Store.FinishPhase(r.Run.ID, name)
				r.mu.Lock()
				r.currentPhase = previous
				r.mu.Unlock()
			}
			value, err := fn(goja.Undefined())
			if err != nil {
				closePhase()
				panic(err)
			}
			if _, ok := value.Export().(*goja.Promise); ok {
				finallyFn, ok := goja.AssertFunction(value.ToObject(vm).Get("finally"))
				if !ok {
					closePhase()
					panic(vm.ToValue("phase callback promise does not support finally"))
				}
				next, err := finallyFn(value, vm.ToValue(closePhase))
				if err != nil {
					closePhase()
					panic(err)
				}
				return next
			}
			closePhase()
			return value
		}
		return goja.Undefined()
	}
}

func (r *Runner) jsAgent(ctx context.Context, vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		prompt := strings.TrimSpace(call.Argument(0).String())
		if prompt == "" {
			panic(vm.ToValue("agent prompt is required"))
		}
		opts := AgentOptions{}
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) && !goja.IsNull(call.Argument(1)) {
			exported := call.Argument(1).Export()
			// Validate the boolean-typed opts against the raw JS value before
			// the struct unmarshal: json.Unmarshal into a bool field would
			// reject a non-boolean with an opaque type error, so check first to
			// throw a clear script-facing message instead.
			if err := validateUserNetwork(exported); err != nil {
				panic(vm.ToValue(err.Error()))
			}
			raw, err := json.Marshal(exported)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			if err := json.Unmarshal(raw, &opts); err != nil {
				panic(vm.ToValue(err.Error()))
			}
		}
		if err := validateUserIsolation(opts.Isolation); err != nil {
			panic(vm.ToValue(err.Error()))
		}
		if capture := r.activeCapture(); capture != nil {
			index := len(capture.Calls)
			capture.Calls = append(capture.Calls, parallelAgentCall{Prompt: prompt, Opts: opts})
			return vm.ToValue(map[string]any{parallelAgentMarkerKey: index})
		}
		output, err := r.runAgentAtCallIndex(ctx, prompt, opts, r.nextAgentCallIndex())
		if err != nil {
			panic(vm.ToValue(r.throwable(err)))
		}
		return vm.ToValue(parseAgentOutput(output))
	}
}

func (r *Runner) jsCheck(ctx context.Context, vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		command := strings.TrimSpace(call.Argument(0).String())
		if command == "" {
			panic(vm.ToValue("check command is required"))
		}
		opts := CheckOptions{}
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) && !goja.IsNull(call.Argument(1)) {
			raw, err := json.Marshal(call.Argument(1).Export())
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			if err := json.Unmarshal(raw, &opts); err != nil {
				panic(vm.ToValue(err.Error()))
			}
		}
		agentOpts := AgentOptions{
			Label:    firstNonEmpty(opts.Label, "check: "+command),
			Provider: opts.Provider,
			Mode:     "test",
			Model:    opts.Model,
			Schema:   opts.Schema,
		}
		if len(agentOpts.Schema) == 0 {
			agentOpts.Schema = defaultCheckSchema()
		}
		prompt := buildCheckPrompt(command)
		if capture := r.activeCapture(); capture != nil {
			index := len(capture.Calls)
			capture.Calls = append(capture.Calls, parallelAgentCall{Prompt: prompt, Opts: agentOpts})
			return vm.ToValue(map[string]any{parallelAgentMarkerKey: index})
		}
		output, err := r.runAgentAtCallIndex(ctx, prompt, agentOpts, r.nextAgentCallIndex())
		if err != nil {
			panic(vm.ToValue(r.throwable(err)))
		}
		return vm.ToValue(parseAgentOutput(output))
	}
}

func (r *Runner) jsVerify(ctx context.Context, vm *goja.Runtime) map[string]any {
	return map[string]any{
		"untilGreen": func(command string, rawOptions ...any) goja.Value {
			options := map[string]any{}
			if len(rawOptions) > 0 && rawOptions[0] != nil {
				raw, err := json.Marshal(rawOptions[0])
				if err != nil {
					panic(vm.ToValue(err.Error()))
				}
				if err := json.Unmarshal(raw, &options); err != nil {
					panic(vm.ToValue(err.Error()))
				}
			}
			result, err := r.runUntilGreen(ctx, strings.TrimSpace(command), options)
			if err != nil {
				panic(vm.ToValue(r.throwable(err)))
			}
			return vm.ToValue(result)
		},
	}
}

func (r *Runner) runUntilGreen(ctx context.Context, command string, options map[string]any) (result map[string]any, err error) {
	if command == "" {
		return nil, fmt.Errorf("verify.untilGreen command is required")
	}
	maxRounds := optionInt(options, "maxRounds", 3)
	if value := optionInt(options, "max_rounds", 0); value > 0 {
		maxRounds = value
	}
	if maxRounds <= 0 {
		maxRounds = 3
	}
	label := optionString(options, "label", "until-green")
	provider := optionString(options, "provider", "")
	testModel := optionString(options, "model", "")
	fixModel := optionString(options, "fix_model", testModel)

	// One persistent worktree hosts the fix rounds so every later check sees
	// the fix agents' edits immediately. The worktree is created lazily: the
	// first check runs in the original cwd, so an already-green command needs
	// no worktree at all (and untilGreen works in a non-git cwd). The combined
	// diff is captured to a deterministic patch file after every fix round so
	// an interrupted loop restores its progress on resume instead of silently
	// registering an empty patch.
	repoRoot := r.Run.CWD
	loopID := fmt.Sprintf("untilgreen-%d", r.nextAgentCallIndex())
	loopPatchPath, err := r.agentPatchPath(loopID)
	if err != nil {
		return nil, err
	}
	worktree := ""
	// Every loop exit (green, stalled, max rounds, mid-loop agent error) runs
	// through this defer: a clean end registers the combined patch on an
	// internal agent row; an error exit captures the durable patch file so
	// resume can restore the fix edits. The worktree is removed either way and
	// kept only when patch capture fails.
	defer func() {
		if worktree == "" {
			return
		}
		if err == nil {
			if regErr := r.registerUntilGreenPatch(label, command, loopID, worktree, repoRoot, result); regErr != nil {
				result, err = nil, regErr
			}
			return
		}
		if _, patchErr := r.writeWorktreePatch(loopID, worktree); patchErr != nil {
			fmt.Fprintf(os.Stderr, "[workflow:%s] untilGreen patch capture failed; keeping worktree %s for debugging: %v\n", r.Run.ID, worktree, patchErr)
			return
		}
		r.removeWorktree(repoRoot, worktree)
	}()
	ensureWorktree := func() error {
		if worktree != "" {
			return nil
		}
		// The fix rounds below have genuine edit intent (their combined patch
		// is applied to the real repo at workflow success via
		// registerUntilGreenPatch), so this loop needs the same advisory repo
		// lock as any other edit-intent agent, acquired before touching disk.
		if err := r.acquireRepoLock(repoRoot); err != nil {
			return err
		}
		path, pathErr := r.worktreePath(loopID)
		if pathErr != nil {
			return pathErr
		}
		r.removeWorktree(repoRoot, path)
		created, createErr := r.createWorktree(loopID, repoRoot)
		if createErr != nil {
			return createErr
		}
		worktree = created
		// Layer prior standalone edit agents' accumulated staging edits (if
		// any) before restoring this loop's own resume patch below, so a fix
		// round builds on top of edits made earlier in this run (mirrors
		// gate()/check(), which already redirect through staging via
		// runAgentCommand).
		if err := r.seedFromStaging(repoRoot, worktree); err != nil {
			return err
		}
		// Restore progress captured before an interrupt so cached fix agents
		// replaying output-only do not silently lose their edits.
		if raw, readErr := os.ReadFile(loopPatchPath); readErr == nil && strings.TrimSpace(string(raw)) != "" {
			apply := exec.Command("git", "apply", "--3way", loopPatchPath)
			apply.Dir = worktree
			var stderr bytes.Buffer
			apply.Stderr = &stderr
			if applyErr := apply.Run(); applyErr != nil {
				fmt.Fprintf(os.Stderr, "[workflow:%s] could not restore untilGreen patch %s: %v: %s; continuing from HEAD\n", r.Run.ID, loopPatchPath, applyErr, strings.TrimSpace(stderr.String()))
			}
			// git apply --3way stages the restored changes; unstage them so
			// the loop's later `git diff` captures keep seeing the edits.
			reset := exec.Command("git", "reset", "-q")
			reset.Dir = worktree
			_ = reset.Run()
		}
		return nil
	}

	var rounds []map[string]any
	lastSignature := ""
	stalled := false
	green := false
	for round := 0; round <= maxRounds; round++ {
		checkRepo := repoRoot
		if worktree != "" {
			checkRepo = worktree
		}
		checkOutput, err := r.RunAgent(ctx, buildCheckPrompt(command), AgentOptions{
			Label:    fmt.Sprintf("%s-check-%d", label, round+1),
			Provider: provider,
			Repo:     checkRepo,
			Mode:     "test",
			Model:    testModel,
			Schema:   defaultCheckSchema(),
		})
		if err != nil {
			return nil, err
		}
		checkResult := parseAgentOutput(checkOutput)
		roundRecord := map[string]any{
			"round":   round + 1,
			"check":   checkResult,
			"fixed":   false,
			"command": command,
		}
		rounds = append(rounds, roundRecord)
		if checkOK(checkResult) {
			green = true
			break
		}
		signature := StableHash(checkResult)
		if round > 0 && signature == lastSignature {
			stalled = true
			break
		}
		lastSignature = signature
		if round == maxRounds {
			break
		}
		if err := ensureWorktree(); err != nil {
			return nil, err
		}
		fixPrompt := "Fix the failing verification for this workflow.\nCommand: " + command + "\nFailure JSON: " + stringifyResult(checkResult) + "\nMake the smallest correct code change. Do not skip, weaken, or hide tests."
		fixOutput, err := r.RunAgent(ctx, fixPrompt, AgentOptions{
			Label:     fmt.Sprintf("%s-fix-%d", label, round+1),
			Provider:  provider,
			Repo:      worktree,
			Mode:      "edit",
			Isolation: "none",
			Model:     fixModel,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary":       map[string]any{"type": "string"},
					"files_changed": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"confidence":    map[string]any{"type": "string"},
				},
				"required": []any{"summary"},
			},
		})
		if err != nil {
			return nil, err
		}
		roundRecord["fixed"] = true
		roundRecord["fix"] = parseAgentOutput(fixOutput)
		// Durable incremental capture: overwrite the loop patch after every
		// completed fix round so a crash or interrupt cannot lose the edits.
		if _, captureErr := r.writeWorktreePatch(loopID, worktree); captureErr != nil {
			fmt.Fprintf(os.Stderr, "[workflow:%s] untilGreen incremental patch capture failed: %v\n", r.Run.ID, captureErr)
		}
	}
	result = map[string]any{"ok": green, "command": command, "rounds": rounds, "stalled": stalled}
	return result, nil
}

// registerUntilGreenPatch captures the combined patch from the untilGreen
// loop worktree on an internal agent row so it auto-applies with the other
// edit-agent patches when the run completes. A row cached from a previous
// execution wins so resume keeps the originally captured patch. The patch is
// written to the loop's deterministic path, matching the incremental captures
// taken during the loop.
func (r *Runner) registerUntilGreenPatch(label, command, loopID, worktree, repoRoot string, result map[string]any) error {
	r.mu.Lock()
	phase := r.currentPhase
	scriptHash := r.scriptHash
	argsHash := r.argsHash
	r.mu.Unlock()
	callIndex := r.nextAgentCallIndex()
	patchLabel := label + "-patch"
	prompt := "verify.untilGreen combined patch: " + command
	if cached, ok, err := r.Store.CompletedAgent(r.Run.ID, callIndex, phase, patchLabel, prompt, "internal", repoRoot, "edit", "worktree", "", "", argsHash, false); err != nil {
		return err
	} else if ok {
		r.removeWorktree(repoRoot, worktree)
		// On resume this run's staging worktree starts empty even though a
		// prior process already captured the loop's combined patch: replay
		// it into staging so later standalone check()/gate() calls and later
		// edit agents see the fix, matching a fresh (non-cached) round.
		if cached.PatchPath != "" {
			return r.mergeIntoStaging(repoRoot, cached.PatchPath)
		}
		return nil
	}
	created, err := r.Store.CreateAgent(Agent{
		RunID:      r.Run.ID,
		CallIndex:  callIndex,
		Phase:      phase,
		Label:      patchLabel,
		Prompt:     prompt,
		Provider:   "internal",
		Repo:       repoRoot,
		Mode:       "edit",
		Isolation:  "worktree",
		ScriptHash: scriptHash,
		ArgsHash:   argsHash,
		Worktree:   worktree,
	})
	if err != nil {
		return err
	}
	patchPath, err := r.captureWorktreePatch(loopID, worktree, repoRoot)
	if err != nil {
		_ = r.Store.FinishAgent(created, "", err.Error())
		return err
	}
	created.PatchPath = patchPath
	// The loop's combined fix is genuine edit intent exactly like a
	// standalone edit agent's patch: merge it into staging so a later
	// standalone check()/gate() call or later edit agent in this run sees
	// it, not just the loop's own already-persistent worktree.
	if patchPath != "" {
		if err := r.mergeIntoStaging(repoRoot, patchPath); err != nil {
			return err
		}
	}
	return r.Store.FinishAgent(created, stringifyResult(result), "")
}

func (r *Runner) jsCoordinator(ctx context.Context, vm *goja.Runtime) map[string]any {
	return map[string]any{
		"replan": func(goal string, rawOptions ...any) goja.Value {
			options := AgentOptions{}
			if len(rawOptions) > 0 && rawOptions[0] != nil {
				raw, err := json.Marshal(rawOptions[0])
				if err != nil {
					panic(vm.ToValue(err.Error()))
				}
				if err := json.Unmarshal(raw, &options); err != nil {
					panic(vm.ToValue(err.Error()))
				}
			}
			result, err := r.runCoordinatorReplan(ctx, goal, options)
			if err != nil {
				panic(vm.ToValue(r.throwable(err)))
			}
			return vm.ToValue(parseAgentOutput(result))
		},
	}
}

func (r *Runner) runCoordinatorReplan(ctx context.Context, goal string, opts AgentOptions) (string, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return "", fmt.Errorf("coordinator.replan goal is required")
	}
	snapshot, err := r.Store.Snapshot(r.Run.ID)
	if err != nil {
		return "", err
	}
	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	if opts.Label == "" {
		opts.Label = "coordinator-replan"
	}
	opts.Mode = "read-only"
	opts.Isolation = ""
	if len(opts.Schema) == 0 {
		opts.Schema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"decision":    map[string]any{"type": "string"},
				"next_steps":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"spawn":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"stop_reason": map[string]any{"type": "string"},
			},
			"required": []any{"decision", "next_steps"},
		}
	}
	prompt := "You are the coordinator for a Pallium dynamic workflow. Replan from current run state without repeating completed work.\nGoal: " + goal + "\nWorkflow snapshot JSON: " + string(rawSnapshot) + "\nReturn the next decision, next_steps, optional spawn prompts, and stop_reason if the workflow should stop."
	return r.RunAgent(ctx, prompt, opts)
}

func (r *Runner) jsPallium(ctx context.Context, vm *goja.Runtime) map[string]any {
	call := func(args ...string) goja.Value {
		value, err := r.runPalliumCommand(ctx, args...)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return vm.ToValue(value)
	}
	return map[string]any{
		"verify": func(tier string) goja.Value {
			return call("verify", strings.TrimSpace(tier))
		},
		"review": func(baseRef ...string) goja.Value {
			args := []string{"review"}
			if len(baseRef) > 0 && strings.TrimSpace(baseRef[0]) != "" {
				args = append(args, strings.TrimSpace(baseRef[0]))
			}
			return call(args...)
		},
		"handoff": func(baseRef ...string) goja.Value {
			args := []string{"handoff"}
			if len(baseRef) > 0 && strings.TrimSpace(baseRef[0]) != "" {
				args = append(args, strings.TrimSpace(baseRef[0]))
			}
			return call(args...)
		},
		"explain": func(path string) goja.Value {
			return call("explain", strings.TrimSpace(path))
		},
		"safe": func(path string) goja.Value {
			return call("safe", strings.TrimSpace(path))
		},
		"plan": func(path string) goja.Value {
			return call("plan", strings.TrimSpace(path))
		},
		"changedNow": func() goja.Value {
			return call("changed-now")
		},
		"preflight": func(task string, scopePaths ...string) goja.Value {
			args := []string{"workflow", "preflight", strings.TrimSpace(task)}
			for _, scope := range scopePaths {
				if strings.TrimSpace(scope) != "" {
					args = append(args, "--scope", strings.TrimSpace(scope))
				}
			}
			return call(args...)
		},
		"decisions": map[string]any{
			"record": func(title string, body string, tags ...string) goja.Value {
				decision, err := r.Store.RecordDecision(r.Run.ID, title, body, tags)
				if err != nil {
					panic(vm.ToValue(err.Error()))
				}
				return vm.ToValue(decision)
			},
			"search": func(query string, limit ...int) goja.Value {
				max := 10
				if len(limit) > 0 && limit[0] > 0 {
					max = limit[0]
				}
				decisions, err := r.Store.SearchDecisions(query, max)
				if err != nil {
					panic(vm.ToValue(err.Error()))
				}
				return vm.ToValue(decisions)
			},
		},
		"task": map[string]any{
			"start": func(goal string, scopePaths ...string) goja.Value {
				args := []string{"task", "start", strings.TrimSpace(goal)}
				for _, scope := range scopePaths {
					if strings.TrimSpace(scope) != "" {
						args = append(args, strings.TrimSpace(scope))
					}
				}
				return call(args...)
			},
			"show": func() goja.Value {
				return call("task", "show")
			},
			"clear": func() goja.Value {
				return call("task", "clear")
			},
		},
	}
}

// unwrapMapperThrow recovers the original error a parallel() mapper threw
// synchronously. goja wraps a thrown value in a *goja.Exception and its
// Error() appends call-site stack info (e.g. " at ...(native)"), which
// breaks the exact-string comparisons isWorkflowStoppedError/
// isWorkflowPausedError rely on for sentinel errors like ErrWorkflowStopped.
// When the mapper threw a plain string (as our internal panics do via
// vm.ToValue(err.Error())), unwrap it back to a clean error so text-based
// classification sees the original message rather than the decorated one.
//
// jsParallel's own mapper-throw catch no longer needs this: it classifies
// fatal errors via Runner.fatalCause() instead (set at the error's true
// point of origin, before any stringification), which is robust to both the
// goja round-trip and to an ordinary provider failure's stderr coincidentally
// containing a sentinel's phrasing. This helper is kept for any caller that
// only has the stringified error to work with and needs the same recovery
// isWorkflowStoppedError/isWorkflowPausedError already tolerate.
func unwrapMapperThrow(err error) error {
	var exception *goja.Exception
	if errors.As(err, &exception) {
		if message, ok := exception.Value().Export().(string); ok {
			return errors.New(message)
		}
	}
	return err
}

func (r *Runner) jsParallel(ctx context.Context, vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		items := call.Argument(0).ToObject(vm)
		lengthValue := items.Get("length")
		length := int(lengthValue.ToInteger())
		if length > maxWorkflowCollectionItems {
			panic(vm.ToValue(fmt.Sprintf("parallel item limit exceeded: %d > %d", length, maxWorkflowCollectionItems)))
		}
		var mapper func(goja.Value, ...goja.Value) (goja.Value, error)
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) && !goja.IsNull(call.Argument(1)) {
			fn, ok := goja.AssertFunction(call.Argument(1))
			if !ok {
				panic(vm.ToValue("parallel callback must be a function"))
			}
			mapper = fn
		}
		rawResults := make([]any, 0, length)
		capture := &parallelCapture{}
		previousCapture := r.swapCapture(capture)
		defer func() {
			r.setCapture(previousCapture)
		}()
		for i := 0; i < length; i++ {
			item := items.Get(fmt.Sprintf("%d", i))
			if mapper != nil {
				callStart := len(capture.Calls)
				fatalBefore := r.fatalCause()
				value, err := mapper(goja.Undefined(), item, vm.ToValue(i))
				if err != nil {
					// A mapper can throw synchronously by calling something
					// like verify.untilGreen(), which crosses the goja panic
					// boundary as a plain string and loses the original
					// error's type before we ever see it here (see
					// unwrapMapperThrow's doc comment) — text-based fallback
					// classification isn't safe either, since an ordinary
					// provider failure's own stderr can coincidentally
					// contain the same phrasing as a fatal sentinel's
					// message. Runner.fatalCause() sidesteps both problems:
					// it's set at the fatal error's true point of origin,
					// before any stringification, so comparing it against
					// its pre-call snapshot tells us whether this specific
					// mapper invocation just recorded a genuinely fatal
					// condition (interrupt, budget, or max-agents), with no
					// text parsing at all.
					if fatal := r.fatalCause(); fatal != nil && fatal != fatalBefore {
						// r.throwable, not fatal.Error() directly: a fatal
						// cause can be a stop/pause interrupt, and
						// classifyRunError only recognizes one crossing the
						// goja boundary if it still carries the interrupt
						// sentinel token throwable appends. Without it, a
						// mapper hitting a pause/stop mid-loop (e.g. via
						// verify.untilGreen()) would get misclassified as an
						// ordinary "failed" run instead of paused/stopped.
						panic(vm.ToValue(r.throwable(fatal)))
					}
					capture.Calls = capture.Calls[:callStart]
					r.recordDroppedItem(fmt.Sprintf("parallel item %d", i), describeStageDrop(err))
					rawResults = append(rawResults, nil)
					continue
				}
				rawResults = append(rawResults, value.Export())
			} else if fn, ok := goja.AssertFunction(item); ok {
				// parallel(arrayOfThunks) — no mapper, each item is itself a
				// zero-arg function. This is the shape Ultracode-native agent
				// reflexes reach for by default; it must degrade a failing
				// thunk to null exactly like the mapper form above (same
				// fatal-vs-transient classification via fatalCause, same
				// rollback of any calls the thunk captured before throwing),
				// not crash the whole run on one ordinary agent failure.
				callStart := len(capture.Calls)
				fatalBefore := r.fatalCause()
				value, err := fn(goja.Undefined())
				if err != nil {
					if fatal := r.fatalCause(); fatal != nil && fatal != fatalBefore {
						panic(vm.ToValue(r.throwable(fatal)))
					}
					capture.Calls = capture.Calls[:callStart]
					r.recordDroppedItem(fmt.Sprintf("parallel item %d", i), describeStageDrop(err))
					rawResults = append(rawResults, nil)
					continue
				}
				rawResults = append(rawResults, value.Export())
			} else {
				rawResults = append(rawResults, item.Export())
			}
		}

		r.assignSequentialCallIndexes(capture.Calls)
		agentResults, err := r.runParallelAgentCalls(ctx, capture.Calls)
		if err != nil {
			panic(vm.ToValue(r.throwable(err)))
		}
		results := make([]any, 0, len(rawResults))
		for _, result := range rawResults {
			results = append(results, replaceParallelAgentMarkers(result, agentResults))
		}
		return vm.ToValue(results)
	}
}

func (r *Runner) jsPipeline(ctx context.Context, vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		items := call.Argument(0).ToObject(vm)
		length := int(items.Get("length").ToInteger())
		if length > maxWorkflowCollectionItems {
			panic(vm.ToValue(fmt.Sprintf("pipeline item limit exceeded: %d > %d", length, maxWorkflowCollectionItems)))
		}
		values := make([]any, 0, length)
		for i := 0; i < length; i++ {
			values = append(values, items.Get(fmt.Sprintf("%d", i)).Export())
		}
		stages := make([]func(goja.Value, ...goja.Value) (goja.Value, error), 0, len(call.Arguments)-1)
		for _, stageValue := range call.Arguments[1:] {
			fn, ok := goja.AssertFunction(stageValue)
			if !ok {
				panic(vm.ToValue("pipeline stages must be functions"))
			}
			stages = append(stages, fn)
		}
		if len(stages) == 0 {
			return vm.ToValue(values)
		}
		pipelineIndex := r.nextPipelineIndex()

		limit := r.MaxConcurrentAgents
		if limit <= 0 {
			limit = 16
		}
		sem := make(chan struct{}, limit)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		results := make([]any, len(values))
		var vmMu sync.Mutex
		var wg sync.WaitGroup
		var errMu sync.Mutex
		var firstErr error
		for itemIndex, item := range values {
			itemIndex, item := itemIndex, item
			wg.Add(1)
			go func() {
				defer wg.Done()
				current := item
				for stageIndex, stage := range stages {
					rawResult, calls, err := r.capturePipelineStage(vm, &vmMu, stage, current, item, pipelineIndex, stageIndex, itemIndex)
					if err != nil {
						r.recordDroppedItem(fmt.Sprintf("pipeline stage %d item %d", stageIndex, itemIndex), describeStageDrop(err))
						results[itemIndex] = nil
						return
					}
					agentResults, err := r.runAgentCallsWithSemaphore(ctx, calls, sem)
					if err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
							cancel()
						}
						errMu.Unlock()
						return
					}
					current = replaceParallelAgentMarkers(rawResult, agentResults)
				}
				results[itemIndex] = current
			}()
		}
		wg.Wait()
		if firstErr != nil {
			panic(vm.ToValue(r.throwable(firstErr)))
		}
		return vm.ToValue(results)
	}
}

func (r *Runner) capturePipelineStage(vm *goja.Runtime, vmMu *sync.Mutex, fn func(goja.Value, ...goja.Value) (goja.Value, error), value, originalItem any, pipelineIndex, stageIndex, itemIndex int) (any, []parallelAgentCall, error) {
	vmMu.Lock()
	defer vmMu.Unlock()

	capture := &parallelCapture{}
	previousCapture := r.swapCapture(capture)
	defer func() {
		r.setCapture(previousCapture)
	}()

	next, err := fn(goja.Undefined(), vm.ToValue(value), vm.ToValue(originalItem), vm.ToValue(itemIndex))
	if err != nil {
		return nil, nil, err
	}
	next, err = awaitPromiseValue(next)
	if err != nil {
		return nil, nil, err
	}
	calls := append([]parallelAgentCall(nil), capture.Calls...)
	for i := range calls {
		calls[i].CallIndex = pipelineCallIndex(pipelineIndex, stageIndex, itemIndex, i)
	}
	return next.Export(), calls, nil
}

func installDeterministicWorkflowGuards(vm *goja.Runtime) {
	determinismError := func(name string) func() {
		return func() {
			panic(vm.ToValue(name + " is disabled in Pallium workflow scripts; pass nondeterministic values through args"))
		}
	}
	if mathObj := vm.Get("Math").ToObject(vm); mathObj != nil {
		_ = mathObj.Set("random", determinismError("Math.random"))
	}
	originalDate := vm.Get("Date")
	if originalDate != nil && !goja.IsUndefined(originalDate) && !goja.IsNull(originalDate) {
		originalDateObj := originalDate.ToObject(vm)
		_ = vm.Set("Date", func(call goja.ConstructorCall) *goja.Object {
			if len(call.Arguments) == 0 {
				panic(vm.ToValue("new Date() is disabled in Pallium workflow scripts; pass nondeterministic values through args"))
			}
			date, err := vm.New(originalDateObj, call.Arguments...)
			if err != nil {
				panic(err)
			}
			return date
		})
		dateObj := vm.Get("Date").ToObject(vm)
		_ = dateObj.Set("now", determinismError("Date.now"))
		_ = dateObj.Set("parse", originalDateObj.Get("parse"))
		_ = dateObj.Set("UTC", originalDateObj.Get("UTC"))
	}
}

func (r *Runner) runPalliumCommand(ctx context.Context, args ...string) (any, error) {
	if stub := os.Getenv("PALLIUM_WORKFLOW_PALLIUM_STUB"); stub != "" {
		return parseAgentOutput(strings.ReplaceAll(stub, "{{ARGS}}", strings.Join(args, " "))), nil
	}
	cleanArgs := make([]string, 0, len(args)+1)
	for _, arg := range args {
		if strings.TrimSpace(arg) != "" {
			cleanArgs = append(cleanArgs, arg)
		}
	}
	cleanArgs = append(cleanArgs, "--json")
	cmd := exec.CommandContext(ctx, r.PalliumBinary, cleanArgs...)
	cmd.Dir = r.Run.CWD
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pallium %s failed: %w: %s", strings.Join(cleanArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return map[string]any{}, nil
	}
	return parseAgentOutput(text), nil
}

func (r *Runner) RunAgent(ctx context.Context, prompt string, opts AgentOptions) (string, error) {
	return r.runAgentAtCallIndex(ctx, prompt, opts, r.nextAgentCallIndex())
}

func (r *Runner) nextAgentCallIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agentCallIndex++
	return r.agentCallIndex
}

func (r *Runner) activeCapture() *parallelCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capture
}

func (r *Runner) swapCapture(capture *parallelCapture) *parallelCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := r.capture
	r.capture = capture
	return previous
}

func (r *Runner) setCapture(capture *parallelCapture) {
	r.mu.Lock()
	r.capture = capture
	r.mu.Unlock()
}

// recordDroppedItem tracks an item that parallel/pipeline converted into a
// null result so partial success stays visible at the run level. failuresMu
// is held across both the append and the store write so concurrent drops
// persist in append order and a stale shorter snapshot can never overwrite a
// longer one.
func (r *Runner) recordDroppedItem(label, errText string) {
	r.failuresMu.Lock()
	defer r.failuresMu.Unlock()
	r.mu.Lock()
	r.failures = append(r.failures, RunFailure{Label: label, Phase: r.currentPhase, Error: errText})
	failures := append([]RunFailure(nil), r.failures...)
	r.mu.Unlock()
	fmt.Fprintf(os.Stderr, "[workflow:%s] dropped %s: %s\n", r.Run.ID, label, errText)
	_ = r.Store.SetRunFailures(r.Run.ID, failures)
}

// recordFatalLocked stores the first run-fatal cause; interrupts take
// precedence over other causes. The caller must hold r.mu.
func (r *Runner) recordFatalLocked(err error) {
	if r.fatalErr == nil || (isWorkflowInterruptedError(err) && !isWorkflowInterruptedError(r.fatalErr)) {
		r.fatalErr = err
	}
}

func (r *Runner) recordFatal(err error) error {
	r.mu.Lock()
	r.recordFatalLocked(err)
	r.mu.Unlock()
	return err
}

func (r *Runner) fatalCause() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fatalErr
}

// interruptSentinel is a run-scoped token embedded in interrupt errors thrown
// into JS, so classifyRunError can tell the original interrupt apart from a
// later unrelated script error (e.g. a script that catches the interrupt and
// throws its own error).
func (r *Runner) interruptSentinel() string {
	return "[pallium-workflow-interrupt:" + r.Run.ID + "]"
}

// throwable renders an error for a goja throw, tagging workflow interrupts
// with the run-scoped sentinel token.
func (r *Runner) throwable(err error) string {
	if isWorkflowInterruptedError(err) {
		return err.Error() + " " + r.interruptSentinel()
	}
	return err.Error()
}

// classifyRunError maps a script-level failure back to a recorded workflow
// interrupt. Errors that cross the goja VM boundary arrive as plain text, so
// reclassification only happens when the surfaced error still carries the
// sentinel token added by throwable; the actual verdict comes from the cause
// recorded where the interrupt originated, never from parsing arbitrary
// error strings.
func (r *Runner) classifyRunError(err error) error {
	if err == nil || isWorkflowInterruptedError(err) {
		return err
	}
	if !strings.Contains(err.Error(), r.interruptSentinel()) {
		return err
	}
	cause := r.fatalCause()
	if errors.Is(cause, ErrWorkflowStopped) {
		return ErrWorkflowStopped
	}
	if errors.Is(cause, ErrWorkflowPaused) {
		return ErrWorkflowPaused
	}
	return err
}

func (r *Runner) assignSequentialCallIndexes(calls []parallelAgentCall) {
	for i := range calls {
		if calls[i].CallIndex == 0 {
			calls[i].CallIndex = r.nextAgentCallIndex()
		}
	}
}

func (r *Runner) nextPipelineIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pipelineIndex++
	return r.pipelineIndex
}

func pipelineCallIndex(pipelineIndex, stageIndex, itemIndex, callOrdinal int) int {
	return pipelineCallIndexBase +
		pipelineIndex*pipelineIndexStride +
		stageIndex*pipelineStageStride +
		itemIndex*pipelineItemStride +
		callOrdinal + 1
}

func (r *Runner) runAgentAtCallIndex(ctx context.Context, prompt string, opts AgentOptions, callIndex int) (string, error) {
	if err := r.ensureNotStopped(ctx); err != nil {
		return "", err
	}
	r.mu.Lock()
	phase := r.currentPhase
	scriptHash := r.scriptHash
	argsHash := r.argsHash
	r.mu.Unlock()
	mode := strings.TrimSpace(opts.Mode)
	if mode == "" {
		mode = "read-only"
	}
	provider := ResolveProvider("", opts.Provider)
	if provider == "internal" {
		// "internal" is reserved for registerUntilGreenPatch's own
		// bookkeeping rows, which Store.AgentUsage excludes from the
		// --max-agents count because they never spawn a worker. Letting a
		// user agent share that provider name would make its real spawns
		// invisible to that same cap on resume.
		return "", fmt.Errorf("workflow agent provider %q is reserved", provider)
	}
	repo := strings.TrimSpace(opts.Repo)
	if repo == "" {
		repo = r.Run.CWD
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	schemaHash := agentSchemaHash(opts.Schema)
	// networkGranted is the effective egress decision (agent opt-in AND the
	// run's --allow-network ceiling), matching resolveAgentNetwork. It is part
	// of the cache identity so re-running the same run-id WITHOUT
	// --allow-network cannot reuse a row produced with network on: the operator
	// believes the rerun was sandboxed, so it must not serve networked output.
	networkGranted := opts.Network && r.Run.AllowNetwork
	if cached, ok, err := r.Store.CompletedAgent(r.Run.ID, callIndex, phase, opts.Label, prompt, provider, absRepo, mode, opts.Isolation, opts.Model, schemaHash, argsHash, networkGranted); err != nil {
		return "", err
	} else if ok {
		if _, err := parseAgentOutputWithSchema(cached.Output, opts.Schema); err != nil {
			return "", fmt.Errorf("cached agent %s failed schema validation: %w", cached.ID, err)
		}
		// A resumed run replays this call from cache instead of spawning a
		// fresh agent, but a cached edit-intent agent's patch still gets
		// applied to the real repo at workflow success (see ApplyPatches) —
		// this resumed run needs the same advisory repo lock for its
		// duration as a run that actually re-executed the agent. On resume
		// this run's staging worktree starts empty even though a prior
		// process already captured this edit agent's patch: replay it into
		// staging so later standalone check()/gate() calls, verify.
		// untilGreen, and later edit agents see the edit exactly as they
		// would have mid-run before the interruption.
		if isEditIntentAgent(mode, opts.Isolation) {
			if err := r.acquireRepoLock(absRepo); err != nil {
				return "", err
			}
			if cached.PatchPath != "" {
				if err := r.mergeIntoStaging(absRepo, cached.PatchPath); err != nil {
					return "", err
				}
			}
		}
		if phase != "" {
			_ = r.Store.IncrementPhaseAgentCount(r.Run.ID, phase)
		}
		return cached.Output, nil
	}

	r.mu.Lock()
	if r.agentCount >= r.MaxAgents {
		err := fmt.Errorf("%w: %d", ErrWorkflowMaxAgentsExceeded, r.MaxAgents)
		r.recordFatalLocked(err)
		r.mu.Unlock()
		return "", err
	}
	if r.budgetLimit > 0 && r.budgetSpent+r.agentCostUSD > r.budgetLimit {
		err := fmt.Errorf("%w: next agent would exceed $%.4f limit", ErrWorkflowBudgetExhausted, r.budgetLimit)
		r.recordFatalLocked(err)
		r.mu.Unlock()
		return "", err
	}
	r.budgetSpent += r.agentCostUSD
	r.agentCount++
	r.mu.Unlock()

	agent := Agent{
		RunID:            r.Run.ID,
		CallIndex:        callIndex,
		Phase:            phase,
		Label:            opts.Label,
		Prompt:           prompt,
		Provider:         provider,
		Repo:             absRepo,
		Mode:             mode,
		Isolation:        opts.Isolation,
		Model:            opts.Model,
		SchemaHash:       schemaHash,
		ScriptHash:       scriptHash,
		ArgsHash:         argsHash,
		EstimatedCostUSD: r.agentCostUSD,
		Networked:        networkGranted,
	}
	created, err := r.Store.CreateAgent(agent)
	if err != nil {
		return "", err
	}
	agent = created
	if phase != "" {
		_ = r.Store.IncrementPhaseAgentCount(r.Run.ID, phase)
	}

	agentCtx, stopWatching := r.contextWithStoredStop(ctx)
	defer stopWatching()
	output, patchPath, worktree, err := r.runAgentCommand(agentCtx, &agent, opts)
	agent.PatchPath = patchPath
	agent.Worktree = worktree
	if delta := agent.EstimatedCostUSD - r.agentCostUSD; delta != 0 {
		r.mu.Lock()
		r.budgetSpent += delta
		spent, limit := r.budgetSpent, r.budgetLimit
		overBudget := limit > 0 && spent > limit
		r.mu.Unlock()
		// The preflight check above only guards against the flat per-agent
		// estimate. A provider that reports a real cost_usd after the fact
		// can push spend past the limit on its own, and a script with no
		// later agent() call would never hit the preflight check again to
		// notice. Fail this agent immediately so the run doesn't complete
		// successfully while already over budget — regardless of whether the
		// provider call itself also errored (a provider that exits nonzero
		// or times out after reporting an over-budget cost is still over
		// budget; that must not be swallowed as an ordinary dropped item in
		// parallel()/pipeline()). spent/limit are captured under the lock
		// above rather than read again here, since a concurrent agent call
		// can mutate r.budgetSpent between unlock and use.
		if overBudget {
			budgetErr := fmt.Errorf("%w: reported cost pushed spend to $%.4f over $%.4f limit", ErrWorkflowBudgetExhausted, spent, limit)
			if err != nil {
				budgetErr = fmt.Errorf("%w (agent also failed: %v)", budgetErr, err)
			}
			budgetErr = r.recordFatal(budgetErr)
			_ = r.Store.FinishAgent(agent, output, budgetErr.Error())
			return "", budgetErr
		}
	}
	if err != nil {
		if normalized := r.normalizeInterruptError(agentCtx, err); isWorkflowInterruptedError(normalized) {
			_ = r.Store.FinishAgentStatus(agent, interruptedStatus(normalized), output, interruptedMessage(normalized))
			return "", normalized
		}
		_ = r.Store.FinishAgent(agent, output, err.Error())
		return "", err
	}
	if err := r.ensureNotStopped(ctx); err != nil {
		_ = r.Store.FinishAgentStatus(agent, interruptedStatus(err), output, interruptedMessage(err))
		return "", err
	}
	if _, schemaErr := parseAgentOutputWithSchema(output, opts.Schema); schemaErr != nil {
		if agent.PatchPath != "" {
			// An edit agent's captured patch is completed WORK. A malformed
			// structured OUTPUT must not discard it — that was a data-loss bug:
			// the edit vanished silently while the run failed. Preserve the
			// patch, finish the agent so its patch applies at workflow success
			// (and is recoverable via `workflow apply`), and surface the schema
			// failure in the run's failures list instead of throwing the edit
			// away. Only the structured output fails, not the completed work.
			r.recordDroppedItem(firstNonEmpty(opts.Label, agent.ID), fmt.Sprintf("structured output failed schema validation; edit patch preserved: %v", schemaErr))
			if err := r.Store.FinishAgent(agent, output, ""); err != nil {
				return "", err
			}
			return output, nil
		}
		// Read-only (no patch): the structured output IS the deliverable, so a
		// schema failure (after the read-only retry) is a real failure.
		agent.PatchPath = ""
		_ = r.Store.FinishAgent(agent, output, schemaErr.Error())
		return "", schemaErr
	}
	if err := r.Store.FinishAgent(agent, output, ""); err != nil {
		return "", err
	}
	return output, nil
}

func (r *Runner) runParallelAgentCalls(ctx context.Context, calls []parallelAgentCall) ([]any, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	limit := r.MaxConcurrentAgents
	if limit <= 0 {
		limit = 16
	}
	return r.runAgentCallsWithSemaphore(ctx, calls, make(chan struct{}, limit))
}

func (r *Runner) runAgentCallsWithSemaphore(ctx context.Context, calls []parallelAgentCall, sem chan struct{}) ([]any, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]any, len(calls))
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	for i, call := range calls {
		i, call := i, call
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errMu.Lock()
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				errMu.Unlock()
				return
			}
			output, err := r.runAgentAtCallIndex(ctx, call.Prompt, call.Opts, call.CallIndex)
			if err != nil {
				if !isWorkflowFatalAgentError(err) {
					r.recordDroppedItem(firstNonEmpty(call.Opts.Label, "agent"), err.Error())
					results[i] = nil
					return
				}
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				errMu.Unlock()
				return
			}
			results[i] = parseAgentOutput(output)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// isWorkflowFatalAgentError only sees Go-side errors from runAgentAtCallIndex
// and the async parallel agent-fanout path, so typed sentinel checks are
// sufficient here; error text is deliberately never parsed for the budget/
// max-agents categories, because an ordinary (non-fatal) provider failure's
// own stderr can coincidentally contain that same phrasing (see
// TestParallelAgentErrorContainingBudgetPhraseIsNonFatal) — substring
// matching there would misclassify a real but unrelated failure as fatal.
// A parallel() mapper's synchronous throw is classified separately in
// jsParallel via Runner.fatalCause(), which sees the original, never-
// stringified error recorded at its true point of origin and so doesn't
// need to parse text at all; see unwrapMapperThrow for why text recovered
// from a goja exception can't reliably carry sentinel identity regardless.
func isWorkflowFatalAgentError(err error) bool {
	if err == nil {
		return false
	}
	return isWorkflowInterruptedError(err) ||
		errors.Is(err, ErrWorkflowBudgetExhausted) ||
		errors.Is(err, ErrWorkflowMaxAgentsExceeded) ||
		errors.Is(err, ErrWorkflowRepoLockContended)
}

func isWorkflowStoppedError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrWorkflowStopped) || strings.TrimSpace(err.Error()) == ErrWorkflowStopped.Error()
}

func isWorkflowPausedError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrWorkflowPaused) || strings.TrimSpace(err.Error()) == ErrWorkflowPaused.Error()
}

func isWorkflowInterruptedError(err error) bool {
	return isWorkflowStoppedError(err) || isWorkflowPausedError(err)
}

func interruptedStatus(err error) string {
	if isWorkflowPausedError(err) {
		return "paused"
	}
	return "stopped"
}

func interruptedMessage(err error) string {
	if isWorkflowPausedError(err) {
		return ErrWorkflowPaused.Error()
	}
	return ErrWorkflowStopped.Error()
}

func (r *Runner) ensureNotStopped(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	run, err := r.Store.Run(r.Run.ID)
	if err != nil {
		return err
	}
	if run.Status == "stopped" {
		return r.recordFatal(ErrWorkflowStopped)
	}
	if run.Status == "paused" {
		return r.recordFatal(ErrWorkflowPaused)
	}
	return nil
}

func (r *Runner) contextWithStoredStop(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run, err := r.Store.Run(r.Run.ID)
				if err == nil && (run.Status == "stopped" || run.Status == "paused") {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

func (r *Runner) normalizeInterruptError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if isWorkflowInterruptedError(err) {
		return err
	}
	if ctx.Err() != nil {
		if run, runErr := r.Store.Run(r.Run.ID); runErr == nil && run.Status == "stopped" {
			return r.recordFatal(ErrWorkflowStopped)
		}
		if run, runErr := r.Store.Run(r.Run.ID); runErr == nil && run.Status == "paused" {
			return r.recordFatal(ErrWorkflowPaused)
		}
	}
	return err
}

// stageChainHint explains the single most common way a pipeline/parallel stage
// silently drops to null: chaining a JS promise combinator onto an agent()/
// check() call. During the capture pass those calls return a placeholder
// marker (not a real promise), so .then()/.catch() either throws a bare
// "Object has no member 'then'" TypeError or leaves an unresolved promise that
// awaitPromiseValue can't settle. Both surface here so the operator sees the
// fix instead of a cryptic goja error.
const stageChainHint = "did you chain .then()/.catch() on agent() or check()? return the agent()/check() call directly and post-process its result after the pipeline/parallel"

func awaitPromiseValue(value goja.Value) (goja.Value, error) {
	promise, ok := value.Export().(*goja.Promise)
	if !ok {
		return value, nil
	}
	switch promise.State() {
	case goja.PromiseStateFulfilled:
		return promise.Result(), nil
	case goja.PromiseStateRejected:
		return nil, fmt.Errorf("%s", promise.Result().String())
	default:
		return nil, fmt.Errorf("workflow stage returned an unresolved (pending) promise; %s", stageChainHint)
	}
}

// describeStageDrop turns a stage/mapper drop error into an operator-facing
// reason. Chaining .then()/.catch() on a captured agent()/check() marker
// throws a bare goja TypeError ("Object has no member 'then'"); this appends
// the actionable hint so a dropped item never carries only that cryptic text.
// Ordinary stage throws and provider failures pass through unchanged.
func describeStageDrop(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "has no member 'then'") || strings.Contains(msg, "has no member 'catch'") {
		return msg + " (" + stageChainHint + ")"
	}
	return msg
}

func parseAgentOutput(output string) any {
	var structured any
	if json.Unmarshal([]byte(output), &structured) == nil {
		return structured
	}
	return output
}

func parseAgentOutputWithSchema(output string, schema map[string]any) (any, error) {
	if len(schema) == 0 {
		return parseAgentOutput(output), nil
	}
	var structured any
	if err := json.Unmarshal([]byte(output), &structured); err != nil {
		return nil, fmt.Errorf("agent output does not match schema: output is not JSON: %w", err)
	}
	normalized, ok := normalizeSchema(schema).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("agent output schema must be an object")
	}
	if err := validateSchemaValue(structured, normalized, "$"); err != nil {
		return nil, fmt.Errorf("agent output does not match schema: %w", err)
	}
	return structured, nil
}

func validateSchemaValue(value any, schema map[string]any, path string) error {
	if rawType, ok := schema["type"]; ok && !schemaTypeMatches(value, rawType) {
		return fmt.Errorf("%s expected %s", path, schemaTypeName(rawType))
	}
	switch schemaConcreteType(value, schema["type"], schema) {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s expected object", path)
		}
		if rawRequired, ok := schema["required"].([]any); ok {
			for _, rawName := range rawRequired {
				name, ok := rawName.(string)
				if ok && name != "" {
					if _, exists := obj[name]; !exists {
						return fmt.Errorf("%s missing required property %q", path, name)
					}
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, child := range properties {
			if childSchema, ok := child.(map[string]any); ok {
				if childValue, exists := obj[name]; exists {
					if err := validateSchemaValue(childValue, childSchema, path+"."+name); err != nil {
						return err
					}
				}
			}
		}
		if allowAdditional, ok := schema["additionalProperties"].(bool); ok && !allowAdditional {
			for name := range obj {
				if _, known := properties[name]; !known {
					return fmt.Errorf("%s has unexpected property %q", path, name)
				}
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s expected array", path)
		}
		itemSchema, _ := schema["items"].(map[string]any)
		if itemSchema != nil {
			for i, item := range items {
				if err := validateSchemaValue(item, itemSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func schemaConcreteType(value any, rawType any, schema map[string]any) string {
	switch typed := rawType.(type) {
	case string:
		if singleSchemaTypeMatches(value, typed) {
			return typed
		}
	case []any:
		for _, option := range typed {
			text, ok := option.(string)
			if ok && singleSchemaTypeMatches(value, text) {
				return text
			}
		}
	}
	if _, ok := schema["properties"]; ok {
		return "object"
	}
	if _, ok := schema["required"]; ok {
		return "object"
	}
	if _, ok := schema["additionalProperties"]; ok {
		return "object"
	}
	if _, ok := schema["items"]; ok {
		return "array"
	}
	return ""
}

func schemaTypeMatches(value any, rawType any) bool {
	switch typed := rawType.(type) {
	case string:
		return singleSchemaTypeMatches(value, typed)
	case []any:
		for _, option := range typed {
			if text, ok := option.(string); ok && singleSchemaTypeMatches(value, text) {
				return true
			}
		}
	}
	return true
}

func singleSchemaTypeMatches(value any, typ string) bool {
	switch typ {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case float64, int, int64, json.Number:
			return true
		default:
			return false
		}
	case "integer":
		switch typed := value.(type) {
		case int, int64:
			return true
		case float64:
			return typed == float64(int64(typed))
		case json.Number:
			_, err := typed.Int64()
			return err == nil
		default:
			return false
		}
	case "null":
		return value == nil
	default:
		return true
	}
}

func schemaTypeName(rawType any) string {
	switch typed := rawType.(type) {
	case string:
		return typed
	case []any:
		names := make([]string, 0, len(typed))
		for _, value := range typed {
			if text, ok := value.(string); ok {
				names = append(names, text)
			}
		}
		return strings.Join(names, "|")
	default:
		return ""
	}
}

func agentSchemaHash(schema map[string]any) string {
	if len(schema) == 0 {
		return ""
	}
	return StableHash(normalizeSchema(schema))
}

// StableHash is a deterministic content hash (sha256 of the JSON encoding,
// or of a Go %#v dump when a value doesn't marshal) used anywhere two
// observations need comparing for equality without storing the full
// value: script/args cache-key hashing, untilGreen's stall-detection
// signature, and loops' own stagnation signature comparison (see
// loop_runtime.go's AdvanceLoopStagnation). Exported so cmd/loop.go can
// compute a script's hash for its start-time staleness stamp without
// duplicating a second hash implementation.
func StableHash(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = []byte(fmt.Sprintf("%#v", value))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func workflowAgentCostUSD() float64 {
	raw := strings.TrimSpace(os.Getenv("PALLIUM_WORKFLOW_AGENT_COST_USD"))
	if raw == "" {
		return 0.01
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		return 0.01
	}
	return value
}

func optionInt(options map[string]any, key string, fallback int) int {
	value, ok := options[key]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func optionString(options map[string]any, key, fallback string) string {
	value, ok := options[key]
	if !ok {
		return fallback
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return fallback
	}
	return strings.TrimSpace(text)
}

func checkOK(value any) bool {
	typed, ok := value.(map[string]any)
	if !ok {
		return false
	}
	okValue, ok := typed["ok"].(bool)
	return ok && okValue
}

func buildCheckPrompt(command string) string {
	rawCommand, _ := json.Marshal(command)
	return "Run this verification command exactly once in the target repo: " + string(rawCommand) + "\n" +
		"Do not edit source files. It is acceptable for the command to write normal ignored build, cache, or test artifacts. " +
		"Use the actual command result as ground truth. Return JSON with ok=true only if the command exits successfully. " +
		"Include a concise summary, the useful tail of output, and specific failing tests or errors when available."
}

func defaultCheckSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ok":          map[string]any{"type": "boolean"},
			"command":     map[string]any{"type": "string"},
			"summary":     map[string]any{"type": "string"},
			"output_tail": map[string]any{"type": "string"},
			"failures": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":    map[string]any{"type": "string"},
						"file":    map[string]any{"type": "string"},
						"message": map[string]any{"type": "string"},
					},
					"required": []any{"name", "message"},
				},
			},
		},
		"required": []any{"ok", "command", "summary", "output_tail", "failures"},
	}
}

func buildGatePrompt(name, message, criteria string) string {
	var b strings.Builder
	b.WriteString("You are an autonomous workflow gate verifier.\n")
	b.WriteString("Decide whether the workflow may continue through gate ")
	b.WriteString(strconv.Quote(name))
	b.WriteString(".\n")
	if strings.TrimSpace(message) != "" {
		b.WriteString("\nGate request:\n")
		b.WriteString(strings.TrimSpace(message))
		b.WriteString("\n")
	}
	if strings.TrimSpace(criteria) != "" {
		b.WriteString("\nApproval criteria:\n")
		b.WriteString(strings.TrimSpace(criteria))
		b.WriteString("\n")
	}
	b.WriteString("\nReturn JSON only. Set approved=true only when the criteria are satisfied. ")
	b.WriteString("If uncertain, set approved=false and explain the blocker in reason.")
	return b.String()
}

func defaultGateSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"approved": map[string]any{"type": "boolean"},
			"reason":   map[string]any{"type": "string"},
			"evidence": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []any{"approved", "reason"},
	}
}

func gateVerdict(value any) (bool, string) {
	result, ok := value.(map[string]any)
	if !ok {
		return false, "gate agent returned an invalid verdict"
	}
	approved, _ := result["approved"].(bool)
	reason, _ := result["reason"].(string)
	return approved, strings.TrimSpace(reason)
}

// validateUserIsolation guards the JS option boundary: scripts may only pick
// "" (default) or "worktree". "none" is reserved for internal Go callers like
// runUntilGreen; from a script it would let an edit agent write to the live
// repo with no worktree and no patch.
func validateUserIsolation(isolation string) error {
	switch strings.TrimSpace(isolation) {
	case "", "worktree":
		return nil
	default:
		return fmt.Errorf("invalid agent isolation %q: allowed values are \"\" and \"worktree\"", isolation)
	}
}

// validateUserNetwork guards the JS option boundary for the `network` opt:
// scripts may only pass a boolean (true or false). A non-boolean (string,
// number, object) is a script mistake and is rejected with a clear message
// instead of the opaque type error json.Unmarshal would raise.
func validateUserNetwork(raw any) error {
	opts, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	// goja preserves the JS property case, but json.Unmarshal into the Go
	// struct matches field names case-insensitively — so `{ Network: "yes" }`
	// still populates the bool field while a case-sensitive opts["network"]
	// lookup would miss it and skip validation. Check any key that spells
	// "network" regardless of case so a non-boolean can't slip past via
	// capitalization.
	for key, value := range opts {
		if !strings.EqualFold(strings.TrimSpace(key), "network") {
			continue
		}
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("invalid agent network %v: must be a boolean (true or false)", value)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func replaceParallelAgentMarkers(value any, agentResults []any) any {
	switch typed := value.(type) {
	case map[string]any:
		if rawIndex, ok := typed[parallelAgentMarkerKey]; ok {
			index, ok := numberToInt(rawIndex)
			if ok && index >= 0 && index < len(agentResults) {
				return agentResults[index]
			}
		}
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			out[key] = replaceParallelAgentMarkers(child, agentResults)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, child := range typed {
			out = append(out, replaceParallelAgentMarkers(child, agentResults))
		}
		return out
	default:
		return value
	}
}

func numberToInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	default:
		return 0, false
	}
}

// resolveAgentNetwork applies the run-level --allow-network ceiling to an
// agent's requested network setting and logs the outcome to stderr. An agent
// only gets egress when it opts in (network: true) AND the operator launched
// the run with --allow-network. If the agent asks but the ceiling is absent it
// runs sandboxed and a warning is logged. Every agent that actually runs with
// network enabled logs one greppable line so egress is auditable.
func (r *Runner) resolveAgentNetwork(agent *Agent, opts AgentOptions) bool {
	if !opts.Network {
		return false
	}
	label := firstNonEmpty(agent.Label, agent.ID)
	if !r.Run.AllowNetwork {
		fmt.Fprintf(os.Stderr, "[workflow:%s] agent %s requested network but run was not started with --allow-network; running sandboxed\n", r.Run.ID, label)
		return false
	}
	fmt.Fprintf(os.Stderr, "[workflow:%s] agent %s running with network access enabled\n", r.Run.ID, label)
	return true
}

func (r *Runner) agentTimeout(opts AgentOptions) time.Duration {
	seconds := r.AgentTimeoutSeconds
	if opts.TimeoutSeconds != nil {
		seconds = *opts.TimeoutSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func agentCommandError(ctx context.Context, timeout time.Duration, err error) error {
	if timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// Keep err (which may already lead with a meaningful provider error
		// line) instead of discarding it: a hung provider process is often
		// hung BECAUSE of the real failure, e.g. it kept waiting on stdin
		// after printing a quota error, so the underlying error is still the
		// most useful diagnostic even though the proximate cause of exit was
		// Pallium's own timeout killing it.
		return fmt.Errorf("workflow agent timed out after %ds: %w", int(timeout/time.Second), err)
	}
	return err
}

func (r *Runner) runAgentCommand(ctx context.Context, agent *Agent, opts AgentOptions) (string, string, string, error) {
	timeout := r.agentTimeout(opts)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if stub := os.Getenv("PALLIUM_WORKFLOW_AGENT_STUB"); stub != "" {
		if sequence := strings.TrimSpace(os.Getenv("PALLIUM_WORKFLOW_AGENT_STUB_SEQUENCE")); sequence != "" {
			var values []string
			if err := json.Unmarshal([]byte(sequence), &values); err != nil {
				return "", "", "", err
			}
			if len(values) > 0 {
				r.mu.Lock()
				index := r.stubIndex
				r.stubIndex++
				r.mu.Unlock()
				if index >= len(values) {
					index = len(values) - 1
				}
				stub = values[index]
			}
		}
		if delay := os.Getenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS"); delay != "" {
			ms, err := strconv.Atoi(delay)
			if err != nil {
				return "", "", "", err
			}
			select {
			case <-time.After(time.Duration(ms) * time.Millisecond):
			case <-ctx.Done():
				return "", "", "", agentCommandError(ctx, timeout, ctx.Err())
			}
		}
		repoRoot := firstNonEmpty(agent.Repo, r.Run.CWD)
		cwd := repoRoot
		worktree := ""
		editIntent := isEditIntentAgent(agent.Mode, agent.Isolation)
		// A prior edit-intent agent in this run may have already staged
		// changes (see mergeIntoStaging): ANY agent — edit-intent or not —
		// that needs to see them gets its OWN disposable worktree seeded from
		// staging, never a cwd pointed directly at the shared staging
		// directory (that would let a side-effecting "read-only" step
		// pollute it, and race a concurrent step touching the same worktree).
		stagingPath := r.stagingPathFor(repoRoot)
		if editIntent || stagingPath != "" {
			if editIntent {
				if err := r.acquireRepoLock(repoRoot); err != nil {
					return "", "", "", err
				}
			}
			var err error
			worktree, err = r.createWorktree(agent.ID, repoRoot)
			if err != nil {
				return "", "", "", err
			}
			// Registered immediately, before seedFromStaging (which can
			// itself fail on a merge conflict), so every error exit below
			// discards a staging-only worktree instead of leaking it —
			// mirrors the real dispatch path's containmentWorktree defer.
			// finalizeWorktreePatch (below) already discards it on the
			// success path for the non-edit case, so this defer is a no-op
			// there; it only fires on an early error return.
			if !editIntent {
				defer func() {
					r.removeWorktree(repoRoot, worktree)
				}()
			}
			if stagingPath != "" {
				if err := r.seedFromStaging(repoRoot, worktree); err != nil {
					return "", "", worktree, err
				}
			}
			cwd = worktree
		}
		if file := os.Getenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_FILE"); file != "" {
			target := filepath.Join(cwd, file)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", "", worktree, err
			}
			content := strings.ReplaceAll(os.Getenv("PALLIUM_WORKFLOW_AGENT_STUB_WRITE_CONTENT"), "{{PROMPT}}", agent.Prompt)
			if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
				return "", "", worktree, err
			}
		}
		// finalizeWorktreePatch discards a non-edit-intent worktree (the new
		// staging-seeded containment case included) and only captures a
		// patch for genuine edit intent, so no separate discard is needed
		// here for the staging-only case.
		patchPath, err := r.finalizeWorktreePatch(agent, worktree, repoRoot)
		if err != nil {
			return "", "", worktree, err
		}
		if editIntent && patchPath != "" {
			if err := r.mergeIntoStaging(repoRoot, patchPath); err != nil {
				return "", "", "", err
			}
		}
		return strings.ReplaceAll(stub, "{{PROMPT}}", agent.Prompt), patchPath, worktree, nil
	}
	provider := ResolveProvider(agent.Provider, opts.Provider)
	networkAllowed := r.resolveAgentNetwork(agent, opts)
	repoRoot := firstNonEmpty(agent.Repo, r.Run.CWD)
	cwd := repoRoot
	worktree := ""
	// containmentWorktree is set (and its discard defer registered) as soon
	// as a throwaway worktree is created below — before seedFromStaging,
	// which can itself fail — and stood down (see "Reached a clean provider
	// exit" below) once finalizeWorktreePatch takes over the worktree's
	// disposition on a successful provider call.
	containmentWorktree := false
	// editIntent is true only when the agent is meant to produce file changes
	// we keep: edit mode, or an explicit isolation:"worktree". Those are the
	// ONLY worktrees whose writes get captured as a patch and applied back to
	// the repo (see finalizeWorktreePatch).
	editIntent := isEditIntentAgent(agent.Mode, agent.Isolation)
	// A prior edit-intent agent in this run may have already staged changes
	// (see mergeIntoStaging). A non-edit, non-networked agent that would
	// otherwise run directly against the pristine repo instead gets its own
	// disposable worktree seeded from staging below — never a cwd pointed
	// directly at the shared staging directory, which would let a
	// side-effecting "read-only" step pollute it and race a concurrent step
	// touching the same worktree.
	stagingPath := r.stagingPathFor(repoRoot)
	// Granting network forces workspace-write (network egress requires it), so
	// a networked read-only/test/check agent would otherwise run fs-write in
	// the operator's LIVE checkout — writes to the real tree PLUS egress. When
	// an agent is actually granted network but has no isolated worktree, force
	// one (treat it like edit/worktree) so its forced workspace-write access is
	// contained to a throwaway worktree, never the live repo. That containment
	// worktree has no edit intent, so its writes are discarded, not applied.
	if networkAllowed || editIntent || stagingPath != "" {
		// The advisory repo lock is only for genuine edit intent: a
		// containment worktree's writes are always discarded, so it can never
		// clobber a concurrent run's edits. Acquired before createWorktree so
		// a losing run fails fast without touching disk.
		if editIntent {
			if err := r.acquireRepoLock(repoRoot); err != nil {
				return "", "", "", err
			}
		}
		var err error
		worktree, err = r.createWorktree(agent.ID, repoRoot)
		if err != nil {
			// A worktree forced purely to contain a networked worker or a
			// staging-only read (no edit intent) needs the run to sit inside
			// a git repo. Turn the raw git "not a git repository" failure
			// into actionable guidance instead of leaking an opaque `exit
			// status 128`.
			if !editIntent && strings.Contains(err.Error(), "not a git repository") {
				if networkAllowed {
					return "", "", "", fmt.Errorf("network access requires the run to execute inside a git repository so the worker can be isolated in a throwaway worktree (run directory %q is not a git repo); run from a git repository or drop network access: %w", repoRoot, err)
				}
				return "", "", "", fmt.Errorf("seeing this run's staged edits requires the run to execute inside a git repository (run directory %q is not a git repo): %w", repoRoot, err)
			}
			return "", "", "", err
		}
		// A worktree that exists ONLY to contain a networked or staging-only
		// non-edit worker holds throwaway writes. Registered immediately —
		// before seedFromStaging, which can itself fail on a merge conflict —
		// so EVERY error exit from here on (seed failure, command failure,
		// timeout, unreadable output) discards it instead of leaking it on
		// disk. Genuine edit/worktree agents are exempt: their tree is kept
		// for its patch or for post-failure debugging.
		containmentWorktree = !editIntent
		defer func() {
			if containmentWorktree {
				r.removeWorktree(repoRoot, worktree)
			}
		}()
		if stagingPath != "" {
			if err := r.seedFromStaging(repoRoot, worktree); err != nil {
				return "", "", worktree, err
			}
		}
		// `git worktree add` always checks out the WHOLE repo, so `worktree`
		// is the equivalent of the repo's top level, not of repoRoot itself.
		// When the run was launched from a subdirectory (repoRoot is that
		// subdirectory, not the git top level), landing the agent at
		// `worktree` would silently relocate it to the repo root and lose the
		// subdirectory it was meant to run in. worktreeSubdirCWD maps
		// repoRoot's offset from the real top level onto the new worktree; it
		// falls back to the worktree root if that mapping can't be computed
		// (e.g. `git rev-parse` fails), which reproduces today's behavior
		// rather than failing the run over a containment nicety.
		cwd = r.worktreeSubdirCWD(repoRoot, worktree)
	}

	tmpDir, err := os.MkdirTemp("", "pallium-workflow-agent-*")
	if err != nil {
		return "", "", worktree, err
	}
	defer os.RemoveAll(tmpDir)
	outFile := filepath.Join(tmpDir, "last-message.txt")
	usageFile := filepath.Join(tmpDir, "usage.json")
	runProvider := func(runCtx context.Context, runPrompt string) (string, error) {
		return r.runProviderCommand(runCtx, provider, tmpDir, outFile, usageFile, cwd, runPrompt, agent, opts, networkAllowed)
	}
	output, err := runProvider(ctx, agent.Prompt)
	usageRaw, usage := readAndRemoveAgentUsage(usageFile)
	// If the first attempt's own reported cost already exhausts the
	// budget, skip the (paid) corrective retry entirely rather than
	// billing a second call before the caller ever gets a chance to
	// check: the caller only applies this attempt's cost delta and
	// checks it against the budget after runAgentCommand returns, which
	// is too late to stop a retry that already started.
	firstAttemptOverBudget := false
	if cost, ok := usage["cost_usd"].(float64); ok && cost >= 0 {
		r.mu.Lock()
		firstAttemptOverBudget = r.budgetLimit > 0 && r.budgetSpent+(cost-r.agentCostUSD) > r.budgetLimit
		r.mu.Unlock()
	}
	if err == nil && len(opts.Schema) > 0 && !firstAttemptOverBudget {
		// The corrective retry re-executes the full provider command in
		// the same cwd with no workspace reset, so it is limited to
		// read-only agents. Edit/test/check agents could apply their side
		// effects twice; they fail schema validation immediately instead.
		if _, schemaErr := parseAgentOutputWithSchema(output, opts.Schema); schemaErr != nil && isReadOnlyAgentMode(agent.Mode) {
			_ = os.Remove(outFile)
			retryOutput, retryErr := runProvider(ctx, buildSchemaRetryPrompt(agent.Prompt, schemaErr))
			// Usage is read after each invocation (the file is removed in
			// between), so the retry's cost adds to attempt one instead of
			// overwriting it.
			if retryRaw, retryUsage := readAndRemoveAgentUsage(usageFile); retryUsage != nil {
				if usage == nil {
					usageRaw, usage = retryRaw, retryUsage
				} else {
					usage = mergeAgentUsage(usage, retryUsage)
					usageRaw = ""
				}
			}
			if retryErr != nil {
				// The retry itself failed (nonzero exit, timeout, etc.).
				// That failure is the real story here, not the stale
				// schema-invalid output from the first attempt: surface it
				// through the normal provider-command error path below
				// instead of silently re-validating attempt one's output.
				err = retryErr
			} else {
				output = retryOutput
			}
		}
	}
	if usage != nil {
		if usageRaw == "" {
			if raw, marshalErr := json.Marshal(usage); marshalErr == nil {
				usageRaw = string(raw)
			}
		}
		agent.UsageJSON = usageRaw
		if cost, ok := usage["cost_usd"].(float64); ok && cost >= 0 {
			agent.EstimatedCostUSD = cost
		}
	}
	if err != nil {
		return output, "", worktree, agentCommandError(ctx, timeout, err)
	}
	// Reached a clean provider exit: finalizeWorktreePatch now owns the
	// worktree's disposition (discard for containment, capture for edit), so
	// the error-path cleanup defer must stand down.
	containmentWorktree = false
	patchPath, err := r.finalizeWorktreePatch(agent, worktree, repoRoot)
	if err != nil {
		return output, "", worktree, err
	}
	if editIntent && patchPath != "" {
		if err := r.mergeIntoStaging(repoRoot, patchPath); err != nil {
			return output, patchPath, worktree, err
		}
	}
	return output, patchPath, worktree, nil
}

func (r *Runner) runConfiguredProviderCommand(ctx context.Context, command, tmpDir, outFile, usageFile, cwd, prompt string, agent *Agent, opts AgentOptions, networkAllowed bool) (string, error) {
	promptFile := filepath.Join(tmpDir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return "", err
	}
	schemaFile := ""
	if len(opts.Schema) > 0 {
		schemaFile = filepath.Join(tmpDir, "schema.json")
		normalizedSchema := normalizeSchema(opts.Schema)
		raw, err := json.MarshalIndent(normalizedSchema, "", "  ")
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(schemaFile, raw, 0o600); err != nil {
			return "", err
		}
	}
	// PALLIUM_WORKFLOW_NETWORK is the provider contract for egress: "1" only
	// when the agent requested network AND the run granted the ceiling,
	// otherwise "0". Wrappers use it to decide whether to grant networked
	// tools; the default is locked down.
	networkEnv := "0"
	if networkAllowed {
		networkEnv = "1"
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(),
		"PALLIUM_WORKFLOW_RUN_ID="+r.Run.ID,
		"PALLIUM_WORKFLOW_AGENT_ID="+agent.ID,
		"PALLIUM_WORKFLOW_PROVIDER="+agent.Provider,
		"PALLIUM_WORKFLOW_LABEL="+agent.Label,
		"PALLIUM_WORKFLOW_MODE="+agent.Mode,
		"PALLIUM_WORKFLOW_MODEL="+agent.Model,
		"PALLIUM_WORKFLOW_REPO="+agent.Repo,
		"PALLIUM_WORKFLOW_CWD="+cwd,
		// The prompt is passed ONLY via the file below, never inline in the
		// environment: a large prompt (a diff, a pasted log) would push the
		// exec argv+env block past ARG_MAX and fail the spawn with E2BIG
		// ("argument list too long"). Wrappers read PALLIUM_WORKFLOW_PROMPT_FILE.
		"PALLIUM_WORKFLOW_PROMPT_FILE="+promptFile,
		"PALLIUM_WORKFLOW_OUTPUT_FILE="+outFile,
		"PALLIUM_WORKFLOW_SCHEMA_FILE="+schemaFile,
		"PALLIUM_WORKFLOW_USAGE_FILE="+usageFile,
		"PALLIUM_WORKFLOW_NETWORK="+networkEnv,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		baseErr := formatProviderFailure(fmt.Sprintf("workflow provider %q", agent.Provider), err, stderr.String())
		return strings.TrimSpace(stdout.String()), wrapProviderCommandError(baseErr, stdout.String()+stderr.String())
	}
	raw, readErr := os.ReadFile(outFile)
	output := strings.TrimSpace(string(raw))
	if readErr != nil || output == "" {
		output = strings.TrimSpace(stdout.String())
	}
	if output == "" {
		return "", fmt.Errorf("workflow provider %q produced no output", agent.Provider)
	}
	return output, nil
}

func buildSchemaRetryPrompt(prompt string, schemaErr error) string {
	return prompt +
		"\n\nCORRECTION REQUIRED: your previous response failed schema validation: " + schemaErr.Error() +
		"\nRespond again with bare JSON only that conforms to the provided schema. No prose, no markdown, no code fences."
}

// isReadOnlyAgentMode reports whether an agent mode has no workspace side
// effects, which is the precondition for the corrective schema retry.
func isReadOnlyAgentMode(mode string) bool {
	mode = strings.TrimSpace(mode)
	return mode == "" || mode == "read-only"
}

// readAndRemoveAgentUsage reads and deletes the provider usage file so the
// next invocation writes a fresh one instead of overwriting this attempt.
// Unreadable or non-JSON files are ignored.
func readAndRemoveAgentUsage(path string) (string, map[string]any) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	_ = os.Remove(path)
	var usage map[string]any
	if json.Unmarshal(raw, &usage) != nil {
		return "", nil
	}
	return strings.TrimSpace(string(raw)), usage
}

// mergeAgentUsage sums numeric fields (cost and token counts) across provider
// invocations; non-numeric fields keep the latest value.
func mergeAgentUsage(total, next map[string]any) map[string]any {
	for key, value := range next {
		if num, ok := value.(float64); ok {
			if prev, ok := total[key].(float64); ok {
				total[key] = prev + num
				continue
			}
		}
		total[key] = value
	}
	return total
}

func providerCommandEnvName(provider string) string {
	normalized := regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(strings.ToUpper(strings.TrimSpace(provider)), "_")
	normalized = strings.Trim(normalized, "_")
	if normalized == "" {
		normalized = "DEFAULT"
	}
	return "PALLIUM_WORKFLOW_PROVIDER_" + normalized + "_COMMAND"
}

func normalizeSchema(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed)+1)
		for key, child := range typed {
			out[key] = normalizeSchema(child)
		}
		if out["type"] == "object" {
			if _, ok := out["additionalProperties"]; !ok {
				out["additionalProperties"] = false
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, child := range typed {
			out = append(out, normalizeSchema(child))
		}
		return out
	default:
		return value
	}
}

// worktreePath returns the deterministic worktree location for an agent or
// loop id under the run's artifact directory.
func (r *Runner) worktreePath(agentID string) (string, error) {
	root, err := RunArtifactDir(r.Run.ID, "worktrees")
	if err != nil {
		return "", err
	}
	return filepath.Join(root, agentID), nil
}

// worktreeSubdirCWD returns where an agent should run inside a freshly
// created worktree so a launch from a repo subdirectory keeps working from
// that same subdirectory rather than the repo's top level. `git worktree add`
// always checks out the entire repository, so `worktree` corresponds to the
// git top level, not to repoRoot: if repoRoot is itself a subdirectory (e.g.
// the run's CWD wasn't the repo root), the equivalent path inside the new
// worktree is worktree+<repoRoot's offset from the top level>, not worktree
// itself. Symlinks are resolved on both sides before computing that offset
// because git resolves them internally (macOS temp dirs are a common case:
// /var is a symlink to /private/var), and an unresolved mismatch would send
// filepath.Rel down the wrong path or report a spurious "..". Any failure
// (git not runnable, path outside the repo) falls back to the worktree root,
// which matches the pre-fix behavior instead of failing the run.
func (r *Runner) worktreeSubdirCWD(repoRoot, worktree string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return worktree
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return worktree
	}
	absRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return worktree
	}
	rel, err := filepath.Rel(top, absRepoRoot)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return worktree
	}
	return filepath.Join(worktree, rel)
}

// createWorktree's target path is deterministic per agentID (worktreePath),
// which is exactly right for a fresh call — but a team member reuses the
// SAME agentID (member.ID) on every turn, stable across retries. Found live
// via this batch's own kill/resume acceptance proof: killing the steering
// process mid-turn leaves that turn's worktree directory on disk (its own
// cleanup never got to run), and the member's NEXT turn's createWorktree
// call then fails outright — `git worktree add` refuses to target an
// existing directory — permanently blocking that member's edit mode until
// someone manually removes it. Pre-existing M1-era gap, not introduced by
// M2; fixed here because M2's own live proof is what surfaced it.
func (r *Runner) createWorktree(agentID, repoRoot string) (string, error) {
	path, err := r.worktreePath(agentID)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		// Accepted, documented risk (found by review, not fully solved
		// here): a team member's stale-takeover window presumes the OLD
		// turn is dead, same as every other stale-reconciliation in this
		// system (ReconcileInterruptedMembers reassigning a session token
		// or task ownership makes the identical presumption) — but unlike
		// those, this cleanup can be WRONG in a way that actively harms the
		// old turn if it merely exceeded --stale-after-minutes while
		// genuinely still running: it deletes that live process's worktree
		// out from under it mid-operation, instead of leaving it to fail
		// (or succeed and be discarded by FinishMemberTurn's own lease
		// check) on its own. Before this fix, the failure mode here was
		// clean (`git worktree add` refuses, THIS turn errors, the old one
		// is undisturbed); the tradeoff made here accepts a messier failure
		// for the old turn in exchange for actually fixing the far more
		// common case — the old turn's OWNING PROCESS was truly killed, not
		// just slow — which otherwise permanently blocks this member's edit
		// mode. A real fix needs per-turn liveness tracking for team
		// members (the 0.9.15 owner_pid/heartbeat pattern exists for
		// workflow runs, not team member turns) — bigger than this fix,
		// tracked as a backlog item rather than attempted here.
		r.removeWorktree(repoRoot, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("git", "worktree", "add", "--detach", path, "HEAD")
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("create worktree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return path, nil
}

// agentPatchPath returns the deterministic patch location for an agent or
// loop id under the run's artifact directory, creating the patches directory.
func (r *Runner) agentPatchPath(agentID string) (string, error) {
	patchDir, err := RunArtifactDir(r.Run.ID, "patches")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(patchDir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(patchDir, agentID+".patch"), nil
}

func (r *Runner) writeWorktreePatch(agentID, worktree string) (string, error) {
	addIntent := exec.Command("git", "add", "-N", "--", ".")
	addIntent.Dir = worktree
	var addStderr bytes.Buffer
	addIntent.Stderr = &addStderr
	if err := addIntent.Run(); err != nil {
		return "", fmt.Errorf("prepare worktree patch: %w: %s", err, strings.TrimSpace(addStderr.String()))
	}
	cmd := exec.Command("git", "diff", "--binary")
	cmd.Dir = worktree
	raw, err := cmd.Output()
	if err != nil {
		return "", err
	}
	patchPath, err := r.agentPatchPath(agentID)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(patchPath, raw, 0o644); err != nil {
		return "", err
	}
	return patchPath, nil
}

// finalizeWorktreePatch decides what happens to an agent's worktree once the
// worker exits. Only worktrees created for genuine edit intent (mode "edit" or
// isolation "worktree") have their writes captured as a patch for later apply.
//
// A worktree created ONLY to contain a networked worker's forced
// workspace-write filesystem access has no edit intent: its writes are
// throwaway, so they are discarded (worktree removed) and no patch is captured.
// Capturing here would defeat the containment guarantee — a prompt-injected
// networked read-only worker could drop a payload file that ApplyPatches would
// then git-apply onto the operator's live checkout at run completion.
func (r *Runner) finalizeWorktreePatch(agent *Agent, worktree, repoRoot string) (string, error) {
	if worktree == "" {
		return "", nil
	}
	if agent.Mode == "edit" || agent.Isolation == "worktree" {
		return r.captureWorktreePatch(agent.ID, worktree, repoRoot)
	}
	r.removeWorktree(repoRoot, worktree)
	return "", nil
}

// captureWorktreePatch writes the agent patch from the worktree and removes
// the worktree once the patch is captured; the patch file is the durable
// artifact. When patch capture fails the worktree is kept for debugging.
func (r *Runner) captureWorktreePatch(agentID, worktree, repoRoot string) (string, error) {
	patchPath, err := r.writeWorktreePatch(agentID, worktree)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[workflow:%s] patch capture failed; keeping worktree %s for debugging: %v\n", r.Run.ID, worktree, err)
		return "", err
	}
	r.removeWorktree(repoRoot, worktree)
	return patchPath, nil
}

// removeWorktree detaches and deletes an agent worktree. Failures fall back
// to deleting the directory and pruning stale git worktree metadata.
func (r *Runner) removeWorktree(repoRoot, worktree string) {
	if worktree == "" {
		return
	}
	cmd := exec.Command("git", "worktree", "remove", "--force", worktree)
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(worktree)
		prune := exec.Command("git", "worktree", "prune")
		prune.Dir = repoRoot
		_ = prune.Run()
	}
}

// isEditIntentAgent reports whether an agent's writes are meant to be kept:
// mode "edit", or an explicit isolation:"worktree". Those are the only
// worktrees whose writes get captured as a patch and eventually applied back
// to the real repo (see finalizeWorktreePatch), and the only agents that
// need the advisory repo lock (see acquireRepoLock) — a containment worktree
// created purely for a networked non-edit worker always discards its
// writes, so it can never conflict with a concurrent run's edits.
func isEditIntentAgent(mode, isolation string) bool {
	return isolation != "none" && (mode == "edit" || isolation == "worktree")
}

// repoLockStaleAfter bounds how long an edit run's advisory repo lock (see
// acquireRepoLock) survives without a refresh before another run may
// reclaim it. Every edit-intent agent in the holding run refreshes it, so a
// live run never goes stale; a crashed process leaves the row for this
// window before the repo becomes available again.
const repoLockStaleAfter = 30 * time.Minute

// acquireRepoLock takes this run's advisory edit lock on repoRoot's
// canonical git root, memoized in-process so repeated edit-intent agents in
// the same run skip the DB round trip after the first. A second run with
// edit-intent work already holding the lock fails fast here, before this
// call creates a worktree or spends a provider call.
func (r *Runner) acquireRepoLock(repoRoot string) error {
	canonical, err := gitlog.CanonicalRepoRoot(repoRoot)
	if err != nil {
		// Fall back to the raw repoRoot rather than failing the lock outright
		// — a repo Pallium can still operate on (e.g. a bare checkout without
		// a resolvable canonical root) must not be blocked by this fallback.
		// Logged because it silently diverges from the canonical key every
		// OTHER caller (including this run's own later agents) will use for
		// the SAME repo if a later call succeeds at canonicalizing it —
		// worth knowing about if two supposedly-identical repo roots ever
		// fail to share one lock.
		fmt.Fprintf(os.Stderr, "[workflow:%s] repo lock: could not canonicalize %s, using raw path: %v\n", r.Run.ID, repoRoot, err)
		canonical = repoRoot
	}
	r.lockMu.Lock()
	alreadyHeld := r.lockedRepos != nil && r.lockedRepos[canonical]
	r.lockMu.Unlock()
	if alreadyHeld {
		return nil
	}
	// The DB round trip (AcquireRepoLock retries on its own for up to ~200ms
	// under SQLITE_BUSY/contention, see repolock.go) runs without r.lockMu
	// held: a slow or contended acquire for one repo must never stall
	// dispatch of unrelated agents against a DIFFERENT repo, or even a fast
	// memoized re-entry for the SAME repo from a concurrent goroutine.
	holder, ok, err := r.Store.AcquireRepoLock(canonical, r.Run.ID, repoLockStaleAfter)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: another edit run holds %s (run %s)", ErrWorkflowRepoLockContended, canonical, holder)
	}
	r.lockMu.Lock()
	if r.lockedRepos == nil {
		r.lockedRepos = map[string]bool{}
	}
	r.lockedRepos[canonical] = true
	r.lockMu.Unlock()
	return nil
}

// releaseRepoLocks releases every repo lock this run acquired. Called
// unconditionally when Execute returns so a normal completion frees the repo
// immediately instead of waiting out the stale-lock timeout; a killed
// process leaves its row for repoLockStaleAfter to reclaim.
func (r *Runner) releaseRepoLocks() {
	r.lockMu.Lock()
	repos := make([]string, 0, len(r.lockedRepos))
	for repo := range r.lockedRepos {
		repos = append(repos, repo)
	}
	r.lockedRepos = nil
	r.lockMu.Unlock()
	for _, repo := range repos {
		if err := r.Store.ReleaseRepoLock(repo, r.Run.ID); err != nil {
			fmt.Fprintf(os.Stderr, "[workflow:%s] release repo lock %s: %v\n", r.Run.ID, repo, err)
		}
	}
}

// applyAndCommitDiff applies a previously captured diff into worktree via a
// three-way merge and, on success, commits it into worktree's own detached
// HEAD. Committing (rather than leaving the merge as an uncommitted
// working-tree change, which an earlier version of this function did via
// `git reset -q` to unstage what --3way auto-stages) is what lets a LATER
// call apply a second, independent patch touching the SAME file: `git apply
// --3way`'s fallback path requires the index to reflect a settled (matching)
// state to attempt a real three-way merge, and refuses outright with "does
// not match index" against a file with pre-existing UNSTAGED changes —
// discovered by a same-file regression test that failed even for two
// entirely non-overlapping edits, not just conflicting ones. A blank diff is
// a no-op. Explicit -c identity: a fresh `git worktree add --detach` carries
// no branch, so this commit floats free of any ref and is discarded along
// with the worktree; a repo with no user.name/user.email configured must not
// fail this commit only to break staging.
func applyAndCommitDiff(worktree string, diff []byte) error {
	if strings.TrimSpace(string(diff)) == "" {
		return nil
	}
	apply := exec.Command("git", "apply", "--3way")
	apply.Dir = worktree
	apply.Stdin = bytes.NewReader(diff)
	var applyStderr bytes.Buffer
	apply.Stderr = &applyStderr
	if err := apply.Run(); err != nil {
		return fmt.Errorf("apply staged edits: %w: %s", err, strings.TrimSpace(applyStderr.String()))
	}
	add := exec.Command("git", "add", "-A")
	add.Dir = worktree
	var addStderr bytes.Buffer
	add.Stderr = &addStderr
	if err := add.Run(); err != nil {
		return fmt.Errorf("stage applied edits: %w: %s", err, strings.TrimSpace(addStderr.String()))
	}
	commit := exec.Command("git", "-c", "user.email=pallium@localhost", "-c", "user.name=pallium",
		"commit", "-q", "-m", "pallium: staged edit")
	commit.Dir = worktree
	var commitStderr bytes.Buffer
	commit.Stderr = &commitStderr
	if err := commit.Run(); err != nil {
		return fmt.Errorf("commit applied edits: %w: %s", err, strings.TrimSpace(commitStderr.String()))
	}
	return nil
}

// resetWorktreeHard discards worktree's current uncommitted state — tracked
// changes, untracked files, and any unmerged/conflicted index entries left
// by a failed `git apply --3way` — back to its last commit. Used to recover
// a shared, long-lived worktree (staging) after a failed merge attempt:
// left in place, --3way's conflict markers and dirty index would be
// silently treated as real file content by every subsequent
// mergeIntoStaging/seedFromStaging call for the rest of the run. Since
// applyAndCommitDiff only ever advances HEAD on a fully successful merge,
// HEAD always names the exact state to recover to — no separate snapshot
// needed.
func resetWorktreeHard(worktree string) error {
	reset := exec.Command("git", "reset", "--hard", "HEAD")
	reset.Dir = worktree
	var resetStderr bytes.Buffer
	reset.Stderr = &resetStderr
	if err := reset.Run(); err != nil {
		return fmt.Errorf("reset to last known-good commit: %w: %s", err, strings.TrimSpace(resetStderr.String()))
	}
	clean := exec.Command("git", "clean", "-fd")
	clean.Dir = worktree
	var cleanStderr bytes.Buffer
	clean.Stderr = &cleanStderr
	if err := clean.Run(); err != nil {
		return fmt.Errorf("clean untracked: %w: %s", err, strings.TrimSpace(cleanStderr.String()))
	}
	return nil
}

// diffAgainstBase returns worktree's cumulative diff from base (the commit
// staging was originally created from) to its current HEAD. Every merge
// into staging is committed (see applyAndCommitDiff), so this is a plain
// two-commit diff — no working-tree/index trickery (`git add -N`, etc.)
// needed, unlike an accumulator that stays uncommitted.
func diffAgainstBase(worktree, base string) ([]byte, error) {
	cmd := exec.Command("git", "diff", "--binary", base, "HEAD")
	cmd.Dir = worktree
	return cmd.Output()
}

// currentHEAD returns worktree's current commit SHA.
func currentHEAD(worktree string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// stagingWorktree is the run-scoped mirror of one real repo root, holding
// every edit-intent agent's accumulated changes (each committed in turn, see
// applyAndCommitDiff) for the lifetime of a single Execute call. base is the
// commit staging was originally created from — fixed for the run, since
// diffAgainstBase needs a stable reference point regardless of how many
// merges have since advanced HEAD. Its own mutex serializes creation and
// merges for that repo, so a concurrent edit agent's mergeIntoStaging and a
// concurrent non-edit step's seedFromStaging read never race against each
// other on the SAME staging directory (see runAgentCommand: a non-edit step
// never runs directly against this path — it always gets its own ephemeral
// worktree seeded FROM this one, so only staging's own bookkeeping needs
// this lock, never a live agent process).
type stagingWorktree struct {
	mu   sync.Mutex
	path string
	base string
}

// stagingEntry returns (creating if necessary) the bookkeeping slot for
// repoRoot's staging worktree. Only the map lookup is guarded by stagingMu;
// callers lock the returned entry's own mutex for the actual work.
func (r *Runner) stagingEntry(repoRoot string) *stagingWorktree {
	r.stagingMu.Lock()
	defer r.stagingMu.Unlock()
	if r.staging == nil {
		r.staging = map[string]*stagingWorktree{}
	}
	entry, ok := r.staging[repoRoot]
	if !ok {
		entry = &stagingWorktree{}
		r.staging[repoRoot] = entry
	}
	return entry
}

// stagingPathFor returns the run's staging worktree path for repoRoot, or ""
// if no edit-intent agent has completed against repoRoot yet in this run.
// Callers never run a live step's cwd against this path directly (that
// would share one working directory across concurrent steps and let a
// side-effecting "read-only" step pollute it — see runAgentCommand); it
// exists only to seed a fresh, disposable worktree via seedFromStaging.
func (r *Runner) stagingPathFor(repoRoot string) string {
	entry := r.stagingEntry(repoRoot)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.path
}

// mergeIntoStaging lazily creates this run's staging worktree for repoRoot
// (branched from repoRoot's HEAD, exactly like any other agent worktree) and
// cumulatively applies patchPath into it, so later steps in THIS run that
// consult stagingPathFor (via seedFromStaging into their own disposable
// worktree) see the edit without waiting for the whole workflow to succeed.
// A blank patchPath is a no-op beyond creating the staging worktree.
func (r *Runner) mergeIntoStaging(repoRoot, patchPath string) error {
	entry := r.stagingEntry(repoRoot)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.path == "" {
		created, err := r.createWorktree(stagingWorktreeID(repoRoot), repoRoot)
		if err != nil {
			return err
		}
		base, err := currentHEAD(created)
		if err != nil {
			return err
		}
		entry.path = created
		entry.base = base
	}
	if patchPath == "" {
		return nil
	}
	raw, err := os.ReadFile(patchPath)
	if err != nil {
		return err
	}
	if err := applyAndCommitDiff(entry.path, raw); err != nil {
		// A failed --3way apply (two edit-intent agents touching the same
		// lines) leaves conflict markers and a dirty index behind — must not
		// leave the shared, long-lived staging worktree corrupted for every
		// later read this run. applyAndCommitDiff only advances HEAD on a
		// fully successful merge, so resetting hard to HEAD always recovers
		// exactly the last known-good state; the conflict then surfaces as a
		// clean, actionable error instead of silent corruption.
		if resetErr := resetWorktreeHard(entry.path); resetErr != nil {
			return fmt.Errorf("%w (additionally failed to restore staging to its last known-good state: %v)", err, resetErr)
		}
		return err
	}
	return nil
}

// stagingWorktreeID deterministically names a repo root's staging worktree
// under the run's artifact directory, so resume recreates the same path
// without any clock- or randomness-derived identity.
func stagingWorktreeID(repoRoot string) string {
	return "staging-" + StableHash(repoRoot)[:16]
}

// seedFromStaging layers this run's current accumulated staging edits (if
// any) onto a freshly created, disposable worktree — an edit agent's own
// worktree, a non-edit step's throwaway containment worktree, or
// verify.untilGreen's persistent fix-loop worktree — so it branches from
// prior edits in this run instead of the pristine checkout. The seed is
// committed into the target worktree's own detached HEAD (never touching any
// branch, since every worktree here is created with `git worktree add
// --detach`) rather than left as an unstaged working-tree diff. For an edit
// agent specifically, that advances the baseline its OWN later patch capture
// (`git diff` against its index/HEAD) diffs against, so that capture reports
// only the agent's own incremental delta — not the seed it started from.
// Without this, two edit agents' captured patches would both claim the same
// seeded file change, and the second would fail to apply sequentially onto
// the real repo at workflow success. A non-edit step's worktree is always
// discarded afterward (see runAgentCommand's containmentWorktree cleanup),
// so the commit there is just a convenient way to apply the seed — nothing
// downstream ever reads it as a patch.
func (r *Runner) seedFromStaging(repoRoot, worktree string) error {
	entry := r.stagingEntry(repoRoot)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.path == "" {
		return nil
	}
	diff, err := diffAgainstBase(entry.path, entry.base)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(diff)) == "" {
		return nil
	}
	apply := exec.Command("git", "apply", "--3way")
	apply.Dir = worktree
	apply.Stdin = bytes.NewReader(diff)
	var applyStderr bytes.Buffer
	apply.Stderr = &applyStderr
	if err := apply.Run(); err != nil {
		return fmt.Errorf("seed worktree from staging: %w: %s", err, strings.TrimSpace(applyStderr.String()))
	}
	add := exec.Command("git", "add", "-A")
	add.Dir = worktree
	if err := add.Run(); err != nil {
		return err
	}
	// Explicit -c identity: a fresh worktree add --detach carries no branch,
	// so this commit floats free of any ref and is discarded along with the
	// worktree; a repo with no user.name/user.email configured must not fail
	// this commit only to break resume-independent seeding.
	commit := exec.Command("git", "-c", "user.email=pallium@localhost", "-c", "user.name=pallium",
		"commit", "-q", "-m", "pallium: seed from this run's staged edits")
	commit.Dir = worktree
	var commitStderr bytes.Buffer
	commit.Stderr = &commitStderr
	if err := commit.Run(); err != nil {
		return fmt.Errorf("commit seeded worktree state: %w: %s", err, strings.TrimSpace(commitStderr.String()))
	}
	return nil
}

// removeStagingWorktrees deletes every staging worktree this run created.
// Called unconditionally when Execute returns (success, failure, or
// interrupt) so a run never leaves a run-scoped worktree registered once its
// own per-agent worktrees are gone.
func (r *Runner) removeStagingWorktrees() {
	r.stagingMu.Lock()
	entries := r.staging
	r.staging = nil
	r.stagingMu.Unlock()
	for repoRoot, entry := range entries {
		entry.mu.Lock()
		if entry.path != "" {
			r.removeWorktree(repoRoot, entry.path)
		}
		entry.mu.Unlock()
	}
}

var metaPrefixRe = regexp.MustCompile(`export\s+const\s+meta\s*=\s*\{`)

// findMetaBlock locates the FIRST `export const meta = {...}` declaration
// in script by scanning brace depth from the opening brace (so a meta
// object with nested objects, like loop config, matches its own closing
// brace correctly rather than the first `}` encountered). literal is the
// object expression text INCLUDING its braces (suitable for evaluating
// directly, e.g. wrapped in parens); rest is the script with that exact
// declaration (through a trailing `;` and whitespace) removed. found is
// false when no declaration is present, or when one is present but
// unbalanced (never terminates) — the caller is expected to fail
// compilation on the latter the same way it always has, not to special-case
// this function's inability to make sense of malformed input.
func findMetaBlock(script string) (literal, rest string, found bool) {
	loc := metaPrefixRe.FindStringIndex(script)
	if loc == nil {
		return "", script, false
	}
	depth := 1
	end := -1
	for i := loc[1]; i < len(script); i++ {
		switch script[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i + 1
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return "", script, false
	}
	// loc[1]-1 is the index of the opening '{' (metaPrefixRe's match itself
	// ends just past it), so literal spans exactly the object expression.
	literal = script[loc[1]-1 : end]
	trimEnd := end
	for trimEnd < len(script) && (script[trimEnd] == ' ' || script[trimEnd] == '\t' || script[trimEnd] == '\n' || script[trimEnd] == '\r') {
		trimEnd++
	}
	if trimEnd < len(script) && script[trimEnd] == ';' {
		trimEnd++
	}
	rest = script[:loc[0]] + script[trimEnd:]
	return literal, rest, true
}

// stripMeta removes every export const meta = {...}; block (looping since
// findMetaBlock only strips the first occurrence per call) so goja never
// sees the ES module `export` syntax it doesn't support. An unbalanced meta
// block is left untouched and fails compilation, same as before this was
// refactored to share findMetaBlock with extractMetaLiteral.
func stripMeta(script string) string {
	for {
		_, rest, found := findMetaBlock(script)
		if !found {
			return script
		}
		script = rest
	}
}

// extractMetaLiteral returns the FIRST meta block's object-expression text
// (see findMetaBlock), or ok=false if the script declares none. This is the
// kernel-level half of meta handling — any service can use it to check
// whether a script opts into that service's own kind (see loop_meta.go for
// how loops interpret meta.kind=="loop"). It does not evaluate the literal;
// evaluating arbitrary script-authored JS is the caller's decision to make
// (and loop_meta.go's decision to do, via a throwaway goja VM), not this
// general-purpose extraction step's.
func extractMetaLiteral(script string) (literal string, ok bool) {
	literal, _, found := findMetaBlock(script)
	return literal, found
}

func stringifyResult(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		raw, err := json.MarshalIndent(typed, "", "  ")
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(raw)
	}
}

func DefaultScript(task string) string {
	escaped, _ := json.Marshal(task)
	return `export const meta = { name: "generated", description: "Default Pallium dynamic workflow", phases: ["plan", "verify"] };
phase("plan");
const plan = await agent("Create a concise workflow plan for this task. Return JSON with keys summary, steps, risks. Task: " + ` + string(escaped) + `, {
  label: "planner",
  mode: "read-only",
  schema: {
    type: "object",
    properties: {
      summary: { type: "string" },
      steps: { type: "array", items: { type: "string" } },
      risks: { type: "array", items: { type: "string" } }
    },
    required: ["summary", "steps", "risks"]
  }
});
phase("verify");
const verified = await agent("Review this plan for missing safety or verification steps and return JSON with keys verdict and notes: " + JSON.stringify(plan), {
  label: "verifier",
  mode: "read-only",
  schema: {
    type: "object",
    properties: {
      verdict: { type: "string" },
      notes: { type: "array", items: { type: "string" } }
    },
    required: ["verdict", "notes"]
  }
});
return { plan, verified };`
}

func WriteRunScript(runID, cwd, script string) (string, error) {
	dir, err := RunArtifactDir(runID, "")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "workflow.js")
	return path, os.WriteFile(path, []byte(script), 0o644)
}

func ResolveSavedWorkflow(cwd, name string) (string, error) {
	name = strings.TrimSpace(strings.TrimPrefix(name, "/"))
	name = strings.TrimSuffix(name, ".js")
	if err := ValidateID(name); err != nil {
		return "", err
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	for {
		for _, dir := range []string{".pallium", ".claude"} {
			candidate := filepath.Join(abs, dir, "workflows", name+".js")
			if isFile(candidate) {
				return candidate, nil
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	home, err := os.UserHomeDir()
	if err == nil {
		for _, dir := range []string{".pallium", ".claude"} {
			candidate := filepath.Join(home, dir, "workflows", name+".js")
			if isFile(candidate) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("saved workflow %q not found", name)
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// RunsRootDir returns the root directory that holds per-run workflow
// artifacts (~/.pallium/workflow-runs).
func RunsRootDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pallium", "workflow-runs"), nil
}

func RunArtifactDir(runID, child string) (string, error) {
	if err := ValidateID(runID); err != nil {
		return "", err
	}
	root, err := RunsRootDir()
	if err != nil {
		return "", err
	}
	parts := []string{root, runID}
	if strings.TrimSpace(child) != "" {
		parts = append(parts, filepath.Clean(child))
	}
	return filepath.Join(parts...), nil
}

func ParseArgsJSON(raw string) (any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Runner) ApplyPatches(ctx context.Context) ([]string, error) {
	if err := r.ensureNotStopped(ctx); err != nil {
		return nil, err
	}
	snapshot, err := r.Store.Snapshot(r.Run.ID)
	if err != nil {
		return nil, err
	}
	return ApplyPatches(ctx, snapshot)
}

func ApplyPatches(ctx context.Context, snapshot Snapshot) ([]string, error) {
	applied := []string{}
	for _, agent := range snapshot.Agents {
		if agent.PatchPath == "" {
			continue
		}
		if agent.Status != "completed" {
			continue
		}
		targetRepo := firstNonEmpty(agent.Repo, snapshot.Run.CWD)
		didApply, err := applyPatch(ctx, targetRepo, agent.PatchPath)
		if err != nil {
			return applied, err
		}
		if didApply {
			applied = append(applied, agent.PatchPath)
		}
	}
	return applied, nil
}

func RevertPatches(ctx context.Context, snapshot Snapshot) ([]string, error) {
	reverted := []string{}
	for i := len(snapshot.Agents) - 1; i >= 0; i-- {
		agent := snapshot.Agents[i]
		if agent.PatchPath == "" {
			continue
		}
		targetRepo := firstNonEmpty(agent.Repo, snapshot.Run.CWD)
		didRevert, err := revertPatch(ctx, targetRepo, agent.PatchPath)
		if err != nil {
			return reverted, err
		}
		if didRevert {
			reverted = append(reverted, agent.PatchPath)
		}
	}
	return reverted, nil
}

func applyPatch(ctx context.Context, cwd, patchPath string) (bool, error) {
	raw, err := os.ReadFile(patchPath)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return false, nil
	}
	if os.Getenv("PALLIUM_WORKFLOW_ALLOW_SECRET_PATCH") != "1" {
		if findings := ScanPatchPolicy(raw); len(findings) > 0 {
			return false, fmt.Errorf("workflow patch policy blocked %s: %s", patchPath, renderPolicyFindings(findings))
		}
	}
	if err := runGitApplyCheck(ctx, cwd, patchPath, false); err != nil {
		if reverseErr := runGitApplyCheck(ctx, cwd, patchPath, true); reverseErr == nil {
			return false, nil
		}
		return false, err
	}
	cmd := exec.CommandContext(ctx, "git", "apply", "--3way", patchPath)
	cmd.Dir = cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("apply %s: %w: %s", patchPath, err, strings.TrimSpace(stderr.String()))
	}
	return true, nil
}

func ScanPatchPolicy(raw []byte) []PolicyFinding {
	patterns := []struct {
		kind string
		re   *regexp.Regexp
		msg  string
	}{
		{kind: "openai-key", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), msg: "added line looks like an OpenAI-style API key"},
		{kind: "aws-access-key", re: regexp.MustCompile(`\bA(KIA|SIA)[A-Z0-9]{16}\b`), msg: "added line looks like an AWS access key"},
		{kind: "generic-secret", re: regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password)\s*[:=]\s*['"]?[A-Za-z0-9_./+=-]{16,}`), msg: "added line looks like a hard-coded secret"},
	}
	var findings []PolicyFinding
	for index, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		added := strings.TrimPrefix(line, "+")
		for _, pattern := range patterns {
			if pattern.re.MatchString(added) {
				findings = append(findings, PolicyFinding{Kind: pattern.kind, Line: index + 1, Message: pattern.msg})
			}
		}
	}
	return findings
}

func renderPolicyFindings(findings []PolicyFinding) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, fmt.Sprintf("%s at patch line %d", finding.Kind, finding.Line))
	}
	return strings.Join(parts, "; ")
}

func revertPatch(ctx context.Context, cwd, patchPath string) (bool, error) {
	raw, err := os.ReadFile(patchPath)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return false, nil
	}
	if err := runGitApplyCheck(ctx, cwd, patchPath, true); err != nil {
		if forwardErr := runGitApplyCheck(ctx, cwd, patchPath, false); forwardErr == nil {
			return false, nil
		}
		return false, err
	}
	cmd := exec.CommandContext(ctx, "git", "apply", "--reverse", "--3way", patchPath)
	cmd.Dir = cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("git apply --reverse %s: %w: %s", patchPath, err, strings.TrimSpace(stderr.String()))
	}
	return true, nil
}

func runGitApplyCheck(ctx context.Context, cwd, patchPath string, reverse bool) error {
	args := []string{"apply", "--check"}
	if reverse {
		args = append(args, "--reverse")
	}
	args = append(args, patchPath)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("check apply %s: %w: %s", patchPath, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
