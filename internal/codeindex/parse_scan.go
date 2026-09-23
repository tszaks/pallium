package codeindex

import (
	"regexp"
	"strings"

	"github.com/tszaks/pallium/internal/db"
)

// Line scanners for the languages Go's standard library cannot parse.
//
// These are not ASTs and do not pretend to be. They are anchored to the start
// of a line, which is what makes them usable rather than noise: a declaration
// keyword at column zero (or one indent level, for a method) is a declaration
// in every language here, while the same keyword mid-expression is not. The
// tradeoff is deliberate. Pallium is a single static binary with no cgo, so a
// real grammar would mean either linking tree-sitter (cgo, breaks the npm
// install and cross-compilation) or shipping WASM grammars and a runtime.
// Anchored scanning gets the declarations right and misses exotic forms;
// a wrong symbol is worse than a missing one, so every pattern below is
// written to under-claim.

type scanRule struct {
	kind    string
	pattern *regexp.Regexp
}

var (
	jsRules = []scanRule{
		{"func", regexp.MustCompile(`(?m)^\s{0,2}(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)},
		{"class", regexp.MustCompile(`(?m)^\s{0,2}(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)},
		{"interface", regexp.MustCompile(`(?m)^\s{0,2}(?:export\s+)?interface\s+([A-Za-z_$][\w$]*)`)},
		{"type", regexp.MustCompile(`(?m)^\s{0,2}(?:export\s+)?type\s+([A-Za-z_$][\w$]*)`)},
		{"enum", regexp.MustCompile(`(?m)^\s{0,2}(?:export\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)`)},
		{"const", regexp.MustCompile(`(?m)^\s{0,2}(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*[:=]`)},
	}
	pyRules = []scanRule{
		{"func", regexp.MustCompile(`(?m)^(?:async\s+)?def\s+([A-Za-z_][\w]*)`)},
		{"method", regexp.MustCompile(`(?m)^\s{1,8}(?:async\s+)?def\s+([A-Za-z_][\w]*)`)},
		{"class", regexp.MustCompile(`(?m)^class\s+([A-Za-z_][\w]*)`)},
		{"const", regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]{2,})\s*[:=]`)},
	}
	swiftRules = []scanRule{
		{"func", regexp.MustCompile(`(?m)^\s{0,4}(?:@\w+\s+)*(?:public\s+|private\s+|internal\s+|fileprivate\s+|open\s+|static\s+|class\s+|override\s+|final\s+|mutating\s+|nonisolated\s+)*func\s+([A-Za-z_][\w]*)`)},
		{"class", regexp.MustCompile(`(?m)^\s{0,2}(?:@\w+\s+)*(?:public\s+|private\s+|internal\s+|fileprivate\s+|open\s+|final\s+)*class\s+([A-Za-z_][\w]*)`)},
		{"struct", regexp.MustCompile(`(?m)^\s{0,2}(?:@\w+\s+)*(?:public\s+|private\s+|internal\s+|fileprivate\s+|frozen\s+)*struct\s+([A-Za-z_][\w]*)`)},
		{"enum", regexp.MustCompile(`(?m)^\s{0,2}(?:@\w+\s+)*(?:public\s+|private\s+|internal\s+|fileprivate\s+|indirect\s+)*enum\s+([A-Za-z_][\w]*)`)},
		{"interface", regexp.MustCompile(`(?m)^\s{0,2}(?:public\s+|private\s+|internal\s+)*protocol\s+([A-Za-z_][\w]*)`)},
		{"extension", regexp.MustCompile(`(?m)^\s{0,2}(?:public\s+|private\s+|internal\s+)*extension\s+([A-Za-z_][\w.]*)`)},
		{"actor", regexp.MustCompile(`(?m)^\s{0,2}(?:public\s+|private\s+|internal\s+|final\s+)*actor\s+([A-Za-z_][\w]*)`)},
		{"var", regexp.MustCompile(`(?m)^\s{0,4}(?:@\w+(?:\([^)]*\))?\s+)*(?:public\s+|private\s+|internal\s+|fileprivate\s+|static\s+|lazy\s+)*(?:var|let)\s+([A-Za-z_][\w]*)\s*[:=]`)},
	}
	rustRules = []scanRule{
		{"func", regexp.MustCompile(`(?m)^\s{0,4}(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+([A-Za-z_][\w]*)`)},
		{"struct", regexp.MustCompile(`(?m)^\s{0,2}(?:pub(?:\([^)]*\))?\s+)?struct\s+([A-Za-z_][\w]*)`)},
		{"enum", regexp.MustCompile(`(?m)^\s{0,2}(?:pub(?:\([^)]*\))?\s+)?enum\s+([A-Za-z_][\w]*)`)},
		{"interface", regexp.MustCompile(`(?m)^\s{0,2}(?:pub(?:\([^)]*\))?\s+)?trait\s+([A-Za-z_][\w]*)`)},
		{"type", regexp.MustCompile(`(?m)^\s{0,2}(?:pub(?:\([^)]*\))?\s+)?type\s+([A-Za-z_][\w]*)`)},
		{"impl", regexp.MustCompile(`(?m)^\s{0,2}impl(?:<[^>]*>)?\s+(?:[A-Za-z_][\w:<>, ]*\s+for\s+)?([A-Za-z_][\w]*)`)},
	}
	rubyRules = []scanRule{
		{"func", regexp.MustCompile(`(?m)^\s{0,6}def\s+(?:self\.)?([A-Za-z_][\w]*[?!=]?)`)},
		{"class", regexp.MustCompile(`(?m)^\s{0,4}class\s+([A-Z][\w:]*)`)},
		{"module", regexp.MustCompile(`(?m)^\s{0,4}module\s+([A-Z][\w:]*)`)},
	}
	jvmRules = []scanRule{
		{"class", regexp.MustCompile(`(?m)^\s{0,4}(?:public\s+|private\s+|protected\s+|internal\s+|abstract\s+|final\s+|open\s+|sealed\s+|data\s+|static\s+)*class\s+([A-Za-z_][\w]*)`)},
		{"interface", regexp.MustCompile(`(?m)^\s{0,4}(?:public\s+|private\s+|protected\s+|internal\s+|sealed\s+)*interface\s+([A-Za-z_][\w]*)`)},
		{"enum", regexp.MustCompile(`(?m)^\s{0,4}(?:public\s+|private\s+|protected\s+|internal\s+)*enum(?:\s+class)?\s+([A-Za-z_][\w]*)`)},
		{"struct", regexp.MustCompile(`(?m)^\s{0,4}(?:public\s+|private\s+|internal\s+)*(?:object|record)\s+([A-Za-z_][\w]*)`)},
		{"func", regexp.MustCompile(`(?m)^\s{0,8}(?:public\s+|private\s+|protected\s+|internal\s+|static\s+|final\s+|abstract\s+|override\s+|suspend\s+|open\s+|synchronized\s+)*fun\s+([A-Za-z_][\w]*)`)},
		{"method", regexp.MustCompile(`(?m)^\s{1,8}(?:public|private|protected)\s+(?:static\s+|final\s+|abstract\s+|synchronized\s+)*[\w<>\[\].]+\s+([A-Za-z_][\w]*)\s*\(`)},
	}
	csharpRules = []scanRule{
		{"class", regexp.MustCompile(`(?m)^\s{0,8}(?:public\s+|private\s+|protected\s+|internal\s+|abstract\s+|sealed\s+|static\s+|partial\s+)*class\s+([A-Za-z_][\w]*)`)},
		{"interface", regexp.MustCompile(`(?m)^\s{0,8}(?:public\s+|private\s+|protected\s+|internal\s+)*interface\s+([A-Za-z_][\w]*)`)},
		{"struct", regexp.MustCompile(`(?m)^\s{0,8}(?:public\s+|private\s+|internal\s+|readonly\s+)*(?:struct|record)\s+([A-Za-z_][\w]*)`)},
		{"enum", regexp.MustCompile(`(?m)^\s{0,8}(?:public\s+|private\s+|internal\s+)*enum\s+([A-Za-z_][\w]*)`)},
	}
	phpRules = []scanRule{
		{"func", regexp.MustCompile(`(?m)^\s{0,8}(?:public\s+|private\s+|protected\s+|static\s+|final\s+|abstract\s+)*function\s+([A-Za-z_][\w]*)`)},
		{"class", regexp.MustCompile(`(?m)^\s{0,4}(?:final\s+|abstract\s+)*class\s+([A-Za-z_][\w]*)`)},
		{"interface", regexp.MustCompile(`(?m)^\s{0,4}interface\s+([A-Za-z_][\w]*)`)},
		{"trait", regexp.MustCompile(`(?m)^\s{0,4}trait\s+([A-Za-z_][\w]*)`)},
	}
	cRules = []scanRule{
		{"struct", regexp.MustCompile(`(?m)^\s{0,2}(?:typedef\s+)?struct\s+([A-Za-z_][\w]*)`)},
		{"enum", regexp.MustCompile(`(?m)^\s{0,2}(?:typedef\s+)?enum\s+([A-Za-z_][\w]*)`)},
		{"class", regexp.MustCompile(`(?m)^\s{0,2}(?:class|@interface|@implementation)\s+([A-Za-z_][\w]*)`)},
		{"func", regexp.MustCompile(`(?m)^[A-Za-z_][\w \t*]*\s\*?([A-Za-z_][\w]*)\s*\([^;]*\)\s*\{`)},
	}

	// callRefRegex finds call-shaped identifiers. This is the same shape the
	// old go-symbol heuristic used, kept so the stem-matching link kinds
	// behave identically once they read from the index instead of the disk.
	callRefRegex = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\s*\(`)

	jsImportSpecRegex = regexp.MustCompile(`(?m)(?:import|export)[^'";]*from\s+['"]([^'"]+)['"]|require\(\s*['"]([^'"]+)['"]\s*\)|import\(\s*['"]([^'"]+)['"]\s*\)|^\s*import\s+['"]([^'"]+)['"]`)
	pyImportSpecRegex = regexp.MustCompile(`(?m)^\s*(?:from\s+([.\w]+)\s+import|import\s+([.\w]+))`)
	swiftImportRegex  = regexp.MustCompile(`(?m)^\s*import\s+([A-Za-z_][\w.]*)`)
)

func rulesFor(lang string) []scanRule {
	switch lang {
	case "typescript", "tsx", "javascript", "jsx":
		return jsRules
	case "python":
		return pyRules
	case "swift":
		return swiftRules
	case "rust":
		return rustRules
	case "ruby":
		return rubyRules
	case "java", "kotlin":
		return jvmRules
	case "csharp":
		return csharpRules
	case "php":
		return phpRules
	case "c", "cpp", "objc":
		return cRules
	default:
		return nil
	}
}

func parseScan(path, lang string, content []byte, resolver *Resolver) (ParsedFile, bool) {
	rules := rulesFor(lang)
	if rules == nil {
		return ParsedFile{}, false
	}

	original := string(content)
	text := original
	importsText := original
	if lang != "python" && lang != "ruby" {
		text = maskSource(original, true)
		importsText = maskSource(original, false)
	}
	offsets := newLineOffsets(text)
	parsed := ParsedFile{Lang: lang, Parser: "scan"}

	declared := make(map[string]struct{})
	seenSymbol := make(map[string]struct{})
	for _, rule := range rules {
		for _, match := range rule.pattern.FindAllStringSubmatchIndex(text, -1) {
			if len(match) < 4 || match[2] < 0 {
				continue
			}
			name := text[match[2]:match[3]]
			if name == "" {
				continue
			}
			key := rule.kind + "::" + name
			if _, ok := seenSymbol[key]; ok {
				continue
			}
			seenSymbol[key] = struct{}{}
			declared[name] = struct{}{}
			startLine := offsets.line(match[2])
			parsed.Symbols = append(parsed.Symbols, db.CodeSymbol{
				Path:      path,
				Name:      name,
				Kind:      rule.kind,
				Signature: strings.TrimSpace(offsets.lineText(original, match[2])),
				Doc:       precedingComment(original, offsets, match[2]),
				StartLine: startLine,
				EndLine:   scanEnd(text, match[2], offsets),
				Exported:  scanExported(lang, original, match[2], name),
			})
		}
	}

	// Class methods are scoped by the enclosing class span. Do not turn
	// arbitrary call-shaped statements into declarations.
	if lang == "typescript" || lang == "tsx" || lang == "javascript" || lang == "jsx" {
		classes := append([]db.CodeSymbol(nil), parsed.Symbols...)
		method := regexp.MustCompile(`(?m)^[ \t]+(?:(?:public|private|protected|static|async|override|readonly)\s+)*([A-Za-z_$][\w$]*)\s*\([^;{}]*\)\s*(?::[^;{\n]+)?\s*\{`)
		for _, m := range method.FindAllStringSubmatchIndex(text, -1) {
			name := text[m[2]:m[3]]
			if isLanguageKeyword(name) {
				continue
			}
			line := offsets.line(m[2])
			for _, c := range classes {
				if c.Kind != "class" || line <= c.StartLine || line >= c.EndLine {
					continue
				}
				// At direct class-body depth only; nested calls and local functions are excluded.
				start := offsets.starts[c.StartLine-1]
				depth := 0
				for _, ch := range text[start:m[2]] {
					if ch == '{' {
						depth++
					}
					if ch == '}' {
						depth--
					}
				}
				if depth != 1 {
					continue
				}
				parsed.Symbols = append(parsed.Symbols, db.CodeSymbol{Path: path, Name: name, Kind: "method", Receiver: c.Name, Signature: strings.TrimSpace(offsets.lineText(original, m[2])), StartLine: line, EndLine: scanEnd(text, m[2], offsets), Exported: !strings.Contains(offsets.lineText(original, m[2]), "private")})
				declared[name] = struct{}{}
				break
			}
		}
	}

	parsed.Imports = scanImports(path, lang, importsText, resolver)
	parsed.Refs = scanRefs(path, text, declared)
	return parsed, true
}

func scanImports(path, lang, text string, resolver *Resolver) []db.CodeImport {
	out := make([]db.CodeImport, 0)
	seen := make(map[string]struct{})
	add := func(spec, kind, target string) {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return
		}
		if _, ok := seen[spec]; ok {
			return
		}
		seen[spec] = struct{}{}
		out = append(out, db.CodeImport{
			FromPath: path,
			RawSpec:  spec,
			ToPath:   target,
			Kind:     kind,
			External: target == "" && !isLocalScanImport(kind, spec) &&
				!(kind == "js-import" && resolver.MatchesJSAlias(path, spec)),
		})
	}

	switch lang {
	case "typescript", "tsx", "javascript", "jsx":
		for _, match := range jsImportSpecRegex.FindAllStringSubmatch(text, -1) {
			spec := firstNonEmpty(match[1:])
			add(spec, "js-import", resolver.ResolveJS(path, spec))
		}
	case "python":
		for _, match := range pyImportSpecRegex.FindAllStringSubmatch(text, -1) {
			spec := firstNonEmpty(match[1:])
			add(spec, "py-import", resolver.ResolvePy(path, spec))
		}
	case "swift":
		// Swift imports name modules, not files, so nothing in a single-repo
		// checkout resolves to a path. Recording them anyway is still worth
		// it: "which files import SwiftUI" is a real question.
		for _, match := range swiftImportRegex.FindAllStringSubmatch(text, -1) {
			add(match[1], "swift-import", "")
		}
	}
	return out
}

func isLocalScanImport(kind, spec string) bool {
	switch kind {
	case "js-import":
		return strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/")
	case "py-import":
		return strings.HasPrefix(spec, ".")
	default:
		return false
	}
}

func scanRefs(path, text string, declared map[string]struct{}) []db.CodeRef {
	seen := make(map[string]struct{})
	refs := make([]db.CodeRef, 0)
	for _, match := range callRefRegex.FindAllStringSubmatch(text, -1) {
		name := match[1]
		if name == "" || isLanguageKeyword(name) {
			continue
		}
		if _, ok := declared[name]; ok {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		refs = append(refs, db.CodeRef{Path: path, Name: name})
	}
	return refs
}

// isLanguageKeyword drops control-flow words that the call-shaped regex
// matches ("if (", "return (", "catch ("). Without this every file in the
// repo would claim to reference "if", and the callers query would be useless.
func isLanguageKeyword(name string) bool {
	switch name {
	case "if", "for", "while", "switch", "catch", "return", "function", "typeof",
		"await", "new", "delete", "in", "of", "do", "else", "case", "with",
		"print", "assert", "raise", "yield", "and", "or", "not", "guard",
		"defer", "go", "range", "select", "throw", "throws", "try", "sizeof",
		"require", "import", "export", "class", "struct", "enum", "init",
		"super", "self", "this", "elif", "except", "finally", "lambda", "when":
		return true
	}
	return false
}

// scanExported decides whether a declaration is part of a file's public
// surface. Every language here spells that differently, and guessing wrong in
// the permissive direction is the cheaper mistake: an over-broad "exported"
// flag shows an extra symbol in a listing, while an over-narrow one hides one.
func scanExported(lang, text string, offset int, name string) bool {
	line := text[offset:]
	if index := strings.IndexByte(line, '\n'); index >= 0 {
		line = line[:index]
	}
	switch lang {
	case "typescript", "tsx", "javascript", "jsx":
		return strings.Contains(line, "export")
	case "python":
		return !strings.HasPrefix(name, "_")
	case "swift":
		return !strings.Contains(line, "private") && !strings.Contains(line, "fileprivate")
	case "rust":
		return strings.Contains(line, "pub")
	case "java", "kotlin", "csharp", "php":
		return !strings.Contains(line, "private") && !strings.Contains(line, "protected")
	default:
		return true
	}
}

// precedingComment picks up the comment block immediately above a declaration.
// It is the closest a scanner gets to a doc comment, and it is what makes
// symbol search over `doc` find anything at all in a TypeScript repo.
func precedingComment(text string, offsets lineOffsets, offset int) string {
	line := offsets.line(offset)
	collected := make([]string, 0, 3)
	for cursor := line - 1; cursor >= 1 && len(collected) < 3; cursor-- {
		content := strings.TrimSpace(offsets.textAtLine(text, cursor))
		switch {
		case strings.HasPrefix(content, "*/"), content == "/**", content == "/*":
			continue
		case strings.HasPrefix(content, "*"):
			collected = append(collected, strings.TrimSpace(strings.TrimPrefix(content, "*")))
		case strings.HasPrefix(content, "//"):
			collected = append(collected, strings.TrimSpace(strings.TrimPrefix(content, "//")))
		case strings.HasPrefix(content, "#"):
			collected = append(collected, strings.TrimSpace(strings.TrimPrefix(content, "#")))
		case strings.HasPrefix(content, "///"):
			collected = append(collected, strings.TrimSpace(strings.TrimPrefix(content, "///")))
		default:
			cursor = 0
		}
	}
	for left, right := 0, len(collected)-1; left < right; left, right = left+1, right-1 {
		collected[left], collected[right] = collected[right], collected[left]
	}
	return strings.TrimSpace(strings.Join(collected, " "))
}

type lineOffsets struct {
	starts []int
}

func newLineOffsets(text string) lineOffsets {
	starts := []int{0}
	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			starts = append(starts, index+1)
		}
	}
	return lineOffsets{starts: starts}
}

func (l lineOffsets) line(offset int) int {
	low, high := 0, len(l.starts)-1
	for low < high {
		mid := (low + high + 1) / 2
		if l.starts[mid] <= offset {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return low + 1
}

func (l lineOffsets) lineText(text string, offset int) string {
	return l.textAtLine(text, l.line(offset))
}

func (l lineOffsets) textAtLine(text string, line int) string {
	if line < 1 || line > len(l.starts) {
		return ""
	}
	start := l.starts[line-1]
	end := len(text)
	if line < len(l.starts) {
		end = l.starts[line] - 1
	}
	if start > end || end > len(text) {
		return ""
	}
	return text[start:end]
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
