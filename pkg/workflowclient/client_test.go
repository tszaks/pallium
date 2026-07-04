package workflowclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientCallsWorkflowAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /workflows/analytics", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "25" {
			t.Fatalf("limit query=%q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"runs_total":1}`))
	})
	mux.HandleFunc("POST /workflows/run", func(w http.ResponseWriter, r *http.Request) {
		var req RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Task != "test task" {
			t.Fatalf("task=%q", req.Task)
		}
		_, _ = w.Write([]byte(`{"run":{"id":"wf-api","status":"completed"}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := New(server.URL)
	if raw, err := client.Health(context.Background()); err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("health raw=%s err=%v", string(raw), err)
	}
	if raw, err := client.Analytics(context.Background(), 25); err != nil || string(raw) != `{"runs_total":1}` {
		t.Fatalf("analytics raw=%s err=%v", string(raw), err)
	}
	raw, err := client.Run(context.Background(), RunRequest{ID: "wf-api", Task: "test task"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"run":{"id":"wf-api","status":"completed"}}` {
		t.Fatalf("unexpected run response: %s", string(raw))
	}
}
