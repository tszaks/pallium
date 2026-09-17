package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tszaks/pallium/internal/db"
)

// The audit pass closes the honest gap in `knowledge build`.
//
// Build verifies that every citation RESOLVES: the file exists, the symbol
// exists. That stops a fabricated symbol from entering the base, and it is
// what a stub test proves. It does not check whether the claim about that
// symbol is TRUE. "Open deletes the database", citing a real Open, passes
// verification and is wrong.
//
// So the audit takes each stored claim, pulls the actual source of the
// symbols it cites, and asks a skeptic whether the source supports it. A
// claim ruled unsupported is removed from the doc and recorded, the same way
// an unresolvable citation is. This is Pallium's own adversarial-verification
// idiom pointed at its own output.

type AuditOptions struct {
	Synth       Synthesizer
	Only        []string
	Concurrency int
	// Materialize rewrites the markdown pages after claims are removed, so
	// disk never keeps a claim the audit rejected.
	Materialize   bool
	MaterializeTo string
}

type AuditReport struct {
	Docs    int `json:"docs"`
	Audited int `json:"audited"`
	// Skipped counts docs with nothing a model asserted: a structural doc is
	// true by construction, so there is nothing to refute.
	Skipped int `json:"skipped"`
	// NeedsRebuild counts docs a model wrote before claims were stored
	// alongside the rendered body. Their assertions are unauditable, and
	// counting them as "structural" would read as a clean bill of health.
	NeedsRebuild int `json:"needs_rebuild"`
	// NeedsReindex counts modules whose files changed since indexing. Their
	// stored line numbers point at the wrong code, so auditing them would
	// produce confident refutations of true claims.
	NeedsReindex int            `json:"needs_reindex"`
	Claims       int            `json:"claims"`
	Supported    int            `json:"supported"`
	Removed      int            `json:"removed"`
	Unclear      int            `json:"unclear"`
	Findings     []AuditRemoval `json:"findings"`
	Failures     []BuildFailure `json:"failures"`
}

type AuditRemoval struct {
	Slug   string `json:"slug"`
	Claim  string `json:"claim"`
	Reason string `json:"reason"`
}

// verdict is one skeptic ruling. Unclear is a first-class outcome on purpose:
// forcing a binary answer on a claim the source neither supports nor refutes
// would either delete good knowledge or keep bad knowledge, and both are
// worse than saying so.
type verdict struct {
	Index  int    `json:"index"`
	Ruling string `json:"ruling"`
	Why    string `json:"why"`
}

type auditResponse struct {
	Verdicts []verdict `json:"verdicts"`
}

const auditUnclearMarker = " unclear, kept pending review, "

func Audit(store *db.Store, repoID int64, repoRoot string, opts AuditOptions) (AuditReport, error) {
	if opts.Synth == nil {
		return AuditReport{}, fmt.Errorf("audit needs a model: it re-reads each claim against the source")
	}

	docs, err := store.KnowledgeDocs(repoID)
	if err != nil {
		return AuditReport{}, err
	}

	only := make(map[string]struct{}, len(opts.Only))
	for _, slug := range opts.Only {
		only[slug] = struct{}{}
	}

	report := AuditReport{Findings: []AuditRemoval{}, Failures: []BuildFailure{}}

	modules, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		return AuditReport{}, err
	}
	moduleBySlug := make(map[string]Module, len(modules))
	for _, module := range modules {
		moduleBySlug[module.Slug] = module
	}

	stale, err := staleModuleSlugs(store, repoID, repoRoot, modules)
	if err != nil {
		return AuditReport{}, err
	}

	type job struct {
		doc    db.KnowledgeDoc
		claims synthesis
		prompt string
	}
	jobs := make([]job, 0, len(docs))

	for _, doc := range docs {
		if doc.Kind != "module" {
			continue
		}
		report.Docs++
		if len(only) > 0 {
			if _, ok := only[doc.Slug]; !ok {
				continue
			}
		}

		if _, drifted := stale[doc.Slug]; drifted {
			report.NeedsReindex++
			continue
		}

		var claims synthesis
		if strings.TrimSpace(doc.Claims) != "" {
			_ = json.Unmarshal([]byte(doc.Claims), &claims)
		}
		flat := flattenClaims(claims)
		if len(flat) == 0 {
			if doc.Generator == "structural+model" {
				report.NeedsRebuild++
			} else {
				// Nothing a model asserted, so nothing to refute. A
				// structural doc is true by construction.
				report.Skipped++
			}
			continue
		}

		prompt, err := auditPrompt(store, repoID, repoRoot, doc, flat, moduleBySlug[doc.Slug])
		if err != nil {
			return AuditReport{}, err
		}
		jobs = append(jobs, job{doc: doc, claims: claims, prompt: prompt})
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > len(jobs) && len(jobs) > 0 {
		concurrency = len(jobs)
	}

	responses := make(map[string]synthResult, len(jobs))
	var mutex sync.Mutex
	var group sync.WaitGroup
	slots := make(chan struct{}, maxInt(concurrency, 1))
	for _, item := range jobs {
		group.Add(1)
		go func(slug, prompt string) {
			defer group.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			raw, err := opts.Synth.Synthesize(context.Background(), prompt)
			mutex.Lock()
			responses[slug] = synthResult{raw: raw, err: err}
			mutex.Unlock()
		}(item.doc.Slug, item.prompt)
	}
	group.Wait()

	for _, item := range jobs {
		response := responses[item.doc.Slug]
		if response.err != nil {
			report.Failures = append(report.Failures, BuildFailure{Slug: item.doc.Slug, Reason: response.err.Error()})
			continue
		}
		rulings, err := parseAuditResponse(response.raw)
		if err != nil {
			report.Failures = append(report.Failures, BuildFailure{Slug: item.doc.Slug, Reason: err.Error()})
			continue
		}

		flat := flattenClaims(item.claims)
		if err := validateVerdicts(rulings, len(flat)); err != nil {
			report.Failures = append(report.Failures, BuildFailure{Slug: item.doc.Slug, Reason: err.Error()})
			continue
		}
		report.Audited++
		report.Claims += len(flat)

		removals := make(map[string]string)
		removedIdx := make(map[int]struct{})
		unclear := make(map[string]string)
		for _, ruling := range rulings {
			claim := flat[ruling.Index-1]
			switch strings.ToLower(strings.TrimSpace(ruling.Ruling)) {
			case "unsupported":
				removals[claim.Claim] = strings.TrimSpace(ruling.Why)
				removedIdx[ruling.Index] = struct{}{}
				report.Removed++
				report.Findings = append(report.Findings, AuditRemoval{
					Slug:   item.doc.Slug,
					Claim:  truncate(claim.Claim, 120),
					Reason: truncate(strings.TrimSpace(ruling.Why), 200),
				})
			case "unclear":
				unclear[claim.Claim] = strings.TrimSpace(ruling.Why)
				report.Unclear++
			case "supported":
				report.Supported++
			}
		}

		hadUnclear := len(stripUnclearNotes(item.doc.DroppedClaims)) != len(item.doc.DroppedClaims)
		if len(removals) == 0 && len(unclear) == 0 && !hadUnclear {
			continue
		}

		updated := item.doc
		pruned := pruneClaims(item.claims, removedIdx)
		var module *Module
		if found, ok := moduleBySlug[item.doc.Slug]; ok {
			module = &found
		}
		if module == nil {
			// The module boundary moved since the doc was written; leave the
			// body alone rather than re-rendering against a guess, but still
			// record the rejections.
			updated.DroppedClaims = stripUnclearNotes(item.doc.DroppedClaims)
			for claim, why := range removals {
				updated.DroppedClaims = append(updated.DroppedClaims, fmt.Sprintf("audit: %q removed, %s", truncate(claim, 90), why))
			}
			for claim, why := range unclear {
				updated.DroppedClaims = append(updated.DroppedClaims, fmt.Sprintf("audit: %q"+auditUnclearMarker+"%s", truncate(claim, 90), why))
			}
			if item.claims.Summary.Claim != "" && pruned.Summary.Claim == "" {
				updated.Summary = updated.Title
			}
			if encoded, err := json.Marshal(pruned); err == nil {
				updated.Claims = string(encoded)
			}
			updated.Verified = len(updated.DroppedClaims) == 0
			updated.GeneratedAt = time.Now().UTC()
			if err := store.UpsertKnowledgeDoc(repoID, updated); err != nil {
				return AuditReport{}, err
			}
			continue
		}

		updated.Body = renderModuleDoc(*module, pruned)
		updated.CitedPaths = citedPaths(*module, pruned)
		updated.CitedSymbols = citedSymbols(pruned)
		updated.Summary = strings.TrimSpace(pruned.Summary.Claim)
		if updated.Summary == "" {
			updated.Summary = structuralSummary(*module)
		}
		if encoded, err := json.Marshal(pruned); err == nil {
			updated.Claims = string(encoded)
		}
		updated.DroppedClaims = stripUnclearNotes(item.doc.DroppedClaims)
		for claim, why := range removals {
			updated.DroppedClaims = append(updated.DroppedClaims, fmt.Sprintf("audit: %q removed, %s", truncate(claim, 90), why))
		}
		for claim, why := range unclear {
			updated.DroppedClaims = append(updated.DroppedClaims, fmt.Sprintf("audit: %q"+auditUnclearMarker+"%s", truncate(claim, 90), why))
		}
		updated.Verified = len(updated.DroppedClaims) == 0
		updated.GeneratedAt = time.Now().UTC()
		if err := store.UpsertKnowledgeDoc(repoID, updated); err != nil {
			return AuditReport{}, err
		}
	}

	if opts.Materialize {
		target := opts.MaterializeTo
		if target == "" {
			target = filepath.Join(repoRoot, ".pallium", "knowledge")
		}
		if err := Materialize(store, repoID, target); err != nil {
			return AuditReport{}, err
		}
	}

	return report, nil
}

// flattenClaims puts every claim in one numbered list so the skeptic's
// verdicts can be matched back by index instead of by fuzzy text matching.
func flattenClaims(result synthesis) []citedClaim {
	out := make([]citedClaim, 0)
	for _, claim := range []citedClaim{result.Summary, result.Purpose} {
		if strings.TrimSpace(claim.Claim) != "" {
			out = append(out, claim)
		}
	}
	for _, group := range [][]citedClaim{result.EntryPoints, result.KeySymbols, result.Invariants, result.Risks} {
		out = append(out, group...)
	}
	return out
}

func pruneClaims(result synthesis, removed map[int]struct{}) synthesis {
	index := 0
	keepOne := func(claim citedClaim) citedClaim {
		index++
		if _, ok := removed[index]; ok {
			return citedClaim{}
		}
		return claim
	}
	keepTopLevel := func(claim citedClaim) citedClaim {
		if strings.TrimSpace(claim.Claim) == "" {
			return claim
		}
		return keepOne(claim)
	}
	keep := func(claims []citedClaim) []citedClaim {
		out := make([]citedClaim, 0, len(claims))
		for _, claim := range claims {
			kept := keepOne(claim)
			if kept.Claim == "" {
				continue
			}
			out = append(out, kept)
		}
		return out
	}
	return synthesis{
		Summary:     keepTopLevel(result.Summary),
		Purpose:     keepTopLevel(result.Purpose),
		EntryPoints: keep(result.EntryPoints),
		KeySymbols:  keep(result.KeySymbols),
		Invariants:  keep(result.Invariants),
		Risks:       keep(result.Risks),
	}
}

func validateVerdicts(rulings []verdict, claims int) error {
	seen := make(map[int]struct{}, len(rulings))
	for _, ruling := range rulings {
		if ruling.Index < 1 || ruling.Index > claims {
			return fmt.Errorf("verdict index %d out of range", ruling.Index)
		}
		if _, ok := seen[ruling.Index]; ok {
			return fmt.Errorf("duplicate verdict index %d", ruling.Index)
		}
		seen[ruling.Index] = struct{}{}
		switch strings.ToLower(strings.TrimSpace(ruling.Ruling)) {
		case "supported":
		case "unsupported", "unclear":
			if strings.TrimSpace(ruling.Why) == "" {
				return fmt.Errorf("verdict index %d has no reason", ruling.Index)
			}
		default:
			return fmt.Errorf("invalid verdict ruling %q", ruling.Ruling)
		}
	}
	for index := 1; index <= claims; index++ {
		if _, ok := seen[index]; !ok {
			return fmt.Errorf("missing verdict index %d", index)
		}
	}
	return nil
}

func stripUnclearNotes(notes []string) []string {
	out := make([]string, 0, len(notes))
	for _, note := range notes {
		if strings.HasPrefix(note, "audit: ") && strings.Contains(note, auditUnclearMarker) {
			continue
		}
		out = append(out, note)
	}
	return out
}

// auditPrompt hands the skeptic the claims and the actual source of every
// symbol they cite. Source excerpts are what make this an audit rather than a
// second opinion: a model asked to re-judge a claim from memory would mostly
// agree with itself.
func auditPrompt(store *db.Store, repoID int64, repoRoot string, doc db.KnowledgeDoc, claims []citedClaim, module Module) (string, error) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "You are auditing generated documentation for the module %s. Your job is to REFUTE claims the source does not support.\n\n", doc.Title)

	builder.WriteString("Claims, numbered:\n")
	for index, claim := range claims {
		citations := append([]string{}, claim.CitedSymbols...)
		citations = append(citations, claim.CitedPaths...)
		fmt.Fprintf(&builder, "%d. %s [cites: %s]\n", index+1, strings.TrimSpace(claim.Claim), strings.Join(citations, ", "))
	}

	excerpts, missing, err := claimExcerpts(store, repoID, repoRoot, claims, module)
	if err != nil {
		return "", err
	}
	if len(excerpts) > 0 {
		builder.WriteString("\nSource for the cited symbols:\n\n")
		for _, excerpt := range excerpts {
			fmt.Fprintf(&builder, "----- %s (%s:%d)\n%s\n", excerpt.name, excerpt.path, excerpt.line, excerpt.code)
		}
	}

	if len(missing) > 0 {
		// Naming what could not be found is the whole reason this list
		// exists. Silently omitting evidence made the auditor rule
		// "unsupported" on claims that were fine, because absence of proof
		// reads identically to disproof.
		builder.WriteString("\nNo declaration was found in this module for: " + strings.Join(missing, ", ") + ".\nTreat claims resting only on those as \"unclear\", never \"unsupported\".\n")
	}

	outlines, err := citedPathOutlines(store, repoID, claims, excerpts)
	if err != nil {
		return "", err
	}
	if len(outlines) > 0 {
		builder.WriteString("\nDeclarations in the cited files:\n\n")
		for _, outline := range outlines {
			fmt.Fprintf(&builder, "----- %s\n%s\n", outline.path, outline.body)
		}
	}

	builder.WriteString(`
Rule for each claim, in order of preference:
- "unsupported" if the source contradicts it, or if it asserts behavior that
  is simply not there. Be willing to say this; a wrong claim in documentation
  is worse than a missing one.
- "unclear" if the excerpt is not enough to judge. This is a real answer, not
  a cop-out, and it is better than guessing either way.
- "supported" only if the source shown actually backs the claim.

Return ONLY JSON:

{"verdicts": [{"index": 1, "ruling": "supported|unsupported|unclear", "why": "one sentence, cite the line or construct that decided it"}]}

One verdict per claim. Judge only what the excerpts show; do not assume code you cannot see.
`)

	return builder.String(), nil
}

type sourceExcerpt struct {
	name string
	path string
	line int
	code string
}

// maxExcerptLines caps one symbol's excerpt. A 400-line function would crowd
// out every other claim's evidence; the opening of a declaration is where its
// contract lives.
//
// Raised from 45 after the first real audit: 242 of 307 claims came back
// "unclear", which is the auditor correctly refusing to judge on evidence it
// did not have rather than a fault in the ruling. Most claims are about
// behavior that spans more than one declaration.
const maxExcerptLines = 90

// maxPathSymbolsListed bounds the declaration list shown for a cited FILE.
// Claims citing a path used to arrive with no evidence at all, which is a
// second reason the first run abstained so often.
const maxPathSymbolsListed = 30

// claimExcerpts pulls the source behind each cited symbol, scoped to the
// module being audited.
//
// The scoping is the entire point. Resolving a name repo-wide and taking the
// first hit by path handed the auditor a stranger's code: Pallium declares
// Store in four packages, Run in six, Open in four. An audit of internal/db
// was shown internal/console's Store, and correctly ruled that the claims did
// not match the code it could see. That produced 44 confident refutations of
// claims that were true. Preference order is the claim's own cited paths,
// then the module's files, then nothing at all — and "nothing" is reported
// rather than substituted, because wrong evidence is far worse than missing
// evidence.
func claimExcerpts(store *db.Store, repoID int64, repoRoot string, claims []citedClaim, module Module) ([]sourceExcerpt, []string, error) {
	moduleFiles := make(map[string]struct{}, len(module.Files))
	for _, path := range module.Files {
		moduleFiles[path] = struct{}{}
	}

	type request struct {
		name  string
		paths map[string]struct{}
	}
	requests := make([]request, 0)
	seen := make(map[string]int)
	for _, claim := range claims {
		for _, name := range claim.CitedSymbols {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			index, ok := seen[name]
			if !ok {
				seen[name] = len(requests)
				requests = append(requests, request{name: name, paths: map[string]struct{}{}})
				index = len(requests) - 1
			}
			for _, path := range claim.CitedPaths {
				requests[index].paths[strings.TrimSpace(path)] = struct{}{}
			}
		}
	}

	out := make([]sourceExcerpt, 0, len(requests))
	missing := make([]string, 0)
	cache := make(map[string][]string)

	for _, item := range requests {
		symbols, err := store.SymbolsNamed(repoID, item.name)
		if err != nil {
			return nil, nil, err
		}
		chosen := pickDeclarations(symbols, item.paths, moduleFiles)
		if len(chosen) == 0 && strings.Contains(item.name, ".") {
			tail := item.name[strings.LastIndex(item.name, ".")+1:]
			symbols, err = store.SymbolsNamed(repoID, tail)
			if err != nil {
				return nil, nil, err
			}
			chosen = pickDeclarations(symbols, item.paths, moduleFiles)
		}
		if len(chosen) == 0 {
			missing = append(missing, item.name)
			continue
		}
		for _, symbol := range chosen {
			excerpt, ok := readExcerpt(repoRoot, symbol, cache)
			if !ok {
				missing = append(missing, item.name)
				continue
			}
			out = append(out, excerpt)
		}
	}
	return out, sortedUnique(missing), nil
}

// maxDeclarationsPerName bounds how many same-named declarations inside one
// module are shown. Two is enough to reveal an interface plus its
// implementation without crowding out other claims' evidence.
const maxDeclarationsPerName = 2

func pickDeclarations(symbols []db.CodeSymbol, citedPaths, moduleFiles map[string]struct{}) []db.CodeSymbol {
	inCited := make([]db.CodeSymbol, 0, 2)
	inModule := make([]db.CodeSymbol, 0, 2)
	for _, symbol := range symbols {
		if _, ok := citedPaths[symbol.Path]; ok {
			inCited = append(inCited, symbol)
			continue
		}
		if _, ok := moduleFiles[symbol.Path]; ok {
			inModule = append(inModule, symbol)
		}
	}

	chosen := inCited
	if len(chosen) == 0 {
		chosen = inModule
	}
	if len(chosen) > maxDeclarationsPerName {
		chosen = chosen[:maxDeclarationsPerName]
	}
	return chosen
}

func readExcerpt(repoRoot string, symbol db.CodeSymbol, cache map[string][]string) (sourceExcerpt, bool) {
	lines, ok := cache[symbol.Path]
	if !ok {
		content, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(symbol.Path)))
		if err != nil {
			cache[symbol.Path] = nil
			return sourceExcerpt{}, false
		}
		lines = strings.Split(string(content), "\n")
		cache[symbol.Path] = lines
	}
	if lines == nil {
		return sourceExcerpt{}, false
	}

	start := symbol.StartLine - 1
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		return sourceExcerpt{}, false
	}
	end := symbol.EndLine
	if end <= start {
		end = start + 1
	}
	if end > start+maxExcerptLines {
		end = start + maxExcerptLines
	}
	if end > len(lines) {
		end = len(lines)
	}
	code := strings.Join(lines[start:end], "\n")
	// Last line of defence against a misaligned index: if the declaration's
	// own name is not near the top of what we are about to quote, the line
	// number is wrong and this excerpt is someone else's code. Report it as
	// missing rather than shipping it as evidence.
	if !declarationVisible(lines, start, symbol.Name) {
		return sourceExcerpt{}, false
	}
	return sourceExcerpt{
		name: symbolName(symbol),
		path: symbol.Path,
		line: symbol.StartLine,
		code: code,
	}, true
}

// declarationHeadLines is how far past the recorded start line the name may
// appear. A declaration can carry an attribute or a decorator above its name,
// but not much more.
const declarationHeadLines = 3

func declarationVisible(lines []string, start int, name string) bool {
	if name == "" {
		return true
	}
	for offset := 0; offset < declarationHeadLines && start+offset < len(lines); offset++ {
		if strings.Contains(lines[start+offset], name) {
			return true
		}
	}
	return false
}

type pathOutline struct {
	path string
	body string
}

// citedPathOutlines gives a claim that cites a FILE something to be judged
// against. It lists that file's declarations from the index rather than
// dumping the file, so the evidence stays bounded and costs no disk reads.
// Files already covered by a symbol excerpt are skipped.
func citedPathOutlines(store *db.Store, repoID int64, claims []citedClaim, excerpts []sourceExcerpt) ([]pathOutline, error) {
	covered := make(map[string]struct{}, len(excerpts))
	for _, excerpt := range excerpts {
		covered[excerpt.path] = struct{}{}
	}

	seen := make(map[string]struct{})
	out := make([]pathOutline, 0)
	for _, claim := range claims {
		for _, path := range claim.CitedPaths {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if _, ok := covered[path]; ok {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}

			symbols, err := store.SymbolsInFile(repoID, path)
			if err != nil {
				return nil, err
			}
			if len(symbols) == 0 {
				continue
			}

			var body strings.Builder
			for index, symbol := range symbols {
				if index >= maxPathSymbolsListed {
					fmt.Fprintf(&body, "  ...and %d more declarations\n", len(symbols)-maxPathSymbolsListed)
					break
				}
				fmt.Fprintf(&body, "  %s:%d %s %s", path, symbol.StartLine, symbol.Kind, symbolName(symbol))
				if symbol.Signature != "" {
					fmt.Fprintf(&body, " — %s", truncate(symbol.Signature, 140))
				}
				body.WriteString("\n")
				if symbol.Doc != "" {
					fmt.Fprintf(&body, "      doc: %s\n", truncate(symbol.Doc, 180))
				}
			}
			out = append(out, pathOutline{path: path, body: body.String()})
		}
	}
	return out, nil
}

func parseAuditResponse(raw string) ([]verdict, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, fmt.Errorf("auditor returned nothing")
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("auditor response was not JSON")
	}
	var parsed auditResponse
	if err := json.Unmarshal([]byte(text[start:end+1]), &parsed); err != nil {
		return nil, fmt.Errorf("auditor response did not parse: %w", err)
	}
	if len(parsed.Verdicts) == 0 {
		return nil, fmt.Errorf("auditor returned no verdicts")
	}
	return parsed.Verdicts, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
