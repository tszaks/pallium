package workflow

import (
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

type ValidationResult struct {
	Valid bool   `json:"valid"`
	Error string `json:"error,omitempty"`
	// Warnings are non-fatal: the script compiles and Valid stays true, but
	// something in it is very likely to misbehave at run time in a way
	// validate cannot prove (that gap is exactly what motivated this field —
	// see detectStageChainWarnings). A caller that wants a hard gate should
	// still check Valid; Warnings is advisory only.
	Warnings []string `json:"warnings,omitempty"`
}

func ValidateScript(script string) ValidationResult {
	body := strings.TrimSpace(stripMeta(script))
	if body == "" {
		return ValidationResult{Valid: false, Error: "workflow script is empty"}
	}
	_, err := goja.Compile("workflow.js", "(async function(){\n"+body+"\n})", false)
	if err != nil {
		return ValidationResult{Valid: false, Error: fmt.Sprintf("%v", err)}
	}
	return ValidationResult{Valid: true, Warnings: detectStageChainWarnings(body)}
}

// detectStageChainWarnings closes the exact validate-vs-run gap an outside
// audit flagged (2026-07-12): ValidateScript only ever checked JS syntax, so
// a script chaining .then()/.catch() straight onto agent()/check() — the
// single most common way a stage silently misbehaves, per stageChainHint in
// runtime.go — compiled clean and only blew up during `workflow run`. This
// is a plain source scan, not real JS parsing, so it only fires on the exact
// literal shape `agent(...).then(` / `check(...).then(` (balanced parens,
// string-literal-aware) — narrow on purpose: false negatives on cleverly
// reformatted code are acceptable for an advisory warning, false positives
// on unrelated code are not.
func detectStageChainWarnings(body string) []string {
	var warnings []string
	for _, fn := range []string{"agent", "check"} {
		searchFrom := 0
		for {
			rel := strings.Index(body[searchFrom:], fn+"(")
			if rel < 0 {
				break
			}
			start := searchFrom + rel
			if start > 0 && isStageChainIdentChar(body[start-1]) {
				searchFrom = start + len(fn)
				continue
			}
			openParen := start + len(fn)
			closeParen := matchingParenIndex(body, openParen)
			if closeParen < 0 {
				break
			}
			rest := strings.TrimLeft(body[closeParen+1:], " \t\n\r")
			if strings.HasPrefix(rest, ".then(") || strings.HasPrefix(rest, ".catch(") {
				warnings = append(warnings, fmt.Sprintf(
					"%s(...) at byte offset %d is chained with .then()/.catch(): this breaks at run time (%s() returns a capture marker, not a real promise) even though it compiles clean — return %s() directly and post-process its result after the pipeline/parallel stage",
					fn, start, fn, fn))
			}
			searchFrom = closeParen + 1
		}
	}
	return warnings
}

func isStageChainIdentChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// matchingParenIndex returns the index of the ) matching the ( at openIdx,
// skipping over parens inside string/template literals. Returns -1 if
// openIdx isn't a ( or no match is found (e.g. genuinely unbalanced source,
// which goja.Compile above would already have rejected before this runs).
func matchingParenIndex(s string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(s) || s[openIdx] != '(' {
		return -1
	}
	depth := 0
	var inString byte
	escaped := false
	for i := openIdx; i < len(s); i++ {
		c := s[i]
		if inString != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == inString:
				inString = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inString = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
