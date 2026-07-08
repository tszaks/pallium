package workflow

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildClaudeArgsReadOnlyMode(t *testing.T) {
	for _, mode := range []string{"", "read-only"} {
		got := buildClaudeArgs(mode, "")
		want := []string{
			"-p", "--output-format", "json",
			"--strict-mcp-config", "--setting-sources", "user",
			"--allowedTools", claudeReadOnlyAllowedTools,
			"--disallowedTools", claudeReadOnlyDisallowedTools,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mode %q: buildClaudeArgs = %v, want %v", mode, got, want)
		}
	}
}

func TestBuildClaudeArgsEditTestCheckModes(t *testing.T) {
	for _, mode := range []string{"edit", "test", "check"} {
		got := buildClaudeArgs(mode, "")
		want := []string{
			"-p", "--output-format", "json",
			"--strict-mcp-config", "--setting-sources", "user",
			"--permission-mode", "acceptEdits",
			"--allowedTools", claudeEditAllowedTools,
			"--disallowedTools", claudeEditDisallowedTools,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mode %q: buildClaudeArgs = %v, want %v", mode, got, want)
		}
	}
}

func TestBuildClaudeArgsIncludesModel(t *testing.T) {
	got := buildClaudeArgs("read-only", "claude-sonnet-5")
	want := []string{
		"-p", "--output-format", "json",
		"--strict-mcp-config", "--setting-sources", "user",
		"--model", "claude-sonnet-5",
		"--allowedTools", claudeReadOnlyAllowedTools,
		"--disallowedTools", claudeReadOnlyDisallowedTools,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildClaudeArgs with model = %v, want %v", got, want)
	}
}

func TestBuildClaudeArgsNeverAllowsRgOrFind(t *testing.T) {
	// rg --pre and find -exec/-delete run arbitrary commands even under a
	// Bash(rg:*)/Bash(find:*) allowlist prefix (see providers/claude.sh),
	// so neither must ever appear regardless of mode.
	for _, mode := range []string{"", "read-only", "edit", "test", "check"} {
		for _, arg := range buildClaudeArgs(mode, "") {
			if arg == "Bash(rg:*)" || arg == "Bash(find:*)" {
				t.Fatalf("mode %q: buildClaudeArgs unexpectedly allowed %q", mode, arg)
			}
		}
	}
}

func TestEditModeAppendsExtraAllowedToolsFromEnv(t *testing.T) {
	t.Setenv(claudeExtraAllowedToolsEnv, "Bash(xcodebuild:*),Bash(sh scripts/lint.sh:*)")
	for _, mode := range []string{"edit", "test", "check"} {
		allowed := allowedToolsArg(buildClaudeArgs(mode, ""))
		if !strings.Contains(allowed, "Bash(sh scripts/lint.sh:*)") {
			t.Fatalf("mode %q: extra tools not appended: %q", mode, allowed)
		}
		// The base set must survive alongside the extras.
		if !strings.Contains(allowed, "Bash(go test:*)") {
			t.Fatalf("mode %q: base allowlist dropped: %q", mode, allowed)
		}
	}
}

func TestReadOnlyModeIgnoresExtraAllowedToolsEnv(t *testing.T) {
	// The env var extends edit-mode build tooling only; a read-only agent must
	// never gain extra Bash reach from it.
	t.Setenv(claudeExtraAllowedToolsEnv, "Bash(xcodebuild:*)")
	for _, mode := range []string{"", "read-only"} {
		if allowed := allowedToolsArg(buildClaudeArgs(mode, "")); allowed != claudeReadOnlyAllowedTools {
			t.Fatalf("mode %q: read-only allowlist changed to %q", mode, allowed)
		}
	}
}

// allowedToolsArg returns the value passed to --allowedTools in args, or "".
func allowedToolsArg(args []string) string {
	for i, a := range args {
		if a == "--allowedTools" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestReadOnlyModeNeverReachesPallium(t *testing.T) {
	// A read-only agent must not reach the pallium CLI: a Bash(pallium:*) grant
	// would permit `pallium workflow run` (nested paid agents) and index/task
	// mutations, so it is not read-only.
	for _, mode := range []string{"", "read-only"} {
		if allowed := allowedToolsArg(buildClaudeArgs(mode, "")); strings.Contains(allowed, "pallium") {
			t.Fatalf("mode %q: read-only allowlist reaches pallium: %q", mode, allowed)
		}
	}
}

func TestBuildClaudeArgsIsolatesAmbientSettings(t *testing.T) {
	// Every mode must block ambient MCP and load only the operator's user
	// settings, so a checked-out repo's .claude config can't widen the allowlist.
	for _, mode := range []string{"", "read-only", "edit", "test", "check"} {
		args := buildClaudeArgs(mode, "")
		if !containsArg(args, "--strict-mcp-config") {
			t.Fatalf("mode %q: missing --strict-mcp-config: %v", mode, args)
		}
		found := false
		for i, a := range args {
			if a == "--setting-sources" && i+1 < len(args) && args[i+1] == "user" {
				found = true
			}
		}
		if !found {
			t.Fatalf("mode %q: --setting-sources user missing: %v", mode, args)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestExtractClaudeOutputSurfacesIsError(t *testing.T) {
	// A CLI that exits 0 but reports is_error must not be treated as success.
	if _, _, err := extractClaudeOutput(`{"type":"result","subtype":"error_max_turns","is_error":true}`, false); err == nil {
		t.Fatal("expected error when envelope has is_error=true")
	}
	// The array/event-stream shape is handled the same way.
	if _, _, err := extractClaudeOutput(`[{"type":"result","is_error":true,"subtype":"error_during_execution"}]`, false); err == nil {
		t.Fatal("expected error for is_error in event-stream array")
	}
	// A non-error envelope with a real result still works.
	if text, _, err := extractClaudeOutput(`{"type":"result","is_error":false,"result":"ok"}`, false); err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestTruncateForError(t *testing.T) {
	if got := truncateForError("short"); got != "short" {
		t.Fatalf("short string changed: %q", got)
	}
	got := truncateForError(strings.Repeat("x", maxErrorOutputBytes+100))
	if len(got) <= maxErrorOutputBytes || !strings.Contains(got, "truncated") {
		t.Fatalf("truncate failed: len=%d", len(got))
	}
}

func TestBuildClaudePromptNoSchema(t *testing.T) {
	got, err := buildClaudePrompt("review the code", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "review the code" {
		t.Fatalf("buildClaudePrompt = %q, want unchanged prompt", got)
	}
}

func TestBuildClaudePromptWithSchema(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}
	got, err := buildClaudePrompt("review the code", schema)
	if err != nil {
		t.Fatal(err)
	}
	if got == "review the code" {
		t.Fatal("buildClaudePrompt did not append schema instruction")
	}
	for _, want := range []string{"review the code", "Respond with ONLY a single JSON object", `"type": "boolean"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("buildClaudePrompt missing %q in: %s", want, got)
		}
	}
}

func TestExtractClaudeOutputObjectEnvelope(t *testing.T) {
	raw := `{"result":"the answer","total_cost_usd":0.02,"usage":{"input_tokens":10,"output_tokens":20}}`
	text, usage, err := extractClaudeOutput(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if text != "the answer" {
		t.Fatalf("text = %q", text)
	}
	if usage["cost_usd"] != 0.02 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(20) {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestExtractClaudeOutputEventStreamArray(t *testing.T) {
	raw := `[{"type":"system"},{"type":"assistant","message":"..."},{"type":"result","result":"final answer","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2}}]`
	text, usage, err := extractClaudeOutput(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if text != "final answer" {
		t.Fatalf("text = %q", text)
	}
	if usage["cost_usd"] != 0.01 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestExtractClaudeOutputNonJSONPassthrough(t *testing.T) {
	text, usage, err := extractClaudeOutput("plain text answer", false)
	if err != nil {
		t.Fatal(err)
	}
	if text != "plain text answer" {
		t.Fatalf("text = %q", text)
	}
	if usage != nil {
		t.Fatalf("usage = %+v, want nil", usage)
	}
}

func TestExtractClaudeOutputStripsFenceWhenSchemaRequested(t *testing.T) {
	raw := `{"result":"` + "```json\\n{\\\"ok\\\":true}\\n```" + `"}`
	text, _, err := extractClaudeOutput(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if text != `{"ok":true}` {
		t.Fatalf("text = %q", text)
	}
}

func TestExtractClaudeOutputRecoversJSONFromProseWhenSchemaRequested(t *testing.T) {
	raw := `{"result":"Sure, here you go: {\"ok\":true} — let me know if you need more."}`
	text, _, err := extractClaudeOutput(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if text != `{"ok":true}` {
		t.Fatalf("text = %q", text)
	}
}

func TestExtractClaudeOutputLeavesUnstructuredFenceAlone(t *testing.T) {
	raw := `{"result":"` + "```json\\n{\\\"ok\\\":true}\\n```" + `"}`
	text, _, err := extractClaudeOutput(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if text != "```json\n{\"ok\":true}\n```" {
		t.Fatalf("text = %q", text)
	}
}

func TestExtractClaudeOutputEmptyIsError(t *testing.T) {
	if _, _, err := extractClaudeOutput("   ", false); err == nil {
		t.Fatal("expected error for empty output")
	}
	if _, _, err := extractClaudeOutput(`{"result":""}`, false); err == nil {
		t.Fatal("expected error for empty result field")
	}
}

func TestExtractBalancedJSONSkipsInvalidBracketedProse(t *testing.T) {
	got, ok := extractBalancedJSON(`Here is [the JSON]: {"ok":true}`)
	if !ok || got != `{"ok":true}` {
		t.Fatalf("extractBalancedJSON = %q, %v", got, ok)
	}
}

func TestExtractBalancedJSONHonorsStringEscapes(t *testing.T) {
	got, ok := extractBalancedJSON(`prefix {"note":"a brace } inside a string"} suffix`)
	if !ok || got != `{"note":"a brace } inside a string"}` {
		t.Fatalf("extractBalancedJSON = %q, %v", got, ok)
	}
}

func TestExtractBalancedJSONNoMatch(t *testing.T) {
	if _, ok := extractBalancedJSON("no json here"); ok {
		t.Fatal("expected no match")
	}
}
