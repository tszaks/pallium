package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
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
}

type BuildReport struct {
	Modules      int            `json:"modules"`
	Written      int            `json:"written"`
	Unchanged    int            `json:"unchanged"`
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
	Summary     string       `json:"summary"`
	Purpose     string       `json:"purpose"`
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

	report := BuildReport{Modules: len(modules), Generator: generator, Docs: []DocSummary{}, Failures: []BuildFailure{}}
	only := make(map[string]struct{}, len(opts.Only))
	for _, slug := range opts.Only {
		only[slug] = struct{}{}
	}

	for _, module := range modules {
		if len(only) > 0 {
			if _, ok := only[module.Slug]; !ok {
				continue
			}
		}

		existing, found, err := store.KnowledgeDoc(repoID, module.Slug)
		if err != nil {
			return BuildReport{}, err
		}
		if found && !opts.Force && existing.Fingerprint == module.Fingerprint {
			report.Unchanged++
			report.Docs = append(report.Docs, DocSummary{
				Slug: module.Slug, Kind: "module", Title: module.Title,
				Verified: existing.Verified, Dropped: len(existing.DroppedClaims), Status: "unchanged",
			})
			continue
		}

		doc, failure := buildModuleDoc(module, index, repo.LastIndexedCommit, opts.Synth)
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

func buildModuleDoc(module Module, index citationIndex, sourceCommit string, synth Synthesizer) (db.KnowledgeDoc, string) {
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
	if synth != nil {
		raw, err := synth.Synthesize(context.Background(), modulePrompt(module))
		if err != nil {
			failure = err.Error()
		} else if parsed, parseErr := parseSynthesis(raw); parseErr != nil {
			failure = parseErr.Error()
		} else {
			result = parsed
			doc.Generator = "structural+model"
		}
	}

	verified, dropped := verifySynthesis(result, module, index)
	doc.DroppedClaims = dropped
	// A doc is unverified when the model said something the index could not
	// confirm. The doc is still stored, because the structural half of it is
	// true regardless, but the flag and the dropped list travel with it.
	doc.Verified = len(dropped) == 0 && failure == ""
	doc.Summary = strings.TrimSpace(verified.Summary)
	if doc.Summary == "" {
		doc.Summary = structuralSummary(module)
	}
	doc.Body = renderModuleDoc(module, verified)
	doc.CitedPaths = citedPaths(module, verified)
	doc.CitedSymbols = citedSymbols(verified)
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

	verified := synthesis{
		Summary:     result.Summary,
		Purpose:     result.Purpose,
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
	for _, group := range [][]citedClaim{result.EntryPoints, result.KeySymbols, result.Invariants, result.Risks} {
		for _, claim := range group {
			out = append(out, claim.CitedPaths...)
		}
	}
	return sortedUnique(out)
}

func citedSymbols(result synthesis) []string {
	out := make([]string, 0)
	for _, group := range [][]citedClaim{result.EntryPoints, result.KeySymbols, result.Invariants, result.Risks} {
		for _, claim := range group {
			out = append(out, claim.CitedSymbols...)
		}
	}
	return sortedUnique(out)
}
