package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
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

type workflowLibraryInstallRequest struct {
	Pack  string `json:"pack"`
	CWD   string `json:"cwd,omitempty"`
	Name  string `json:"name,omitempty"`
	Force bool   `json:"force,omitempty"`
}

func runWorkflowServe(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("workflow serve")
	dbPath := fs.String("db", "", "")
	addr := fs.String("addr", "127.0.0.1:8765", "")
	token := fs.String("token", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "addr": {}, "token": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: pallium workflow serve [--addr host:port] [--token token]")
	}
	apiToken := strings.TrimSpace(firstNonEmpty(*token, os.Getenv("PALLIUM_WORKFLOW_API_TOKEN")))
	if apiToken == "" && !isLocalHTTPAddr(*addr) {
		return fmt.Errorf("workflow serve requires --token or PALLIUM_WORKFLOW_API_TOKEN when binding outside localhost")
	}
	server := &http.Server{Addr: *addr, Handler: newWorkflowHTTPHandler(*dbPath, apiToken)}
	fmt.Fprintf(out, "pallium workflow API listening on http://%s\n", *addr)
	return server.ListenAndServe()
}

func newWorkflowHTTPHandler(dbPath, token string) http.Handler {
	token = strings.TrimSpace(token)
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
	mux.HandleFunc("GET /workflows/analytics", func(w http.ResponseWriter, r *http.Request) {
		limit := 500
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 {
				http.Error(w, "invalid limit", http.StatusBadRequest)
				return
			}
			limit = parsed
		}
		store, err := workflow.Open(dbPath)
		if err != nil {
			writeHTTPError(w, err)
			return
		}
		defer store.Close()
		analytics, err := buildWorkflowAnalytics(store, limit)
		if err != nil {
			writeHTTPError(w, err)
			return
		}
		writeJSON(w, analytics)
	})
	mux.HandleFunc("GET /workflows/library", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, workflow.WorkflowPacks())
	})
	mux.HandleFunc("GET /workflows/library/{name}", func(w http.ResponseWriter, r *http.Request) {
		pack, ok := workflow.WorkflowPack(r.PathValue("name"))
		if !ok {
			http.Error(w, "unknown workflow library pack", http.StatusNotFound)
			return
		}
		writeJSON(w, pack)
	})
	mux.HandleFunc("POST /workflows/library/install", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req workflowLibraryInstallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		runArgs, err := workflowLibraryInstallRequestArgs(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var out bytes.Buffer
		if err := runWorkflowLibraryInstall(&out, runArgs, true); err != nil {
			writeHTTPError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out.Bytes())
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
	if strings.TrimSpace(token) == "" {
		return mux
	}
	return requireWorkflowAPIToken(mux, token)
}

func requireWorkflowAPIToken(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if subtleConstantTimeCompareBearer(r.Header.Get("Authorization"), token) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "missing or invalid workflow API token", http.StatusUnauthorized)
	})
}

func subtleConstantTimeCompareBearer(header, token string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if got == "" || token == "" || len(got) != len(token) {
		return false
	}
	var diff byte
	for i := 0; i < len(token); i++ {
		diff |= got[i] ^ token[i]
	}
	return diff == 0
}

func isLocalHTTPAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

func workflowLibraryInstallRequestArgs(req workflowLibraryInstallRequest) ([]string, error) {
	if strings.TrimSpace(req.Pack) == "" {
		return nil, fmt.Errorf("pack is required")
	}
	args := []string{req.Pack}
	if req.CWD != "" {
		args = append(args, "--cwd", req.CWD)
	}
	if req.Name != "" {
		args = append(args, "--name", req.Name)
	}
	if req.Force {
		args = append(args, "--force")
	}
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
