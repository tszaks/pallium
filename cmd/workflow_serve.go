package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tszaks/pallium/internal/workflow"
)

type workflowRunRequest struct {
	ID           string         `json:"id,omitempty"`
	Task         string         `json:"task"`
	CWD          string         `json:"cwd,omitempty"`
	ScriptPath   string         `json:"script_path,omitempty"`
	WorkflowName string         `json:"workflow_name,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	ArgsJSON     string         `json:"args_json,omitempty"`
}

func runWorkflowServe(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow serve")
	dbPath := fs.String("db", "", "")
	addr := fs.String("addr", "127.0.0.1:8765", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "addr": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: pallium workflow serve [--addr host:port]")
	}
	server := &http.Server{Addr: *addr, Handler: newWorkflowHTTPHandler(*dbPath)}
	fmt.Fprintf(out, "pallium workflow API listening on http://%s\n", *addr)
	return server.ListenAndServe()
}

func newWorkflowHTTPHandler(dbPath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /workflows/fleet", func(w http.ResponseWriter, r *http.Request) {
		store, err := workflow.Open(dbPath)
		if err != nil {
			writeHTTPError(w, err)
			return
		}
		defer store.Close()
		status, err := buildWorkflowFleetStatus(store, 50)
		if err != nil {
			writeHTTPError(w, err)
			return
		}
		writeJSON(w, status)
	})
	mux.HandleFunc("GET /workflows/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		store, err := workflow.Open(dbPath)
		if err != nil {
			writeHTTPError(w, err)
			return
		}
		defer store.Close()
		snapshot, err := store.Snapshot(r.PathValue("id"))
		if err != nil {
			writeHTTPError(w, err)
			return
		}
		writeJSON(w, snapshot)
	})
	mux.HandleFunc("POST /workflows/run", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req workflowRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		runArgs, err := workflowRunRequestArgs(dbPath, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var out bytes.Buffer
		if err := runWorkflowRun(&out, runArgs, true); err != nil {
			writeHTTPError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out.Bytes())
	})
	return mux
}

func workflowRunRequestArgs(dbPath string, req workflowRunRequest) ([]string, error) {
	if strings.TrimSpace(req.Task) == "" {
		return nil, fmt.Errorf("task is required")
	}
	if req.ScriptPath != "" && req.WorkflowName != "" {
		return nil, fmt.Errorf("use either script_path or workflow_name, not both")
	}
	args := []string{"--db", dbPath}
	if req.ID != "" {
		args = append(args, "--id", req.ID)
	}
	if req.CWD != "" {
		args = append(args, "--cwd", req.CWD)
	}
	if req.ScriptPath != "" {
		args = append(args, "--script", req.ScriptPath)
	}
	if req.WorkflowName != "" {
		args = append(args, "--workflow", req.WorkflowName)
	}
	argsJSON := strings.TrimSpace(req.ArgsJSON)
	if argsJSON == "" && len(req.Args) > 0 {
		raw, err := json.Marshal(req.Args)
		if err != nil {
			return nil, err
		}
		argsJSON = string(raw)
	}
	if argsJSON != "" {
		args = append(args, "--args", argsJSON)
	}
	args = append(args, req.Task)
	return args, nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func writeHTTPError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
