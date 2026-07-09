package workflow

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Team is a lead + independent peer agents that coordinate over a shared
// task board and mailbox, all persisted in SQLite (see teamSchema). Unlike a
// workflow run, a team has no single script driving it end to end: its state
// is the durable source of truth, so any agent (or human) can `pallium team
// attach <id>` later and keep driving it — including after the process that
// started it was killed.
type Team struct {
	ID             string  `json:"id"`
	Goal           string  `json:"goal"`
	CWD            string  `json:"cwd"`
	Status         string  `json:"status"` // active | parked | stopped
	BudgetUSDLimit float64 `json:"budget_usd_limit,omitempty"`
	SpendUSD       float64 `json:"spend_usd"`
	// TasksUpdatedAt is a team-wide watermark bumped on every task-board
	// mutation (create/claim/complete/revert-to-pending) — NOT the same as
	// any single task's own updated_at, because completing task A can make
	// task B newly claimable (a dependency satisfied) without touching B's
	// row at all. The scheduler (see RunTeam) compares this against a
	// member's LastTurnAt to decide whether "claimable work exists" is NEW
	// information for that member or the same board it already declined.
	TasksUpdatedAt string `json:"tasks_updated_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// TeamMember is a named, persistent teammate identity. Its life is a series
// of one-shot provider invocations ("turns"); SessionToken is the provider's
// own resume handle (a UUID Pallium mints for claude, a thread id codex
// assigns and Pallium captures) that turn N+1 passes back so the provider
// resumes its native conversation instead of starting over. TurnStartedAt is
// non-empty only while a turn is actually in flight — see BeginMemberTurn —
// so an interrupted turn (the owning process died mid-turn) is detectable
// and resumable rather than looking like a live member that never responds.
type TeamMember struct {
	ID           string `json:"id"`
	TeamID       string `json:"team_id"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	Model        string `json:"model,omitempty"`
	Role         string `json:"role,omitempty"`
	Mode         string `json:"mode"`   // read-only | edit
	Status       string `json:"status"` // idle | active | blocked | interrupted | stale | stopped | error
	SessionToken string `json:"session_token,omitempty"`
	// SessionEstablished is sticky-true once a turn has ever completed with
	// a status other than "error" (see FinishMemberTurn). It is deliberately
	// NOT the same as TurnCount>0: TurnCount increments even on a failed
	// turn, but a failed claude turn may never have actually created the
	// native session claude expects `--resume` to find. dispatchTeamTurn
	// uses THIS field, not TurnCount, to decide `--session-id` vs `--resume`.
	SessionEstablished bool    `json:"session_established"`
	Worktree           string  `json:"worktree,omitempty"`
	TurnCount          int     `json:"turn_count"`
	TurnStartedAt      string  `json:"turn_started_at,omitempty"`
	LastTurnAt         string  `json:"last_turn_at,omitempty"`
	LastTurnStatus     string  `json:"last_turn_status,omitempty"`
	LastTurnError      string  `json:"last_turn_error,omitempty"`
	NudgedAt           string  `json:"nudged_at,omitempty"`
	SpendUSD           float64 `json:"spend_usd"`
	CreatedAt          string  `json:"created_at"`
	UpdatedAt          string  `json:"updated_at"`
}

// TeamTask is one item on the shared task board. DependsOn holds task ids
// (JSON-encoded, SQLite has no array column) that must all be "completed"
// before this task is claimable.
type TeamTask struct {
	ID          string   `json:"id"`
	TeamID      string   `json:"team_id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status"` // pending | in_progress | completed | blocked
	Owner       string   `json:"owner,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	Result      string   `json:"result,omitempty"`
	ClaimedAt   string   `json:"claimed_at,omitempty"`
	CompletedAt string   `json:"completed_at,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// TeamMessage is one mailbox entry. From/To are member names or "lead" (the
// human operator's steering agent). DeliveredAt is empty until the message
// has been injected into a turn's prompt; see teamTrustWrap for how a
// delivered message is framed for the recipient so it can never be confused
// with the human operator's own instructions.
type TeamMessage struct {
	ID            string `json:"id"`
	TeamID        string `json:"team_id"`
	From          string `json:"from"`
	To            string `json:"to"`
	Body          string `json:"body"`
	CreatedAt     string `json:"created_at"`
	DeliveredAt   string `json:"delivered_at,omitempty"`
	DeliveredTurn int    `json:"delivered_turn,omitempty"`
}

const teamSchema = `
CREATE TABLE IF NOT EXISTS teams (
  id TEXT PRIMARY KEY,
  goal TEXT NOT NULL,
  cwd TEXT NOT NULL,
  status TEXT NOT NULL,
  budget_usd_limit REAL DEFAULT 0,
  spend_usd REAL DEFAULT 0,
  tasks_updated_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_teams_updated ON teams(updated_at DESC);

CREATE TABLE IF NOT EXISTS team_members (
  id TEXT PRIMARY KEY,
  team_id TEXT NOT NULL,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT,
  role TEXT,
  mode TEXT NOT NULL,
  status TEXT NOT NULL,
  session_token TEXT,
  session_established INTEGER NOT NULL DEFAULT 0,
  worktree TEXT,
  turn_count INTEGER DEFAULT 0,
  turn_started_at TEXT,
  last_turn_at TEXT,
  last_turn_status TEXT,
  last_turn_error TEXT,
  nudged_at TEXT,
  spend_usd REAL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(team_id, name)
);
CREATE INDEX IF NOT EXISTS idx_team_members_team ON team_members(team_id);

CREATE TABLE IF NOT EXISTS team_tasks (
  id TEXT PRIMARY KEY,
  team_id TEXT NOT NULL,
  title TEXT NOT NULL,
  description TEXT,
  status TEXT NOT NULL,
  owner TEXT,
  depends_on TEXT,
  result TEXT,
  claimed_at TEXT,
  completed_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_team_tasks_team ON team_tasks(team_id, status);

CREATE TABLE IF NOT EXISTS team_messages (
  id TEXT PRIMARY KEY,
  team_id TEXT NOT NULL,
  from_name TEXT NOT NULL,
  to_name TEXT NOT NULL,
  body TEXT NOT NULL,
  created_at TEXT NOT NULL,
  delivered_at TEXT,
  delivered_turn INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_team_messages_inbox ON team_messages(team_id, to_name, delivered_at);
`

// initTeams creates the team_* tables. Called from Store.init alongside the
// rest of the schema, so a single Store (and its one sqlite connection) owns
// both workflow and team state.
func (s *Store) initTeams() error {
	_, err := s.db.Exec(teamSchema)
	return err
}

// CreateTeam starts a new team in the "active" state.
func (s *Store) CreateTeam(goal, cwd string, budgetUSDLimit float64) (Team, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return Team{}, fmt.Errorf("team requires a goal")
	}
	now := nowString()
	t := Team{ID: NewID("team"), Goal: goal, CWD: cwd, Status: "active", BudgetUSDLimit: budgetUSDLimit, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.Exec(`INSERT INTO teams(id,goal,cwd,status,budget_usd_limit,spend_usd,created_at,updated_at) VALUES(?,?,?,?,?,0,?,?)`,
		t.ID, t.Goal, t.CWD, t.Status, t.BudgetUSDLimit, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return Team{}, err
	}
	return t, nil
}

func (s *Store) GetTeam(id string) (Team, error) {
	var t Team
	var tasksUpdatedAt sql.NullString
	err := s.db.QueryRow(`SELECT id,goal,cwd,status,budget_usd_limit,spend_usd,tasks_updated_at,created_at,updated_at FROM teams WHERE id=?`, id).
		Scan(&t.ID, &t.Goal, &t.CWD, &t.Status, &t.BudgetUSDLimit, &t.SpendUSD, &tasksUpdatedAt, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return Team{}, fmt.Errorf("team %q not found", id)
	}
	t.TasksUpdatedAt = tasksUpdatedAt.String
	return t, err
}

// bumpTeamTasksUpdated advances the team-wide task-board watermark (see
// Team.TasksUpdatedAt) — called after every task mutation that can change
// what's claimable, even one that touches a DIFFERENT task's row than the
// one whose claimability actually changed (completing a dependency doesn't
// write to the task it unblocks).
func bumpTeamTasksUpdated(exec interface {
	Exec(query string, args ...any) (sql.Result, error)
}, teamID, now string) error {
	_, err := exec.Exec(`UPDATE teams SET tasks_updated_at=? WHERE id=?`, now, teamID)
	return err
}

func (s *Store) SetTeamStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE teams SET status=?, updated_at=? WHERE id=?`, status, nowString(), id)
	return err
}

// AddTeamSpend increments the team's running spend and reports whether it is
// now at or over its budget ceiling (0/unset means no ceiling). The
// increment happens SQL-side (spend_usd=spend_usd+?), not via a Go-computed
// SELECT-then-UPDATE — a live dogfooded review caught that the latter is a
// lost-update race: two concurrent calls (e.g. two members finishing turns
// in the same round) can both read the same starting value and each write
// back their own delta on top of it, losing one increment entirely. The
// SQL-side increment is atomic regardless of how many callers race it.
func (s *Store) AddTeamSpend(id string, delta float64) (overBudget bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE teams SET spend_usd=spend_usd+?, updated_at=? WHERE id=?`, delta, nowString(), id); err != nil {
		return false, err
	}
	var spend, limit float64
	if err := tx.QueryRow(`SELECT spend_usd, budget_usd_limit FROM teams WHERE id=?`, id).Scan(&spend, &limit); err != nil {
		return false, err
	}
	return limit > 0 && spend >= limit, tx.Commit()
}

// SpawnMember creates a new teammate identity, idle and with no session yet
// (its first BeginMemberTurn is turn 1). The (team_id, name) UNIQUE
// constraint makes spawning an already-used name a clear conflict rather
// than silently creating a second identity with the same address.
func (s *Store) SpawnMember(teamID, name, provider, model, role, mode string) (TeamMember, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TeamMember{}, fmt.Errorf("team member requires a name")
	}
	if mode != "read-only" && mode != "edit" {
		return TeamMember{}, fmt.Errorf("team member mode must be \"read-only\" or \"edit\", got %q", mode)
	}
	now := nowString()
	m := TeamMember{ID: NewID("tm"), TeamID: teamID, Name: name, Provider: provider, Model: model, Role: role, Mode: mode, Status: "idle", CreatedAt: now, UpdatedAt: now}
	_, err := s.db.Exec(`INSERT INTO team_members(id,team_id,name,provider,model,role,mode,status,turn_count,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,0,?,?)`,
		m.ID, m.TeamID, m.Name, m.Provider, m.Model, m.Role, m.Mode, m.Status, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return TeamMember{}, fmt.Errorf("team member %q already exists on team %s", name, teamID)
		}
		return TeamMember{}, err
	}
	return m, nil
}

func scanMember(row *sql.Row) (TeamMember, error) {
	var m TeamMember
	var model, role, session, worktree, turnStarted, lastAt, lastStatus, lastErr, nudged sql.NullString
	err := row.Scan(&m.ID, &m.TeamID, &m.Name, &m.Provider, &model, &role, &m.Mode, &m.Status, &session, &m.SessionEstablished, &worktree,
		&m.TurnCount, &turnStarted, &lastAt, &lastStatus, &lastErr, &nudged, &m.SpendUSD, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return TeamMember{}, err
	}
	m.Model, m.Role, m.SessionToken, m.Worktree = model.String, role.String, session.String, worktree.String
	m.TurnStartedAt, m.LastTurnAt, m.LastTurnStatus, m.LastTurnError, m.NudgedAt = turnStarted.String, lastAt.String, lastStatus.String, lastErr.String, nudged.String
	return m, nil
}

const memberSelectCols = `id,team_id,name,provider,model,role,mode,status,session_token,session_established,worktree,turn_count,turn_started_at,last_turn_at,last_turn_status,last_turn_error,nudged_at,spend_usd,created_at,updated_at`

func (s *Store) GetMember(teamID, name string) (TeamMember, error) {
	m, err := scanMember(s.db.QueryRow(`SELECT `+memberSelectCols+` FROM team_members WHERE team_id=? AND name=?`, teamID, name))
	if err == sql.ErrNoRows {
		return TeamMember{}, fmt.Errorf("team member %q not found on team %s", name, teamID)
	}
	return m, err
}

func (s *Store) ListMembers(teamID string) ([]TeamMember, error) {
	rows, err := s.db.Query(`SELECT `+memberSelectCols+` FROM team_members WHERE team_id=? ORDER BY created_at`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []TeamMember
	for rows.Next() {
		var m TeamMember
		var model, role, session, worktree, turnStarted, lastAt, lastStatus, lastErr, nudged sql.NullString
		if err := rows.Scan(&m.ID, &m.TeamID, &m.Name, &m.Provider, &model, &role, &m.Mode, &m.Status, &session, &m.SessionEstablished, &worktree,
			&m.TurnCount, &turnStarted, &lastAt, &lastStatus, &lastErr, &nudged, &m.SpendUSD, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Model, m.Role, m.SessionToken, m.Worktree = model.String, role.String, session.String, worktree.String
		m.TurnStartedAt, m.LastTurnAt, m.LastTurnStatus, m.LastTurnError, m.NudgedAt = turnStarted.String, lastAt.String, lastStatus.String, lastErr.String, nudged.String
		members = append(members, m)
	}
	return members, rows.Err()
}

// errMemberTurnInFlight means another process already holds this member's
// turn (or a stale-takeover raced and won first). The caller should skip
// this member for the current scheduling round, not treat it as an error.
var errMemberTurnInFlight = fmt.Errorf("team member turn already in flight")

// BeginMemberTurn is the compare-and-swap that makes turn-taking safe under
// concurrent `pallium team run` invocations on the same team (the exact bug
// class fixed in the repo lock: a naive read-then-write here would let two
// racing schedulers both start the same member's turn). It only starts a
// turn when the member has NO turn in flight, OR its existing turn is older
// than staleAfter (an owning process that died without finishing it) — and
// the UPDATE's WHERE clause re-checks that exact stale condition so two
// racers reading the same stale row cannot both win: only one UPDATE's
// RowsAffected is 1.
//
// The returned lease is the EXACT turn_started_at value this call wrote —
// callers must pass it back to FinishMemberTurn/PersistMemberSession (which
// only act when the row still carries that specific value, not merely
// "some value is present"). A second dogfooded review caught the gap a
// nullity-only guard leaves: a zombie/belated call (e.g. an orphaned
// provider subprocess whose owning `team run` process was killed, and whose
// slot was later reassigned by ReconcileInterruptedMembers or a stale
// takeover) could otherwise finish or overwrite a turn it no longer owns,
// silently corrupting a DIFFERENT, currently-live turn's state.
//
// Reusable shape for loops (#35): a per-loop lease with stale-holder
// takeover is the SAME pattern — `UPDATE ... SET holder=?, acquired_at=?
// WHERE (holder IS NULL OR acquired_at < ?staleAfter) `, checking
// RowsAffected==1, with a companion release that only clears the holder
// when acquired_at STILL equals the exact value this call wrote (not just
// "the holder is set to something"). Copy this shape (and repolock.go's
// AcquireRepoLock, which additionally handles the first-row-insert race and
// an idempotent same-holder refresh) rather than re-deriving it.
func (s *Store) BeginMemberTurn(teamID, name string, staleAfter string) (lease string, err error) {
	now := nowString()
	res, err := s.db.Exec(
		`UPDATE team_members SET turn_started_at=?, status='active', updated_at=?
		 WHERE team_id=? AND name=? AND (turn_started_at IS NULL OR turn_started_at < ?)`,
		now, now, teamID, name, staleAfter)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", errMemberTurnInFlight
	}
	return now, nil
}

// FinishMemberTurn closes out a turn: clears turn_started_at (so the member
// is schedulable again), records the outcome, and — if sessionToken is
// non-empty — persists the provider's resume handle. lease must be the exact
// value BeginMemberTurn returned for THIS turn; the UPDATE's WHERE clause
// checks turn_started_at=lease (not merely IS NOT NULL), so a call whose
// lease has since been reassigned (stale takeover, or reconciled as
// interrupted) cannot finish — or silently corrupt — a turn it no longer owns.
//
// turn_count increments unconditionally, on error or success — it is a
// how-many-attempts counter, not a success counter. session_established is
// the field that means "success": it flips true (and stays true — the OR is
// sticky, never reset) the first time status != "error". This split matters
// because a claude member's SessionToken is pre-minted at spawn (see
// SpawnTeamMember), not left empty until first success the way codex's is —
// so if turn 1 fails before claude ever actually creates that session,
// TurnCount alone would say "not the first turn anymore" while claude's side
// never established anything for `--resume` to find. dispatchTeamTurn must
// key `--session-id` vs `--resume` off SessionEstablished, not TurnCount==0.
func (s *Store) FinishMemberTurn(teamID, name, lease, status, sessionToken, turnError string, spendDelta float64) error {
	now := nowString()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current TeamMember
	current, err = scanMember(tx.QueryRow(`SELECT `+memberSelectCols+` FROM team_members WHERE team_id=? AND name=?`, teamID, name))
	if err != nil {
		return err
	}
	token := current.SessionToken
	if sessionToken != "" {
		token = sessionToken
	}
	res, err := tx.Exec(
		`UPDATE team_members SET turn_started_at=NULL, status=?, session_token=?, turn_count=turn_count+1,
		 last_turn_at=?, last_turn_status=?, last_turn_error=?, spend_usd=spend_usd+?, updated_at=?,
		 session_established = session_established OR (? <> 'error')
		 WHERE team_id=? AND name=? AND turn_started_at=?`,
		status, token, now, status, turnError, spendDelta, now, status, teamID, name, lease)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("team member %q turn was not owned (lease %q no longer matches; lost to a stale takeover or reconciliation): not finishing it", name, lease)
	}
	return tx.Commit()
}

// PersistMemberSession writes a provider-captured session token immediately
// (not deferred to FinishMemberTurn) so a mid-turn interruption still leaves
// the session resumable. codex_provider.go calls this the instant it parses
// a `thread.started` event, guarded on the SAME lease FinishMemberTurn will
// later check — an orphaned codex subprocess (its owning `team run` process
// was killed, but the child keeps running independently; see the live kill/
// resume acceptance test) that eventually emits its own thread.started must
// not clobber a session token that now belongs to a different, later turn.
// The claude path never calls this at all: it mints and writes the session
// id before the turn even starts (see cmd/team.go's runTeamSpawn, which
// calls this same method right after SpawnMember, before any lease exists).
func (s *Store) PersistMemberSession(teamID, name, sessionToken string) error {
	_, err := s.db.Exec(`UPDATE team_members SET session_token=?, updated_at=? WHERE team_id=? AND name=?`, sessionToken, nowString(), teamID, name)
	return err
}

// PersistMemberSessionForLease is PersistMemberSession's lease-guarded
// sibling, used mid-turn (see the previous doc comment). Spawn-time writes
// use the unguarded PersistMemberSession instead, since no lease exists yet.
func (s *Store) PersistMemberSessionForLease(teamID, name, lease, sessionToken string) error {
	res, err := s.db.Exec(`UPDATE team_members SET session_token=?, updated_at=? WHERE team_id=? AND name=? AND turn_started_at=?`,
		sessionToken, nowString(), teamID, name, lease)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("team member %q session capture dropped: lease %q no longer matches (turn reassigned)", name, lease)
	}
	return nil
}

func (s *Store) SetMemberWorktree(teamID, name, worktree string) error {
	_, err := s.db.Exec(`UPDATE team_members SET worktree=?, updated_at=? WHERE team_id=? AND name=?`, worktree, nowString(), teamID, name)
	return err
}

func (s *Store) SetMemberMode(teamID, name, mode string) error {
	_, err := s.db.Exec(`UPDATE team_members SET mode=?, updated_at=? WHERE team_id=? AND name=?`, mode, nowString(), teamID, name)
	return err
}

func (s *Store) NudgeMember(teamID, name string) error {
	_, err := s.db.Exec(`UPDATE team_members SET nudged_at=?, updated_at=? WHERE team_id=? AND name=?`, nowString(), nowString(), teamID, name)
	if err != nil {
		return err
	}
	res, err := s.db.Query(`SELECT 1 FROM team_members WHERE team_id=? AND name=?`, teamID, name)
	if err != nil {
		return err
	}
	defer res.Close()
	if !res.Next() {
		return fmt.Errorf("team member %q not found on team %s", name, teamID)
	}
	return nil
}

func (s *Store) ClearNudge(teamID, name string) error {
	_, err := s.db.Exec(`UPDATE team_members SET nudged_at=NULL, updated_at=? WHERE team_id=? AND name=?`, nowString(), teamID, name)
	return err
}

// ReconcileInterruptedMembers finds members whose turn_started_at is older
// than staleAfter (an owning `pallium team run` process died mid-turn) and
// marks them "interrupted" so `team status` reports it honestly instead of
// looking busy. Whether an owned in-progress task is reverted to "pending"
// (claimable by someone else) depends on whether the member has a
// SessionToken to resume: a member with a live session is expected to pick
// up its OWN work again via --resume/`codex exec resume` on its next turn —
// stripping its task away in that case would actively defeat the point of
// session persistence (a killed orchestrator process, not a dead teammate).
// Only a member with no session at all (interrupted before turn 1 ever
// captured one) has genuinely orphaned work, so only then is its task
// reverted for someone else to claim. Safe to call on every `team
// run`/`team attach`.
func (s *Store) ReconcileInterruptedMembers(teamID, staleAfter string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name, COALESCE(session_token,'') FROM team_members WHERE team_id=? AND turn_started_at IS NOT NULL AND turn_started_at < ?`, teamID, staleAfter)
	if err != nil {
		return nil, err
	}
	var names []string
	sessions := map[string]string{}
	for rows.Next() {
		var n, sess string
		if err := rows.Scan(&n, &sess); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
		sessions[n] = sess
	}
	rows.Close()
	now := nowString()
	var reconciled []string
	for _, name := range names {
		// Both statements below run in ONE transaction: a live dogfooded
		// review caught that they were previously two separate, individually
		// autocommitted Exec calls — a crash between them (or a process kill
		// mid-reconcile) could mark the member interrupted while leaving its
		// task permanently stuck "in_progress" with no live owner, forever
		// unclaimable (not pending) and never resumed (owner's turn already
		// closed out).
		if err := func() error {
			tx, err := s.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			// CAS, matching BeginMemberTurn: re-check the exact staleness
			// condition at UPDATE time, not just at the earlier SELECT.
			// Found by the same review: without the re-check, a member
			// whose turn legitimately restarted via stale-takeover — or
			// finished cleanly — between the SELECT above and this UPDATE
			// would get clobbered back to "interrupted" with
			// turn_started_at wiped, letting a second BeginMemberTurn
			// succeed and double-run the same member. Concurrent `team
			// run`/`team attach` on one team is an explicitly supported
			// scenario, so this window is real, not theoretical.
			res, err := tx.Exec(`UPDATE team_members SET turn_started_at=NULL, status='interrupted', updated_at=?
				WHERE team_id=? AND name=? AND turn_started_at IS NOT NULL AND turn_started_at < ?`, now, teamID, name, staleAfter)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				// Lost the race: this member's turn is no longer the stale
				// one we observed (it restarted or finished already) —
				// leave it alone.
				return nil
			}
			reconciled = append(reconciled, name)
			if sessions[name] != "" {
				return tx.Commit()
			}
			revertRes, err := tx.Exec(`UPDATE team_tasks SET status='pending', owner=NULL, updated_at=? WHERE team_id=? AND owner=? AND status='in_progress'`, now, teamID, name)
			if err != nil {
				return err
			}
			if n, _ := revertRes.RowsAffected(); n > 0 {
				// A task just became claimable again (owner cleared) — bump
				// the board watermark so the scheduler doesn't treat this as
				// the same unchanged board every other idle member already
				// declined (see RunTeam / Team.TasksUpdatedAt).
				if err := bumpTeamTasksUpdated(tx, teamID, now); err != nil {
					return err
				}
			}
			return tx.Commit()
		}(); err != nil {
			return nil, err
		}
	}
	return reconciled, nil
}

// CreateTeamTask adds a task to the board. dependsOn task ids need not exist
// yet (a task can be added before its dependency), but a task can never be
// claimed while any dependency is missing or not completed — see ClaimTask.
func (s *Store) CreateTeamTask(teamID, title, description string, dependsOn []string) (TeamTask, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return TeamTask{}, fmt.Errorf("team task requires a title")
	}
	deps, err := json.Marshal(dependsOn)
	if err != nil {
		return TeamTask{}, err
	}
	now := nowString()
	t := TeamTask{ID: NewID("tt"), TeamID: teamID, Title: title, Description: description, Status: "pending", DependsOn: dependsOn, CreatedAt: now, UpdatedAt: now}
	_, err = s.db.Exec(`INSERT INTO team_tasks(id,team_id,title,description,status,depends_on,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, t.TeamID, t.Title, t.Description, t.Status, string(deps), t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return TeamTask{}, err
	}
	if err := bumpTeamTasksUpdated(s.db, teamID, now); err != nil {
		return TeamTask{}, err
	}
	return t, nil
}

func scanTask(row *sql.Row) (TeamTask, error) {
	var t TeamTask
	var desc, owner, deps, result, claimedAt, completedAt sql.NullString
	if err := row.Scan(&t.ID, &t.TeamID, &t.Title, &desc, &t.Status, &owner, &deps, &result, &claimedAt, &completedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return TeamTask{}, err
	}
	t.Description, t.Owner, t.Result, t.ClaimedAt, t.CompletedAt = desc.String, owner.String, result.String, claimedAt.String, completedAt.String
	if deps.Valid && deps.String != "" {
		_ = json.Unmarshal([]byte(deps.String), &t.DependsOn)
	}
	return t, nil
}

const taskSelectCols = `id,team_id,title,description,status,owner,depends_on,result,claimed_at,completed_at,created_at,updated_at`

func (s *Store) ListTeamTasks(teamID string) ([]TeamTask, error) {
	rows, err := s.db.Query(`SELECT `+taskSelectCols+` FROM team_tasks WHERE team_id=? ORDER BY created_at`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []TeamTask
	for rows.Next() {
		var t TeamTask
		var desc, owner, deps, result, claimedAt, completedAt sql.NullString
		if err := rows.Scan(&t.ID, &t.TeamID, &t.Title, &desc, &t.Status, &owner, &deps, &result, &claimedAt, &completedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.Description, t.Owner, t.Result, t.ClaimedAt, t.CompletedAt = desc.String, owner.String, result.String, claimedAt.String, completedAt.String
		if deps.Valid && deps.String != "" {
			_ = json.Unmarshal([]byte(deps.String), &t.DependsOn)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *Store) GetTeamTask(teamID, taskID string) (TeamTask, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskSelectCols+` FROM team_tasks WHERE team_id=? AND id=?`, teamID, taskID))
	if err == sql.ErrNoRows {
		return TeamTask{}, fmt.Errorf("team task %q not found on team %s", taskID, teamID)
	}
	return t, err
}

// errTaskNotClaimable covers every reason a claim can't proceed: already
// claimed/completed by the time this call runs, or a dependency isn't done
// yet. The caller (a teammate mid-turn, or the scheduler) should treat this
// as "not mine, move on" rather than a fatal error.
var errTaskNotClaimable = fmt.Errorf("team task is not claimable")

// ClaimTask is the second compare-and-swap in the team model. The dependency
// check happens first (a plain read — dependencies only ever move toward
// "completed", never back, so a stale read can only under- not over-claim),
// then the actual claim is a single UPDATE guarded on status='pending' AND
// owner IS NULL. That WHERE clause is the CAS: exactly one concurrent
// claimer's UPDATE can match a given row (status/owner flip together), so
// two teammates racing to claim the same task can never both succeed —
// the loser's RowsAffected is 0 and it gets errTaskNotClaimable, the same
// "lost the race, not an error" shape as BeginMemberTurn.
func (s *Store) ClaimTask(teamID, taskID, owner string) (TeamTask, error) {
	task, err := s.GetTeamTask(teamID, taskID)
	if err != nil {
		return TeamTask{}, err
	}
	for _, depID := range task.DependsOn {
		dep, derr := s.GetTeamTask(teamID, depID)
		if derr != nil || dep.Status != "completed" {
			return TeamTask{}, errTaskNotClaimable
		}
	}
	now := nowString()
	res, err := s.db.Exec(`UPDATE team_tasks SET status='in_progress', owner=?, claimed_at=?, updated_at=? WHERE team_id=? AND id=? AND status='pending' AND owner IS NULL`,
		owner, now, now, teamID, taskID)
	if err != nil {
		return TeamTask{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return TeamTask{}, errTaskNotClaimable
	}
	// A claimed task leaves the claimable pool — bump the board watermark so
	// members who already saw (and declined) this exact task before it was
	// claimed don't need to re-examine an unchanged board (see RunTeam).
	if err := bumpTeamTasksUpdated(s.db, teamID, now); err != nil {
		return TeamTask{}, err
	}
	return s.GetTeamTask(teamID, taskID)
}

// CompleteTask marks a task completed, but only for the owner that claimed
// it — a teammate cannot complete another's task, and a task that was
// reclaimed away from a stale owner (see ReconcileInterruptedMembers) can no
// longer be completed by the member that lost it.
func (s *Store) CompleteTask(teamID, taskID, owner, result string) (TeamTask, error) {
	now := nowString()
	res, err := s.db.Exec(`UPDATE team_tasks SET status='completed', result=?, completed_at=?, updated_at=? WHERE team_id=? AND id=? AND owner=? AND status='in_progress'`,
		result, now, now, teamID, taskID, owner)
	if err != nil {
		return TeamTask{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return TeamTask{}, fmt.Errorf("team task %q is not owned by %q (or already completed); cannot complete it", taskID, owner)
	}
	// Completing a task can make a DIFFERENT task newly claimable (a
	// dependency just got satisfied) without ever touching that other
	// task's own row — bump the team-wide watermark, not just this task's
	// updated_at, so the scheduler notices (see RunTeam).
	if err := bumpTeamTasksUpdated(s.db, teamID, now); err != nil {
		return TeamTask{}, err
	}
	return s.GetTeamTask(teamID, taskID)
}

// HasClaimableWork reports whether any pending task's dependencies are all
// satisfied — used by the scheduler to decide whether idle members should
// still get a turn to go look for work even with no new mail.
func (s *Store) HasClaimableWork(teamID string) (bool, error) {
	tasks, err := s.ListTeamTasks(teamID)
	if err != nil {
		return false, err
	}
	completed := map[string]bool{}
	for _, t := range tasks {
		if t.Status == "completed" {
			completed[t.ID] = true
		}
	}
	for _, t := range tasks {
		if t.Status != "pending" {
			continue
		}
		blocked := false
		for _, dep := range t.DependsOn {
			if !completed[dep] {
				blocked = true
				break
			}
		}
		if !blocked {
			return true, nil
		}
	}
	return false, nil
}

// SendTeamMessage records a mailbox entry. It does not deliver it — delivery
// (marking delivered_at) happens only when the recipient's turn actually
// injects it into that turn's prompt, so a message sent to a member whose
// turn gets interrupted before it runs is still undelivered and will be
// injected on the retry, never silently lost.
func (s *Store) SendTeamMessage(teamID, from, to, body string) (TeamMessage, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return TeamMessage{}, fmt.Errorf("team message body must not be empty")
	}
	now := nowString()
	m := TeamMessage{ID: NewID("tmsg"), TeamID: teamID, From: from, To: to, Body: body, CreatedAt: now}
	_, err := s.db.Exec(`INSERT INTO team_messages(id,team_id,from_name,to_name,body,created_at) VALUES(?,?,?,?,?,?)`,
		m.ID, m.TeamID, m.From, m.To, m.Body, m.CreatedAt)
	if err != nil {
		return TeamMessage{}, err
	}
	return m, nil
}

// UndeliveredMessages returns "to" recipient's mailbox entries not yet
// injected into a turn, oldest first.
func (s *Store) UndeliveredMessages(teamID, to string) ([]TeamMessage, error) {
	rows, err := s.db.Query(`SELECT id,team_id,from_name,to_name,body,created_at,delivered_at,delivered_turn FROM team_messages WHERE team_id=? AND to_name=? AND delivered_at IS NULL ORDER BY created_at`, teamID, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamMessage
	for rows.Next() {
		var m TeamMessage
		var delivered sql.NullString
		if err := rows.Scan(&m.ID, &m.TeamID, &m.From, &m.To, &m.Body, &m.CreatedAt, &delivered, &m.DeliveredTurn); err != nil {
			return nil, err
		}
		m.DeliveredAt = delivered.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListTeamMessages returns every message on the team (for `team inbox
// --all`/audit), oldest first.
func (s *Store) ListTeamMessages(teamID string) ([]TeamMessage, error) {
	rows, err := s.db.Query(`SELECT id,team_id,from_name,to_name,body,created_at,delivered_at,delivered_turn FROM team_messages WHERE team_id=? ORDER BY created_at`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamMessage
	for rows.Next() {
		var m TeamMessage
		var delivered sql.NullString
		if err := rows.Scan(&m.ID, &m.TeamID, &m.From, &m.To, &m.Body, &m.CreatedAt, &delivered, &m.DeliveredTurn); err != nil {
			return nil, err
		}
		m.DeliveredAt = delivered.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkMessagesDelivered stamps a batch of messages as consumed by turn
// turnCount. Called once, after a turn completes successfully — never
// before the provider call, so a turn interrupted before finishing leaves
// its messages undelivered and they get re-injected on retry.
func (s *Store) MarkMessagesDelivered(ids []string, turnCount int) error {
	if len(ids) == 0 {
		return nil
	}
	now := nowString()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE team_messages SET delivered_at=?, delivered_turn=? WHERE id=?`, now, turnCount, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
