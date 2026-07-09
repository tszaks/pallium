package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// claudeCLIName is a var (not a const) so tests can point it at a fake
// binary without touching PATH.
var claudeCLIName = "claude"

// Tool allowlists for the built-in claude provider, mirroring the mode split
// codex gets from --sandbox read-only/workspace-write. Read/search/inspect
// commands only (ls, cat, grep, wc, pallium); `rg` and `find` are
// deliberately left out even though they're read-only in the common case:
// ripgrep's `--pre COMMAND` and find's `-exec`/`-delete` run arbitrary
// commands, and a `Bash(rg:*)`/`Bash(find:*)` prefix match can't rule that
// out since the dangerous flags can be appended after the fixed prefix (the
// built-in Grep/Glob tools already cover the same need). `Bash(pallium:*)` is
// likewise excluded from the read-only set: a `Bash(pallium:*)` prefix would
// allow `pallium workflow run` (spawns nested paid agents, risking cost blowups
// and recursion) and `pallium index`/`task` (mutate the store) — not read-only
// at all. Read/Grep/Glob/LS already cover code orientation. See
// providers/claude.sh for the empirically-verified writeup this mirrors.
//
// The edit set spans the common ecosystems (Go, Node, Deno, Python, Rust,
// Make, Apple/Swift) so edit agents can build/test/lint on non-Go repos too. Note
// that any build runner (make, npm run, cargo, xcodebuild) executes
// repo-authored scripts, so the curl/gh denial is a speed bump, not a
// sandbox: edit mode trusts the repo's own tooling. Repos needing a tool
// that isn't in the default set (a project script like design-lint.sh, or
// xcrun) extend the edit allowlist via PALLIUM_WORKFLOW_CLAUDE_ALLOWED_TOOLS
// (a comma-separated list of extra `--allowedTools` entries) rather than
// editing this list.
const (
	claudeReadOnlyAllowedTools    = "Read,Grep,Glob,LS,Bash(ls:*),Bash(cat:*),Bash(grep:*),Bash(wc:*)"
	claudeReadOnlyDisallowedTools = "Edit,Write,NotebookEdit"
	claudeEditAllowedTools        = "Read,Grep,Glob,LS,Edit,Write,Bash(go test:*),Bash(go build:*),Bash(go vet:*),Bash(gofmt:*),Bash(npm test:*),Bash(npm run:*),Bash(yarn:*),Bash(pnpm:*),Bash(deno test:*),Bash(deno fmt:*),Bash(deno lint:*),Bash(deno check:*),Bash(deno task:*),Bash(pytest:*),Bash(cargo:*),Bash(make:*),Bash(xcodebuild:*),Bash(swift:*),Bash(swiftlint:*),Bash(ls:*),Bash(cat:*),Bash(grep:*),Bash(wc:*)"
	claudeEditDisallowedTools     = "Bash(curl:*),Bash(gh:*)"
	claudeExtraAllowedToolsEnv    = "PALLIUM_WORKFLOW_CLAUDE_ALLOWED_TOOLS"
)

// editAllowedTools returns the edit-mode allowlist plus any repo-specific
// extras from PALLIUM_WORKFLOW_CLAUDE_ALLOWED_TOOLS, so a non-Go repo can add
// its own build/lint commands (e.g. "Bash(sh scripts/lint.sh:*),Bash(xcrun:*)")
// without a code change. Returns the base set unchanged when the env is unset.
//
// The env value is operator-controlled configuration, trusted at the same
// level as the workflow script itself, and is appended verbatim (no
// validation). It cannot re-enable the curl/gh denial: --disallowedTools takes
// precedence over --allowedTools in the claude CLI, so claudeEditDisallowedTools
// still blocks those even if an entry here names them.
func editAllowedTools() string {
	if extra := strings.TrimSpace(os.Getenv(claudeExtraAllowedToolsEnv)); extra != "" {
		return claudeEditAllowedTools + "," + extra
	}
	return claudeEditAllowedTools
}

// buildClaudePrompt appends a structured-output instruction to the base
// prompt when a schema is set, matching the instruction configured provider
// wrappers use (see providers/claude.sh) so Pallium's local schema
// validation sees the same bare-JSON contract either way.
func buildClaudePrompt(prompt string, schema map[string]any) (string, error) {
	if len(schema) == 0 {
		return prompt, nil
	}
	raw, err := json.MarshalIndent(normalizeSchema(schema), "", "  ")
	if err != nil {
		// Dropping the schema instruction silently would make the model
		// return prose and fail Pallium's later schema validation for an
		// invisible reason; surface it instead.
		return "", fmt.Errorf("encode schema for claude prompt: %w", err)
	}
	return prompt + "\n\nRespond with ONLY a single JSON object conforming to this JSON Schema — no markdown fences, no prose:\n" + string(raw), nil
}

// buildClaudeArgs builds the `claude` CLI argv (excluding the binary name
// and the prompt itself, which is piped over stdin) for the given agent mode
// and model. A pure function so argv construction is testable without
// invoking a real claude binary, mirroring how the codex sandbox flags are
// derived from agent.Mode in runAgentCommand.
//
// --safe-mode and --strict-mcp-config isolate the worker. --safe-mode disables
// all customizations — user, project, AND local settings files plus hooks — so
// neither a checked-out repo's .claude/settings.json nor the operator's own
// global settings can widen the allow-rules past the explicit --allowedTools/
// --disallowedTools set below; those CLI flags still enforce (permissions work
// normally under safe mode). --strict-mcp-config additionally blocks any
// ambient MCP servers, since none are passed via --mcp-config. Without these
// the built-in provider (the zero-config default under CLAUDECODE) would
// inherit whatever tools ambient settings/hooks/MCP grant. Verified: the -p
// JSON flow returns normally under --safe-mode.
func buildClaudeArgs(mode, model string) []string {
	args := []string{"-p", "--output-format", "json", "--safe-mode", "--strict-mcp-config"}
	if model != "" {
		args = append(args, "--model", model)
	}
	switch mode {
	case "edit", "test", "check":
		args = append(args,
			"--permission-mode", "acceptEdits",
			"--allowedTools", editAllowedTools(),
			"--disallowedTools", claudeEditDisallowedTools,
		)
	default:
		args = append(args,
			"--allowedTools", claudeReadOnlyAllowedTools,
			"--disallowedTools", claudeReadOnlyDisallowedTools,
		)
	}
	return args
}

// runBuiltinClaudeCommand invokes the `claude` CLI directly, used when the
// resolved provider is "claude" and no PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND
// wrapper is configured, so provider adoption works with just the CLI on
// PATH. Parallels the codex exec block: same cwd (worktree for edit/
// isolation, else the run cwd), same last-message-style return contract.
func (r *Runner) runBuiltinClaudeCommand(ctx context.Context, usageFile, cwd, prompt string, agent *Agent, opts AgentOptions) (string, error) {
	fullPrompt, err := buildClaudePrompt(prompt, opts.Schema)
	if err != nil {
		return "", fmt.Errorf("workflow provider \"claude\": %w", err)
	}
	args := buildClaudeArgs(agent.Mode, opts.Model)
	cmd := exec.CommandContext(ctx, claudeCLIName, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	// Piped via stdin rather than a `-p <prompt>` argv value: a large prompt
	// (many files/findings) as a single argv string can exceed the kernel's
	// per-argument length limit (MAX_ARG_STRLEN, 128KiB on Linux) and fail
	// the exec itself before the CLI starts. `claude -p` with no value reads
	// the prompt from stdin.
	cmd.Stdin = strings.NewReader(fullPrompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Cap the embedded CLI output so a huge or malformed response can't
		// bloat the stored error record.
		baseErr := fmt.Errorf("workflow provider \"claude\" failed: %w: %s", err, truncateForError(strings.TrimSpace(stderr.String())))
		return truncateForError(strings.TrimSpace(stdout.String())), wrapProviderCommandError(baseErr, stdout.String()+stderr.String())
	}
	text, usage, err := extractClaudeOutput(stdout.String(), len(opts.Schema) > 0)
	if err != nil {
		return "", fmt.Errorf("workflow provider \"claude\" produced no output: %w", err)
	}
	if usage != nil {
		if raw, marshalErr := json.Marshal(usage); marshalErr == nil {
			_ = os.WriteFile(usageFile, raw, 0o600)
		}
	}
	return text, nil
}

// buildClaudeTeamArgs builds argv for one team-turn invocation. isFirstTurn
// picks --session-id (claude lets the caller MINT a session id, so team.go
// generates one at spawn time) vs --resume (every later turn continues that
// same native conversation). isFirstTurn means "no turn has SUCCEEDED yet",
// not "no turn has been ATTEMPTED yet" — see Store.FinishMemberTurn's
// session_established field: a failed attempt retries with --session-id
// again rather than falling through to --resume against a session claude
// may never have actually created. The tool
// allowlist is unchanged from the normal worker split: coordination
// (messaging, task claim/complete) happens via a structured end-of-turn
// decision Pallium parses and applies itself (see teamDecisionSchema in
// team_runtime.go), not via mid-turn tool calls — deliberately, so a
// teammate's coordination ability never depends on a provider's Bash
// allowlist model (codex has no per-command allowlist, only a coarse
// read-only/workspace-write sandbox toggle).
func buildClaudeTeamArgs(mode, model, sessionToken string, isFirstTurn bool) []string {
	args := buildClaudeArgs(mode, model)
	if isFirstTurn {
		args = append(args, "--session-id", sessionToken)
	} else {
		args = append(args, "--resume", sessionToken)
	}
	return args
}

// runClaudeTeamTurn runs one turn of a claude teammate's native conversation
// and returns its raw text output — expected to be the structured decision
// JSON described by schema (see teamDecisionSchema in team_runtime.go), built
// into the prompt the same way runBuiltinClaudeCommand does for a regular
// worker's schema option — plus usage (cost_usd/token counts, or nil if the
// envelope carried none), which the caller feeds into team/member spend
// tracking (see dispatchTeamTurn). cwd is the teammate's own worktree for
// edit mode, or the team's repo root for read-only — resolved by the caller
// exactly like a regular worker's cwd, so edit-mode teammates get the same
// isolation.
func (r *Runner) runClaudeTeamTurn(ctx context.Context, mode, model, sessionToken string, isFirstTurn bool, cwd, prompt string, schema map[string]any) (string, map[string]any, error) {
	fullPrompt, err := buildClaudePrompt(prompt, schema)
	if err != nil {
		return "", nil, fmt.Errorf("team turn (claude): %w", err)
	}
	args := buildClaudeTeamArgs(mode, model, sessionToken, isFirstTurn)
	cmd := exec.CommandContext(ctx, claudeCLIName, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(fullPrompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		baseErr := fmt.Errorf("team turn (claude) failed: %w: %s", err, truncateForError(strings.TrimSpace(stderr.String())))
		return "", nil, wrapProviderCommandError(baseErr, stdout.String()+stderr.String())
	}
	text, usage, err := extractClaudeOutput(stdout.String(), len(schema) > 0)
	if err != nil {
		return "", nil, fmt.Errorf("team turn (claude) produced no output: %w", err)
	}
	return text, usage, nil
}

// maxErrorOutputBytes caps CLI stdout/stderr embedded in a provider error so
// a huge or malformed response can't bloat the stored error record.
const maxErrorOutputBytes = 4096

func truncateForError(s string) string {
	if len(s) <= maxErrorOutputBytes {
		return s
	}
	return s[:maxErrorOutputBytes] + fmt.Sprintf("... [truncated %d bytes]", len(s)-maxErrorOutputBytes)
}

// extractClaudeOutput pulls the final answer text (and, if present, usage/
// cost) out of `claude --output-format json` output. That flag is documented
// as a single JSON envelope but has been observed (CLI 2.1.x) to instead
// emit the full event stream as a JSON array — the last {"type":"result"}
// event then carries the same result/total_cost_usd/usage fields the
// envelope shape has at its top level, so both are handled. Falls back to
// the raw trimmed text when the output isn't JSON at all (e.g. the CLI
// printed a plain-text error to stdout). When a schema was requested,
// accidental markdown fences are stripped and, if the result still isn't
// bare JSON, the outermost balanced JSON value is recovered from
// surrounding prose — skipped entirely for unstructured calls so a response
// that's legitimately a fenced code block comes back unmodified.
func extractClaudeOutput(raw string, hasSchema bool) (string, map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, fmt.Errorf("empty output")
	}
	text := raw
	var usage map[string]any
	if envelope := parseClaudeEnvelope(raw); envelope != nil {
		// The CLI can exit 0 yet report a failure in the envelope (e.g.
		// {"is_error":true,"subtype":"error_max_turns"}), often with no usable
		// "result" string. Treat that as an error rather than returning the
		// raw envelope JSON as if it were the answer.
		if isErr, _ := envelope["is_error"].(bool); isErr {
			msg, _ := envelope["result"].(string)
			if strings.TrimSpace(msg) == "" {
				msg, _ = envelope["subtype"].(string)
			}
			if strings.TrimSpace(msg) == "" {
				msg = "unspecified error"
			}
			return "", nil, fmt.Errorf("claude CLI reported an error: %s", strings.TrimSpace(msg))
		}
		if result, ok := envelope["result"].(string); ok {
			text = result
		}
		usage = claudeUsageFromEnvelope(envelope)
	}
	text = strings.TrimSpace(text)
	if hasSchema {
		text = stripJSONFence(text)
		if json.Unmarshal([]byte(text), new(any)) != nil {
			if candidate, ok := extractBalancedJSON(text); ok {
				text = candidate
			}
		}
	}
	if text == "" {
		return "", nil, fmt.Errorf("empty output")
	}
	return text, usage, nil
}

// parseClaudeEnvelope returns the result envelope from a claude JSON
// response, whether it's the documented single-object shape or the
// observed event-stream-array shape. Returns nil when raw isn't JSON or no
// envelope/result event is found, in which case the caller falls back to
// the raw text.
func parseClaudeEnvelope(raw string) map[string]any {
	var parsed any
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return nil
	}
	switch typed := parsed.(type) {
	case map[string]any:
		return typed
	case []any:
		for i := len(typed) - 1; i >= 0; i-- {
			if item, ok := typed[i].(map[string]any); ok && item["type"] == "result" {
				return item
			}
		}
	}
	return nil
}

// claudeUsageFromEnvelope maps a claude result envelope's cost/usage fields
// onto Pallium's usage shape ({"input_tokens","output_tokens","cost_usd"}),
// the same shape configured provider wrappers write to
// PALLIUM_WORKFLOW_USAGE_FILE. Returns nil when the envelope carries neither.
func claudeUsageFromEnvelope(envelope map[string]any) map[string]any {
	cost, hasCost := envelope["total_cost_usd"].(float64)
	tokens, _ := envelope["usage"].(map[string]any)
	if !hasCost && len(tokens) == 0 {
		return nil
	}
	usage := map[string]any{}
	if hasCost {
		usage["cost_usd"] = cost
	}
	if v, ok := tokens["input_tokens"]; ok {
		usage["input_tokens"] = v
	}
	if v, ok := tokens["output_tokens"]; ok {
		usage["output_tokens"] = v
	}
	return usage
}

var claudeJSONFenceRe = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")

// stripJSONFence removes a single markdown code fence wrapping the entire
// string, if present.
func stripJSONFence(text string) string {
	if m := claudeJSONFenceRe.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	return text
}

// extractBalancedJSON returns the first substring of s that is both a
// balanced {...} or [...] span and valid JSON, skipping past any
// balanced-but-invalid span (e.g. bracketed prose like "see [here]") to keep
// scanning. Honors quoted strings and escapes so brackets inside string
// values don't confuse the scan.
func extractBalancedJSON(s string) (string, bool) {
	closers := map[byte]byte{'{': '}', '[': ']'}
	for pos := 0; pos < len(s); pos++ {
		want, isOpen := closers[s[pos]]
		if !isOpen {
			continue
		}
		end, ok := scanBalancedFrom(s, pos, want)
		if !ok {
			continue
		}
		candidate := s[pos : end+1]
		if json.Unmarshal([]byte(candidate), new(any)) == nil {
			return candidate, true
		}
	}
	return "", false
}

// scanBalancedFrom returns the index in s of the character that closes the
// bracket opened at start (whose matching close is firstClose), or
// ok=false if the brackets never balance or mismatch before s ends.
func scanBalancedFrom(s string, start int, firstClose byte) (int, bool) {
	stack := []byte{firstClose}
	inString := false
	escape := false
	for i := start + 1; i < len(s); i++ {
		ch := s[i]
		if inString {
			switch {
			case escape:
				escape = false
			case ch == '\\':
				escape = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != ch {
				return 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i, true
			}
		}
	}
	return 0, false
}
