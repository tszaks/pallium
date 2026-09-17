package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tszaks/pallium/internal/db"
)

// Synthesizer turns a prompt into text. Pallium's workflow provider layer
// satisfies it, and so does a stub in tests. Keeping it an interface is what
// makes the whole knowledge build runnable, and testable, with no model at
// all: pass nil and every doc is still produced from the index, just without
// the prose.
type Synthesizer interface {
	Synthesize(ctx context.Context, prompt string) (string, error)
}

type BuildOptions struct {
	Modules       ModuleOptions
	Synth         Synthesizer
	Force         bool
	Only          []string
	Materialize   bool
	MaterializeTo string
	// AllowStale documents modules whose files have changed since indexing.
	// Off by default: a synthesis prompt is built from stored line numbers,
	// signatures and doc comments, so a stale module produces a doc written
	// about code that is no longer there.
	AllowStale bool
	// Concurrency bounds how many modules are synthesized at once. Synthesis
	// is the only slow part and it is a pure function of the module card, so
	// it fans out; storage stays strictly sequential because the repo's
	// sqlite file has exactly one writer by design.
	Concurrency int
}

type BuildReport struct {
	Modules   int `json:"modules"`
	Written   int `json:"written"`
	Unchanged int `json:"unchanged"`
	Pruned    int `json:"pruned"`
	// NeedsReindex counts modules skipped because their files drifted from
	// the index.
	NeedsReindex int            `json:"needs_reindex"`
	Verified     int            `json:"verified"`
	Unverified   int            `json:"unverified"`
	Dropped      int            `json:"dropped_claims"`
	Generator    string         `json:"generator"`
	Materialized string         `json:"materialized_to,omitempty"`
	Docs         []DocSummary   `json:"docs"`
	Failures     []BuildFailure `json:"failures"`
}

type DocSummary struct {
	Slug     string `json:"slug"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Verified bool   `json:"verified"`
	Dropped  int    `json:"dropped_claims"`
	Status   string `json:"status"`
}

type BuildFailure struct {
	Slug   string `json:"slug"`
	Reason string `json:"reason"`
}

// synthesis is the shape the model must return. It never writes markdown: the
// renderer below turns this struct into the doc body. A model that can only
// fill named fields cannot smuggle an unchecked citation into prose, which is
// the difference between a knowledge base and a confident essay.
type synthesis struct {
	Summary     citedClaim   `json:"summary"`
	Purpose     citedClaim   `json:"purpose"`
	EntryPoints []citedClaim `json:"entry_points"`
	KeySymbols  []citedClaim `json:"key_symbols"`
	Invariants  []citedClaim `json:"invariants"`
	Risks       []citedClaim `json:"risks"`
}

type citedClaim struct {
	Claim        string   `json:"claim"`
	CitedPaths   []string `json:"cited_paths"`
	CitedSymbols []string `json:"cited_symbols"`
}

func (c *citedClaim) UnmarshalJSON(data []byte) error {
	type alias citedClaim
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var claim string
		if err := json.Unmarshal(data, &claim); err != nil {
			return err
		}
		*c = citedClaim{Claim: claim}
		return nil
	}
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*c = citedClaim(value)
	return nil
}

func Build(store *db.Store, repoID int64, repoRoot string, opts BuildOptions) (BuildReport, error) {
	modules, err := Modules(store, repoID, opts.Modules)
	if err != nil {
		return BuildReport{}, err
	}

	repo, err := store.Repo()
	if err != nil {
		return BuildReport{}, err
	}

	index, err := newCitationIndex(store, repoID)
	if err != nil {
		return BuildReport{}, err
	}

	generator := "structural"
	if opts.Synth != nil {
		generator = "structural+model"
	}

	stale := map[string]struct{}{}
	if !opts.AllowStale {
		stale, err = staleModuleSlugs(store, repoID, repoRoot, modules)
		if err != nil {
			return BuildReport{}, err
		}
	}

	report := BuildReport{Modules: len(modules), Generator: generator, Docs: []DocSummary{}, Failures: []BuildFailure{}}
	only := make(map[string]struct{}, len(opts.Only))
	for _, slug := range opts.Only {
		only[slug] = struct{}{}
	}

	// First pass decides what needs work, so synthesis can fan out over
	// exactly that set instead of discovering skips one at a time.
	pending := make([]Module, 0, len(modules))
	for _, module := range modules {
		if len(only) > 0 {
			if _, ok := only[module.Slug]; !ok {
				continue
			}
		}

		if _, drifted := stale[module.Slug]; drifted {
			report.NeedsReindex++
			continue
		}

		existing, found, err := store.KnowledgeDoc(repoID, module.Slug)
		if err != nil {
			return BuildReport{}, err
		}
		reusableClaims := true
		if opts.Synth != nil {
			var stored synthesis
			if err := json.Unmarshal([]byte(existing.Claims), &stored); err != nil {
				reusableClaims = false
			} else {
				flat := flattenClaims(stored)
				reusableClaims = len(flat) > 0 && !hasUncitedClaims(flat)
			}
		}
		if found && !opts.Force && existing.Fingerprint == module.Fingerprint &&
			(opts.Synth == nil || (existing.Generator == "structural+model" && reusableClaims)) {
			report.Unchanged++
			report.Docs = append(report.Docs, DocSummary{
				Slug: module.Slug, Kind: "module", Title: module.Title,
				Verified: existing.Verified, Dropped: len(existing.DroppedClaims), Status: "unchanged",
			})
			continue
		}
		pending = append(pending, module)
	}

	responses := synthesizeModules(pending, opts.Synth, opts.Concurrency)

	for _, module := range pending {
		doc, failure := buildModuleDoc(module, index, repo.LastIndexedCommit, responses[module.Slug])
		if failure != "" {
			report.Failures = append(report.Failures, BuildFailure{Slug: module.Slug, Reason: failure})
		}
		if err := store.UpsertKnowledgeDoc(repoID, doc); err != nil {
			return BuildReport{}, err
		}

		report.Written++
		report.Dropped += len(doc.DroppedClaims)
		if doc.Verified {
			report.Verified++
		} else {
			report.Unverified++
		}
		report.Docs = append(report.Docs, DocSummary{
			Slug: doc.Slug, Kind: doc.Kind, Title: doc.Title,
			Verified: doc.Verified, Dropped: len(doc.DroppedClaims), Status: "written",
		})
	}

	if len(only) == 0 {
		overview := buildOverviewDoc(modules, repo.LastIndexedCommit)
		if err := store.UpsertKnowledgeDoc(repoID, overview); err != nil {
			return BuildReport{}, err
		}
		report.Written++
		report.Verified++
		report.Docs = append(report.Docs, DocSummary{Slug: overview.Slug, Kind: overview.Kind, Title: overview.Title, Verified: true, Status: "written"})

		incidents, err := buildIncidentDoc(store, repoID, repo.LastIndexedCommit)
		if err != nil {
			return BuildReport{}, err
		}
		if err := store.UpsertKnowledgeDoc(repoID, incidents); err != nil {
			return BuildReport{}, err
		}
		report.Written++
		report.Verified++
		report.Docs = append(report.Docs, DocSummary{Slug: incidents.Slug, Kind: incidents.Kind, Title: incidents.Title, Verified: true, Status: "written"})

		current := make(map[string]struct{}, len(modules))
		for _, module := range modules {
			current[module.Slug] = struct{}{}
		}
		docs, err := store.KnowledgeDocs(repoID)
		if err != nil {
			return BuildReport{}, err
		}
		for _, doc := range docs {
			if doc.Kind != "module" {
				continue
			}
			if _, ok := current[doc.Slug]; ok {
				continue
			}
			if err := store.DeleteKnowledgeDoc(repoID, doc.Slug); err != nil {
				return BuildReport{}, err
			}
			report.Pruned++
		}
	}

	if opts.Materialize {
		target := opts.MaterializeTo
		if target == "" {
			target = filepath.Join(repoRoot, ".pallium", "knowledge")
		}
		if err := Materialize(store, repoID, target); err != nil {
			return BuildReport{}, err
		}
		report.Materialized = target
	}

	return report, nil
}

// synthResult is one module's model response, or the error that replaced it.
type synthResult struct {
	raw string
	err error
}

// synthesizeModules runs the model over every pending module, bounded by
// concurrency. A single failure never fails the batch: the module falls back
// to its structural doc and the reason is reported.
func synthesizeModules(modules []Module, synth Synthesizer, concurrency int) map[string]synthResult {
	out := make(map[string]synthResult, len(modules))
	if synth == nil || len(modules) == 0 {
		return out
	}
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > len(modules) {
		concurrency = len(modules)
	}

	var mutex sync.Mutex
	var group sync.WaitGroup
	slots := make(chan struct{}, concurrency)

	for _, module := range modules {
		group.Add(1)
		go func(module Module) {
			defer group.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			raw, err := synth.Synthesize(context.Background(), modulePrompt(module))
			mutex.Lock()
			out[module.Slug] = synthResult{raw: raw, err: err}
			mutex.Unlock()
		}(module)
	}
	group.Wait()
	return out
}

func buildModuleDoc(module Module, index citationIndex, sourceCommit string, response synthResult) (db.KnowledgeDoc, string) {
	doc := db.KnowledgeDoc{
		Slug:         module.Slug,
		Kind:         "module",
		Title:        module.Title,
		SourceCommit: sourceCommit,
		Fingerprint:  module.Fingerprint,
		Generator:    "structural",
		GeneratedAt:  time.Now().UTC(),
		Verified:     true,
	}

	var result synthesis
	failure := ""
	switch {
	case response.err != nil:
		failure = response.err.Error()
	case strings.TrimSpace(response.raw) != "":
		if parsed, parseErr := parseSynthesis(response.raw); parseErr != nil {
			failure = parseErr.Error()
		} else {
			result = parsed
			doc.Generator = "structural+model"
		}
	}

	verified, dropped := verifySynthesis(result, module, index)
	if failure != "" {
		doc.Generator = "structural+model-failed"
	}
	doc.DroppedClaims = dropped
	// A doc is unverified when the model said something the index could not
	// confirm. The doc is still stored, because the structural half of it is
	// true regardless, but the flag and the dropped list travel with it.
	doc.Verified = len(dropped) == 0 && failure == ""
	doc.Summary = strings.TrimSpace(verified.Summary.Claim)
	if doc.Summary == "" {
		doc.Summary = structuralSummary(module)
	}
	doc.Body = renderModuleDoc(module, verified)
	doc.CitedPaths = citedPaths(module, verified)
	doc.CitedSymbols = citedSymbols(verified)
	if encoded, err := json.Marshal(verified); err == nil {
		doc.Claims = string(encoded)
	}
	return doc, failure
}

// verifySynthesis is the gate. Every claim must cite at least one path or
// symbol, and every citation must resolve against the content index. A claim
// that fails is not repaired or softened, it is removed and recorded.
func verifySynthesis(result synthesis, module Module, index citationIndex) (synthesis, []string) {
	moduleFiles := make(map[string]struct{}, len(module.Files))
	for _, path := range module.Files {
		moduleFiles[path] = struct{}{}
	}

	dropped := make([]string, 0)
	keep := func(claims []citedClaim, label string) []citedClaim {
		out := make([]citedClaim, 0, len(claims))
		for _, claim := range claims {
			text := strings.TrimSpace(claim.Claim)
			if text == "" {
				continue
			}
			if len(claim.CitedPaths) == 0 && len(claim.CitedSymbols) == 0 {
				dropped = append(dropped, fmt.Sprintf("%s: %q cited nothing", label, truncate(text, 90)))
				continue
			}
			bad := ""
			for _, path := range claim.CitedPaths {
				if !index.hasPath(path) {
					bad = "no such file " + path
					break
				}
				if _, ok := moduleFiles[strings.TrimSpace(path)]; !ok {
					bad = "file outside module " + strings.TrimSpace(path)
					break
				}
			}
			if bad == "" {
				for _, symbol := range claim.CitedSymbols {
					if !index.hasSymbol(symbol) {
						bad = "no such symbol " + symbol
						break
					}
				}
			}
			if bad != "" {
				dropped = append(dropped, fmt.Sprintf("%s: %q dropped, %s", label, truncate(text, 90), bad))
				continue
			}
			out = append(out, claim)
		}
		return out
	}

	keepOne := func(claim citedClaim, label string) citedClaim {
		kept := keep([]citedClaim{claim}, label)
		if len(kept) == 0 {
			return citedClaim{}
		}
		return kept[0]
	}
	verified := synthesis{
		Summary:     keepOne(result.Summary, "summary"),
		Purpose:     keepOne(result.Purpose, "purpose"),
		EntryPoints: keep(result.EntryPoints, "entry point"),
		KeySymbols:  keep(result.KeySymbols, "key symbol"),
		Invariants:  keep(result.Invariants, "invariant"),
		Risks:       keep(result.Risks, "risk"),
	}
	return verified, dropped
}

type citationIndex struct {
	paths   map[string]struct{}
	symbols map[string]struct{}
}

func newCitationIndex(store *db.Store, repoID int64) (citationIndex, error) {
	paths, err := store.CodeIndexedPaths(repoID)
	if err != nil {
		return citationIndex{}, err
	}
	index := citationIndex{
		paths:   make(map[string]struct{}, len(paths)),
		symbols: map[string]struct{}{},
	}
	for _, path := range paths {
		index.paths[path] = struct{}{}
	}

	names, err := store.AllSymbolNames(repoID)
	if err != nil {
		return citationIndex{}, err
	}
	for _, name := range names {
		index.symbols[name] = struct{}{}
	}
	return index, nil
}

func (c citationIndex) hasPath(path string) bool {
	_, ok := c.paths[strings.TrimSpace(path)]
	return ok
}

func (c citationIndex) hasSymbol(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	// A model often cites "Store.Open" or "pkg.Open" where the index holds
	// "Open". Accept the tail rather than failing a claim that is right about
	// the code and only verbose about the name.
	if _, ok := c.symbols[name]; ok {
		return true
	}
	if index := strings.LastIndex(name, "."); index >= 0 && index+1 < len(name) {
		_, ok := c.symbols[name[index+1:]]
		return ok
	}
	return false
}

func parseSynthesis(raw string) (synthesis, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return synthesis{}, fmt.Errorf("model returned nothing")
	}
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) >= 2 {
			lines = lines[1:]
			if strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
				lines = lines[:len(lines)-1]
			}
			text = strings.Join(lines, "\n")
		}
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return synthesis{}, fmt.Errorf("model response was not JSON")
	}

	var result synthesis
	if err := json.Unmarshal([]byte(text[start:end+1]), &result); err != nil {
		return synthesis{}, fmt.Errorf("model response did not parse: %w", err)
	}
	return result, nil
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if len(value) <= limit {
		return value
	}
	return value[:limit-1] + "…"
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func citedPaths(module Module, result synthesis) []string {
	out := append([]string{}, module.Files...)
	for _, group := range [][]citedClaim{{result.Summary, result.Purpose}, result.EntryPoints, result.KeySymbols, result.Invariants, result.Risks} {
		for _, claim := range group {
			out = append(out, claim.CitedPaths...)
		}
	}
	return sortedUnique(out)
}

func citedSymbols(result synthesis) []string {
	out := make([]string, 0)
	for _, group := range [][]citedClaim{{result.Summary, result.Purpose}, result.EntryPoints, result.KeySymbols, result.Invariants, result.Risks} {
		for _, claim := range group {
			out = append(out, claim.CitedSymbols...)
		}
	}
	return sortedUnique(out)
}
