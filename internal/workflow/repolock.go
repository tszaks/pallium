package workflow

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

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
		if err == nil || !isSQLiteBusyErr(err) {
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
		if _, err := tx.Exec(`INSERT INTO workflow_repo_locks(repo_root,run_id,acquired_at,updated_at) VALUES(?,?,?,?)`, repoRoot, runID, now, now); err != nil {
			return "", false, err
		}
		return runID, true, tx.Commit()
	case err != nil:
		return "", false, err
	}
	if holder == runID {
		if _, err := tx.Exec(`UPDATE workflow_repo_locks SET updated_at=? WHERE repo_root=?`, now, repoRoot); err != nil {
			return "", false, err
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
	if _, err := tx.Exec(`UPDATE workflow_repo_locks SET run_id=?,acquired_at=?,updated_at=? WHERE repo_root=?`, runID, now, now, repoRoot); err != nil {
		return "", false, err
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
