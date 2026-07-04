package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dop251/goja"
)

type Runner struct {
	Store        *Store
	Run          Run
	MaxAgents    int
	MaxBudgetUSD string
	CodexBinary  string

	currentPhase string
	agentCount   int
}

type AgentOptions struct {
	Label     string         `json:"label,omitempty"`
	Mode      string         `json:"mode,omitempty"`
	Isolation string         `json:"isolation,omitempty"`
	Schema    map[string]any `json:"schema,omitempty"`
	Model     string         `json:"model,omitempty"`
}

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
	if r.CodexBinary == "" {
		r.CodexBinary = "codex"
	}
	if err := r.Store.SetRunStatus(r.Run.ID, "running", "", ""); err != nil {
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
	if err := vm.Set("parallel", r.jsParallel(vm)); err != nil {
		return "", err
	}
	if err := vm.Set("pipeline", r.jsPipeline(vm)); err != nil {
		return "", err
	}
	if err := vm.Set("workflow", func(name string, input any) any {
		panic(vm.ToValue(fmt.Sprintf("nested workflow %q is not supported in this v1 runtime", name)))
	}); err != nil {
		return "", err
	}

	value, err := vm.RunString("(function(){\n" + body + "\n})()")
	if r.currentPhase != "" {
		_ = r.Store.FinishPhase(r.Run.ID, r.currentPhase)
		r.currentPhase = ""
	}
	if err != nil {
		_ = r.Store.SetRunStatus(r.Run.ID, "failed", "", err.Error())
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
			value, err := fn(goja.Undefined())
			_ = r.Store.FinishPhase(r.Run.ID, name)
			r.currentPhase = previous
			if err != nil {
				panic(err)
			}
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
		output, err := r.RunAgent(ctx, prompt, opts)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		var structured any
		if json.Unmarshal([]byte(output), &structured) == nil {
			return vm.ToValue(structured)
		}
		return vm.ToValue(output)
	}
}

func (r *Runner) jsParallel(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		items := call.Argument(0).ToObject(vm)
		lengthValue := items.Get("length")
		length := int(lengthValue.ToInteger())
		results := make([]any, 0, length)
		for i := 0; i < length; i++ {
			item := items.Get(fmt.Sprintf("%d", i))
			if fn, ok := goja.AssertFunction(item); ok {
				value, err := fn(goja.Undefined())
				if err != nil {
					panic(err)
				}
				results = append(results, value.Export())
			} else {
				results = append(results, item.Export())
			}
		}
		return vm.ToValue(results)
	}
}

func (r *Runner) jsPipeline(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		items := call.Argument(0).ToObject(vm)
		length := int(items.Get("length").ToInteger())
		results := make([]any, 0, length)
		for i := 0; i < length; i++ {
			value := items.Get(fmt.Sprintf("%d", i))
			for _, stageValue := range call.Arguments[1:] {
				fn, ok := goja.AssertFunction(stageValue)
				if !ok {
					panic(vm.ToValue("pipeline stages must be functions"))
				}
				next, err := fn(goja.Undefined(), value)
				if err != nil {
					panic(err)
				}
				value = next
			}
			results = append(results, value.Export())
		}
		return vm.ToValue(results)
	}
}

func (r *Runner) RunAgent(ctx context.Context, prompt string, opts AgentOptions) (string, error) {
	if r.agentCount >= r.MaxAgents {
		return "", fmt.Errorf("workflow exceeded max agents: %d", r.MaxAgents)
	}
	r.agentCount++
	mode := strings.TrimSpace(opts.Mode)
	if mode == "" {
		mode = "read-only"
	}
	agent := Agent{
		RunID:     r.Run.ID,
		Phase:     r.currentPhase,
		Label:     opts.Label,
		Prompt:    prompt,
		Mode:      mode,
		Isolation: opts.Isolation,
	}
	created, err := r.Store.CreateAgent(agent)
	if err != nil {
		return "", err
	}
	agent = created
	if r.currentPhase != "" {
		_ = r.Store.IncrementPhaseAgentCount(r.Run.ID, r.currentPhase)
	}

	output, patchPath, worktree, err := r.runAgentCommand(ctx, agent, opts)
	agent.PatchPath = patchPath
	agent.Worktree = worktree
	if err != nil {
		_ = r.Store.FinishAgent(agent, output, err.Error())
		return "", err
	}
	if err := r.Store.FinishAgent(agent, output, ""); err != nil {
		return "", err
	}
	return output, nil
}

func (r *Runner) runAgentCommand(ctx context.Context, agent Agent, opts AgentOptions) (string, string, string, error) {
	if stub := os.Getenv("PALLIUM_WORKFLOW_AGENT_STUB"); stub != "" {
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
	if agent.Mode == "edit" {
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
const plan = agent("Create a concise workflow plan for this task. Return JSON with keys summary, steps, risks. Task: " + ` + string(escaped) + `, {
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
const verified = agent("Review this plan for missing safety or verification steps and return JSON with keys verdict and notes: " + JSON.stringify(plan), {
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
