package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tszaks/pallium/internal/index"
)

// Explicit opt-in: four real provider attempts, counted in the shared ledger.
// A paired task measures observed answer quality; it does not establish broad
// productivity gains. Output is retained as a reproducible release artifact.
func TestPairedAgentEvaluation(t *testing.T) {
	root := os.Getenv("PALLIUM_AGENT_EVAL_REPO")
	if root == "" {
		t.Skip("set PALLIUM_AGENT_EVAL_REPO for paid paired evaluation")
	}
	store, err := index.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo, err := store.Repo()
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		Task, Mode, Answer  string
		InputBytes          int
		DurationMS          int64
		ExpectedSourceFound bool
	}
	var results []result
	tasks := []struct{ q, path string }{{"Which source file implements stopping a loop? Give the function name and its stop behavior.", "cmd/loop.go"}, {"Which source file handles updating the installed Pallium binary? Give the function name and the update paths.", "cmd/update.go"}}
	synth := ProviderSynthesizer{RepoRoot: root}
	for _, task := range tasks {
		pack, err := Context(store, repo.ID, task.q)
		if err != nil {
			t.Fatal(err)
		}
		compact, _ := json.Marshal(pack)
		docs, err := store.SearchKnowledge(repo.ID, task.q, 10)
		if err != nil {
			t.Fatal(err)
		}
		full, _ := json.Marshal(docs)
		for _, variant := range []struct{ mode, input string }{{"legacy_full_docs", string(full)}, {"compact_context", string(compact)}} {
			prompt := fmt.Sprintf("Answer the task using the supplied repository context. You may read source files if needed. Do not modify files. Answer in at most 150 words, naming the exact source path and function.\nTask: %s\nContext:\n%s", task.q, variant.input)
			start := time.Now()
			answer, err := synth.Synthesize(context.Background(), prompt)
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, result{task.q, variant.mode, answer, len(prompt), time.Since(start).Milliseconds(), strings.Contains(answer, task.path)})
		}
	}
	b, _ := json.MarshalIndent(results, "", "  ")
	out := filepath.Join(os.TempDir(), "pallium-paired-agent-results.json")
	if err := os.WriteFile(out, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(out)
	for _, r := range results {
		t.Logf("%s bytes=%d duration=%d source_match=%t", r.Mode, r.InputBytes, r.DurationMS, r.ExpectedSourceFound)
	}
}
