package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type mcpServer struct {
	dbPath string
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func runWorkflowMCP(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow mcp")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: pallium workflow mcp [--db path]")
	}
	return (&mcpServer{dbPath: *dbPath}).serveStdio(bufio.NewReader(os.Stdin), out)
}

func (s *mcpServer) serveStdio(reader *bufio.Reader, writer io.Writer) error {
	for {
		raw, err := readMCPMessage(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var req mcpRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			_ = writeMCPMessage(writer, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: -32700, Message: err.Error()}})
			continue
		}
		if req.ID == nil {
			continue
		}
		resp := s.handle(req)
		if err := writeMCPMessage(writer, resp); err != nil {
			return err
		}
	}
}

func (s *mcpServer) handle(req mcpRequest) mcpResponse {
	result, err := s.handleResult(req)
	if err != nil {
		return mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32000, Message: err.Error()}}
	}
	return mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *mcpServer) handleResult(req mcpRequest) (any, error) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pallium-workflow", "version": "1.0.0"},
		}, nil
	case "tools/list":
		return map[string]any{"tools": workflowMCPTools()}, nil
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, err
		}
		text, err := s.callTool(params.Name, params.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported MCP method %q", req.Method)
	}
}

func workflowMCPTools() []mcpTool {
	return []mcpTool{
		{Name: "pallium_workflow_run", Description: "Run a structured multi-step workflow with verification, parallel workers, and resumable state. Prefer this over ad-hoc agent loops for any non-trivial task.", InputSchema: objectSchema(map[string]any{"task": stringSchema(), "id": stringSchema(), "cwd": stringSchema(), "script_path": stringSchema(), "workflow_name": stringSchema(), "args_json": stringSchema()})},
		{Name: "pallium_workflow_status", Description: "Check the progress, step results, and failures of a workflow run. Use after starting a run, or when picking up an earlier run id.", InputSchema: objectSchema(map[string]any{"id": stringSchema()})},
		{Name: "pallium_workflow_fleet", Description: "List recent workflow runs and their states. Use to find an existing run id or check what is already running before starting new work.", InputSchema: objectSchema(map[string]any{"limit": numberSchema()})},
		{Name: "pallium_workflow_analytics", Description: "Summarize aggregate workflow outcomes, durations, and costs. Use when reviewing how past runs performed or reporting on workflow activity.", InputSchema: objectSchema(map[string]any{"limit": numberSchema()})},
		{Name: "pallium_workflow_library", Description: "Browse or install prebuilt workflow packs. Use before writing a workflow script from scratch to check whether a ready-made recipe already covers the task.", InputSchema: objectSchema(map[string]any{"action": stringSchema(), "pack": stringSchema(), "cwd": stringSchema(), "name": stringSchema(), "force": boolSchema()})},
	}
}

func (s *mcpServer) callTool(name string, args map[string]any) (string, error) {
	switch name {
	case "pallium_workflow_run":
		task := stringArg(args, "task")
		if task == "" {
			return "", fmt.Errorf("task is required")
		}
		runArgs := []string{"run", "--db", s.dbPath}
		appendStringFlag(&runArgs, "--id", stringArg(args, "id"))
		appendStringFlag(&runArgs, "--cwd", stringArg(args, "cwd"))
		appendStringFlag(&runArgs, "--script", stringArg(args, "script_path"))
		appendStringFlag(&runArgs, "--workflow", stringArg(args, "workflow_name"))
		appendStringFlag(&runArgs, "--args", stringArg(args, "args_json"))
		runArgs = append(runArgs, task)
		return runWorkflowMCPCommand(runArgs)
	case "pallium_workflow_status":
		id := stringArg(args, "id")
		if id == "" {
			return "", fmt.Errorf("id is required")
		}
		return runWorkflowMCPCommand([]string{"show", id, "--db", s.dbPath})
	case "pallium_workflow_fleet":
		runArgs := []string{"fleet", "status", "--db", s.dbPath}
		appendNumberFlag(&runArgs, "--limit", args["limit"])
		return runWorkflowMCPCommand(runArgs)
	case "pallium_workflow_analytics":
		runArgs := []string{"analytics", "--db", s.dbPath}
		appendNumberFlag(&runArgs, "--limit", args["limit"])
		return runWorkflowMCPCommand(runArgs)
	case "pallium_workflow_library":
		action := stringArg(args, "action")
		if action == "" {
			action = "list"
		}
		switch action {
		case "list":
			return runWorkflowMCPCommand([]string{"library", "list"})
		case "show":
			pack := stringArg(args, "pack")
			if pack == "" {
				return "", fmt.Errorf("pack is required")
			}
			return runWorkflowMCPCommand([]string{"library", "show", pack})
		case "install":
			pack := stringArg(args, "pack")
			if pack == "" {
				return "", fmt.Errorf("pack is required")
			}
			runArgs := []string{"library", "install", pack}
			appendStringFlag(&runArgs, "--cwd", stringArg(args, "cwd"))
			appendStringFlag(&runArgs, "--name", stringArg(args, "name"))
			if boolArg(args, "force") {
				runArgs = append(runArgs, "--force")
			}
			return runWorkflowMCPCommand(runArgs)
		default:
			return "", fmt.Errorf("unsupported library action %q", action)
		}
	default:
		return "", fmt.Errorf("unknown workflow MCP tool %q", name)
	}
}

func runWorkflowMCPCommand(args []string) (string, error) {
	var out bytes.Buffer
	if err := runWorkflow(&out, args, true); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func readMCPMessage(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") {
		return []byte(trimmed), nil
	}
	contentLength := 0
	for {
		if key, value, ok := strings.Cut(trimmed, ":"); ok && strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			value = strings.TrimSpace(value)
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return nil, err
			}
			contentLength = parsed
		}
		if trimmed == "" {
			break
		}
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed = strings.TrimSpace(line)
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	raw := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func writeMCPMessage(writer io.Writer, resp mcpResponse) error {
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(raw), raw)
	return err
}

func objectSchema(properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties}
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }
func numberSchema() map[string]any { return map[string]any{"type": "number"} }
func boolSchema() map[string]any   { return map[string]any{"type": "boolean"} }

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func boolArg(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}

func appendStringFlag(args *[]string, name, value string) {
	if value != "" {
		*args = append(*args, name, value)
	}
}

func appendNumberFlag(args *[]string, name string, value any) {
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			*args = append(*args, name, strconv.Itoa(int(typed)))
		}
	case int:
		if typed > 0 {
			*args = append(*args, name, strconv.Itoa(typed))
		}
	}
}
