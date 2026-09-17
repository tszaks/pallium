package knowledge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tszaks/pallium/internal/codeindex"
	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/index"
)

// stubSynth is called from several goroutines once synthesis fans out, so it
// guards its own bookkeeping.
type stubSynth struct {
	mu       sync.Mutex
	response string
	err      error
	prompts  []string
}

func (s *stubSynth) Synthesize(_ context.Context, prompt string) (string, error) {
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	return s.response, nil
}

func (s *stubSynth) promptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

// TestBuildDropsUnresolvableClaims is the reason this package exists. A model
// that invents a symbol must not be able to put it in the knowledge base, and
// the doc must say so rather than quietly shipping a shorter version.
func TestBuildDropsUnresolvableClaims(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)

	synth := &stubSynth{response: `{
  "summary": "Storage layer.",
  "purpose": "Opens and queries the database.",
  "entry_points": [{"claim": "Open is the way in", "cited_paths": ["storage/store.go"], "cited_symbols": ["Open"]}],
  "invariants": [
    {"claim": "Real claim about a real symbol", "cited_symbols": ["Store"], "cited_paths": []},
    {"claim": "Invented claim", "cited_symbols": ["QuantumReconciler"], "cited_paths": []},
    {"claim": "Points at a file that does not exist", "cited_paths": ["storage/ghost.go"], "cited_symbols": []},
    {"claim": "Cites nothing at all", "cited_paths": [], "cited_symbols": []}
  ]
}`}

	report, err := Build(store, repoID, repoRoot, BuildOptions{Synth: synth, Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if report.Dropped != 3 {
		t.Fatalf("expected 3 dropped claims, got %d", report.Dropped)
	}

	doc, found, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil || !found {
		t.Fatalf("doc not stored: %v found=%t", err, found)
	}
	if doc.Verified {
		t.Fatal("a doc with dropped claims must not be marked verified")
	}
	if !strings.Contains(doc.Body, "Real claim about a real symbol") {
		t.Fatalf("the surviving claim should be in the body:\n%s", doc.Body)
	}
	for _, forbidden := range []string{"QuantumReconciler", "storage/ghost.go", "Cites nothing at all"} {
		if strings.Contains(doc.Body, forbidden) {
			t.Fatalf("unverified claim %q leaked into the body:\n%s", forbidden, doc.Body)
		}
	}
	if len(doc.DroppedClaims) != 3 {
		t.Fatalf("dropped claims should be recorded on the doc, got %v", doc.DroppedClaims)
	}
}

func TestBuildWithoutModelStillProducesDocs(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)

	report, err := Build(store, repoID, repoRoot, BuildOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if report.Written == 0 {
		t.Fatal("a structural build should still write docs")
	}
	if report.Generator != "structural" {
		t.Fatalf("expected a structural generator, got %q", report.Generator)
	}
	if report.Unverified != 0 {
		t.Fatalf("structural docs have nothing to disprove, got %d unverified", report.Unverified)
	}

	for _, slug := range []string{"overview", "incidents", "storage"} {
		if _, found, err := store.KnowledgeDoc(repoID, slug); err != nil || !found {
			t.Fatalf("expected a %s doc: %v found=%t", slug, err, found)
		}
	}
}

// TestBuildSkipsUnchangedModules covers the fingerprint: rebuilding an
// untouched repo must not pay for a second round of model calls.
func TestBuildSkipsUnchangedModules(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)

	if _, err := Build(store, repoID, repoRoot, BuildOptions{}); err != nil {
		t.Fatalf("first build: %v", err)
	}

	synth := &stubSynth{response: `{"summary":"x","purpose":"y"}`}
	second, err := Build(store, repoID, repoRoot, BuildOptions{Synth: synth})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if synth.promptCount() != 0 {
		t.Fatalf("unchanged modules should not be synthesized again, got %d prompts", synth.promptCount())
	}
	if second.Unchanged == 0 {
		t.Fatalf("expected unchanged modules, got %+v", second)
	}
}

func TestBuildRecordsSynthesizerFailureWithoutLosingTheDoc(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)

	synth := &stubSynth{err: errors.New("provider exploded")}
	report, err := Build(store, repoID, repoRoot, BuildOptions{Synth: synth, Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("a failing model should not fail the build: %v", err)
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Reason, "provider exploded") {
		t.Fatalf("expected the failure recorded, got %+v", report.Failures)
	}

	doc, found, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil || !found {
		t.Fatalf("the structural doc should still exist: %v found=%t", err, found)
	}
	if !strings.Contains(doc.Body, "storage/store.go") {
		t.Fatalf("structural content missing from the fallback doc:\n%s", doc.Body)
	}
}

func TestModulesDeriveDependenciesFromImports(t *testing.T) {
	store, repoID, _ := indexedRepo(t)

	modules, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		t.Fatalf("modules: %v", err)
	}

	bySlug := map[string]Module{}
	for _, module := range modules {
		bySlug[module.Slug] = module
	}

	api, ok := bySlug["api"]
	if !ok {
		t.Fatalf("expected an api module, got %v", slugs(modules))
	}
	if !contains(api.DependsOn, "storage") {
		t.Fatalf("api imports storage, expected the edge: %+v", api.DependsOn)
	}

	storage, ok := bySlug["storage"]
	if !ok {
		t.Fatal("expected a storage module")
	}
	if !contains(storage.DependedOnBy, "api") {
		t.Fatalf("expected the reverse edge on storage: %+v", storage.DependedOnBy)
	}
	// Ranking is exported-first, then by how many files reference the name.
	// Here every exported symbol is referenced once, so the tie breaks
	// alphabetically; what matters is that the module's surface is what shows
	// up, not the unexported field or a test helper.
	names := map[string]bool{}
	for _, symbol := range storage.KeySymbols {
		if !symbol.Exported {
			t.Fatalf("unexported symbol ranked into the surface: %+v", symbol)
		}
		names[symbol.Name] = true
	}
	for _, want := range []string{"Open", "Store", "Query"} {
		if !names[want] {
			t.Fatalf("expected %s in the module surface, got %+v", want, storage.KeySymbols)
		}
	}
}

func TestMaterializeWritesAndClearsStalePages(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	dir := filepath.Join(repoRoot, ".pallium", "knowledge")

	if _, err := Build(store, repoID, repoRoot, BuildOptions{Materialize: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "storage.md")); err != nil {
		t.Fatalf("expected a materialized page: %v", err)
	}

	stale := filepath.Join(dir, "module-that-merged-away.md")
	if err := os.WriteFile(stale, []byte("# old\n"), 0o644); err != nil {
		t.Fatalf("write stale page: %v", err)
	}
	if err := Materialize(store, repoID, dir); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a page with no doc behind it should be removed, not left looking current")
	}
}

func TestIncidentDocOnlyNamesRealCommits(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)

	if _, err := Build(store, repoID, repoRoot, BuildOptions{}); err != nil {
		t.Fatalf("build: %v", err)
	}

	doc, found, err := store.KnowledgeDoc(repoID, "incidents")
	if err != nil || !found {
		t.Fatalf("incidents doc missing: %v found=%t", err, found)
	}
	if !strings.Contains(doc.Body, "Revert the broken cache write") {
		t.Fatalf("expected the revert commit in the incident list:\n%s", doc.Body)
	}
	if strings.Contains(doc.Body, "add api handler") {
		t.Fatalf("an ordinary commit should not be an incident:\n%s", doc.Body)
	}
}

func TestSearchKnowledgeRanksBySummaryAndBody(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	if _, err := Build(store, repoID, repoRoot, BuildOptions{}); err != nil {
		t.Fatalf("build: %v", err)
	}

	docs, err := store.SearchKnowledge(repoID, "storage", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("expected a match for storage")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func slugs(modules []Module) []string {
	out := make([]string, 0, len(modules))
	for _, module := range modules {
		out = append(out, module.Slug)
	}
	return out
}

func indexedRepo(t *testing.T) (*db.Store, int64, string) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Test User")
	git(t, repo, "config", "user.email", "test@example.com")

	files := map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.26.0\n",
		"storage/store.go": `package storage

// Store owns the connection.
type Store struct{ name string }

// Open returns a Store.
func Open(name string) *Store { return &Store{name: name} }

func (s *Store) Name() string { return s.name }
`,
		"storage/query.go": "package storage\n\nfunc Query(s *Store) string { return s.Name() }\n",
		"storage/cache.go": "package storage\n\nfunc Cache() string { return \"c\" }\n",
		"api/handler.go": `package api

import "example.com/app/storage"

func Handle() string {
	s := storage.Open("x")
	return storage.Query(s)
}
`,
		"api/router.go": "package api\n\nfunc Route() string { return Handle() }\n",
		"api/mw.go":     "package api\n\nfunc Middleware() string { return Route() }\n",
	}
	for path, content := range files {
		write(t, filepath.Join(repo, path), content)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "feat: add api handler")

	write(t, filepath.Join(repo, "storage", "cache.go"), "package storage\n\nfunc Cache() string { return \"cached\" }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "Revert the broken cache write")

	store, err := db.OpenPath(repo, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index: %v", err)
	}
	record, err := store.Repo()
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	if !store.HasCodeIndex(record.ID) {
		t.Fatal("expected a content index")
	}
	_ = codeindex.Lang("x.go")
	_ = time.Now()
	return store, record.ID, repo
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// TestBuildFansOutWithoutLosingModules covers the concurrency added for real
// repos: synthesis runs in parallel, storage does not, and every module still
// gets exactly one call and one doc.
func TestBuildFansOutWithoutLosingModules(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)

	synth := &stubSynth{response: `{"summary":"A module.","purpose":"It does a thing.","invariants":[{"claim":"Open returns a Store","cited_symbols":["Open"],"cited_paths":[]}]}`}
	report, err := Build(store, repoID, repoRoot, BuildOptions{Synth: synth, Concurrency: 4})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	modules, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		t.Fatalf("modules: %v", err)
	}
	if synth.promptCount() != len(modules) {
		t.Fatalf("expected one call per module (%d), got %d", len(modules), synth.promptCount())
	}

	docs, err := store.KnowledgeDocs(repoID)
	if err != nil {
		t.Fatalf("docs: %v", err)
	}
	// One doc per module, plus the overview and incident docs.
	if len(docs) != len(modules)+2 {
		t.Fatalf("expected %d docs, got %d", len(modules)+2, len(docs))
	}
	if report.Dropped != 0 {
		t.Fatalf("every claim cited a real symbol, got %d dropped", report.Dropped)
	}
	for _, doc := range docs {
		if doc.Kind == "module" && !strings.Contains(doc.Body, "Open returns a Store") {
			t.Fatalf("module doc %s lost its synthesized claim:\n%s", doc.Slug, doc.Body)
		}
	}
}

// TestAuditRemovesClaimsTheSourceDoesNotSupport covers the gap build cannot
// close: a claim can cite a symbol that exists and still be false.
func TestAuditRemovesClaimsTheSourceDoesNotSupport(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)

	builder := &stubSynth{response: `{
  "summary": "Storage layer.",
  "purpose": "Opens and queries the database.",
  "invariants": [
    {"claim": "Open returns a Store", "cited_symbols": ["Open"], "cited_paths": []},
    {"claim": "Open deletes every row in the database", "cited_symbols": ["Open"], "cited_paths": []}
  ]
}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
		t.Fatalf("build: %v", err)
	}

	before, _, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil {
		t.Fatalf("doc: %v", err)
	}
	if !before.Verified {
		t.Fatalf("both citations resolve, so build should call this verified: %v", before.DroppedClaims)
	}
	if !strings.Contains(before.Body, "deletes every row") {
		t.Fatal("build cannot catch a false claim about a real symbol; that is the point of the audit")
	}

	auditor := &stubSynth{response: `{"verdicts": [
  {"index": 1, "ruling": "supported", "why": "Open constructs and returns a *Store."},
  {"index": 2, "ruling": "unsupported", "why": "Open only builds a struct; there is no delete."}
]}`}
	report, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if report.Removed != 1 || report.Supported != 1 {
		t.Fatalf("expected one removal and one survivor, got %+v", report)
	}

	after, _, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil {
		t.Fatalf("doc after audit: %v", err)
	}
	if strings.Contains(after.Body, "deletes every row") {
		t.Fatalf("the refuted claim survived in the body:\n%s", after.Body)
	}
	if !strings.Contains(after.Body, "Open returns a Store") {
		t.Fatalf("the supported claim should remain:\n%s", after.Body)
	}
	if after.Verified {
		t.Fatal("a doc that lost a claim to the audit is no longer clean")
	}
	found := false
	for _, dropped := range after.DroppedClaims {
		if strings.HasPrefix(dropped, "audit:") && strings.Contains(dropped, "no delete") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the audit's reason should travel with the doc, got %v", after.DroppedClaims)
	}

	// The prompt must contain real source, or this is a second opinion rather
	// than an audit.
	if len(auditor.prompts) == 0 || !strings.Contains(auditor.prompts[0], "func Open(name string) *Store") {
		t.Fatal("the auditor was not shown the cited symbol's source")
	}
}

func TestAuditNeedsAModel(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{}); err == nil {
		t.Fatal("an audit with no model should refuse rather than report everything clean")
	}
}

// TestAuditSkipsStructuralDocs keeps the audit from spending calls on docs
// where no model asserted anything.
func TestAuditSkipsStructuralDocs(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	if _, err := Build(store, repoID, repoRoot, BuildOptions{}); err != nil {
		t.Fatalf("build: %v", err)
	}

	auditor := &stubSynth{response: `{"verdicts": [{"index": 1, "ruling": "supported", "why": "x"}]}`}
	report, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if report.Audited != 0 || report.Skipped == 0 {
		t.Fatalf("structural docs have no claims to audit, got %+v", report)
	}
	if auditor.promptCount() != 0 {
		t.Fatalf("no model calls should have been made, got %d", auditor.promptCount())
	}
}
