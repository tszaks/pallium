package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// allowedProviderExecFiles are the only files permitted to name a codex or
// claude binary in a process invocation: the single codex exec site
// (codex_provider.go) and the single built-in claude exec site
// (claude_provider.go). The configured-wrapper site
// (runtime.go's runConfiguredProviderCommand) runs a generic `sh -c <command>`
// and never names codex/claude in source, so runtime.go is deliberately NOT
// allowlisted — reintroducing a raw codex/claude exec there must fail this
// test. Tyler's ruling: any model can power a Pallium worker, codex is never a
// special case, so there is exactly ONE place that invokes each built-in
// binary, never scattered inline elsewhere.
var allowedProviderExecFiles = map[string]bool{
	"codex_provider.go":  true,
	"claude_provider.go": true,
}

// providerBinaryIndicatorRe matches an exec.Command/exec.CommandContext
// call's argument text that names a codex or claude binary, whether via a Go
// identifier (r.CodexBinary, claudeCLIName, codexBinary) or a bare string
// literal ("codex", "claude").
var providerBinaryIndicatorRe = regexp.MustCompile(`(?i)codex|claude`)

// TestNoHardcodedProviderExecOutsideProviderFiles walks every non-test .go
// file in the module and fails if a codex or claude process invocation shows
// up anywhere other than the dedicated provider files. This is the tripwire
// for a regression back to scattered, provider-specific exec calls.
func TestNoHardcodedProviderExecOutsideProviderFiles(t *testing.T) {
	root := moduleRoot(t)
	var violations []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		for _, call := range findExecCalls(string(raw)) {
			if !providerBinaryIndicatorRe.MatchString(call) {
				continue
			}
			if allowedProviderExecFiles[base] {
				continue
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			violations = append(violations, fmt.Sprintf("%s: %s", rel, strings.TrimSpace(call)))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("codex/claude process invocation(s) found outside the dedicated provider files (codex_provider.go, claude_provider.go, runtime.go):\n%s", strings.Join(violations, "\n"))
	}
}

// moduleRoot locates the repo root from this test file's own path via
// runtime.Caller, so the walk works regardless of the working directory a
// test runner invokes `go test` from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve test file path")
	}
	// This file lives at <root>/internal/workflow/no_hardcoded_provider_test.go.
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// findExecCalls returns the full text (function name through the matching
// closing paren) of every exec.Command(...)/exec.CommandContext(...) call in
// src, so callers inspect actual call arguments rather than risk a false
// match against an unrelated comment on the same or a nearby line.
func findExecCalls(src string) []string {
	var calls []string
	for _, name := range []string{"exec.Command(", "exec.CommandContext("} {
		searchFrom := 0
		for {
			idx := strings.Index(src[searchFrom:], name)
			if idx < 0 {
				break
			}
			absStart := searchFrom + idx
			openParen := absStart + len(name) - 1
			end, ok := scanBalancedParen(src, openParen)
			if !ok {
				searchFrom = absStart + len(name)
				continue
			}
			calls = append(calls, src[absStart:end+1])
			searchFrom = end + 1
		}
	}
	return calls
}

// scanBalancedParen returns the index of the ')' that closes the '(' at
// start, honoring quoted string/raw-string literals so a paren inside a
// quoted argument can't unbalance the scan.
func scanBalancedParen(s string, start int) (int, bool) {
	depth := 0
	inString := false
	var quote byte
	escape := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			switch {
			case escape:
				escape = false
			case ch == '\\' && quote == '"':
				escape = true
			case ch == quote:
				inString = false
			}
			continue
		}
		switch ch {
		case '"', '`':
			inString = true
			quote = ch
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}
