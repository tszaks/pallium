package workflow

import (
	"encoding/json"
	"regexp"
	"testing"
)

// maliciousTask exercises the characters that would break naive string
// concatenation into a JS template literal: a double quote to close an
// enclosing string early, a backtick, a newline, and a semicolon to
// terminate a statement.
const maliciousTask = "task\" ; alert(`pwned`);\n// injected"

func TestGenerateScriptEscapesTaskAcrossStyles(t *testing.T) {
	for _, style := range []string{"review", "research", "test-fix"} {
		t.Run(style, func(t *testing.T) {
			script, err := GenerateScript(GenerateOptions{Task: maliciousTask, Style: style, TestCommand: "go test ./..."})
			if err != nil {
				t.Fatalf("GenerateScript(%q) returned error: %v", style, err)
			}
			if result := ValidateScript(script); !result.Valid {
				t.Fatalf("generated %s script is not valid JS: %s\nscript:\n%s", style, result.Error, script)
			}
			gotTask := extractConstLiteral(t, script, "task")
			if gotTask != maliciousTask {
				t.Fatalf("task round-trip mismatch: got %q, want %q", gotTask, maliciousTask)
			}
		})
	}
}

func TestDefaultScriptEscapesTask(t *testing.T) {
	script := DefaultScript(maliciousTask)
	if result := ValidateScript(script); !result.Valid {
		t.Fatalf("DefaultScript output is not valid JS: %s\nscript:\n%s", result.Error, script)
	}
}

// extractConstLiteral pulls the JSON string literal out of a generated
// `const <name> = "...";` declaration and decodes it, proving the value
// survives the round trip through the generated script unmangled.
func extractConstLiteral(t *testing.T, script, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)const ` + name + ` = ("(?:[^"\\]|\\.)*");`)
	match := re.FindStringSubmatch(script)
	if match == nil {
		t.Fatalf("could not find const %s literal in script:\n%s", name, script)
	}
	var value string
	if err := json.Unmarshal([]byte(match[1]), &value); err != nil {
		t.Fatalf("const %s literal is not valid JSON: %v", name, err)
	}
	return value
}
