package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// serviceOwnedTables maps a SQLite table name to the ONE file allowed to
// reference it in a query (SELECT/INSERT/UPDATE/DELETE/CREATE — anywhere
// inside a backtick-raw-string SQL statement). This is the "which service
// owns this" test made mechanical, same spirit as
// no_hardcoded_provider_test.go's single-owner tripwire for provider exec
// calls: a service's tables are its own business, touched only from its
// own store file, never reached into from elsewhere.
//
// Scope is deliberately honest, not aspirational: team_* and workflow_loops
// are strictly enforced because they already ARE cleanly isolated (verified
// by inspection before this test was written — store.go has zero
// references to any team_* table). workflow_runs/workflow_agents/
// workflow_phases/workflow_triggers/workflow_decisions/workflow_gates are
// NOT in this map: those are already legitimately split across store.go
// plus their own dedicated files (trigger.go, decision.go, gate.go) from
// before this lint existed, and retroactively re-architecting that is out
// of scope for the PR that added this test. Extend the map as each of
// those gets its own cleanup, not by loosening the rule for new tables.
var serviceOwnedTables = map[string]string{
	"teams":          "team_store.go",
	"team_members":   "team_store.go",
	"team_tasks":     "team_store.go",
	"team_messages":  "team_store.go",
	"workflow_loops": "loop_store.go",
}

// backtickStringRe extracts the contents of every raw (backtick-delimited)
// string literal in a Go source file. Go raw strings cannot contain a
// backtick at all, so this is exact, not merely approximate — no need to
// handle escaping. Every SQL statement in this package is written as a
// backtick raw string (never a double-quoted Go string, which would need
// SQL's own single-quote literals escaped awkwardly), so scanning ONLY
// raw-string contents — never comments, never plain identifiers — is what
// keeps this lint from false-positiving on a doc comment that merely
// mentions a table name in English (several files, including this one and
// loop_store.go's own doc comments, do exactly that).
var backtickStringRe = regexp.MustCompile("`([^`]*)`")

// TestServiceStateOwnership is the build-failing tripwire: each table in
// serviceOwnedTables may be referenced in a SQL string ONLY from its
// designated owning file. A regression back to some other file reaching
// into a service's tables directly — the exact coupling the kernel/
// services ruling forbids — fails this test.
func TestServiceStateOwnership(t *testing.T) {
	root := moduleRoot(t)
	workflowDir := filepath.Join(root, "internal", "workflow")
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(workflowDir, name))
		if err != nil {
			t.Fatal(err)
		}
		var sqlText strings.Builder
		for _, m := range backtickStringRe.FindAllStringSubmatch(string(raw), -1) {
			sqlText.WriteString(m[1])
			sqlText.WriteString("\n")
		}
		content := sqlText.String()
		for table, owner := range serviceOwnedTables {
			if owner == name {
				continue
			}
			if tableNameReferenced(content, table) {
				violations = append(violations, fmt.Sprintf("%s: references table %q, owned by %s", name, table, owner))
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("service state ownership violation(s) — a table was referenced outside its owning file:\n%s", strings.Join(violations, "\n"))
	}
}

// tableNameReferenced checks for the table name as a whole word within SQL
// text, so "team_tasks" doesn't spuriously match inside some unrelated
// longer identifier.
func tableNameReferenced(sqlText, table string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(table) + `\b`)
	return re.MatchString(sqlText)
}
