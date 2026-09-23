package knowledge

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/tszaks/pallium/internal/db"
)

const ContractVersion = 2

type Hit struct {
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	Kind         string   `json:"kind"`
	Snippet      string   `json:"snippet"`
	Freshness    string   `json:"freshness"`
	State        string   `json:"state"`
	Paths        []string `json:"paths"`
	SourceCommit string   `json:"source_commit"`
}
type Retrieval struct {
	Version   int    `json:"version"`
	Query     string `json:"query"`
	Results   []Hit  `json:"results"`
	Truncated bool   `json:"truncated"`
	Next      string `json:"next"`
}

func queryTerms(query string) []string {
	stop := " a an the is are was were do does did how what where why when i we you it to of in on for this that can my me get gives entire repo project "
	var terms []string
	for _, term := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		if strings.Contains(stop, " "+term+" ") {
			continue
		}
		terms = append(terms, term)
	}
	return terms
}

// Search returns bounded excerpts, never the duplicated full document and claims.
func Search(store *db.Store, id int64, query string, limit, budget int) (Retrieval, error) {
	result := Retrieval{Version: ContractVersion, Query: query, Results: []Hit{}, Next: "knowledge get <slug> for source and evidence"}
	if len(result.Query) > 500 {
		result.Query = result.Query[:500]
	}
	terms := queryTerms(query)
	if len(terms) == 0 {
		return result, nil
	}
	symbols, err := store.SearchSymbols(id, strings.Join(terms, " "), 60)
	if err != nil {
		return result, err
	}
	docs, err := store.SearchKnowledge(id, strings.Join(terms, " "), 60)
	if err != nil {
		return result, err
	}
	if err := Assess(store, docs); err != nil {
		return result, err
	}
	score := func(d db.KnowledgeDoc) int {
		n := 0
		title := strings.ToLower(d.Title + " " + d.Slug)
		summary := strings.ToLower(d.Summary)
		body := strings.ToLower(d.Body)
		for _, t := range terms {
			if strings.Contains(title, t) {
				n += 12
			}
			if strings.Contains(summary, t) {
				n += 5
			}
			if strings.Contains(body, t) {
				n += 2
			}
		}
		if strings.Contains(body, strings.Join(terms, " ")) {
			n += 8
		}
		return n
	}
	sort.SliceStable(docs, func(i, j int) bool { return score(docs[i]) > score(docs[j]) })
	if limit <= 0 {
		limit = 5
	}
	if budget <= 0 {
		budget = 8192
	}
	for _, d := range docs {
		if len(result.Results) >= limit {
			result.Truncated = true
			break
		}
		snippet := d.Summary
		if d.Freshness != "current" && d.Kind != "decision" {
			snippet = "Historical document; refresh before relying on these claims."
		}
		if len(snippet) > 700 {
			snippet = snippet[:700]
		}
		owned := map[string]bool{}
		for _, p := range d.CitedPaths {
			owned[p] = true
		}
		paths := []string{}
		seen := map[string]bool{}
		for _, symbol := range symbols {
			if owned[symbol.Path] && !seen[symbol.Path] && !isTestPath(symbol.Path) {
				paths = append(paths, symbol.Path)
				seen[symbol.Path] = true
			}
		}
		for _, p := range d.CitedPaths {
			if !seen[p] && !isTestPath(p) {
				paths = append(paths, p)
				seen[p] = true
			}
		}
		for _, p := range d.CitedPaths {
			if !seen[p] {
				paths = append(paths, p)
				seen[p] = true
			}
		}
		if len(paths) > 4 {
			paths = paths[:4]
		}
		h := Hit{d.Slug, d.Title, d.Kind, snippet, d.Freshness, d.Evidence.State, paths, d.SourceCommit}
		result.Results = append(result.Results, h)
		b, _ := json.Marshal(result)
		if len(b) > budget-128 {
			result.Results = result.Results[:len(result.Results)-1]
			result.Truncated = true
			break
		}
	}
	return result, nil
}

type MapEntry struct {
	Slug      string   `json:"slug"`
	Directory string   `json:"directory"`
	Files     int      `json:"files"`
	Symbols   int      `json:"symbols"`
	DependsOn []string `json:"depends_on"`
}

func CompactMap(modules []Module) []MapEntry {
	out := make([]MapEntry, 0, len(modules))
	for _, m := range modules {
		out = append(out, MapEntry{m.Slug, m.Dir, len(m.Files), m.SymbolCount, m.DependsOn})
	}
	return out
}
