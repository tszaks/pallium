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

	mu            sync.Mutex
	currentPhase  string
	agentCount    int
	budgetLimit   float64
	budgetSpent   float64
	agentCostUSD  float64
	workflowDepth int
	stubIndex     int
	capture       *parallelCapture
	scriptHash    string
	argsHash      string
}

type AgentOptions struct {
	Label     string         `json:"label,omitempty"`
	Provider  string         `json:"provider,omitempty"`
	Repo      string         `json:"repo,omitempty"`
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

type PolicyFinding struct {
	Kind    string `json:"kind"`
	Line    int    `json:"line"`
	Message string `json:"message"`
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
	r.agentCostUSD = workflowAgentCostUSD()
	if strings.TrimSpace(r.MaxBudgetUSD) != "" {
		limit, err := strconv.ParseFloat(strings.TrimSpace(r.MaxBudgetUSD), 64)
		if err != nil || limit < 0 {
			return "", fmt.Errorf("invalid max budget usd %q", r.MaxBudgetUSD)
		}
		r.budgetLimit = limit
	}
	if err := r.Store.SetRunStatus(r.Run.ID, "running", "", ""); err != nil {
		return "", err
	}
	return r.executeScript(ctx, script, args, true)
}

func (r *Runner) executeScript(ctx context.Context, script string, args any, topLevel bool) (string, error) {
	previousScriptHash := r.scriptHash
	previousArgsHash := r.argsHash
	defer func() {
		r.scriptHash = previousScriptHash
		r.argsHash = previousArgsHash
	}()
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
			panic(vm.ToValue(err.Error()))
		}
		return vm.ToValue(parseAgentOutput(result))
	}); err != nil {
		return "", err
	}
	if err := vm.Set("coordinator", r.jsCoordinator(ctx, vm)); err != nil {
		return "", err
	}
	if err := vm.Set("gate", func(name string, message ...string) goja.Value {
		text := ""
		if len(message) > 0 {
			text = message[0]
		}
		gate, err := r.Store.EnsureGate(r.Run.ID, name, text)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		if gate.Status == "approved" {
			return vm.ToValue(gate)
		}
		panic(vm.ToValue(ErrWorkflowPaused.Error()))
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
		if err := r.Store.SetRunStatus(r.Run.ID, "completed", resultText, ""); err != nil {
			return "", err
		}
	}
	return resultText, nil
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
				panic(vm.ToValue(err.Error()))
			}
			return vm.ToValue(result)
		},
	}
}

func (r *Runner) runUntilGreen(ctx context.Context, command string, options map[string]any) (map[string]any, error) {
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
	testModel := optionString(options, "model", "")
	fixModel := optionString(options, "fix_model", testModel)
	var rounds []map[string]any
	lastSignature := ""
	stalled := false
	for round := 0; round <= maxRounds; round++ {
		checkOutput, err := r.RunAgent(ctx, buildCheckPrompt(command), AgentOptions{
			Label:  fmt.Sprintf("%s-check-%d", label, round+1),
			Mode:   "test",
			Model:  testModel,
			Schema: defaultCheckSchema(),
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
			return map[string]any{"ok": true, "command": command, "rounds": rounds, "stalled": false}, nil
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
		fixPrompt := "Fix the failing verification for this workflow.\nCommand: " + command + "\nFailure JSON: " + stringifyResult(checkResult) + "\nMake the smallest correct code change. Do not skip, weaken, or hide tests."
		fixOutput, err := r.RunAgent(ctx, fixPrompt, AgentOptions{
			Label:     fmt.Sprintf("%s-fix-%d", label, round+1),
			Mode:      "edit",
			Isolation: "worktree",
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
	}
	return map[string]any{"ok": false, "command": command, "rounds": rounds, "stalled": stalled}, nil
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
				panic(vm.ToValue(err.Error()))
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
	repo := strings.TrimSpace(opts.Repo)
	if repo == "" {
		repo = r.Run.CWD
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	schemaHash := agentSchemaHash(opts.Schema)
	if cached, ok, err := r.Store.CompletedAgent(r.Run.ID, phase, opts.Label, prompt, provider, absRepo, mode, opts.Isolation, opts.Model, schemaHash, r.scriptHash, r.argsHash); err != nil {
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
		RunID:            r.Run.ID,
		Phase:            phase,
		Label:            opts.Label,
		Prompt:           prompt,
		Provider:         provider,
		Repo:             absRepo,
		Mode:             mode,
		Isolation:        opts.Isolation,
		Model:            opts.Model,
		SchemaHash:       schemaHash,
		ScriptHash:       r.scriptHash,
		ArgsHash:         r.argsHash,
		EstimatedCostUSD: r.agentCostUSD,
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
				return "", "", "", ctx.Err()
			}
		}
		cwd := firstNonEmpty(agent.Repo, r.Run.CWD)
		worktree := ""
		if agent.Mode == "edit" || agent.Isolation == "worktree" {
			var err error
			worktree, err = r.createWorktree(agent.ID, cwd)
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
	cwd := firstNonEmpty(agent.Repo, r.Run.CWD)
	worktree := ""
	if agent.Mode == "edit" || agent.Isolation == "worktree" {
		var err error
		worktree, err = r.createWorktree(agent.ID, cwd)
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
	if provider != "codex" {
		command := strings.TrimSpace(os.Getenv(providerCommandEnvName(provider)))
		if command == "" {
			return "", "", worktree, fmt.Errorf("workflow agent provider %q is not configured; set %s", provider, providerCommandEnvName(provider))
		}
		output, err := r.runConfiguredProviderCommand(ctx, command, tmpDir, outFile, cwd, agent, opts)
		if err != nil {
			return output, "", worktree, err
		}
		patchPath := ""
		if worktree != "" {
			patchPath, err = r.writeWorktreePatch(agent.ID, worktree)
			if err != nil {
				return output, "", worktree, err
			}
		}
		return output, patchPath, worktree, nil
	}
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

func (r *Runner) runConfiguredProviderCommand(ctx context.Context, command, tmpDir, outFile, cwd string, agent Agent, opts AgentOptions) (string, error) {
	promptFile := filepath.Join(tmpDir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte(agent.Prompt), 0o600); err != nil {
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
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"PALLIUM_WORKFLOW_RUN_ID="+r.Run.ID,
		"PALLIUM_WORKFLOW_AGENT_ID="+agent.ID,
		"PALLIUM_WORKFLOW_PROVIDER="+agent.Provider,
		"PALLIUM_WORKFLOW_LABEL="+agent.Label,
		"PALLIUM_WORKFLOW_MODE="+agent.Mode,
		"PALLIUM_WORKFLOW_MODEL="+agent.Model,
		"PALLIUM_WORKFLOW_REPO="+agent.Repo,
		"PALLIUM_WORKFLOW_CWD="+cwd,
		"PALLIUM_WORKFLOW_PROMPT="+agent.Prompt,
		"PALLIUM_WORKFLOW_PROMPT_FILE="+promptFile,
		"PALLIUM_WORKFLOW_OUTPUT_FILE="+outFile,
		"PALLIUM_WORKFLOW_SCHEMA_FILE="+schemaFile,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("workflow provider %q failed: %w: %s", agent.Provider, err, strings.TrimSpace(stderr.String()))
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

func (r *Runner) createWorktree(agentID, repoRoot string) (string, error) {
	root, err := RunArtifactDir(r.Run.ID, "worktrees")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(root, agentID)
	cmd := exec.Command("git", "worktree", "add", "--detach", path, "HEAD")
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("create worktree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return path, nil
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
