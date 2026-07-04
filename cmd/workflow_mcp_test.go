package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
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
