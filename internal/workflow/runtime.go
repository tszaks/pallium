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
)

var (
	ErrWorkflowStopped           = errors.New("workflow stopped")
	ErrWorkflowPaused            = errors.New("workflow paused")
	ErrWorkflowBudgetExhausted   = errors.New("workflow budget exhausted")
	ErrWorkflowMaxAgentsExceeded = errors.New("workflow exceeded max agents")
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

	mu             sync.Mutex
	failuresMu     sync.Mutex
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
	currentHash := stableHash(script)
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
	r.scriptHash = stableHash(script)
	r.argsHash = stableHash(args)
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
			_ = r.Store.SetRunStatus(r.Run.ID, "failed", "", err.Error())
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
			_ = r.Store.SetRunStatus(r.Run.ID, "failed", "", err.Error())
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
			_ = r.Store.SetRunStatus(r.Run.ID, "failed", "", err.Error())
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
		signature := stableHash(checkResult)
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
	if _, ok, err := r.Store.CompletedAgent(r.Run.ID, callIndex, phase, patchLabel, prompt, "internal", repoRoot, "edit", "worktree", "", "", argsHash, false); err != nil {
		return err
	} else if ok {
		r.removeWorktree(repoRoot, worktree)
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
				value, err := fn(goja.Undefined())
				if err != nil {
					panic(err)
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
	if _, err := parseAgentOutputWithSchema(output, opts.Schema); err != nil {
		agent.PatchPath = ""
		_ = r.Store.FinishAgent(agent, output, err.Error())
		return "", err
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
		errors.Is(err, ErrWorkflowMaxAgentsExceeded)
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
	return stableHash(normalizeSchema(schema))
}

func stableHash(value any) string {
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
		if agent.Isolation != "none" && (agent.Mode == "edit" || agent.Isolation == "worktree") {
			var err error
			worktree, err = r.createWorktree(agent.ID, repoRoot)
			if err != nil {
				return "", "", "", err
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
		patchPath, err := r.finalizeWorktreePatch(agent, worktree, repoRoot)
		if err != nil {
			return "", "", worktree, err
		}
		return strings.ReplaceAll(stub, "{{PROMPT}}", agent.Prompt), patchPath, worktree, nil
	}
	provider := ResolveProvider(agent.Provider, opts.Provider)
	networkAllowed := r.resolveAgentNetwork(agent, opts)
	repoRoot := firstNonEmpty(agent.Repo, r.Run.CWD)
	cwd := repoRoot
	worktree := ""
	// editIntent is true only when the agent is meant to produce file changes
	// we keep: edit mode, or an explicit isolation:"worktree". Those are the
	// ONLY worktrees whose writes get captured as a patch and applied back to
	// the repo (see finalizeWorktreePatch).
	editIntent := agent.Isolation != "none" && (agent.Mode == "edit" || agent.Isolation == "worktree")
	// Granting network forces workspace-write (network egress requires it), so
	// a networked read-only/test/check agent would otherwise run fs-write in
	// the operator's LIVE checkout — writes to the real tree PLUS egress. When
	// an agent is actually granted network but has no isolated worktree, force
	// one (treat it like edit/worktree) so its forced workspace-write access is
	// contained to a throwaway worktree, never the live repo. That containment
	// worktree has no edit intent, so its writes are discarded, not applied.
	if networkAllowed || editIntent {
		var err error
		worktree, err = r.createWorktree(agent.ID, repoRoot)
		if err != nil {
			// A worktree forced purely to contain a networked worker (no edit
			// intent) needs the run to sit inside a git repo. Turn the raw git
			// "not a git repository" failure into actionable guidance instead
			// of leaking an opaque `exit status 128`.
			if networkAllowed && !editIntent && strings.Contains(err.Error(), "not a git repository") {
				return "", "", "", fmt.Errorf("network access requires the run to execute inside a git repository so the worker can be isolated in a throwaway worktree (run directory %q is not a git repo); run from a git repository or drop network access: %w", repoRoot, err)
			}
			return "", "", "", err
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

	// A worktree that exists ONLY to contain a networked non-edit worker holds
	// throwaway writes. The success paths below discard it via
	// finalizeWorktreePatch; on any error exit (setup failure, command failure,
	// timeout, unreadable output) the early returns would otherwise leak that
	// worktree on disk. Discard it on those error exits too. Genuine
	// edit/worktree agents are exempt: their tree is kept for its patch or for
	// post-failure debugging.
	containmentWorktree := worktree != "" && !editIntent
	defer func() {
		if containmentWorktree {
			r.removeWorktree(repoRoot, worktree)
		}
	}()

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
		baseErr := fmt.Errorf("workflow provider %q failed: %w: %s", agent.Provider, err, strings.TrimSpace(stderr.String()))
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

func (r *Runner) createWorktree(agentID, repoRoot string) (string, error) {
	path, err := r.worktreePath(agentID)
	if err != nil {
		return "", err
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

var metaPrefixRe = regexp.MustCompile(`export\s+const\s+meta\s*=\s*\{`)

// stripMeta removes the export const meta = {...}; block by scanning brace
// depth from the opening brace, so meta objects with nested objects strip
// cleanly. An unbalanced meta block is left untouched and fails compilation.
func stripMeta(script string) string {
	for {
		loc := metaPrefixRe.FindStringIndex(script)
		if loc == nil {
			return script
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
			return script
		}
		for end < len(script) && (script[end] == ' ' || script[end] == '\t' || script[end] == '\n' || script[end] == '\r') {
			end++
		}
		if end < len(script) && script[end] == ';' {
			end++
		}
		script = script[:loc[0]] + script[end:]
	}
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
