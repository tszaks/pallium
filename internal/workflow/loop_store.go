package workflow

import (
	"database/sql"
	"fmt"
	"strings"
)

// Loop is a persistent, named cycle: `pallium loop start` creates one,
// `pallium loop tick <name>` advances it by exactly one bounded cycle — no
// daemon, the caller (typically cron, a trigger, or another agent) decides
// when to tick again, mirroring how `workflow trigger run` already works.
//
// Each tick spawns its OWN child workflow run through the exact same front
// door `pallium workflow run` uses (see Run.LoopName), rather than reusing
// one run across ticks. This is deliberate (Tyler's kernel/services ruling,
// 2026-07-09): a loop never squats on a workflow_runs row. Reusing one run
// across many ticks would eventually hit Runner's default 1000-agent
// LIFETIME cap purely from ticking (tracked cumulatively across resumes of
// one run id, even when each individual tick's work is small), and would
// force the agent-call resume cache to be salted with a cycle number so
// tick N+1's identical-looking agent() calls don't replay tick N's stale
// cached output as if it were fresh work. Fresh child runs make both
// problems disappear for free — this table stores NO run_id at all;
// `loop status` aggregates history via Store.RunsByLoop instead.
type Loop struct {
	Name       string `json:"name"`
	ScriptPath string `json:"script_path"`
	CWD        string `json:"cwd"`
	ArgsJSON   string `json:"args_json,omitempty"`
	Status     string `json:"status"` // active | stopped
	Cycle      int    `json:"cycle"`
	// LastTerminalState/LastSignature/StagnationCount are the tick-to-tick
	// bookkeeping loop_runtime.go's pure decision helpers operate on — this
	// file only persists whatever the caller already decided, under CAS, the
	// same split team_store.go's FinishMemberTurn uses (RunTeamTurn decides,
	// FinishMemberTurn just writes it).
	LastTerminalState   string `json:"last_terminal_state,omitempty"`
	LastSignature       string `json:"last_signature,omitempty"`
	StagnationCount     int    `json:"stagnation_count"`
	StagnationThreshold int    `json:"stagnation_threshold"`
	// TickStartedAt is non-empty only while a tick is actually in flight —
	// the lease BeginLoopTick/FinishLoopTick guard, identical in shape to
	// TeamMember.TurnStartedAt.
	TickStartedAt        string  `json:"tick_started_at,omitempty"`
	CycleBudgetUSD       float64 `json:"cycle_budget_usd,omitempty"`
	LifetimeBudgetUSD    float64 `json:"lifetime_budget_usd,omitempty"`
	LifetimeSpendUSD     float64 `json:"lifetime_spend_usd"`
	StartedBinaryVersion string  `json:"started_binary_version,omitempty"`
	StartedScriptHash    string  `json:"started_script_hash,omitempty"`
	CreatedAt            string  `json:"created_at"`
	UpdatedAt            string  `json:"updated_at"`
}

const loopSchema = `
CREATE TABLE IF NOT EXISTS workflow_loops (
  name TEXT PRIMARY KEY,
  script_path TEXT NOT NULL,
  cwd TEXT NOT NULL,
  args_json TEXT,
  status TEXT NOT NULL,
  cycle INTEGER DEFAULT 0,
  last_terminal_state TEXT,
  last_signature TEXT,
  stagnation_count INTEGER DEFAULT 0,
  stagnation_threshold INTEGER DEFAULT 3,
  tick_started_at TEXT,
  cycle_budget_usd REAL DEFAULT 0,
  lifetime_budget_usd REAL DEFAULT 0,
  lifetime_spend_usd REAL DEFAULT 0,
  started_binary_version TEXT,
  started_script_hash TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_loops_updated ON workflow_loops(updated_at DESC);
`

// initLoops creates the workflow_loops table. Called from Store.init
// alongside initTeams, so a single Store (and its one sqlite connection)
// owns every service's state.
func (s *Store) initLoops() error {
	_, err := s.db.Exec(loopSchema)
	return err
}

// WithTx runs fn inside a transaction, committing on a nil return and
// rolling back otherwise (including on panic, via the deferred Rollback,
// which is a no-op after a successful Commit). Unlike internal/db.Store's
// WithTx, this does NOT hand fn a tx-bound *Store — workflow.Store.db is a
// concrete *sql.DB, not an interface, so shadow-swapping it would mean
// converting every existing query method's receiver type, touching every
// transactional call site already proven correct in team_store.go. This
// narrower helper (fn gets the *sql.Tx directly) gets the same boilerplate
// reduction for loops' own new code without that risk.
func (s *Store) WithTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// CreateLoop starts a new loop in the "active" state. stagnationThreshold<=0
// falls back to 3 (Grok's stagnation-via-hash+counter design starts active
// by default, day one — a loop with no threshold configured still needs
// SOME bound rather than spinning on an identical signature forever).
func (s *Store) CreateLoop(loop Loop) (Loop, error) {
	loop.Name = strings.TrimSpace(loop.Name)
	if err := ValidateID(loop.Name); err != nil {
		return Loop{}, err
	}
	if strings.TrimSpace(loop.ScriptPath) == "" {
		return Loop{}, fmt.Errorf("loop requires a script path")
	}
	if loop.StagnationThreshold <= 0 {
		loop.StagnationThreshold = 3
	}
	now := nowString()
	loop.Status = "active"
	loop.CreatedAt = now
	loop.UpdatedAt = now
	_, err := s.db.Exec(`INSERT INTO workflow_loops(name,script_path,cwd,args_json,status,cycle,stagnation_threshold,cycle_budget_usd,lifetime_budget_usd,lifetime_spend_usd,started_binary_version,started_script_hash,created_at,updated_at)
VALUES(?,?,?,?,?,0,?,?,?,0,?,?,?,?)`,
		loop.Name, loop.ScriptPath, loop.CWD, loop.ArgsJSON, loop.Status, loop.StagnationThreshold, loop.CycleBudgetUSD, loop.LifetimeBudgetUSD, loop.StartedBinaryVersion, loop.StartedScriptHash, loop.CreatedAt, loop.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Loop{}, fmt.Errorf("loop %q already exists", loop.Name)
		}
		return Loop{}, err
	}
	return loop, nil
}

const loopSelectColumns = `name,script_path,cwd,COALESCE(args_json,''),status,cycle,COALESCE(last_terminal_state,''),COALESCE(last_signature,''),stagnation_count,stagnation_threshold,COALESCE(tick_started_at,''),cycle_budget_usd,lifetime_budget_usd,lifetime_spend_usd,COALESCE(started_binary_version,''),COALESCE(started_script_hash,''),created_at,updated_at`

func scanLoop(row interface{ Scan(dest ...any) error }) (Loop, error) {
	var l Loop
	if err := row.Scan(&l.Name, &l.ScriptPath, &l.CWD, &l.ArgsJSON, &l.Status, &l.Cycle, &l.LastTerminalState, &l.LastSignature, &l.StagnationCount, &l.StagnationThreshold, &l.TickStartedAt, &l.CycleBudgetUSD, &l.LifetimeBudgetUSD, &l.LifetimeSpendUSD, &l.StartedBinaryVersion, &l.StartedScriptHash, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return Loop{}, err
	}
	return l, nil
}

func (s *Store) GetLoop(name string) (Loop, error) {
	l, err := scanLoop(s.db.QueryRow(`SELECT `+loopSelectColumns+` FROM workflow_loops WHERE name=?`, name))
	if err == sql.ErrNoRows {
		return Loop{}, fmt.Errorf("loop %q not found", name)
	}
	return l, err
}

func (s *Store) ListLoops() ([]Loop, error) {
	rows, err := s.db.Query(`SELECT ` + loopSelectColumns + ` FROM workflow_loops ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var loops []Loop
	for rows.Next() {
		l, err := scanLoop(rows)
		if err != nil {
			return nil, err
		}
		loops = append(loops, l)
	}
	return loops, rows.Err()
}

// SetLoopStatus flips a loop active/stopped. A stopped loop's BeginLoopTick
// always fails fast (its UPDATE requires status='active'), the same
// idle-safety shape as team `stop`.
func (s *Store) SetLoopStatus(name, status string) error {
	res, err := s.db.Exec(`UPDATE workflow_loops SET status=?, updated_at=? WHERE name=?`, status, nowString(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("loop %q not found", name)
	}
	return nil
}

// ResetLoop clears stagnation_count/last_signature without touching cycle
// history or identity — the deliberate unstick lever for a loop an operator
// has manually confirmed is fine to keep going (e.g. they fixed whatever was
// actually blocking progress), without losing its tick count or needing to
// `loop stop` + re-`loop start` and lose its identity/config.
//
// Known limitation (found by adversarial review, accepted not fixed): this
// is NOT lease-aware. A reset issued while a tick is genuinely in flight can
// be silently clobbered when that tick's FinishLoopTick lands — it writes
// stagnation_count/last_signature computed from the loop state it read
// BEFORE the reset happened, overwriting the just-applied reset with the
// stale pre-reset values. The operator sees "reset" succeed but the next
// `loop status` shows the same stagnation as before. Narrow (requires a
// reset landing in the exact window while a tick is dispatched) and the
// workaround is simple (reset again after confirming no tick is in flight
// via `loop status`'s tick_started_at field), so this was accepted as a
// documented gap rather than fixed in M1 — a real fix would need ResetLoop
// to participate in the same lease CAS as BeginLoopTick/FinishLoopTick, or
// FinishLoopTick to detect a reset happened during its own tick and skip
// overwriting it, both bigger changes than this milestone's scope.
func (s *Store) ResetLoop(name string) error {
	res, err := s.db.Exec(`UPDATE workflow_loops SET stagnation_count=0, last_signature='', updated_at=? WHERE name=?`, nowString(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("loop %q not found", name)
	}
	return nil
}

// ErrLoopTickInFlight means another `loop tick` invocation already holds
// this loop's lease (e.g. cron fired again while a slow previous tick is
// still running). The CLI maps this to the already_running terminal
// state/exit code WITHOUT calling FinishLoopTick — no lease was ever
// acquired, so there is nothing to release and no state to mutate.
var ErrLoopTickInFlight = fmt.Errorf("loop tick already in flight")

// BeginLoopTick is the compare-and-swap that makes tick-taking safe under
// concurrent `pallium loop tick <name>` invocations. Mirrors
// BeginMemberTurn's lease shape exactly (see team_store.go) — the returned
// lease is the EXACT tick_started_at value this call wrote, and
// FinishLoopTick's WHERE clause re-checks that specific value (not merely
// "some value is present"), so a zombie/belated call whose lease was
// reassigned by a stale takeover cannot finish — or corrupt — a tick it no
// longer owns. Also borrows repolock.go's insert-vs-update-race handling
// shape via the two-step "try the CAS, then look up why it failed" pattern
// below, per both files' explicit "copy this shape" breadcrumbs.
func (s *Store) BeginLoopTick(name, staleAfter string) (lease string, err error) {
	now := nowString()
	res, err := s.db.Exec(
		`UPDATE workflow_loops SET tick_started_at=?, updated_at=?
		 WHERE name=? AND status='active' AND (tick_started_at IS NULL OR tick_started_at < ?)`,
		now, now, name, staleAfter)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return now, nil
	}
	// Distinguish "no such loop" / "stopped" from "genuinely in flight" so
	// the CLI can report a clearer error than a blanket already_running.
	loop, gerr := s.GetLoop(name)
	if gerr != nil {
		return "", gerr
	}
	if loop.Status != "active" {
		return "", fmt.Errorf("loop %q is not active (status=%s)", name, loop.Status)
	}
	return "", ErrLoopTickInFlight
}

// FinishLoopTick closes out a tick: clears tick_started_at (so the loop is
// tickable again), and persists whatever the CALLER already decided — the
// new cycle number, terminal state, signature, stagnation count, and spend
// delta. This function makes no decisions of its own (see
// loop_runtime.go's pure AdvanceLoopStagnation/EnforceLoopBudget helpers for
// where those decisions actually get made) — the same split
// team_store.go's FinishMemberTurn uses against RunTeamTurn. lease must be
// the exact value BeginLoopTick returned for THIS tick; a lease that no
// longer matches (reassigned via stale takeover) means this call no longer
// owns the tick, and it fails rather than silently corrupting a later one.
func (s *Store) FinishLoopTick(name, lease, terminalState, signature string, stagnationCount int, spendDelta float64) error {
	now := nowString()
	res, err := s.db.Exec(
		`UPDATE workflow_loops SET tick_started_at=NULL, cycle=cycle+1, last_terminal_state=?,
		 last_signature=?, stagnation_count=?, lifetime_spend_usd=lifetime_spend_usd+?, updated_at=?
		 WHERE name=? AND tick_started_at=?`,
		terminalState, signature, stagnationCount, spendDelta, now, name, lease)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("loop %q tick was not owned (lease %q no longer matches; lost to a stale takeover): not finishing it", name, lease)
	}
	return nil
}

// FinishLoopTickAsError is the minimal release used when something failed
// so early after BeginLoopTick succeeded that the caller doesn't even have
// the loop's other fields (last_signature/stagnation_count) to make a real
// FinishLoopTick call with — e.g. GetLoop itself failing right after the
// lease was acquired (found by adversarial review: that exact path used to
// return the bare error with NO release at all, leaking the lease for up to
// --stale-after-minutes over a single transient failure that had nothing
// to do with the tick). Deliberately does NOT touch last_signature or
// stagnation_count at all (they're simply absent from the SET clause, so
// SQL leaves them exactly as they were) — consistent with error ticks never
// advancing OR resetting stagnation (see AdvanceLoopStagnation).
func (s *Store) FinishLoopTickAsError(name, lease string) error {
	now := nowString()
	// RowsAffected==0 is deliberately not treated as an error here: this is
	// the safety-net path (see runLoopTick's defer), so "the lease was
	// already released by the normal FinishLoopTick call" is the expected,
	// harmless common case, not a bug worth surfacing.
	_, err := s.db.Exec(
		`UPDATE workflow_loops SET tick_started_at=NULL, cycle=cycle+1, last_terminal_state=?, updated_at=?
		 WHERE name=? AND tick_started_at=?`,
		LoopStateError, now, name, lease)
	return err
}
