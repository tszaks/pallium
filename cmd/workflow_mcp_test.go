package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowMCPToolsListAndCall(t *testing.T) {
	tmp := t.TempDir()
	server := &mcpServer{dbPath: filepath.Join(tmp, "sessions.sqlite")}

	initResp := server.handle(mcpRequest{ID: float64(1), Method: "initialize"})
	if initResp.Error != nil {
		t.Fatalf("initialize error: %+v", initResp.Error)
	}
	listResp := server.handle(mcpRequest{ID: float64(2), Method: "tools/list"})
	if listResp.Error != nil {
		t.Fatalf("tools/list error: %+v", listResp.Error)
	}
	rawList, _ := json.Marshal(listResp.Result)
	if !strings.Contains(string(rawList), "pallium_workflow_run") {
		t.Fatalf("expected workflow run tool, got %s", string(rawList))
	}

	params, err := json.Marshal(map[string]any{
		"name":      "pallium_workflow_analytics",
		"arguments": map[string]any{"limit": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	callResp := server.handle(mcpRequest{ID: float64(3), Method: "tools/call", Params: params})
	if callResp.Error != nil {
		t.Fatalf("tools/call error: %+v", callResp.Error)
	}
	rawCall, _ := json.Marshal(callResp.Result)
	if !strings.Contains(string(rawCall), "runs_total") {
		t.Fatalf("expected analytics payload, got %s", string(rawCall))
	}
}

// TestWorkflowMCPRunAllowNetwork verifies the pallium_workflow_run MCP tool
// exposes the operator's --allow-network ceiling: previously only the CLI
// run/resume/trigger parsers could set it, leaving the MCP entry point unable
// to grant network access at all. allow_network:true must thread through to
// the same --allow-network flag the CLI path uses, and omitting it must keep
// the safe default (no egress).
func TestWorkflowMCPRunAllowNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	// A granted-network agent is forced into an isolated worktree, so the run
	// cwd must be a git repo.
	initGitRepo(t, tmp)
	setupNetworkProvider(t, tmp)
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	scriptPath := filepath.Join(tmp, "workflow.js")
	if err := os.WriteFile(scriptPath, []byte(`return agent("go", { provider: "net", mode: "read-only", label: "netter", network: true });`), 0o644); err != nil {
		t.Fatal(err)
	}
	server := &mcpServer{dbPath: dbPath}

	if _, err := server.callTool("pallium_workflow_run", map[string]any{
		"task":          "net test",
		"id":            "wf-mcp-net",
		"cwd":           tmp,
		"script_path":   scriptPath,
		"allow_network": true,
	}); err != nil {
		t.Fatalf("callTool with allow_network failed: %v", err)
	}
	assertNetworkRun(t, dbPath, "wf-mcp-net", true, "1")

	if _, err := server.callTool("pallium_workflow_run", map[string]any{
		"task":        "net test",
		"id":          "wf-mcp-net-off",
		"cwd":         tmp,
		"script_path": scriptPath,
	}); err != nil {
		t.Fatalf("callTool without allow_network failed: %v", err)
	}
	assertNetworkRun(t, dbPath, "wf-mcp-net-off", false, "0")
}

func TestWorkflowMCPFramedIO(t *testing.T) {
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	var input bytes.Buffer
	input.WriteString("Content-Length: ")
	input.WriteString("58")
	input.WriteString("\r\n\r\n")
	input.Write(request)
	raw, err := readMCPMessage(bufio.NewReader(&input))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(request) {
		t.Fatalf("raw=%s", string(raw))
	}
	var output bytes.Buffer
	if err := writeMCPMessage(&output, mcpResponse{JSONRPC: "2.0", ID: float64(1), Result: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "Content-Length: ") || !strings.Contains(output.String(), `"ok":true`) {
		t.Fatalf("unexpected framed output: %q", output.String())
	}
}
