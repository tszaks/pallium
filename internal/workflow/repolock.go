package workflow

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// errRepoLockContended means the lock row changed under an in-flight acquire
// (another run took over, refreshed, or inserted first). It is retriable: the
// next acquire attempt re-reads the row and reports the live holder.
var errRepoLockContended = errors.New("workflow repo lock contended")

// isSQLiteConstraintErr reports whether err is a UNIQUE/PRIMARY KEY violation,
// which is how a first-time INSERT loses the acquire race (repo_root is the
// primary key) — that is contention, not a real failure.
func isSQLiteConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_CONSTRAINT") || strings.Contains(msg, "constraint failed") || strings.Contains(msg, "UNIQUE constraint")
}

// repoLockBusyRetries bounds how many times a repo lock operation retries
// after SQLITE_BUSY before giving up. Two separate `pallium workflow run`
// processes each open their own *sql.DB handle onto the same file (see
// Store.Open's SetMaxOpenConns(1)); PRAGMA busy_timeout already asks SQLite
// to wait out a transient cross-connection write lock, but this retry is a
// deliberate second layer so a rare missed wait doesn't surface as a spurious
// lock-acquisition failure unrelated to bug #34's actual contention.
const repoLockBusyRetries = 20

func isSQLiteBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// AcquireRepoLock takes the advisory edit lock on repoRoot for runID. It
// succeeds when: no lock is held, runID already holds it (idempotent
// re-entry for a run with more than one edit-intent agent), or the existing
// lock has gone stale (no refresh in longer than staleAfter, e.g. a crashed
// run). On success it returns (runID, true). When a different run holds a
// live lock it returns that run's id and false without taking the lock, so
// the caller can fail fast instead of proceeding.
func (s *Store) AcquireRepoLock(repoRoot, runID string, staleAfter time.Duration) (string, bool, error) {
	var holder string
	var ok bool
	var err error
	for attempt := 0; attempt <= repoLockBusyRetries; attempt++ {
		holder, ok, err = s.acquireRepoLockOnce(repoRoot, runID, staleAfter)
		// Retry on SQLITE_BUSY and on contention (a concurrent acquire changed
		// the row between our read and write): the next attempt re-reads and
		// reports the live holder, so contention resolves into a clean
		// (holder, false) fail-fast rather than a raw error or a double-grant.
		if err == nil || (!isSQLiteBusyErr(err) && !errors.Is(err, errRepoLockContended)) {
			return holder, ok, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return holder, ok, err
}

func (s *Store) acquireRepoLockOnce(repoRoot, runID string, staleAfter time.Duration) (string, bool, error) {
	repoRoot = strings.TrimSpace(repoRoot)
	runID = strings.TrimSpace(runID)
	if repoRoot == "" || runID == "" {
		return "", false, fmt.Errorf("workflow repo lock requires a repo root and run id")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var holder, updatedAt string
	err = tx.QueryRow(`SELECT run_id, updated_at FROM workflow_repo_locks WHERE repo_root=?`, repoRoot).Scan(&holder, &updatedAt)
	now := nowString()
	switch {
	case err == sql.ErrNoRows:
		// No lock yet. If a concurrent run inserts first we lose the primary-key
		// race (isSQLiteConstraintErr) — that is contention, retried above.
		if _, ierr := tx.Exec(`INSERT INTO workflow_repo_locks(repo_root,run_id,acquired_at,updated_at) VALUES(?,?,?,?)`, repoRoot, runID, now, now); ierr != nil {
			if isSQLiteConstraintErr(ierr) {
				return "", false, errRepoLockContended
			}
			return "", false, ierr
		}
		return runID, true, tx.Commit()
	case err != nil:
		return "", false, err
	}
	if holder == runID {
		// Idempotent re-entry. Guard on run_id so a stale-takeover that
		// reassigned the row between our SELECT and this UPDATE cannot let us
		// refresh (and believe we hold) the NEW holder's lock.
		res, uerr := tx.Exec(`UPDATE workflow_repo_locks SET updated_at=? WHERE repo_root=? AND run_id=?`, now, repoRoot, runID)
		if uerr != nil {
			return "", false, uerr
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return "", false, errRepoLockContended
		}
		return holder, true, tx.Commit()
	}
	stale := staleAfter > 0
	if stale {
		if parsed, perr := time.Parse(time.RFC3339, updatedAt); perr == nil {
			stale = time.Since(parsed) > staleAfter
		}
	}
	if !stale {
		return holder, false, tx.Rollback()
	}
	// Stale takeover as a compare-and-swap: only win if the row STILL shows the
	// exact holder+timestamp we observed. Two racers that both saw the same
	// stale row cannot both take over — the second's WHERE no longer matches
	// (RowsAffected 0) and it retries into a clean fail-fast.
	res, uerr := tx.Exec(`UPDATE workflow_repo_locks SET run_id=?,acquired_at=?,updated_at=? WHERE repo_root=? AND run_id=? AND updated_at=?`, runID, now, now, repoRoot, holder, updatedAt)
	if uerr != nil {
		return "", false, uerr
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", false, errRepoLockContended
	}
	return runID, true, tx.Commit()
}

// ReleaseRepoLock releases repoRoot's lock, but only if runID is still the
// holder — a run that lost its lock to a stale takeover must not delete the
// new holder's row.
func (s *Store) ReleaseRepoLock(repoRoot, runID string) error {
	var err error
	for attempt := 0; attempt <= repoLockBusyRetries; attempt++ {
		_, err = s.db.Exec(`DELETE FROM workflow_repo_locks WHERE repo_root=? AND run_id=?`, repoRoot, runID)
		if err == nil || !isSQLiteBusyErr(err) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}
