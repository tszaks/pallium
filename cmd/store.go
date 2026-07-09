package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/tszaks/pallium/internal/workflow"
)

// openPalliumStore is the single chokepoint every `pallium team`/`pallium
// loop` subcommand opens its store through — a kernel-level concern (the
// SQLite store is shared substrate every service sits on top of), not
// something either service should reimplement or copy-paste its own copy
// of. Without --db, workflow.Open falls back to the real, shared, global
// ~/.pallium/codex-sessions.sqlite — fine for a genuine long-lived team or
// loop, but a landmine for throwaway/test ones: this is the exact mistake
// that polluted Tyler's real production DB with test-team rows during
// Agent Teams M1 development, requiring a manual SQL cleanup.
// PALLIUM_TEST_DB is the safety net: set it ONCE per test/dogfood session
// (e.g. export PALLIUM_TEST_DB=/tmp/throwaway.sqlite) and every subsequent
// `team ...`/`loop ...` call that forgets --db lands there instead of the
// real global DB. It is deliberately opt-in (never silently redirects a
// genuine user who hasn't set it) and never silent when active (always
// warns to stderr so a forgotten env var from an earlier session can't
// quietly redirect real work either).
func openPalliumStore(dbPath string) (*workflow.Store, error) {
	return workflow.Open(resolvePalliumDBPath(dbPath))
}

// resolvePalliumDBPath applies the PALLIUM_TEST_DB redirect without opening
// anything — split out from openPalliumStore for `loop tick`, which must
// pass the SAME resolved path on to the child run it spawns (see
// runWorkflowRun in cmd/loop.go). Resolving independently in each place
// would silently split the loop's own bookkeeping and its child run's data
// across two different databases whenever PALLIUM_TEST_DB is set and no
// explicit --db was passed to `loop tick` — exactly the kind of quiet
// leak into the real global DB this env var exists to prevent, just one
// level removed (found live: a real `loop tick` run this way landed the
// child run in ~/.pallium/codex-sessions.sqlite while the loop's own row
// stayed correctly redirected, and the immediate next store.Run(childID)
// lookup failed with sql.ErrNoRows because it was looking in the wrong DB).
func resolvePalliumDBPath(dbPath string) string {
	if dbPath == "" {
		if testDB := strings.TrimSpace(os.Getenv("PALLIUM_TEST_DB")); testDB != "" {
			fmt.Fprintf(os.Stderr, "[pallium] PALLIUM_TEST_DB is set: using %s instead of the default global DB (no --db was passed)\n", testDB)
			dbPath = testDB
		}
	}
	return dbPath
}
