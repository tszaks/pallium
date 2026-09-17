package analysis

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/codeindex"
	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
)

type StructuralLink struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// structuralLinksByScan is the fallback for a repo with no content index: an
// index written before content indexing existed, or a directory that is not a
// git repo. It reads every candidate file on every call, which is why the
// indexed path in structural.go exists.
//
// It reuses codeindex.Parse rather than keeping its own copy of the import
// rules. The two paths then cannot drift: a fix to how a tsconfig alias
// resolves lands in both at once, and this file stops carrying 250 lines of
// resolution logic that has to stay in sync with the indexer by hand.
func structuralLinksByScan(store *db.Store, targetPath string, limit int) ([]StructuralLink, error) {
	normalized, err := normalizeRepoPath(store.RepoRoot, targetPath)
	if err != nil {
		return nil, err
	}

	files, err := repoFiles(store.RepoRoot)
	if err != nil {
		return nil, err
	}

	resolver := codeindex.NewResolver(store.RepoRoot, files)
	targetParsed := parseForLinks(store.RepoRoot, normalized, resolver)

	targetDir := filepath.ToSlash(filepath.Dir(normalized))
	targetName := filepath.Base(normalized)
	targetStem := fileStem(targetName)
	targetIsTest := isTestFile(targetName)
	targetIsGo := strings.HasSuffix(targetName, ".go")

	out := make([]StructuralLink, 0)
	for _, candidate := range files {
		if candidate == normalized {
			continue
		}

		candidateName := filepath.Base(candidate)
		candidateDir := filepath.ToSlash(filepath.Dir(candidate))
		candidateStem := fileStem(candidateName)
		candidateIsTest := isTestFile(candidateName)
		candidateIsGo := strings.HasSuffix(candidateName, ".go")

		needsCandidateParse := candidateIsGo || isJSImportFile(candidate) || isPythonFile(candidate)
		candidateParsed := parsedFile{}
		if needsCandidateParse {
			candidateParsed = parseForLinks(store.RepoRoot, candidate, resolver)
		}

		switch {
		case targetStem != "" && candidateStem == targetStem && targetIsTest != candidateIsTest:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "test-pair",
				Reason: "Shares the same source stem as the target file.",
			})
		case targetStem != "" && candidateStem == targetStem:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "same-stem",
				Reason: "Shares the same file stem as the target file.",
			})
		case targetIsGo && candidateIsGo && candidateStem != "" && targetParsed.referencesStem(candidateStem):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-symbol",
				Reason: "Target file references a symbol that matches this Go file's stem.",
			})
		case targetIsGo && candidateIsGo && targetParsed.importsGoDir(candidateDir):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-import",
				Reason: "Target file imports this Go package from the same repo.",
			})
		case targetIsGo && candidateIsGo && targetStem != "" && candidateParsed.referencesStem(targetStem):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-dependent",
				Reason: "This Go file appears to reference the target file's symbol stem.",
			})
		case targetIsGo && candidateIsGo && candidateParsed.importsGoDir(targetDir):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-package-dependent",
				Reason: "This Go file imports the target package from the same repo.",
			})
		case targetParsed.importsPath(candidate, "js-import"):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "js-import",
				Reason: "Target file imports this JS/TS module with a relative path.",
			})
		case candidateParsed.importsPath(normalized, "js-import"):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "js-dependent",
				Reason: "This JS/TS file imports the target module with a relative path.",
			})
		case targetParsed.importsPath(candidate, "py-import"):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "py-import",
				Reason: "Target file imports this Python module with a local import path.",
			})
		case candidateParsed.importsPath(normalized, "py-import"):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "py-dependent",
				Reason: "This Python file imports the target module with a local import path.",
			})
		case candidateDir == targetDir && candidateIsTest:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "same-dir-test",
				Reason: "Test file in the same directory as the target file.",
			})
		case candidateDir == targetDir && filepath.Ext(candidateName) == filepath.Ext(targetName):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "same-dir",
				Reason: "File in the same directory with the same extension.",
			})
		}
	}

	return uniqueStructuralLinks(out, limit), nil
}

// parsedFile wraps a codeindex parse in the lookups the scan loop needs.
type parsedFile struct {
	refs       map[string]struct{}
	importDirs map[string]struct{}
	importPath map[string]string
}

func parseForLinks(repoRoot, path string, resolver *codeindex.Resolver) parsedFile {
	out := parsedFile{
		refs:       map[string]struct{}{},
		importDirs: map[string]struct{}{},
		importPath: map[string]string{},
	}

	content, err := osReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
	if err != nil || len(content) == 0 {
		return out
	}
	parsed, ok := codeindex.Parse(path, content, resolver)
	if !ok {
		return out
	}

	for _, ref := range parsed.Refs {
		out.refs[strings.ToLower(ref.Name)] = struct{}{}
	}
	for _, imp := range parsed.Imports {
		if imp.ToPath == "" {
			continue
		}
		if imp.Kind == "go-import" {
			out.importDirs[imp.ToPath] = struct{}{}
			continue
		}
		out.importPath[imp.ToPath] = imp.Kind
	}
	return out
}

func (p parsedFile) referencesStem(stem string) bool {
	_, ok := p.refs[strings.ToLower(stem)]
	return ok
}

func (p parsedFile) importsGoDir(dir string) bool {
	_, ok := p.importDirs[dir]
	return ok
}

func (p parsedFile) importsPath(path, kind string) bool {
	found, ok := p.importPath[path]
	return ok && found == kind
}

func SuggestedTests(store *db.Store, targetPath string, limit int) ([]string, error) {
	links, err := StructuralLinks(store, targetPath, limit*3)
	if err != nil {
		return nil, err
	}

	tests := make([]string, 0, limit)
	for _, link := range links {
		if !isSuggestedTestLink(link) {
			continue
		}
		if !isTestFile(filepath.Base(link.Path)) {
			continue
		}
		tests = append(tests, link.Path)
	}

	if len(tests) == 0 && isTestFile(filepath.Base(targetPath)) {
		tests = append(tests, filepath.ToSlash(filepath.Clean(targetPath)))
	}

	return uniqueStrings(tests, limit), nil
}

func BlastRadius(store *db.Store, targetPath string, limit int) ([]string, error) {
	normalized, err := normalizeRepoPath(store.RepoRoot, targetPath)
	if err != nil {
		return nil, err
	}

	neighbors, err := Neighbors(store, normalized, limit)
	if err != nil {
		return nil, err
	}
	links, err := StructuralLinks(store, normalized, limit)
	if err != nil {
		return nil, err
	}
	tests, err := SuggestedTests(store, normalized, limit)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, limit*3)
	for _, neighbor := range neighbors {
		out = append(out, neighbor.Path)
	}
	for _, link := range links {
		out = append(out, link.Path)
	}
	out = append(out, tests...)

	return uniqueStrings(out, limit), nil
}

// maxScannedFileBytes caps how much of any single file a content scanner will
// read. Minified bundles, lockfiles and generated clients are megabytes of one
// line: regexing them costs real time and yields nothing a human would call an
// import. Files larger than this are listed but their contents are skipped.
const maxScannedFileBytes = 512 * 1024

// repoFiles lists the repo-relative paths worth scanning for content.
//
// This asks git rather than walking the filesystem. The walk it replaced
// descended into every directory except .git, .pallium and .codex-memory,
// which meant a repo with a node_modules or a vendor directory had its entire
// dependency tree read from disk on every explain, risk and review call. A
// measured case: a three-file repo answered explain in 1.9s, and 12.2s once
// 20,000 gitignored files existed beside it, for the same single result.
// git ls-files with --exclude-standard inherits .gitignore for free, and
// --others keeps files the agent just created but has not staged yet, which a
// tracked-only listing would miss.
//
// Falling back to the walk matters for tests and for directories that are not
// git repos; the fallback keeps the old skip list.
func repoFiles(repoRoot string) ([]string, error) {
	files, err := gitlog.TrackedFiles(repoRoot)
	if err == nil {
		return files, nil
	}
	return walkRepoFiles(repoRoot)
}

func walkRepoFiles(repoRoot string) ([]string, error) {
	out := make([]string, 0)
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			name := d.Name()
			switch name {
			case ".git", ".pallium", ".codex-memory":
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(out)
	return out, nil
}

func fileStem(name string) string {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	stem = strings.TrimSuffix(stem, "_test")
	stem = strings.TrimSuffix(stem, ".test")
	return stem
}

func isTestFile(name string) bool {
	return strings.HasSuffix(name, "_test.go") ||
		strings.HasSuffix(name, "_spec.rb") ||
		strings.HasSuffix(name, ".test.js") ||
		strings.HasSuffix(name, ".test.ts") ||
		strings.HasSuffix(name, ".test.tsx") ||
		strings.HasSuffix(name, ".spec.js") ||
		strings.HasSuffix(name, ".spec.ts") ||
		strings.HasSuffix(name, ".spec.tsx") ||
		strings.HasSuffix(name, "_test.py") ||
		strings.HasPrefix(name, "test_")
}

func uniqueStructuralLinks(links []StructuralLink, limit int) []StructuralLink {
	seen := make(map[string]struct{})
	out := make([]StructuralLink, 0, len(links))
	for _, link := range links {
		key := link.Kind + "::" + link.Path
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, link)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func uniqueStrings(values []string, limit int) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func isJSImportFile(path string) bool {
	return strings.HasSuffix(path, ".js") ||
		strings.HasSuffix(path, ".jsx") ||
		strings.HasSuffix(path, ".ts") ||
		strings.HasSuffix(path, ".tsx")
}

func isPythonFile(path string) bool {
	return strings.HasSuffix(path, ".py")
}

func isSuggestedTestLink(link StructuralLink) bool {
	switch link.Kind {
	case "test-pair", "same-dir-test", "js-dependent", "py-dependent", "go-package-dependent":
		return true
	default:
		return false
	}
}

func hasGoTests(paths []string) bool {
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			return true
		}
	}
	return false
}

// osReadFile reads a file for content scanning, refusing anything past
// maxScannedFileBytes. Scanners treat an empty result as "nothing to see",
// which is the right answer for a 4MB minified bundle.
func osReadFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxScannedFileBytes {
		return nil, nil
	}
	return os.ReadFile(path)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
