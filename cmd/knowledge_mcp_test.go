package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestKnowledgeMCPToolsAreAdvertised guards the whole point of Phase 3: before
// this, an agent on Pallium's MCP could start a workflow but could not ask a
// single question about the repo.
func TestKnowledgeMCPToolsAreAdvertised(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range workflowMCPTools() {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"pallium_knowledge_search", "pallium_knowledge_get", "pallium_knowledge_map",
		"pallium_explain", "pallium_symbols", "pallium_callers", "pallium_search_code",
	} {
		if !names[want] {
			t.Fatalf("%s is not advertised", want)
		}
	}
	if !names["pallium_workflow_run"] {
		t.Fatal("the workflow tools must survive the addition")
	}
}

func TestKnowledgeMCPToolsAnswerFromTheIndex(t *testing.T) {
	repo := mcpTestRepo(t)
	server := &mcpServer{}

	if _, err := runIndexForTest(repo); err != nil {
		t.Fatalf("index: %v", err)
	}

	symbolsOut, err := server.callTool("pallium_symbols", map[string]any{"path": "storage/store.go", "cwd": repo})
	if err != nil {
		t.Fatalf("pallium_symbols: %v", err)
	}
	var symbolsReport struct {
		Symbols []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal([]byte(symbolsOut), &symbolsReport); err != nil {
		t.Fatalf("symbols output was not JSON: %v\n%s", err, symbolsOut)
	}
	found := false
	for _, symbol := range symbolsReport.Symbols {
		if symbol.Name == "Open" && symbol.Kind == "func" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected Open in the symbols response: %s", symbolsOut)
	}

	callersOut, err := server.callTool("pallium_callers", map[string]any{"symbol": "Open", "cwd": repo})
	if err != nil {
		t.Fatalf("pallium_callers: %v", err)
	}
	if !strings.Contains(callersOut, "api/handler.go") {
		t.Fatalf("expected the calling file in the callers response: %s", callersOut)
	}

	if _, err := server.callTool("pallium_symbols", map[string]any{"cwd": repo}); err == nil {
		t.Fatal("a missing path should be an error, not an empty answer")
	}
}

func runIndexForTest(repo string) (string, error) {
	var out strings.Builder
	err := runIndex(&out, []string{repo}, true)
	return out.String(), err
}

func mcpTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}

	files := map[string]string{
		"go.mod":           "module example.com/app\n\ngo 1.26.0\n",
		"storage/store.go": "package storage\n\ntype Store struct{}\n\n// Open returns a Store.\nfunc Open() *Store { return &Store{} }\n",
		"api/handler.go":   "package api\n\nimport \"example.com/app/storage\"\n\nfunc Handle() *storage.Store { return storage.Open() }\n",
	}
	for path, content := range files {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	return repo
}

func TestKnowledgeContextContractAndFreshness(t *testing.T) {
	t.Setenv("PALLIUM_TEST_DB", filepath.Join(t.TempDir(), "global.sqlite"))
	repo := mcpTestRepo(t)
	if _, err := runIndexForTest(repo); err != nil {
		t.Fatal(err)
	}
	var build strings.Builder
	if err := runKnowledge(&build, []string{"build", "--no-model", "--no-materialize", repo}, true); err != nil {
		t.Fatal(err)
	}
	server := &mcpServer{}
	text, err := server.callTool("pallium_knowledge_context", map[string]any{"query": "where does Open return a Store", "cwd": repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > 12000 {
		t.Fatal("MCP context budget exceeded")
	}
	var pack struct {
		Version int `json:"version"`
		Results []struct {
			Freshness string `json:"freshness"`
		}
	}
	if err := json.Unmarshal([]byte(text), &pack); err != nil {
		t.Fatal(err)
	}
	if pack.Version != 2 || len(pack.Results) == 0 {
		t.Fatal(text)
	}
	for _, r := range pack.Results {
		if r.Freshness != "current" {
			t.Fatal("fresh document not current")
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "storage/store.go"), []byte("package storage\nfunc Refresh(){}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	text, err = server.callTool("pallium_knowledge_search", map[string]any{"query": "storage", "cwd": repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > 8192 || !strings.Contains(text, `"freshness": "stale"`) {
		t.Fatal(text)
	}
	var help strings.Builder
	if err := runKnowledge(&help, []string{"--help"}, false); err != nil {
		t.Fatal(err)
	}
}
