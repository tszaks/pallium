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
	NeedsRebuild int            `json:"needs_rebuild"`
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

		var claims synthesis
		if strings.TrimSpace(doc.Claims) != "" {
			_ = json.Unmarshal([]byte(doc.Claims), &claims)
		}
		flat := flattenClaims(claims)
		if len(flat) == 0 {
			if strings.Contains(doc.Generator, "model") {
				report.NeedsRebuild++
			} else {
				// Nothing a model asserted, so nothing to refute. A
				// structural doc is true by construction.
				report.Skipped++
			}
			continue
		}

		prompt, err := auditPrompt(store, repoID, repoRoot, doc, flat)
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
		report.Audited++
		report.Claims += len(flat)

		removals := make(map[string]string)
		for _, ruling := range rulings {
			if ruling.Index < 1 || ruling.Index > len(flat) {
				continue
			}
			claim := flat[ruling.Index-1]
			switch strings.ToLower(strings.TrimSpace(ruling.Ruling)) {
			case "unsupported":
				removals[claim.Claim] = strings.TrimSpace(ruling.Why)
				report.Removed++
				report.Findings = append(report.Findings, AuditRemoval{
					Slug:   item.doc.Slug,
					Claim:  truncate(claim.Claim, 120),
					Reason: truncate(strings.TrimSpace(ruling.Why), 200),
				})
			case "unclear":
				report.Unclear++
			default:
				report.Supported++
			}
		}

		if len(removals) == 0 {
			continue
		}

		updated := item.doc
		pruned := pruneClaims(item.claims, removals)
		module, err := moduleForDoc(store, repoID, item.doc.Slug)
		if err != nil {
			return AuditReport{}, err
		}
		if module == nil {
			// The module boundary moved since the doc was written; leave the
			// body alone rather than re-rendering against a guess, but still
			// record the rejections.
			for claim, why := range removals {
				updated.DroppedClaims = append(updated.DroppedClaims, fmt.Sprintf("audit: %q removed, %s", truncate(claim, 90), why))
			}
			updated.Verified = false
			if err := store.UpsertKnowledgeDoc(repoID, updated); err != nil {
				return AuditReport{}, err
			}
			continue
		}

		updated.Body = renderModuleDoc(*module, pruned)
		updated.CitedPaths = citedPaths(*module, pruned)
		updated.CitedSymbols = citedSymbols(pruned)
		if encoded, err := json.Marshal(pruned); err == nil {
			updated.Claims = string(encoded)
		}
		for claim, why := range removals {
			updated.DroppedClaims = append(updated.DroppedClaims, fmt.Sprintf("audit: %q removed, %s", truncate(claim, 90), why))
		}
		updated.Verified = false
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
	for _, group := range [][]citedClaim{result.EntryPoints, result.KeySymbols, result.Invariants, result.Risks} {
		out = append(out, group...)
	}
	return out
}

func pruneClaims(result synthesis, removals map[string]string) synthesis {
	keep := func(claims []citedClaim) []citedClaim {
		out := make([]citedClaim, 0, len(claims))
		for _, claim := range claims {
			if _, removed := removals[claim.Claim]; removed {
				continue
			}
			out = append(out, claim)
		}
		return out
	}
	return synthesis{
		Summary:     result.Summary,
		Purpose:     result.Purpose,
		EntryPoints: keep(result.EntryPoints),
		KeySymbols:  keep(result.KeySymbols),
		Invariants:  keep(result.Invariants),
		Risks:       keep(result.Risks),
	}
}

func moduleForDoc(store *db.Store, repoID int64, slug string) (*Module, error) {
	modules, err := Modules(store, repoID, ModuleOptions{})
	if err != nil {
		return nil, err
	}
	for index := range modules {
		if modules[index].Slug == slug {
			return &modules[index], nil
		}
	}
	return nil, nil
}

// auditPrompt hands the skeptic the claims and the actual source of every
// symbol they cite. Source excerpts are what make this an audit rather than a
// second opinion: a model asked to re-judge a claim from memory would mostly
// agree with itself.
func auditPrompt(store *db.Store, repoID int64, repoRoot string, doc db.KnowledgeDoc, claims []citedClaim) (string, error) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "You are auditing generated documentation for the module %s. Your job is to REFUTE claims the source does not support.\n\n", doc.Title)

	builder.WriteString("Claims, numbered:\n")
	for index, claim := range claims {
		citations := append([]string{}, claim.CitedSymbols...)
		citations = append(citations, claim.CitedPaths...)
		fmt.Fprintf(&builder, "%d. %s [cites: %s]\n", index+1, strings.TrimSpace(claim.Claim), strings.Join(citations, ", "))
	}

	excerpts, err := claimExcerpts(store, repoID, repoRoot, claims)
	if err != nil {
		return "", err
	}
	if len(excerpts) > 0 {
		builder.WriteString("\nSource for the cited symbols:\n\n")
		for _, excerpt := range excerpts {
			fmt.Fprintf(&builder, "----- %s (%s:%d)\n%s\n", excerpt.name, excerpt.path, excerpt.line, excerpt.code)
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
const maxExcerptLines = 45

func claimExcerpts(store *db.Store, repoID int64, repoRoot string, claims []citedClaim) ([]sourceExcerpt, error) {
	wanted := make([]string, 0)
	seen := make(map[string]struct{})
	for _, claim := range claims {
		for _, name := range claim.CitedSymbols {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			wanted = append(wanted, name)
		}
	}

	out := make([]sourceExcerpt, 0, len(wanted))
	cache := make(map[string][]string)
	for _, name := range wanted {
		symbols, err := store.SymbolsNamed(repoID, name)
		if err != nil {
			return nil, err
		}
		for _, symbol := range symbols {
			lines, ok := cache[symbol.Path]
			if !ok {
				content, readErr := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(symbol.Path)))
				if readErr != nil {
					cache[symbol.Path] = nil
					continue
				}
				lines = strings.Split(string(content), "\n")
				cache[symbol.Path] = lines
			}
			if lines == nil {
				continue
			}

			start := symbol.StartLine - 1
			if start < 0 {
				start = 0
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
			out = append(out, sourceExcerpt{
				name: symbolName(symbol),
				path: symbol.Path,
				line: symbol.StartLine,
				code: strings.Join(lines[start:end], "\n"),
			})
			// One declaration per name is enough evidence; a name declared in
			// several places would otherwise flood the prompt.
			break
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
