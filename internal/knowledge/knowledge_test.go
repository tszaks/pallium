package knowledge

import (
	"context"
	"encoding/json"
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
	respond  func(string) string
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
	if s.respond != nil {
		return s.respond(prompt), nil
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
  "summary": {"claim": "Storage layer.", "cited_paths": ["storage/store.go"], "cited_symbols": []},
  "purpose": {"claim": "Opens and queries the database.", "cited_paths": ["storage/store.go"], "cited_symbols": []},
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

	firstSynth := &stubSynth{response: `{"summary":{"claim":"A module.","cited_symbols":["Open"]},"purpose":{"claim":"It does a thing.","cited_symbols":["Open"]}}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: firstSynth}); err != nil {
		t.Fatalf("first build: %v", err)
	}

	secondSynth := &stubSynth{response: firstSynth.response}
	second, err := Build(store, repoID, repoRoot, BuildOptions{Synth: secondSynth})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if secondSynth.promptCount() != 0 {
		t.Fatalf("unchanged modules should not be re-synthesized, got %d prompts", secondSynth.promptCount())
	}
	if second.Unchanged == 0 {
		t.Fatalf("expected unchanged model docs to be reused, got %+v", second)
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

	synth := &stubSynth{response: `{"summary":{"claim":"A module.","cited_symbols":["Open"]},"purpose":{"claim":"It does a thing.","cited_symbols":["Open"]},"invariants":[{"claim":"Open returns a Store","cited_symbols":["Open"],"cited_paths":[]}]}`}
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
		t.Fatalf("shared symbol citations should resolve for every module, got %d dropped", report.Dropped)
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
  "summary": {"claim": "Storage layer.", "cited_paths": ["storage/store.go"], "cited_symbols": []},
  "purpose": {"claim": "Opens and queries the database.", "cited_paths": ["storage/store.go"], "cited_symbols": []},
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
  {"index": 1, "ruling": "supported", "why": "The storage file describes the layer."},
  {"index": 2, "ruling": "supported", "why": "The storage file describes its purpose."},
  {"index": 3, "ruling": "supported", "why": "Open constructs and returns a *Store."},
  {"index": 4, "ruling": "unsupported", "why": "Open only builds a struct; there is no delete."}
]}`}
	report, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if report.Removed != 1 || report.Supported != 3 {
		t.Fatalf("expected one removal and three survivors, got %+v", report)
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

func TestAuditRequiresRebuildForLegacyUncitedClaims(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	modules, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		t.Fatalf("modules: %v", err)
	}
	var storage Module
	for _, module := range modules {
		if module.Slug == "storage" {
			storage = module
			break
		}
	}
	legacy := db.KnowledgeDoc{
		Slug:        "storage",
		Kind:        "module",
		Title:       storage.Title,
		Body:        "legacy body",
		Claims:      `{"summary":"legacy summary","purpose":"legacy purpose","entry_points":[{"claim":"Open","cited_symbols":["Open"]}]}`,
		Fingerprint: storage.Fingerprint,
		Generator:   "structural+model",
		Verified:    true,
		GeneratedAt: time.Now().UTC(),
	}
	if err := store.UpsertKnowledgeDoc(repoID, legacy); err != nil {
		t.Fatalf("store legacy doc: %v", err)
	}
	auditor := &stubSynth{err: errors.New("legacy claims must not be audited")}
	report, err := Audit(store, repoID, repoRoot, AuditOptions{
		Synth: auditor,
		Only:  []string{"storage"},
	})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if report.NeedsRebuild != 1 || auditor.promptCount() != 0 {
		t.Fatalf("legacy uncited claims should require rebuild without auditing: %+v calls=%d", report, auditor.promptCount())
	}
	after, _, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil {
		t.Fatalf("read legacy doc: %v", err)
	}
	if after.Claims != legacy.Claims || after.Body != legacy.Body || !after.Verified {
		t.Fatalf("legacy doc changed during audit: before=%+v after=%+v", legacy, after)
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

// TestAuditEvidenceIsScopedToTheModule is the regression for the worst bug in
// this feature. Symbol names repeat across packages (Pallium declares Store in
// four and Run in six). Resolving a cited name repo-wide and taking the first
// hit fed the auditor a stranger's source, and it then confidently refuted 44
// claims that were true, because the code it was shown genuinely did not match
// them. Wrong evidence is worse than no evidence.
func TestAuditEvidenceIsScopedToTheModule(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Test User")
	git(t, repo, "config", "user.email", "test@example.com")

	files := map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.26.0\n",
		// Sorts first by path, so a repo-wide lookup would pick this one.
		"alpha/store.go": "package alpha\n\ntype Store struct{ alphaOnlyField int }\n\nfunc Open() *Store { return &Store{} }\n",
		"alpha/more.go":  "package alpha\n\nfunc AlphaMore() {}\n",
		"alpha/extra.go": "package alpha\n\nfunc AlphaExtra() {}\n",
		// The module under audit declares the same names.
		"zeta/store.go": "package zeta\n\ntype Store struct{ zetaOnlyField string }\n\nfunc Open() *Store { return &Store{} }\n",
		"zeta/more.go":  "package zeta\n\nfunc ZetaMore() {}\n",
		"zeta/extra.go": "package zeta\n\nfunc ZetaExtra() {}\n",
	}
	for path, content := range files {
		write(t, filepath.Join(repo, path), content)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "init")

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

	builder := &stubSynth{response: `{
  "summary": {"claim": "Zeta storage.", "cited_paths": ["zeta/store.go"]},
  "purpose": {"claim": "Holds the zeta store.", "cited_paths": ["zeta/store.go"]},
  "invariants": [{"claim": "Store carries a zeta-only field", "cited_symbols": ["Store"], "cited_paths": ["zeta/store.go"]}]
}`}
	if _, err := Build(store, record.ID, repo, BuildOptions{Synth: builder, Only: []string{"zeta"}}); err != nil {
		t.Fatalf("build: %v", err)
	}

	auditor := &stubSynth{response: `{"verdicts": [
  {"index": 1, "ruling": "supported", "why": "zeta storage."},
  {"index": 2, "ruling": "supported", "why": "zeta purpose."},
  {"index": 3, "ruling": "supported", "why": "zetaOnlyField is right there."}
]}`}
	if _, err := Audit(store, record.ID, repo, AuditOptions{Synth: auditor, Only: []string{"zeta"}}); err != nil {
		t.Fatalf("audit: %v", err)
	}

	if len(auditor.prompts) != 1 {
		t.Fatalf("expected one audit prompt, got %d", len(auditor.prompts))
	}
	prompt := auditor.prompts[0]
	if !strings.Contains(prompt, "zetaOnlyField") {
		t.Fatalf("the audited module's own Store was not shown:\n%s", prompt)
	}
	if strings.Contains(prompt, "alphaOnlyField") {
		t.Fatalf("a same-named symbol from another module leaked into the evidence:\n%s", prompt)
	}
}

// TestAuditReportsMissingEvidenceInsteadOfSubstituting keeps the fix honest:
// when a cited symbol is nowhere in the module, the auditor must be told so
// and steered to "unclear", not handed something else.
func TestAuditReportsMissingEvidenceInsteadOfSubstituting(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)

	builder := &stubSynth{response: `{
  "summary": {"claim": "Storage.", "cited_paths": ["storage/store.go"]},
  "purpose": {"claim": "Storage things.", "cited_paths": ["storage/store.go"]},
  "invariants": [{"claim": "Handle does the work", "cited_symbols": ["Handle"], "cited_paths": []}]
}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
		t.Fatalf("build: %v", err)
	}

	auditor := &stubSynth{response: `{"verdicts": [
  {"index": 1, "ruling": "supported", "why": "Storage."},
  {"index": 2, "ruling": "supported", "why": "Storage."},
  {"index": 3, "ruling": "unclear", "why": "Not visible here."}
]}`}
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}}); err != nil {
		t.Fatalf("audit: %v", err)
	}

	prompt := auditor.prompts[0]
	// Handle is declared in the api module, not storage.
	if !strings.Contains(prompt, "No declaration was found in this module for: Handle") {
		t.Fatalf("missing evidence was not declared to the auditor:\n%s", prompt)
	}
	if strings.Contains(prompt, "api/handler.go") {
		t.Fatalf("evidence from another module leaked in:\n%s", prompt)
	}
}

func TestAuditRejectsIncompleteVerdictSets(t *testing.T) {
	tests := []struct {
		name     string
		verdicts string
	}{
		{"missing index", `[{"index":1,"ruling":"supported"}]`},
		{"duplicate index", `[{"index":1,"ruling":"supported"},{"index":1,"ruling":"supported"},{"index":2,"ruling":"supported"},{"index":3,"ruling":"supported"}]`},
		{"out of range", `[{"index":1,"ruling":"supported"},{"index":2,"ruling":"supported"},{"index":4,"ruling":"supported"}]`},
		{"unknown ruling", `[{"index":1,"ruling":"not_supported"},{"index":2,"ruling":"supported"},{"index":3,"ruling":"supported"}]`},
		{"unsupported without reason", `[{"index":1,"ruling":"unsupported"},{"index":2,"ruling":"supported"},{"index":3,"ruling":"supported"}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, repoID, repoRoot := indexedRepo(t)
			builder := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]},"invariants":[{"claim":"Invariant","cited_symbols":["Open"]}]}`}
			if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
				t.Fatalf("build: %v", err)
			}
			before, _, err := store.KnowledgeDoc(repoID, "storage")
			if err != nil {
				t.Fatalf("before: %v", err)
			}
			auditor := &stubSynth{response: `{"verdicts":` + test.verdicts + `}`}
			report, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}})
			if err != nil {
				t.Fatalf("audit: %v", err)
			}
			if len(report.Failures) != 1 || report.Removed != 0 || report.Supported != 0 {
				t.Fatalf("invalid verdict set should fail atomically, got %+v", report)
			}
			after, _, err := store.KnowledgeDoc(repoID, "storage")
			if err != nil {
				t.Fatalf("after: %v", err)
			}
			if after.Body != before.Body || !after.Verified || len(after.DroppedClaims) != len(before.DroppedClaims) {
				t.Fatalf("invalid verdict set changed the doc: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestAuditPersistsUnclearAsUnverified(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	builder := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]},"invariants":[{"claim":"Invariant","cited_symbols":["Open"]}]}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
		t.Fatalf("build: %v", err)
	}
	auditor := &stubSynth{response: `{"verdicts":[{"index":1,"ruling":"unclear","why":"not enough"},{"index":2,"ruling":"unclear","why":"not enough"},{"index":3,"ruling":"unclear","why":"not enough"}]}`}
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	doc, _, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil {
		t.Fatalf("doc: %v", err)
	}
	if doc.Verified || !strings.Contains(doc.Body, "Invariant") {
		t.Fatalf("unclear claims should stay in an unverified body: %+v", doc)
	}
	found := false
	for _, dropped := range doc.DroppedClaims {
		found = found || strings.Contains(dropped, "unclear")
	}
	if !found {
		t.Fatalf("unclear outcome was not persisted: %v", doc.DroppedClaims)
	}
}

func TestAuditPrunesByIndexNotText(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	builder := &stubSynth{response: `{"summary":{"claim":"Same claim","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]},"key_symbols":[{"claim":"Same claim","cited_symbols":["Store"]}]}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
		t.Fatalf("build: %v", err)
	}
	auditor := &stubSynth{response: `{"verdicts":[{"index":1,"ruling":"unsupported","why":"summary is wrong"},{"index":2,"ruling":"supported"},{"index":3,"ruling":"supported"}]}`}
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	doc, _, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil {
		t.Fatalf("doc: %v", err)
	}
	var claims synthesis
	if err := json.Unmarshal([]byte(doc.Claims), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims.Summary.Claim != "" || len(claims.KeySymbols) != 1 || claims.KeySymbols[0].Claim != "Same claim" {
		t.Fatalf("index-based pruning removed the wrong duplicate: %+v", claims)
	}
}

func TestAuditRefreshesSummaryAfterRejection(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	builder := &stubSynth{response: `{"summary":{"claim":"Rejected summary","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]}}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
		t.Fatalf("build: %v", err)
	}
	auditor := &stubSynth{response: `{"verdicts":[{"index":1,"ruling":"unsupported","why":"wrong"},{"index":2,"ruling":"supported"}]}`}
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	doc, _, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil {
		t.Fatalf("doc: %v", err)
	}
	modules, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		t.Fatalf("modules: %v", err)
	}
	module := moduleBySlugForTest(modules)["storage"]
	if doc.Summary == "Rejected summary" || doc.Summary != structuralSummary(module) {
		t.Fatalf("summary was not refreshed after rejection: got %q want %q", doc.Summary, structuralSummary(module))
	}
}

func TestAuditRestoresVerificationAfterUnclearResolved(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	builder := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]},"invariants":[{"claim":"Invariant","cited_symbols":["Open"]}]}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
		t.Fatalf("build: %v", err)
	}
	first := &stubSynth{response: `{"verdicts":[{"index":1,"ruling":"supported"},{"index":2,"ruling":"supported"},{"index":3,"ruling":"unclear","why":"not enough"}]}`}
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: first, Only: []string{"storage"}}); err != nil {
		t.Fatalf("first audit: %v", err)
	}
	doc, _, _ := store.KnowledgeDoc(repoID, "storage")
	if doc.Verified {
		t.Fatal("unclear audit should make the doc unverified")
	}
	second := &stubSynth{response: `{"verdicts":[{"index":1,"ruling":"supported"},{"index":2,"ruling":"supported"},{"index":3,"ruling":"supported"}]}`}
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: second, Only: []string{"storage"}}); err != nil {
		t.Fatalf("second audit: %v", err)
	}
	doc, _, _ = store.KnowledgeDoc(repoID, "storage")
	for _, note := range doc.DroppedClaims {
		if strings.Contains(note, "unclear, kept pending review") {
			t.Fatalf("resolved unclear note remained: %v", doc.DroppedClaims)
		}
	}
	if !doc.Verified {
		t.Fatalf("resolved unclear outcome did not restore verification: %v", doc.DroppedClaims)
	}
}

func TestAuditEvidenceNormalizesQualifiedSymbols(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	builder := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]},"invariants":[{"claim":"Qualified open","cited_symbols":["Store.Open"]}]}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
		t.Fatalf("build: %v", err)
	}
	auditor := &stubSynth{response: `{"verdicts":[{"index":1,"ruling":"supported"},{"index":2,"ruling":"supported"},{"index":3,"ruling":"supported"}]}`}
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(auditor.prompts) != 1 || !strings.Contains(auditor.prompts[0], "func Open(") ||
		strings.Contains(auditor.prompts[0], "No declaration was found in this module for: Store.Open") {
		t.Fatalf("qualified symbol evidence was not normalized:\n%s", auditor.prompts[0])
	}
}

func TestAuditEvidenceFallsBackToTailAfterModuleScoping(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	write(t, filepath.Join(repoRoot, "api", "fake.go"), "package api\nfunc Fake() {}\n")
	if err := store.ReplaceCodeFile(repoID, db.CodeFile{
		Path:       "api/fake.go",
		Lang:       "go",
		SizeBytes:  25,
		ContentSHA: "fake",
		Parser:     "go/ast",
	}, []db.CodeSymbol{{
		Path:      "api/fake.go",
		Name:      "Store.Open",
		Kind:      "func",
		Signature: "func Store.Open() *Store",
		StartLine: 1,
		EndLine:   1,
	}}, nil, nil, time.Now().UTC()); err != nil {
		t.Fatalf("insert qualified symbol: %v", err)
	}
	builder := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]},"invariants":[{"claim":"Qualified open","cited_symbols":["Store.Open"]}]}`}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: builder, Only: []string{"storage"}}); err != nil {
		t.Fatalf("build: %v", err)
	}
	auditor := &stubSynth{response: `{"verdicts":[{"index":1,"ruling":"supported"},{"index":2,"ruling":"supported"},{"index":3,"ruling":"supported"}]}`}
	if _, err := Audit(store, repoID, repoRoot, AuditOptions{Synth: auditor, Only: []string{"storage"}}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(auditor.prompts) != 1 || !strings.Contains(auditor.prompts[0], "func Open(name string) *Store") ||
		strings.Contains(auditor.prompts[0], "No declaration was found in this module for: Store.Open") {
		t.Fatalf("qualified symbol evidence did not fall back after scoping:\n%s", auditor.prompts[0])
	}
}

func TestBuildRequiresCitedSummaryAndPurpose(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	synth := &stubSynth{response: `{"summary":"uncited summary","purpose":{"claim":"uncited purpose"} }`}
	report, err := Build(store, repoID, repoRoot, BuildOptions{Synth: synth, Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if report.Dropped != 2 {
		t.Fatalf("expected both uncited top-level claims dropped, got %+v", report)
	}
	doc, _, err := store.KnowledgeDoc(repoID, "storage")
	if err != nil {
		t.Fatalf("doc: %v", err)
	}
	if doc.Verified || !strings.HasPrefix(doc.Body, "# storage\n\n3 go files declaring") {
		t.Fatalf("uncited summary/purpose should fall back structurally: %+v", doc)
	}
	var legacy synthesis
	if err := json.Unmarshal([]byte(`{"summary":"text","purpose":"text"}`), &legacy); err != nil {
		t.Fatalf("legacy claims should parse: %v", err)
	}
	if legacy.Summary.Claim != "text" || legacy.Purpose.Claim != "text" {
		t.Fatalf("legacy claims did not round-trip: %+v", legacy)
	}
}

func TestBuildRetriesFallbackDocsWhenModelIsAvailable(t *testing.T) {
	t.Run("failed then working", func(t *testing.T) {
		store, repoID, repoRoot := indexedRepo(t)
		if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: &stubSynth{err: errors.New("offline")}, Only: []string{"storage"}}); err != nil {
			t.Fatalf("failed build: %v", err)
		}
		working := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]}}`}
		if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: working, Only: []string{"storage"}}); err != nil {
			t.Fatalf("working build: %v", err)
		}
		doc, _, _ := store.KnowledgeDoc(repoID, "storage")
		if working.promptCount() != 1 || doc.Generator != "structural+model" {
			t.Fatalf("failed fallback was incorrectly reused: prompts=%d generator=%q", working.promptCount(), doc.Generator)
		}
	})
	t.Run("structural then model", func(t *testing.T) {
		store, repoID, repoRoot := indexedRepo(t)
		if _, err := Build(store, repoID, repoRoot, BuildOptions{Only: []string{"storage"}}); err != nil {
			t.Fatalf("structural build: %v", err)
		}
		working := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]}}`}
		if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: working, Only: []string{"storage"}}); err != nil {
			t.Fatalf("model build: %v", err)
		}
		if working.promptCount() != 1 {
			t.Fatalf("structural fallback was incorrectly reused: %d prompts", working.promptCount())
		}
	})
	t.Run("model then structural", func(t *testing.T) {
		store, repoID, repoRoot := indexedRepo(t)
		working := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]}}`}
		if _, err := Build(store, repoID, repoRoot, BuildOptions{Synth: working, Only: []string{"storage"}}); err != nil {
			t.Fatalf("model build: %v", err)
		}
		report, err := Build(store, repoID, repoRoot, BuildOptions{Only: []string{"storage"}})
		if err != nil {
			t.Fatalf("structural build: %v", err)
		}
		if report.Unchanged == 0 {
			t.Fatalf("model doc should be reusable without a model: %+v", report)
		}
	})
}

func TestBuildRebuildsLegacyUncitedDocs(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	modules, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		t.Fatalf("modules: %v", err)
	}
	var storage Module
	for _, module := range modules {
		if module.Slug == "storage" {
			storage = module
			break
		}
	}
	legacy := db.KnowledgeDoc{
		Slug:        "storage",
		Kind:        "module",
		Title:       storage.Title,
		Body:        "legacy body",
		Claims:      `{"summary":"legacy summary","purpose":"legacy purpose","entry_points":[{"claim":"Open","cited_symbols":["Open"]}]}`,
		Fingerprint: storage.Fingerprint,
		Generator:   "structural+model",
		Verified:    true,
		GeneratedAt: time.Now().UTC(),
	}
	if err := store.UpsertKnowledgeDoc(repoID, legacy); err != nil {
		t.Fatalf("store legacy doc: %v", err)
	}
	structural, err := Build(store, repoID, repoRoot, BuildOptions{Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("no-model build: %v", err)
	}
	if structural.Unchanged != 1 {
		t.Fatalf("no-model build should reuse legacy docs: %+v", structural)
	}
	synth := &stubSynth{response: `{"summary":{"claim":"Storage","cited_symbols":["Open"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]}}`}
	model, err := Build(store, repoID, repoRoot, BuildOptions{
		Synth: synth,
		Only:  []string{"storage"},
	})
	if err != nil {
		t.Fatalf("model build: %v", err)
	}
	if model.Unchanged != 0 || synth.promptCount() != 1 {
		t.Fatalf("model build should rebuild legacy docs: report=%+v calls=%d", model, synth.promptCount())
	}
}

func TestBuildRejectsCitationsOutsideModule(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	synth := &stubSynth{response: `{"summary":{"claim":"Storage","cited_paths":["api/handler.go"]},"purpose":{"claim":"Purpose","cited_symbols":["Open"]}}`}
	report, err := Build(store, repoID, repoRoot, BuildOptions{Synth: synth, Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if report.Dropped != 1 {
		t.Fatalf("expected one out-of-module claim dropped, got %+v", report)
	}
	doc, _, _ := store.KnowledgeDoc(repoID, "storage")
	found := false
	for _, dropped := range doc.DroppedClaims {
		found = found || strings.Contains(dropped, "outside module")
	}
	if !found {
		t.Fatalf("missing outside-module reason: %v", doc.DroppedClaims)
	}
}

func TestBuildPrunesObsoleteModuleDocs(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	fake := db.KnowledgeDoc{Slug: "ghost", Kind: "module", Title: "Ghost", GeneratedAt: time.Now().UTC()}
	if err := store.UpsertKnowledgeDoc(repoID, fake); err != nil {
		t.Fatalf("insert ghost: %v", err)
	}
	report, err := Build(store, repoID, repoRoot, BuildOptions{})
	if err != nil {
		t.Fatalf("full build: %v", err)
	}
	if report.Pruned != 1 {
		t.Fatalf("expected one pruned module, got %+v", report)
	}
	if _, found, _ := store.KnowledgeDoc(repoID, "ghost"); found {
		t.Fatal("obsolete module survived full build")
	}
	if err := store.UpsertKnowledgeDoc(repoID, fake); err != nil {
		t.Fatalf("reinsert ghost: %v", err)
	}
	if _, err := Build(store, repoID, repoRoot, BuildOptions{Only: []string{"storage"}}); err != nil {
		t.Fatalf("only build: %v", err)
	}
	if _, found, _ := store.KnowledgeDoc(repoID, "ghost"); !found {
		t.Fatal("only build pruned an unrelated module")
	}
}

func TestFingerprintTracksDependencies(t *testing.T) {
	repo := newKnowledgeRepo(t, map[string]string{
		"go.mod":       "module example.com/app\n\ngo 1.26.0\n",
		"storage/a.go": "package storage\nfunc Open() {}\n",
		"storage/b.go": "package storage\nfunc B() {}\n",
		"storage/c.go": "package storage\nfunc C() {}\n",
		"api/a.go":     "package api\nfunc A() {}\n",
		"api/b.go":     "package api\nfunc B() {}\n",
		"api/c.go":     "package api\nfunc C() {}\n",
	})
	store, repoID := openKnowledgeRepo(t, repo)
	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index: %v", err)
	}
	before, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		t.Fatalf("modules before: %v", err)
	}
	old := moduleBySlugForTest(before)["storage"].Fingerprint
	write(t, filepath.Join(repo, "api", "import.go"), "package api\n\nimport \"example.com/app/storage\"\n\nfunc Use() { storage.Open() }\n")
	git(t, repo, "add", "-A")
	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	after, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		t.Fatalf("modules after: %v", err)
	}
	if old == moduleBySlugForTest(after)["storage"].Fingerprint {
		t.Fatal("storage fingerprint did not include dependency changes")
	}
}

func TestModuleHistoryOnlyOwnedFiles(t *testing.T) {
	repo := newKnowledgeRepo(t, map[string]string{
		"go.mod":    "module example.com/app\n\ngo 1.26.0\n",
		"root/a.go": "package root\nfunc A() {}\n",
		"root/b.go": "package root\nfunc B() {}\n",
		"root/c.go": "package root\nfunc C() {}\n",
		"sub/a.go":  "package sub\nfunc A() {}\n",
		"sub/b.go":  "package sub\nfunc B() {}\n",
		"sub/c.go":  "package sub\nfunc C() {}\n",
	})
	store, repoID := openKnowledgeRepo(t, repo)
	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index: %v", err)
	}
	write(t, filepath.Join(repo, "sub", "a.go"), "package sub\nfunc A() { println(\"changed\") }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "touch sub only")
	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	modules, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		t.Fatalf("modules: %v", err)
	}
	bySlug := moduleBySlugForTest(modules)
	for _, commit := range bySlug["root"].RecentCommits {
		if commit.Subject == "touch sub only" {
			t.Fatal("root module history included a sub-only commit")
		}
	}
	found := false
	for _, commit := range bySlug["sub"].RecentCommits {
		found = found || commit.Subject == "touch sub only"
	}
	if !found {
		t.Fatal("sub module history omitted its own commit")
	}
}

func TestClusterPathsMergesNewParents(t *testing.T) {
	paths := []string{"a/b/c/x.go", "a/b/y.go", "a/z.go", "root.go"}
	buckets := clusterPaths(paths, ModuleOptions{MinFiles: 3})
	for dir, files := range buckets {
		if dir != "." && len(files) < 3 {
			t.Fatalf("small parent bucket survived re-evaluation: %s=%v", dir, files)
		}
	}
}

func TestBuildDetectsNewlyAddedFiles(t *testing.T) {
	store, repoID, repoRoot := indexedRepo(t)
	write(t, filepath.Join(repoRoot, "storage", "new.go"), "package storage\nfunc New() {}\n")
	git(t, repoRoot, "add", "-A")
	report, err := Build(store, repoID, repoRoot, BuildOptions{Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("stale build: %v", err)
	}
	if report.NeedsReindex < 1 {
		t.Fatalf("new tracked file did not mark module stale: %+v", report)
	}
	if _, found, _ := store.KnowledgeDoc(repoID, "storage"); found {
		t.Fatal("stale module should not have been written")
	}
	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	report, err = Build(store, repoID, repoRoot, BuildOptions{Only: []string{"storage"}})
	if err != nil {
		t.Fatalf("fresh build: %v", err)
	}
	if report.NeedsReindex != 0 {
		t.Fatalf("reindexed module remained stale: %+v", report)
	}
}

func moduleBySlugForTest(modules []Module) map[string]Module {
	out := make(map[string]Module, len(modules))
	for _, module := range modules {
		out[module.Slug] = module
	}
	return out
}

func newKnowledgeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Test User")
	git(t, repo, "config", "user.email", "test@example.com")
	for path, content := range files {
		write(t, filepath.Join(repo, path), content)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "init")
	return repo
}

func openKnowledgeRepo(t *testing.T, repo string) (*db.Store, int64) {
	t.Helper()
	store, err := db.OpenPath(repo, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	record, err := store.UpsertRepo("main", "", time.Now().UTC())
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	return store, record.ID
}
