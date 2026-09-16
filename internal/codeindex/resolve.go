package codeindex

// Import resolution. These helpers moved here from internal/analysis, which
// used them to answer "does this file's import spec point at that candidate
// file" one candidate at a time. The content indexer needs the other shape:
// resolve a spec to a path once, at index time, and store the edge. Same
// rules, called once per import instead of once per (file, candidate) pair.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var jsonCommentRegex = regexp.MustCompile(`(?m)//.*$|/\*[\s\S]*?\*/`)

// TSConfigAliases holds the baseUrl and paths mappings that decide where a
// non-relative TypeScript import points.
type TSConfigAliases struct {
	BaseURL string
	Paths   map[string][]string
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

func tsAliasesFor(repoRoot, sourcePath string) TSConfigAliases {
	configPath := findNearestTSConfig(repoRoot, filepath.Dir(sourcePath))
	if configPath == "" {
		return TSConfigAliases{}
	}
	return readTSConfigFile(repoRoot, configPath, map[string]struct{}{})
}

func findNearestTSConfig(repoRoot, startDir string) string {
	dir := filepath.ToSlash(filepath.Clean(startDir))
	for {
		for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
			candidate := filepath.Join(repoRoot, filepath.FromSlash(dir), name)
			if fileExists(candidate) {
				rel, err := filepath.Rel(repoRoot, candidate)
				if err == nil {
					return filepath.ToSlash(rel)
				}
			}
		}
		if dir == "." || dir == "" || dir == "/" {
			break
		}
		next := filepath.ToSlash(filepath.Dir(dir))
		if next == dir {
			break
		}
		dir = next
	}
	for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
		if fileExists(filepath.Join(repoRoot, name)) {
			return name
		}
	}
	return ""
}

func readTSConfigFile(repoRoot, configPath string, visited map[string]struct{}) TSConfigAliases {
	configPath = filepath.ToSlash(filepath.Clean(configPath))
	if _, ok := visited[configPath]; ok {
		return TSConfigAliases{}
	}
	visited[configPath] = struct{}{}

	content, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(configPath)))
	if err != nil {
		return TSConfigAliases{}
	}
	cleaned := jsonCommentRegex.ReplaceAllString(string(content), "")
	var parsed struct {
		Extends         string `json:"extends"`
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return TSConfigAliases{}
	}

	merged := TSConfigAliases{}
	if strings.TrimSpace(parsed.Extends) != "" {
		parentPath := resolveTSConfigExtendsPath(repoRoot, filepath.Dir(configPath), parsed.Extends)
		if parentPath != "" {
			merged = readTSConfigFile(repoRoot, parentPath, visited)
		}
	}

	configDir := filepath.ToSlash(filepath.Dir(configPath))
	if configDir == "." {
		configDir = ""
	}
	if strings.TrimSpace(parsed.CompilerOptions.BaseURL) != "" {
		merged.BaseURL = repoJoinSlash(configDir, parsed.CompilerOptions.BaseURL)
	}
	if len(parsed.CompilerOptions.Paths) > 0 {
		if merged.Paths == nil {
			merged.Paths = map[string][]string{}
		}
		for pattern, targets := range parsed.CompilerOptions.Paths {
			resolvedTargets := make([]string, 0, len(targets))
			for _, target := range targets {
				resolvedTargets = append(resolvedTargets, repoJoinSlash(configDir, target))
			}
			merged.Paths[pattern] = resolvedTargets
		}
	}
	return merged
}

func resolveTSConfigExtendsPath(repoRoot, configDir, extends string) string {
	value := strings.TrimSpace(extends)
	if value == "" {
		return ""
	}
	if !strings.HasSuffix(value, ".json") {
		value += ".json"
	}
	if strings.HasPrefix(value, ".") {
		return filepath.ToSlash(filepath.Clean(filepath.Join(configDir, value)))
	}
	candidate := filepath.ToSlash(filepath.Clean(value))
	if fileExists(filepath.Join(repoRoot, filepath.FromSlash(candidate))) {
		return candidate
	}
	return ""
}

func resolveTSAliasBases(spec string, aliases TSConfigAliases) []string {
	if len(aliases.Paths) == 0 {
		return nil
	}
	out := make([]string, 0, 4)
	for pattern, targets := range aliases.Paths {
		remainder, ok := matchTSAlias(pattern, spec)
		if !ok {
			continue
		}
		for _, target := range targets {
			resolved := strings.Replace(target, "*", remainder, 1)
			resolved = repoJoinSlash("", resolved)
			out = append(out, resolved)
		}
	}
	return uniqueStrings(out, 0)
}

func matchTSAlias(pattern, spec string) (string, bool) {
	if strings.Contains(pattern, "*") {
		parts := strings.SplitN(pattern, "*", 2)
		if strings.HasPrefix(spec, parts[0]) && strings.HasSuffix(spec, parts[1]) {
			return strings.TrimSuffix(strings.TrimPrefix(spec, parts[0]), parts[1]), true
		}
		return "", false
	}
	return "", pattern == spec
}

func resolvePyImportCandidates(repoRoot, sourceDir, spec string) []string {
	spec = strings.TrimSpace(spec)
	candidates := make([]string, 0, 8)
	if strings.HasPrefix(spec, ".") {
		trimmed := strings.TrimLeft(spec, ".")
		parts := []string{}
		if trimmed != "" {
			parts = strings.Split(trimmed, ".")
		}
		up := len(spec) - len(trimmed)
		baseDir := sourceDir
		for i := 1; i < up; i++ {
			baseDir = filepath.ToSlash(filepath.Dir(baseDir))
		}
		candidateBase := baseDir
		if len(parts) > 0 {
			candidateBase = filepath.ToSlash(filepath.Join(baseDir, filepath.Join(parts...)))
		}
		candidates = append(candidates, candidateBase+".py", filepath.ToSlash(filepath.Join(candidateBase, "__init__.py")))
		return uniqueStrings(candidates, 0)
	}

	dotted := strings.ReplaceAll(spec, ".", "/")
	for _, root := range pythonImportRoots(repoRoot, sourceDir) {
		base := repoJoinSlash(root, dotted)
		candidates = append(candidates, base+".py", filepath.ToSlash(filepath.Join(base, "__init__.py")))
	}
	return uniqueStrings(candidates, 0)
}

func pythonImportRoots(repoRoot, sourceDir string) []string {
	roots := []string{""}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(sourceDir)), "/")
	prefix := ""
	for i, part := range parts {
		if part == "." || part == "" {
			continue
		}
		if prefix == "" {
			prefix = part
		} else {
			prefix = filepath.ToSlash(filepath.Join(prefix, part))
		}
		if part == "src" {
			roots = append(roots, prefix)
		}
		if i == 0 && part == "src" {
			roots = append(roots, "src")
		}
	}
	if dirExists(filepath.Join(repoRoot, "src")) {
		roots = append(roots, "src")
	}
	return uniqueStrings(roots, 0)
}

func repoJoinSlash(base, value string) string {
	if strings.TrimSpace(base) == "" {
		return filepath.ToSlash(filepath.Clean(value))
	}
	return filepath.ToSlash(filepath.Clean(filepath.Join(base, value)))
}

func readGoModulePath(repoRoot string) string {
	content, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func resolveJSImportCandidates(repoRoot, sourceDir, spec string, aliases TSConfigAliases) []string {
	bases := make([]string, 0, 4)
	if strings.HasPrefix(spec, ".") {
		bases = append(bases, filepath.ToSlash(filepath.Clean(filepath.Join(sourceDir, spec))))
	} else {
		bases = append(bases, resolveTSAliasBases(spec, aliases)...)
		if aliases.BaseURL != "" {
			bases = append(bases, filepath.ToSlash(filepath.Clean(filepath.Join(aliases.BaseURL, spec))))
		}
	}

	candidates := make([]string, 0, len(bases)*9)
	for _, base := range uniqueStrings(bases, 0) {
		candidates = append(candidates,
			base,
			base+".js",
			base+".jsx",
			base+".ts",
			base+".tsx",
			base+"/index.js",
			base+"/index.jsx",
			base+"/index.ts",
			base+"/index.tsx",
		)
	}
	_ = repoRoot
	return uniqueStrings(candidates, 0)
}

func isJSImportFile(path string) bool {
	switch filepath.Ext(path) {
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts":
		return true
	}
	return false
}

func isPythonFile(path string) bool {
	return strings.HasSuffix(path, ".py")
}

// Resolver turns an import spec into a repo-relative path. It exists to be
// reused across a whole index run: reading go.mod and walking up for the
// nearest tsconfig.json are per-directory answers, and doing them per import
// in a large TypeScript repo dominates the index time. Not safe for
// concurrent use; the indexer is single-threaded by design so that a repo's
// sqlite file only ever has one writer.
type Resolver struct {
	repoRoot  string
	goModule  string
	knownPath map[string]struct{}
	knownDir  map[string]struct{}
	tsCache   map[string]TSConfigAliases
}

// NewResolver takes the repo's file list so resolution can check whether a
// candidate path actually exists without touching the filesystem.
func NewResolver(repoRoot string, paths []string) *Resolver {
	known := make(map[string]struct{}, len(paths))
	dirs := make(map[string]struct{})
	for _, path := range paths {
		known[path] = struct{}{}
		// Root-level files keep "." as their directory rather than "", so a
		// resolved package directory is never confused with "not found".
		dirs[filepath.ToSlash(filepath.Dir(path))] = struct{}{}
	}
	return &Resolver{
		repoRoot:  repoRoot,
		goModule:  readGoModulePath(repoRoot),
		knownPath: known,
		knownDir:  dirs,
		tsCache:   map[string]TSConfigAliases{},
	}
}

func (r *Resolver) GoModulePath() string { return r.goModule }

// ResolveGo maps a Go import path to the in-repo package directory it names,
// or "" when the import is stdlib or a third-party module. The result is a
// directory, not a file, because a Go import addresses a package: callers
// expand it to that directory's files.
func (r *Resolver) ResolveGo(spec string) string {
	if r.goModule == "" || spec == "" {
		return ""
	}
	if spec == r.goModule {
		if _, ok := r.knownDir["."]; ok {
			return "."
		}
		return ""
	}
	prefix := r.goModule + "/"
	if !strings.HasPrefix(spec, prefix) {
		return ""
	}
	dir := strings.TrimPrefix(spec, prefix)
	if _, ok := r.knownDir[dir]; !ok {
		return ""
	}
	return dir
}

// ResolveJS maps a JS/TS import spec to a repo path, honoring relative paths,
// tsconfig baseUrl and tsconfig paths aliases. Returns "" for a package
// import (react, @scope/pkg) or an unresolvable alias.
func (r *Resolver) ResolveJS(sourcePath, spec string) string {
	if !isJSImportFile(sourcePath) || spec == "" {
		return ""
	}
	sourceDir := filepath.ToSlash(filepath.Dir(sourcePath))
	aliases := r.tsAliases(sourceDir, sourcePath)
	for _, candidate := range resolveJSImportCandidates(r.repoRoot, sourceDir, spec, aliases) {
		if _, ok := r.knownPath[candidate]; ok {
			return candidate
		}
	}
	return ""
}

// ResolvePy maps a Python import spec to a repo path, handling both relative
// (from .thing import x) and absolute dotted module paths.
func (r *Resolver) ResolvePy(sourcePath, spec string) string {
	if !isPythonFile(sourcePath) || spec == "" {
		return ""
	}
	sourceDir := filepath.ToSlash(filepath.Dir(sourcePath))
	for _, candidate := range resolvePyImportCandidates(r.repoRoot, sourceDir, spec) {
		if _, ok := r.knownPath[candidate]; ok {
			return candidate
		}
	}
	return ""
}

func (r *Resolver) tsAliases(sourceDir, sourcePath string) TSConfigAliases {
	if cached, ok := r.tsCache[sourceDir]; ok {
		return cached
	}
	aliases := tsAliasesFor(r.repoRoot, sourcePath)
	r.tsCache[sourceDir] = aliases
	return aliases
}
