package analysis

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/db"
)

type StructuralLink struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

var goSymbolRegex = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
var goImportRegex = regexp.MustCompile(`(?m)^\s*(?:"([^"]+)"|import\s+"([^"]+)")`)
var jsImportRegex = regexp.MustCompile(`(?m)(?:import|export)[^'"\n]*from\s+['"]([^'"]+)['"]|require\(\s*['"]([^'"]+)['"]\s*\)`)
var pyImportRegex = regexp.MustCompile(`(?m)^\s*(?:from\s+([.\w]+)\s+import|import\s+([.\w]+))`)
var jsonCommentRegex = regexp.MustCompile(`(?m)//.*$|/\*[\s\S]*?\*/`)

func StructuralLinks(store *db.Store, targetPath string, limit int) ([]StructuralLink, error) {
	normalized, err := normalizeRepoPath(store.RepoRoot, targetPath)
	if err != nil {
		return nil, err
	}

	files, err := repoFiles(store.RepoRoot)
	if err != nil {
		return nil, err
	}
	targetAbs := filepath.Join(store.RepoRoot, filepath.FromSlash(normalized))
	targetContent, _ := osReadFile(targetAbs)
	goModulePath := readGoModulePath(store.RepoRoot)

	out := make([]StructuralLink, 0)
	targetDir := filepath.ToSlash(filepath.Dir(normalized))
	targetName := filepath.Base(normalized)
	targetStem := fileStem(targetName)
	targetIsTest := isTestFile(targetName)

	for _, candidate := range files {
		if candidate == normalized {
			continue
		}

		candidateName := filepath.Base(candidate)
		candidateDir := filepath.ToSlash(filepath.Dir(candidate))
		candidateStem := fileStem(candidateName)
		candidateIsTest := isTestFile(candidateName)
		candidateAbs := filepath.Join(store.RepoRoot, filepath.FromSlash(candidate))
		candidateContent, _ := osReadFile(candidateAbs)

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
		case strings.HasSuffix(targetName, ".go") && strings.HasSuffix(candidateName, ".go") && referencesGoFile(targetContent, candidateStem):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-symbol",
				Reason: "Target file references a symbol that matches this Go file's stem.",
			})
		case referencesGoImport(normalized, targetContent, candidate, goModulePath):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-import",
				Reason: "Target file imports this Go package from the same repo.",
			})
		case strings.HasSuffix(candidateName, ".go") && strings.HasSuffix(targetName, ".go") && referencesGoFile(candidateContent, targetStem):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-dependent",
				Reason: "This Go file appears to reference the target file's symbol stem.",
			})
		case referencesGoImport(candidate, candidateContent, normalized, goModulePath):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-package-dependent",
				Reason: "This Go file imports the target package from the same repo.",
			})
		case referencesJSImport(store.RepoRoot, normalized, targetContent, candidate):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "js-import",
				Reason: "Target file imports this JS/TS module with a relative path.",
			})
		case referencesJSImport(store.RepoRoot, candidate, candidateContent, normalized):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "js-dependent",
				Reason: "This JS/TS file imports the target module with a relative path.",
			})
		case referencesPyImport(store.RepoRoot, normalized, targetContent, candidate):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "py-import",
				Reason: "Target file imports this Python module with a local import path.",
			})
		case referencesPyImport(store.RepoRoot, candidate, candidateContent, normalized):
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

func repoFiles(repoRoot string) ([]string, error) {
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

func referencesGoFile(content []byte, stem string) bool {
	if len(content) == 0 || stem == "" {
		return false
	}
	for _, match := range goSymbolRegex.FindAllStringSubmatch(string(content), -1) {
		if len(match) > 1 && strings.EqualFold(match[1], stem) {
			return true
		}
	}
	return false
}

func referencesGoImport(sourcePath string, content []byte, candidatePath, modulePath string) bool {
	if len(content) == 0 || modulePath == "" || !strings.HasSuffix(sourcePath, ".go") || !strings.HasSuffix(candidatePath, ".go") {
		return false
	}
	candidateDir := filepath.ToSlash(filepath.Dir(candidatePath))
	if candidateDir == "." {
		candidateDir = ""
	}
	for _, match := range goImportRegex.FindAllStringSubmatch(string(content), -1) {
		if len(match) < 3 {
			continue
		}
		importPath := strings.TrimSpace(match[1])
		if importPath == "" {
			importPath = strings.TrimSpace(match[2])
		}
		if importPath == modulePath && candidateDir == "" {
			return true
		}
		if candidateDir != "" && importPath == modulePath+"/"+candidateDir {
			return true
		}
	}
	return false
}

func referencesJSImport(repoRoot, sourcePath string, content []byte, candidatePath string) bool {
	if len(content) == 0 || !isJSImportFile(sourcePath) || !isJSImportFile(candidatePath) {
		return false
	}

	sourceDir := filepath.ToSlash(filepath.Dir(sourcePath))
	aliases := readTSConfigAliases(repoRoot, sourcePath)
	for _, match := range jsImportRegex.FindAllStringSubmatch(string(content), -1) {
		spec := ""
		if len(match) > 1 && match[1] != "" {
			spec = match[1]
		} else if len(match) > 2 {
			spec = match[2]
		}
		for _, resolved := range resolveJSImportCandidates(repoRoot, sourceDir, spec, aliases) {
			if resolved == candidatePath {
				return true
			}
		}
	}
	return false
}

func referencesPyImport(repoRoot, sourcePath string, content []byte, candidatePath string) bool {
	if len(content) == 0 || !isPythonFile(sourcePath) || !isPythonFile(candidatePath) {
		return false
	}
	sourceDir := filepath.ToSlash(filepath.Dir(sourcePath))
	for _, match := range pyImportRegex.FindAllStringSubmatch(string(content), -1) {
		spec := ""
		if len(match) > 1 && match[1] != "" {
			spec = match[1]
		} else if len(match) > 2 {
			spec = match[2]
		}
		if spec == "" {
			continue
		}
		for _, resolved := range resolvePyImportCandidates(repoRoot, sourceDir, spec) {
			if resolved == candidatePath {
				return true
			}
		}
	}
	return false
}

func resolveJSImportCandidates(repoRoot, sourceDir, spec string, aliases tsConfigAliases) []string {
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

func osReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

type tsConfigAliases struct {
	BaseURL string
	Paths   map[string][]string
}

func readTSConfigAliases(repoRoot, sourcePath string) tsConfigAliases {
	configPath := findNearestTSConfig(repoRoot, filepath.Dir(sourcePath))
	if configPath == "" {
		return tsConfigAliases{}
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

func readTSConfigFile(repoRoot, configPath string, visited map[string]struct{}) tsConfigAliases {
	configPath = filepath.ToSlash(filepath.Clean(configPath))
	if _, ok := visited[configPath]; ok {
		return tsConfigAliases{}
	}
	visited[configPath] = struct{}{}

	content, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(configPath)))
	if err != nil {
		return tsConfigAliases{}
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
		return tsConfigAliases{}
	}

	merged := tsConfigAliases{}
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

func resolveTSAliasBases(spec string, aliases tsConfigAliases) []string {
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
