package workflow

import (
	"errors"
	"os"
	"sort"
	"syscall"
	"time"
)

// isProcessAlive reports whether pid currently names a live process on this
// machine. syscall.Kill(pid, 0) sends no actual signal — it only asks the
// kernel whether the target exists and is signalable. ESRCH means gone;
// EPERM means it exists but is owned by a different user (still alive, just
// not ours to signal) — both darwin and linux (the only two release
// platforms, see scripts/build-release-assets.sh) support this identically,
// so no build-tag split is needed.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// ClaimRunOwnership records this process as the run's owner the moment it
// starts actually executing (Runner.Execute, right after the row flips to
// "running"). ReconcileStaleRuns treats a claimed row's liveness as a direct
// question — "is owner_pid still alive" — rather than a timing guess, so a
// run stuck on one long agent call is never mistaken for dead just because
// heartbeat_at hasn't moved recently.
func (s *Store) ClaimRunOwnership(runID string) error {
	now := nowString()
	_, err := s.db.Exec(`UPDATE workflow_runs SET owner_pid=?, heartbeat_at=?, updated_at=? WHERE id=?`, os.Getpid(), now, now, runID)
	return err
}

// HeartbeatRun refreshes heartbeat_at for a run that is still genuinely
// running. Callers besides FinishAgentStatus's automatic refresh (e.g. a
// long single-agent call with no completions yet) may call this directly to
// prove liveness without waiting for an agent to finish. No-ops harmlessly
// once the run has left "running".
func (s *Store) HeartbeatRun(runID string) error {
	_, err := s.db.Exec(`UPDATE workflow_runs SET heartbeat_at=? WHERE id=? AND status='running'`, nowString(), runID)
	return err
}

// ReconciledRun is one run ReconcileStaleRuns flipped to "interrupted", plus
// how many of its agents it swept along with it — reconciled-not-hidden: the
// caller (list/status/inspect/fleet/gc) prints exactly what changed instead
// of silently rewriting history.
type ReconciledRun struct {
	RunID            string `json:"run_id"`
	AgentsReconciled int    `json:"agents_reconciled"`
}

// ReconcileStaleRuns is safe to call on every list/status/inspect/fleet/gc
// invocation (mirrors ReconcileInterruptedMembers's "safe on every team
// run/attach" contract). Two passes:
//
//  1. Runs: a "running" row is stale when its owner_pid is recorded and no
//     longer alive (a killed `pallium workflow run` process — confirmed,
//     not a guess), OR it was never claimed at all (owner_pid==0, e.g. a
//     pre-0.9.15 row) and its heartbeat/created_at predates staleAfter. A
//     claimed row whose owner IS alive is never touched regardless of how
//     old heartbeat_at looks — one long agent call must not look dead.
//  2. Agents: any "running" agent whose parent run is no longer "running"
//     (either just reconciled above, or already stopped/failed/completed by
//     some other path while its own agent rows never got closed out) is
//     unambiguously orphaned — its run is definitionally done, so there is
//     no "give it more time" case to protect, unlike pass 1.
func (s *Store) ReconcileStaleRuns(staleAfter string) ([]ReconciledRun, error) {
	rows, err := s.db.Query(`SELECT id, COALESCE(owner_pid,0), COALESCE(heartbeat_at,''), created_at FROM workflow_runs WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id          string
		ownerPID    int
		heartbeatAt string
		createdAt   string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.ownerPID, &c.heartbeatAt, &c.createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	now := nowString()
	reconciledRunIDs := map[string]bool{}
	for _, c := range candidates {
		stale := false
		if c.ownerPID > 0 {
			stale = !isProcessAlive(c.ownerPID)
		} else {
			reference := c.heartbeatAt
			if reference == "" {
				reference = c.createdAt
			}
			stale = reference < staleAfter
		}
		if !stale {
			continue
		}
		res, err := s.db.Exec(`UPDATE workflow_runs SET status='interrupted', updated_at=? WHERE id=? AND status='running'`, now, c.id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			reconciledRunIDs[c.id] = true
		}
	}

	orphanRows, err := s.db.Query(`SELECT DISTINCT a.run_id FROM workflow_agents a JOIN workflow_runs r ON r.id = a.run_id WHERE a.status='running' AND r.status <> 'running'`)
	if err != nil {
		return nil, err
	}
	var orphanRunIDs []string
	for orphanRows.Next() {
		var id string
		if err := orphanRows.Scan(&id); err != nil {
			orphanRows.Close()
			return nil, err
		}
		orphanRunIDs = append(orphanRunIDs, id)
	}
	orphanRows.Close()

	agentsReconciled := map[string]int{}
	for _, runID := range orphanRunIDs {
		n, err := s.reconcileRunAgentsOnce(runID, now)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			agentsReconciled[runID] = n
		}
	}

	result := make(map[string]int, len(reconciledRunIDs)+len(agentsReconciled))
	for id := range reconciledRunIDs {
		result[id] = 0
	}
	for id, n := range agentsReconciled {
		result[id] = n
	}
	out := make([]ReconciledRun, 0, len(result))
	for id, n := range result {
		out = append(out, ReconciledRun{RunID: id, AgentsReconciled: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

// reconcileRunAgentsOnce sweeps one run's orphaned "running" agents inside a
// single transaction, matching ReconcileInterruptedMembers's per-row-tx shape
// so a crash mid-sweep cannot leave the count reported to a caller out of
// sync with what was actually written.
func (s *Store) reconcileRunAgentsOnce(runID, now string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE workflow_agents SET status='interrupted', updated_at=? WHERE run_id=? AND status='running'`, now, runID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, tx.Rollback()
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// DefaultStaleRunAfterMinutes is the auto-reconcile window used by
// list/status/inspect/fleet (workflow gc --stale exposes its own
// --stale-after-minutes for an operator who wants a different bulk-sweep
// window). 15 minutes matches team/loop's existing stale-takeover default.
const DefaultStaleRunAfterMinutes = 15

// StaleAfterString formats a stale-after cutoff the same way team/loop
// already do (time.RFC3339Nano, lexicographically comparable against the
// TEXT timestamps this package stores everywhere).
func StaleAfterString(minutesAgo int) string {
	if minutesAgo <= 0 {
		minutesAgo = DefaultStaleRunAfterMinutes
	}
	return time.Now().Add(-time.Duration(minutesAgo) * time.Minute).UTC().Format(time.RFC3339Nano)
}
