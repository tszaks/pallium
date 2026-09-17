package analysis

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/codeindex"
	"github.com/tszaks/pallium/internal/db"
)

// SymbolsReport is what one file declares, plus its resolved import edges in
// both directions. It answers the question explain never could: not "how risky
// is this file" but "what is in it".
type SymbolsReport struct {
	Path       string          `json:"path"`
	Lang       string          `json:"lang"`
	Symbols    []db.CodeSymbol `json:"symbols"`
	Imports    []string        `json:"imports"`
	Dependents []string        `json:"dependents"`
	Note       string          `json:"note,omitempty"`
}

func Symbols(store *db.Store, targetPath string) (SymbolsReport, error) {
	normalized, err := normalizeRepoPath(store.RepoRoot, targetPath)
	if err != nil {
		return SymbolsReport{}, err
	}

	repo, err := store.Repo()
	if err != nil {
		return SymbolsReport{}, err
	}

	report := SymbolsReport{Path: normalized, Lang: codeindex.Lang(normalized)}
	if !store.HasCodeIndex(repo.ID) {
		report.Note = "No content index for this repo yet. Run `pallium index`."
		report.Symbols = []db.CodeSymbol{}
		report.Imports = []string{}
		report.Dependents = []string{}
		return report, nil
	}

	symbols, err := store.SymbolsInFile(repo.ID, normalized)
	if err != nil {
		return SymbolsReport{}, err
	}
	report.Symbols = symbols

	outgoing, err := store.ImportsFrom(repo.ID, normalized)
	if err != nil {
		return SymbolsReport{}, err
	}
	imports := make([]string, 0, len(outgoing))
	for _, imp := range outgoing {
		if imp.ToPath == "" {
			continue
		}
		imports = append(imports, imp.ToPath)
	}
	report.Imports = uniqueStrings(imports, 0)

	dependents, err := dependentPaths(store, repo.ID, normalized)
	if err != nil {
		return SymbolsReport{}, err
	}
	report.Dependents = dependents

	if len(symbols) == 0 && report.Note == "" {
		report.Note = "Indexed, but no symbols were found. The file may be generated, too large, or in a language with no parser."
	}
	return report, nil
}

// dependentPaths merges the two ways a file can be depended on: a direct file
// import (JS, Python) and a package import that names its directory (Go).
func dependentPaths(store *db.Store, repoID int64, normalized string) ([]string, error) {
	direct, err := store.ImportsTo(repoID, normalized)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(direct))
	for _, imp := range direct {
		out = append(out, imp.FromPath)
	}

	if strings.HasSuffix(normalized, ".go") {
		packageImporters, err := store.ImportsTo(repoID, filepath.ToSlash(filepath.Dir(normalized)))
		if err != nil {
			return nil, err
		}
		for _, imp := range packageImporters {
			if imp.Kind != "go-import" || imp.FromPath == normalized {
				continue
			}
			out = append(out, imp.FromPath)
		}
	}

	sort.Strings(out)
	return uniqueStrings(out, 0), nil
}

// CallersReport separates where a name is declared from where it is used. Both
// halves matter: a name with three declarations is usually an interface and
// its implementations, which changes what "who calls this" means.
type CallersReport struct {
	Name         string          `json:"name"`
	DeclaredIn   []db.CodeSymbol `json:"declared_in"`
	ReferencedBy []string        `json:"referenced_by"`
}

func Callers(store *db.Store, name string) (CallersReport, error) {
	repo, err := store.Repo()
	if err != nil {
		return CallersReport{}, err
	}

	name = strings.TrimSpace(name)
	report := CallersReport{Name: name, DeclaredIn: []db.CodeSymbol{}, ReferencedBy: []string{}}
	if name == "" || !store.HasCodeIndex(repo.ID) {
		return report, nil
	}

	declarations, err := store.SymbolsNamed(repo.ID, name)
	if err != nil {
		return CallersReport{}, err
	}
	report.DeclaredIn = declarations

	referencedBy, err := store.PathsReferencing(repo.ID, name)
	if err != nil {
		return CallersReport{}, err
	}
	report.ReferencedBy = referencedBy
	return report, nil
}

// CodeSearchReport is an FTS5 search over the symbol table: names, receivers,
// signatures and doc comments, ranked.
type CodeSearchReport struct {
	Query   string          `json:"query"`
	Matches []db.CodeSymbol `json:"matches"`
}

func SearchCode(store *db.Store, query string, limit int) (CodeSearchReport, error) {
	repo, err := store.Repo()
	if err != nil {
		return CodeSearchReport{}, err
	}

	report := CodeSearchReport{Query: query, Matches: []db.CodeSymbol{}}
	if !store.HasCodeIndex(repo.ID) {
		return report, nil
	}

	matches, err := store.SearchSymbols(repo.ID, query, limit)
	if err != nil {
		return CodeSearchReport{}, err
	}
	report.Matches = matches
	return report, nil
}
