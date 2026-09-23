package knowledge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/tszaks/pallium/internal/index"
)

type evalQuestion struct{ Topic, Question, Description, Symbol string }

func TestKnowledgeGoldenQuestions(t *testing.T) {
	b, err := os.ReadFile("../../eval/knowledge/questions.json")
	if err != nil {
		t.Fatal(err)
	}
	var questions []evalQuestion
	if err := json.Unmarshal(b, &questions); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "eval@example.test")
	git(t, root, "config", "user.name", "Evaluation")
	for _, lang := range []string{"go", "ts", "swift"} {
		for _, q := range questions {
			for i := 0; i < 3; i++ {
				content := fmt.Sprintf("// %s\nexport function %s%d() { return true; }\n", q.Description, q.Symbol, i)
				if lang == "go" {
					content = fmt.Sprintf("package %s\n// %s\nfunc %s%d() bool { return true }\n", q.Topic, q.Description, q.Symbol, i)
				}
				if lang == "swift" {
					content = fmt.Sprintf("// %s\nfunc %s%d() -> Bool { return true }\n", q.Description, q.Symbol, i)
				}
				write(t, filepath.Join(root, lang, q.Topic, fmt.Sprintf("source%d.%s", i, lang)), content)
			}
		}
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "golden corpus")
	store, err := index.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := index.New(store).Run(); err != nil {
		t.Fatal(err)
	}
	repo, _ := store.Repo()
	if _, err := Build(store, repo.ID, root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	passed := 0
	for _, lang := range []string{"go", "ts", "swift"} {
		for _, q := range questions {
			result, err := Search(store, repo.ID, q.Question, 3, 8192)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, hit := range result.Results {
				if hit.Slug == slugForDir(lang+"/"+q.Topic) {
					found = true
				}
				if hit.Freshness != "current" {
					t.Fatalf("unexpected freshness: %+v", hit)
				}
			}
			if found {
				passed++
			} else {
				t.Logf("miss %s %s: %+v", lang, q.Question, result.Results)
			}
			encoded, _ := json.Marshal(result)
			if len(encoded) > 8192 {
				t.Fatal("search exceeded budget")
			}
			pack, err := Context(store, repo.ID, q.Question)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ = json.Marshal(pack)
			if len(encoded) > 12000 {
				t.Fatal("context exceeded budget")
			}
		}
	}
	t.Logf("top-3 recall %d/30", passed)
	if passed < 27 {
		t.Fatalf("top-3 recall %d/30 below 90%%", passed)
	}
}
func TestWarmRetrievalTwoThousandFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("performance fixture")
	}
	store, id, root := indexedRepo(t)
	for i := 0; i < 2000; i++ {
		write(t, filepath.Join(root, "cache", fmt.Sprintf("file%04d.go", i)), fmt.Sprintf("package cache\n// Cache evicts expired entries.\nfunc Evict%d() {}\n", i))
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "2000 file retrieval fixture")
	if _, err := index.New(store).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(store, id, root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	var samples []time.Duration
	for i := 0; i < 21; i++ {
		start := time.Now()
		if _, err := Search(store, id, "expired cached entries", 5, 8192); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			samples = append(samples, time.Since(start))
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p95 := samples[18]
	t.Logf("warm retrieval p95 %s over 2000 added source files", p95)
	if p95 > 250*time.Millisecond {
		t.Fatalf("p95 exceeds 250ms: %s", p95)
	}
}
