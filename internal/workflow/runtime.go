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
	ErrWorkflowStopped = errors.New("workflow stopped")
	ErrWorkflowPaused  = errors.New("workflow paused")
)

type Runner struct {
	Store               *Store
	Run                 Run
	MaxAgents           int
	MaxBudgetUSD        string
	CodexBinary         string
	MaxConcurrentAgents int
	PalliumBinary       string

	mu           sync.Mutex
	currentPhase string
	agentCount   int
	budgetLimit  float64
	budgetSpent  float64
	agentCostUSD float64
	capture      *parallelCapture
	scriptHash   string
	argsHash     string
}

type AgentOptions struct {
	Label     string         `json:"label,omitempty"`
	Provider  string         `json:"provider,omitempty"`
	Mode      string         `json:"mode,omitempty"`
	Isolation string         `json:"isolation,omitempty"`
	Schema    map[string]any `json:"schema,omitempty"`
	Model     string         `json:"model,omitempty"`
}

type CheckOptions struct {
	Label    string         `json:"label,omitempty"`
	Provider string         `json:"provider,omitempty"`
	Model    string         `json:"model,omitempty"`
	Schema   map[string]any `json:"schema,omitempty"`
}

type parallelAgentCall struct {
	Prompt string
	Opts   AgentOptions
}

type parallelCapture struct {
	Calls []parallelAgentCall
}

const parallelAgentMarkerKey = "__pallium_parallel_agent__"

func (r *Runner) Execute(ctx context.Context, script string, args any) (string, error) {
	if r.Store == nil {
		return "", fmt.Errorf("workflow store is required")
	}
	if r.Run.ID == "" {
		return "", fmt.Errorf("workflow run is required")
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
	if strings.TrimSpace(r.MaxBudgetUSD) != "" {
		limit, err := strconv.ParseFloat(strings.TrimSpace(r.MaxBudgetUSD), 64)
		if err != nil || limit < 0 {
			return "", fmt.Errorf("invalid max budget usd %q", r.MaxBudgetUSD)
		}
		r.budgetLimit = limit
		r.agentCostUSD = workflowAgentCostUSD()
	}
	if err := r.Store.SetRunStatus(r.Run.ID, "running", "", ""); err != nil {
		return "", err
	}
	r.scriptHash = stableHash(script)
	r.argsHash = stableHash(args)
	if err := r.ensureNotStopped(ctx); err != nil {
		return "", err
	}

	body := stripMeta(script)
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	if err := vm.Set("args", args); err != nil {
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
	if err := vm.Set("pallium", r.jsPallium(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("parallel", r.jsParallel(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("pipeline", r.jsPipeline(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("workflow", func(name string, input any) any {
		panic(vm.ToValue(fmt.Sprintf("nested workflow %q is not supported in this v1 runtime", name)))
	}); err != nil {
		return "", err
	}

	value, err := vm.RunString("(async function(){\n" + body + "\n})()")
	if r.currentPhase != "" {
		_ = r.Store.FinishPhase(r.Run.ID, r.currentPhase)
		r.currentPhase = ""
	}
	if err != nil {
		if isWorkflowStoppedError(err) {
			_ = r.Store.SetRunStatus(r.Run.ID, "stopped", "", ErrWorkflowStopped.Error())
			return "", ErrWorkflowStopped
		}
		if isWorkflowPausedError(err) {
			_ = r.Store.SetRunStatus(r.Run.ID, "paused", "", ErrWorkflowPaused.Error())
			return "", ErrWorkflowPaused
		}
		_ = r.Store.SetRunStatus(r.Run.ID, "failed", "", err.Error())
		return "", err
	}
	value, err = awaitPromiseValue(value)
	if err != nil {
		if isWorkflowStoppedError(err) {
			_ = r.Store.SetRunStatus(r.Run.ID, "stopped", "", ErrWorkflowStopped.Error())
			return "", ErrWorkflowStopped
		}
		if isWorkflowPausedError(err) {
			_ = r.Store.SetRunStatus(r.Run.ID, "paused", "", ErrWorkflowPaused.Error())
			return "", ErrWorkflowPaused
		}
		_ = r.Store.SetRunStatus(r.Run.ID, "failed", "", err.Error())
		return "", err
	}
	if err := r.ensureNotStopped(ctx); err != nil {
		_ = r.Store.SetRunStatus(r.Run.ID, interruptedStatus(err), "", interruptedMessage(err))
		return "", err
	}
	if _, err := r.ApplyPatches(ctx); err != nil {
		if isWorkflowInterruptedError(err) {
			_ = r.Store.SetRunStatus(r.Run.ID, interruptedStatus(err), "", interruptedMessage(err))
			return "", err
		}
		_ = r.Store.SetRunStatus(r.Run.ID, "failed", "", err.Error())
		return "", err
	}
	if err := r.ensureNotStopped(ctx); err != nil {
		_ = r.Store.SetRunStatus(r.Run.ID, interruptedStatus(err), "", interruptedMessage(err))
		return "", err
	}
	result := value.Export()
	resultText := stringifyResult(result)
	if err := r.Store.SetRunStatus(r.Run.ID, "completed", resultText, ""); err != nil {
		return "", err
	}
	return resultText, nil
}

func (r *Runner) jsPhase(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		name := strings.TrimSpace(call.Argument(0).String())
		if name == "" {
			name = "default"
		}
		previous := r.currentPhase
		if previous != "" {
			_ = r.Store.FinishPhase(r.Run.ID, previous)
		}
		r.currentPhase = name
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
				r.currentPhase = previous
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
			raw, err := json.Marshal(call.Argument(1).Export())
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			if err := json.Unmarshal(raw, &opts); err != nil {
				panic(vm.ToValue(err.Error()))
			}
		}
		if r.capture != nil {
			index := len(r.capture.Calls)
			r.capture.Calls = append(r.capture.Calls, parallelAgentCall{Prompt: prompt, Opts: opts})
			return vm.ToValue(map[string]any{parallelAgentMarkerKey: index})
		}
		output, err := r.RunAgent(ctx, prompt, opts)
		if err != nil {
			panic(vm.ToValue(err.Error()))
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
		if r.capture != nil {
			index := len(r.capture.Calls)
			r.capture.Calls = append(r.capture.Calls, parallelAgentCall{Prompt: prompt, Opts: agentOpts})
			return vm.ToValue(map[string]any{parallelAgentMarkerKey: index})
		}
		output, err := r.RunAgent(ctx, prompt, agentOpts)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return vm.ToValue(parseAgentOutput(output))
	}
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

func (r *Runner) jsParallel(ctx context.Context, vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		items := call.Argument(0).ToObject(vm)
		lengthValue := items.Get("length")
		length := int(lengthValue.ToInteger())
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
		previousCapture := r.capture
		r.capture = capture
		defer func() {
			r.capture = previousCapture
		}()
		for i := 0; i < length; i++ {
			item := items.Get(fmt.Sprintf("%d", i))
			if mapper != nil {
				value, err := mapper(goja.Undefined(), item, vm.ToValue(i))
				if err != nil {
					panic(err)
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
		r.capture = previousCapture

		agentResults, err := r.runParallelAgentCalls(ctx, capture.Calls)
		if err != nil {
			panic(vm.ToValue(err.Error()))
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
		values := make([]goja.Value, 0, length)
		for i := 0; i < length; i++ {
			values = append(values, items.Get(fmt.Sprintf("%d", i)))
		}
		for _, stageValue := range call.Arguments[1:] {
			fn, ok := goja.AssertFunction(stageValue)
			if !ok {
				panic(vm.ToValue("pipeline stages must be functions"))
			}
			rawResults := make([]any, 0, len(values))
			capture := &parallelCapture{}
			previousCapture := r.capture
			r.capture = capture
			for _, value := range values {
				next, err := fn(goja.Undefined(), value)
				if err != nil {
					r.capture = previousCapture
					panic(err)
				}
				rawResults = append(rawResults, next.Export())
			}
			r.capture = previousCapture

			agentResults, err := r.runParallelAgentCalls(ctx, capture.Calls)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			values = values[:0]
			for _, raw := range rawResults {
				values = append(values, vm.ToValue(replaceParallelAgentMarkers(raw, agentResults)))
			}
		}
		results := make([]any, 0, len(values))
		for _, value := range values {
			results = append(results, value.Export())
		}
		return vm.ToValue(results)
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
	if err := r.ensureNotStopped(ctx); err != nil {
		return "", err
	}
	r.mu.Lock()
	phase := r.currentPhase
	r.mu.Unlock()
	mode := strings.TrimSpace(opts.Mode)
	if mode == "" {
		mode = "read-only"
	}
	provider := strings.TrimSpace(opts.Provider)
	if provider == "" || provider == "default" {
		provider = "codex"
	}
	schemaHash := agentSchemaHash(opts.Schema)
	if cached, ok, err := r.Store.CompletedAgent(r.Run.ID, phase, opts.Label, prompt, provider, mode, opts.Isolation, opts.Model, schemaHash, r.scriptHash, r.argsHash); err != nil {
		return "", err
	} else if ok {
		if phase != "" {
			_ = r.Store.IncrementPhaseAgentCount(r.Run.ID, phase)
		}
		return cached.Output, nil
	}

	r.mu.Lock()
	if r.agentCount >= r.MaxAgents {
		r.mu.Unlock()
		return "", fmt.Errorf("workflow exceeded max agents: %d", r.MaxAgents)
	}
	if r.budgetLimit > 0 && r.budgetSpent+r.agentCostUSD > r.budgetLimit {
		r.mu.Unlock()
		return "", fmt.Errorf("workflow budget exhausted: next agent would exceed $%.4f limit", r.budgetLimit)
	}
	if r.budgetLimit > 0 {
		r.budgetSpent += r.agentCostUSD
	}
	r.agentCount++
	r.mu.Unlock()

	agent := Agent{
		RunID:      r.Run.ID,
		Phase:      phase,
		Label:      opts.Label,
		Prompt:     prompt,
		Provider:   provider,
		Mode:       mode,
		Isolation:  opts.Isolation,
		Model:      opts.Model,
		SchemaHash: schemaHash,
		ScriptHash: r.scriptHash,
		ArgsHash:   r.argsHash,
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
	output, patchPath, worktree, err := r.runAgentCommand(agentCtx, agent, opts)
	agent.PatchPath = patchPath
	agent.Worktree = worktree
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
	if err := r.Store.FinishAgent(agent, output, ""); err != nil {
		return "", err
	}
	return output, nil
}

func (r *Runner) runParallelAgentCalls(ctx context.Context, calls []parallelAgentCall) ([]any, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]any, len(calls))
	limit := r.MaxConcurrentAgents
	if limit <= 0 {
		limit = 16
	}
	sem := make(chan struct{}, limit)
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
			output, err := r.RunAgent(ctx, call.Prompt, call.Opts)
			if err != nil {
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
		return ErrWorkflowStopped
	}
	if run.Status == "paused" {
		return ErrWorkflowPaused
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
			return ErrWorkflowStopped
		}
		if run, runErr := r.Store.Run(r.Run.ID); runErr == nil && run.Status == "paused" {
			return ErrWorkflowPaused
		}
	}
	return err
}

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
		return nil, fmt.Errorf("workflow returned a pending promise")
	}
}

func parseAgentOutput(output string) any {
	var structured any
	if json.Unmarshal([]byte(output), &structured) == nil {
		return structured
	}
	return output
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

func buildCheckPrompt(command string) string {
	rawCommand, _ := json.Marshal(command)
	return "Run this verification command exactly once in the target repo: " + string(rawCommand) + "\n" +
		"Do not edit source files. It is acceptable for the command to write normal ignored build, cache, or test artifacts. " +
		"Use the real command result as ground truth. Return JSON with ok=true only if the command exits successfully. " +
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

func (r *Runner) runAgentCommand(ctx context.Context, agent Agent, opts AgentOptions) (string, string, string, error) {
	if stub := os.Getenv("PALLIUM_WORKFLOW_AGENT_STUB"); stub != "" {
		if delay := os.Getenv("PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS"); delay != "" {
			ms, err := strconv.Atoi(delay)
			if err != nil {
				return "", "", "", err
			}
			select {
			case <-time.After(time.Duration(ms) * time.Millisecond):
			case <-ctx.Done():
				return "", "", "", ctx.Err()
			}
		}
		cwd := r.Run.CWD
		worktree := ""
		if agent.Mode == "edit" || agent.Isolation == "worktree" {
			var err error
			worktree, err = r.createWorktree(agent.ID)
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
		patchPath := ""
		if worktree != "" {
			var err error
			patchPath, err = r.writeWorktreePatch(agent.ID, worktree)
			if err != nil {
				return "", "", worktree, err
			}
		}
		return strings.ReplaceAll(stub, "{{PROMPT}}", agent.Prompt), patchPath, worktree, nil
	}
	provider := firstNonEmpty(agent.Provider, opts.Provider, "codex")
	if provider != "codex" {
		return "", "", "", fmt.Errorf("workflow agent provider %q is not configured; available provider: codex", provider)
	}
	cwd := r.Run.CWD
	worktree := ""
	if agent.Mode == "edit" || agent.Isolation == "worktree" {
		var err error
		worktree, err = r.createWorktree(agent.ID)
		if err != nil {
			return "", "", "", err
		}
		cwd = worktree
	}

	tmpDir, err := os.MkdirTemp("", "pallium-workflow-agent-*")
	if err != nil {
		return "", "", worktree, err
	}
	defer os.RemoveAll(tmpDir)
	outFile := filepath.Join(tmpDir, "last-message.txt")
	cmdArgs := []string{"exec", "--cd", cwd, "--output-last-message", outFile}
	if agent.Mode == "edit" || agent.Mode == "test" || agent.Mode == "check" {
		cmdArgs = append(cmdArgs, "--sandbox", "workspace-write")
	} else {
		cmdArgs = append(cmdArgs, "--sandbox", "read-only")
	}
	if opts.Model != "" {
		cmdArgs = append(cmdArgs, "--model", opts.Model)
	}
	if len(opts.Schema) > 0 {
		schemaPath := filepath.Join(tmpDir, "schema.json")
		normalizedSchema := normalizeSchema(opts.Schema)
		raw, err := json.MarshalIndent(normalizedSchema, "", "  ")
		if err != nil {
			return "", "", worktree, err
		}
		if err := os.WriteFile(schemaPath, raw, 0o644); err != nil {
			return "", "", worktree, err
		}
		cmdArgs = append(cmdArgs, "--output-schema", schemaPath)
	}
	cmdArgs = append(cmdArgs, agent.Prompt)
	cmd := exec.CommandContext(ctx, r.CodexBinary, cmdArgs...)
	cmd.Dir = cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", worktree, fmt.Errorf("codex agent failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		return "", "", worktree, err
	}
	output := strings.TrimSpace(string(raw))
	patchPath := ""
	if worktree != "" {
		patchPath, err = r.writeWorktreePatch(agent.ID, worktree)
		if err != nil {
			return output, "", worktree, err
		}
	}
	return output, patchPath, worktree, nil
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

func (r *Runner) createWorktree(agentID string) (string, error) {
	root, err := RunArtifactDir(r.Run.ID, "worktrees")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(root, agentID)
	cmd := exec.Command("git", "worktree", "add", "--detach", path, "HEAD")
	cmd.Dir = r.Run.CWD
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("create worktree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return path, nil
}

func (r *Runner) writeWorktreePatch(agentID, worktree string) (string, error) {
	cmd := exec.Command("git", "diff", "--binary")
	cmd.Dir = worktree
	raw, err := cmd.Output()
	if err != nil {
		return "", err
	}
	patchDir, err := RunArtifactDir(r.Run.ID, "patches")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(patchDir, 0o755); err != nil {
		return "", err
	}
	patchPath := filepath.Join(patchDir, agentID+".patch")
	if err := os.WriteFile(patchPath, raw, 0o644); err != nil {
		return "", err
	}
	return patchPath, nil
}

func stripMeta(script string) string {
	re := regexp.MustCompile(`(?s)export\s+const\s+meta\s*=\s*\{.*?\}\s*;?`)
	return re.ReplaceAllString(script, "")
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

func RunArtifactDir(runID, child string) (string, error) {
	if err := ValidateID(runID); err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	parts := []string{home, ".pallium", "workflow-runs", runID}
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
		didApply, err := applyPatch(ctx, snapshot.Run.CWD, agent.PatchPath)
		if err != nil {
			return applied, err
		}
		if didApply {
			applied = append(applied, agent.PatchPath)
		}
	}
	return applied, nil
}

func applyPatch(ctx context.Context, cwd, patchPath string) (bool, error) {
	raw, err := os.ReadFile(patchPath)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return false, nil
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
