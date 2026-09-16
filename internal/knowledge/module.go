// Package knowledge builds Pallium's codebase knowledge base: a set of docs
// describing what each part of a repo is for, grounded in the content index.
//
// The design rule here is that everything a computer can determine, a computer
// determines. Module boundaries, file lists, symbol rankings, dependency
// edges and the incident list are all derived from the index with no model
// involved, and they are correct by construction. A model is used for exactly
// one thing, writing the prose that explains why a module exists, and anything
// it claims is checked against the index before it is stored.
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/codeindex"
	"github.com/tszaks/pallium/internal/db"
)

// Module is a cluster of files treated as one unit of knowledge.
type Module struct {
	Slug            string          `json:"slug"`
	Dir             string          `json:"dir"`
	Title           string          `json:"title"`
	Files           []string        `json:"files"`
	Languages       []string        `json:"languages"`
	KeySymbols      []db.CodeSymbol `json:"key_symbols"`
	DependsOn       []string        `json:"depends_on"`
	DependedOnBy    []string        `json:"depended_on_by"`
	ExternalDeps    []string        `json:"external_deps"`
	RecentCommits   []CommitNote    `json:"recent_commits"`
	SymbolCount     int             `json:"symbol_count"`
	Fingerprint     string          `json:"fingerprint"`
	EntryCandidates []string        `json:"entry_candidates"`
}

type CommitNote struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
}

// ModuleOptions tunes clustering. The defaults aim at a map a person can hold
// in their head: a few dozen modules at most, none of them a single file.
type ModuleOptions struct {
	MinFiles   int
	MaxModules int
}

func (o ModuleOptions) withDefaults() ModuleOptions {
	if o.MinFiles <= 0 {
		o.MinFiles = 3
	}
	if o.MaxModules <= 0 {
		o.MaxModules = 40
	}
	return o
}

// Modules clusters the indexed files into directory-shaped modules.
//
// Directories are the clustering signal because they are the one grouping
// every codebase already agrees on, and because a boundary a person chose
// beats one inferred from an import graph that happens to be densely
// connected. Small directories merge upward until they carry their weight.
func Modules(store *db.Store, repoID int64, opts ModuleOptions) ([]Module, error) {
	opts = opts.withDefaults()

	paths, err := store.CodeIndexedPaths(repoID)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return []Module{}, nil
	}

	buckets := clusterPaths(paths, opts)

	shas, err := store.CodeFileSHAs(repoID)
	if err != nil {
		return nil, err
	}
	refCounts, err := store.RefCounts(repoID)
	if err != nil {
		return nil, err
	}

	dirs := make([]string, 0, len(buckets))
	for dir := range buckets {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	owner := newOwnerIndex(dirs)

	modules := make([]Module, 0, len(dirs))
	for _, dir := range dirs {
		files := buckets[dir]
		sort.Strings(files)

		module := Module{
			Slug:          slugForDir(dir),
			Dir:           dir,
			Title:         titleForDir(dir),
			Files:         files,
			Fingerprint:   fingerprint(files, shas),
			RecentCommits: []CommitNote{},
		}
		module.Languages = languagesFor(files)
		module.EntryCandidates = entryCandidates(files)

		symbols, err := store.SymbolsUnderPrefix(repoID, dir)
		if err != nil {
			return nil, err
		}
		owned := make([]db.CodeSymbol, 0, len(symbols))
		for _, symbol := range symbols {
			if owner.moduleFor(symbol.Path) != dir {
				continue
			}
			owned = append(owned, symbol)
		}
		module.SymbolCount = len(owned)
		module.KeySymbols = rankSymbols(owned, refCounts, 12)

		commits, err := store.RecentSubjectsUnderPrefix(repoID, dir, 5)
		if err != nil {
			return nil, err
		}
		for _, commit := range commits {
			module.RecentCommits = append(module.RecentCommits, CommitNote{
				SHA:     commit.SHA,
				Subject: commit.Subject,
				Date:    commit.CommittedAt.Format("2006-01-02"),
			})
		}

		modules = append(modules, module)
	}

	if err := attachDependencies(store, repoID, owner, modules); err != nil {
		return nil, err
	}

	return modules, nil
}

// clusterPaths does the merging. Deepest directories are considered first, so
// a small leaf folds into its parent before the parent is judged for size.
func clusterPaths(paths []string, opts ModuleOptions) map[string][]string {
	buckets := make(map[string][]string)
	for _, path := range paths {
		dir := filepath.ToSlash(filepath.Dir(path))
		buckets[dir] = append(buckets[dir], path)
	}

	mergeOnce := func(minFiles int) {
		dirs := make([]string, 0, len(buckets))
		for dir := range buckets {
			dirs = append(dirs, dir)
		}
		// Deepest first: sorting by depth then reverse-lexically means a
		// child is always merged before its parent is measured.
		sort.Slice(dirs, func(i, j int) bool {
			di, dj := depth(dirs[i]), depth(dirs[j])
			if di != dj {
				return di > dj
			}
			return dirs[i] > dirs[j]
		})
		for _, dir := range dirs {
			if dir == "." || dir == "" {
				continue
			}
			if len(buckets[dir]) >= minFiles {
				continue
			}
			parent := filepath.ToSlash(filepath.Dir(dir))
			buckets[parent] = append(buckets[parent], buckets[dir]...)
			delete(buckets, dir)
		}
	}

	mergeOnce(opts.MinFiles)
	// Still too many modules: raise the bar and merge again rather than
	// truncating, because a truncated map silently hides part of the repo.
	for minFiles := opts.MinFiles * 2; len(buckets) > opts.MaxModules && minFiles < 4096; minFiles *= 2 {
		mergeOnce(minFiles)
	}
	return buckets
}

// ownerIndex maps any path to the module directory that owns it: the deepest
// module directory that is a prefix of the path.
type ownerIndex struct {
	dirs map[string]struct{}
}

func newOwnerIndex(dirs []string) ownerIndex {
	set := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		set[dir] = struct{}{}
	}
	return ownerIndex{dirs: set}
}

func (o ownerIndex) moduleFor(path string) string {
	dir := filepath.ToSlash(filepath.Dir(path))
	for {
		if _, ok := o.dirs[dir]; ok {
			return dir
		}
		if dir == "." || dir == "" || dir == "/" {
			return ""
		}
		next := filepath.ToSlash(filepath.Dir(dir))
		if next == dir {
			return ""
		}
		dir = next
	}
}

// moduleForImportTarget handles the one asymmetry in the import table: a Go
// edge points at a package directory while a JS or Python edge points at a
// file.
func (o ownerIndex) moduleForImportTarget(target string) string {
	if _, ok := o.dirs[target]; ok {
		return target
	}
	return o.moduleFor(target)
}

func attachDependencies(store *db.Store, repoID int64, owner ownerIndex, modules []Module) error {
	imports, err := store.ResolvedImports(repoID)
	if err != nil {
		return err
	}

	dependsOn := make(map[string]map[string]struct{})
	dependedOnBy := make(map[string]map[string]struct{})
	for _, imp := range imports {
		from := owner.moduleFor(imp.FromPath)
		to := owner.moduleForImportTarget(imp.ToPath)
		if from == "" || to == "" || from == to {
			continue
		}
		if dependsOn[from] == nil {
			dependsOn[from] = map[string]struct{}{}
		}
		dependsOn[from][to] = struct{}{}
		if dependedOnBy[to] == nil {
			dependedOnBy[to] = map[string]struct{}{}
		}
		dependedOnBy[to][from] = struct{}{}
	}

	externalRows, err := store.ExternalImports(repoID)
	if err != nil {
		return err
	}
	external := make(map[string]map[string]int)
	for _, row := range externalRows {
		module := owner.moduleFor(row.FromPath)
		if module == "" {
			continue
		}
		if external[module] == nil {
			external[module] = map[string]int{}
		}
		external[module][row.RawSpec]++
	}

	for index := range modules {
		dir := modules[index].Dir
		modules[index].DependsOn = sortedSlugs(dependsOn[dir])
		modules[index].DependedOnBy = sortedSlugs(dependedOnBy[dir])
		modules[index].ExternalDeps = topExternal(external[dir], 8)
	}
	return nil
}

func sortedSlugs(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for dir := range set {
		out = append(out, slugForDir(dir))
	}
	sort.Strings(out)
	return out
}

func topExternal(counts map[string]int, limit int) []string {
	type entry struct {
		name  string
		count int
	}
	entries := make([]entry, 0, len(counts))
	for name, count := range counts {
		entries = append(entries, entry{name: name, count: count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].name < entries[j].name
	})
	out := make([]string, 0, limit)
	for _, item := range entries {
		if len(out) >= limit {
			break
		}
		out = append(out, item.name)
	}
	return out
}

// rankSymbols puts the symbols other files actually use first. Test symbols
// sink to the bottom regardless of how many there are: a module's surface is
// what it offers callers, and TestRunDropsDeletedFiles is not that. Then
// exported beats unexported, then reference count, then name.
func rankSymbols(symbols []db.CodeSymbol, refCounts map[string]int, limit int) []db.CodeSymbol {
	ranked := make([]db.CodeSymbol, len(symbols))
	copy(ranked, symbols)
	sort.SliceStable(ranked, func(i, j int) bool {
		ti, tj := isTestPath(ranked[i].Path), isTestPath(ranked[j].Path)
		if ti != tj {
			return tj
		}
		if ranked[i].Exported != ranked[j].Exported {
			return ranked[i].Exported
		}
		ri, rj := refCounts[ranked[i].Name], refCounts[ranked[j].Name]
		if ri != rj {
			return ri > rj
		}
		return ranked[i].Name < ranked[j].Name
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	if ranked == nil {
		return []db.CodeSymbol{}
	}
	return ranked
}

// entryCandidates picks the files most likely to be where a reader should
// start: a main, an index, an app, or a file named after its own directory.
func entryCandidates(files []string) []string {
	out := make([]string, 0, 3)
	for _, path := range files {
		base := strings.ToLower(filepath.Base(path))
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		dirName := strings.ToLower(filepath.Base(filepath.Dir(path)))
		switch {
		case stem == "main", stem == "index", stem == "app", stem == "mod", stem == "__init__":
			out = append(out, path)
		case stem == dirName:
			out = append(out, path)
		}
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func languagesFor(files []string) []string {
	counts := map[string]int{}
	for _, path := range files {
		if lang := codeindex.Lang(path); lang != "" {
			counts[lang]++
		}
	}
	out := make([]string, 0, len(counts))
	for lang := range counts {
		out = append(out, lang)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// fingerprint hashes the module's file content hashes. A doc records the
// fingerprint it was written against, which is how refresh knows the module
// changed without re-reading a line of it.
func fingerprint(files []string, shas map[string]string) string {
	hasher := sha256.New()
	for _, path := range files {
		hasher.Write([]byte(path))
		hasher.Write([]byte{0})
		hasher.Write([]byte(shas[path]))
		hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))[:16]
}

func slugForDir(dir string) string {
	if dir == "." || dir == "" {
		return "root"
	}
	slug := strings.ToLower(dir)
	slug = strings.NewReplacer("/", "-", " ", "-", "_", "-", ".", "-").Replace(slug)
	slug = strings.Trim(slug, "-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	if slug == "" {
		return "root"
	}
	return slug
}

func titleForDir(dir string) string {
	if dir == "." || dir == "" {
		return "Repository root"
	}
	return dir
}

func depth(dir string) int {
	if dir == "." || dir == "" {
		return 0
	}
	return strings.Count(dir, "/") + 1
}

// isTestPath recognizes the test-file naming every language in the index uses.
func isTestPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasSuffix(base, "_test.py"),
		strings.HasSuffix(base, "_spec.rb"),
		strings.HasPrefix(base, "test_"):
		return true
	}
	for _, suffix := range []string{".test.", ".spec."} {
		if strings.Contains(base, suffix) {
			return true
		}
	}
	return strings.Contains(path, "/__tests__/") || strings.HasPrefix(path, "__tests__/")
}
