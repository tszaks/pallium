package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/codeindex"
	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
	"github.com/tszaks/pallium/internal/index"
	"golang.org/x/sys/unix"
)

// Maintain runs independently of any editor/browser. Registry and queue survive
// restarts. File locks release on process death, unlike stale PID files.
func Maintain(ctx context.Context, once bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Join(home, ".pallium"), 0700)
	lock, err := os.OpenFile(filepath.Join(home, ".pallium", "knowledge-daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("knowledge daemon already running: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	m, err := OpenMaintenance()
	if err != nil {
		return err
	}
	defer m.Close()
	// The singleton lock proves no previous daemon is still executing a job.
	if err := m.Recover(); err != nil {
		return err
	}
	for {
		regs, err := m.Registrations()
		if err != nil {
			return err
		}
		for _, r := range regs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !r.Enabled {
				continue
			}
			err := maintenanceTick(m, ctx, r, time.Now())
			if err != nil {
				message := err.Error()
				if len(message) > 400 {
					message = message[:400]
				}
				if e := m.SetError(r.Root, message, time.Now().Unix()); e != nil {
					return e
				}
			}
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Second):
		}
	}
}

func maintenanceTick(m *MaintenanceStore, ctx context.Context, r Registration, now time.Time) error {
	snapshot, err := Snapshot(r.Root)
	if err != nil {
		return err
	}
	if snapshot != r.Observed {
		err = m.Observe(r.Root, snapshot, now.Unix())
		return err
	}
	if now.Unix()-r.Changed < 10 {
		return nil
	}
	store, err := index.OpenStore(r.Root)
	if err != nil {
		return err
	}
	defer store.Close()
	if r.Indexed != snapshot {
		// History is expensive and independent of dirty working-tree content.
		repo, repoErr := store.Repo()
		head, e := gitlog.CurrentCommit(r.Root)
		if e != nil {
			return e
		}
		if repoErr != nil || repo.LastIndexedCommit != head {
			if _, err := index.New(store).Run(); err != nil {
				return err
			}
		} else {
			if err := store.WithTx(func(tx *db.Store) error { _, err := codeindex.Run(tx, repo.ID, now); return err }); err != nil {
				return err
			}
		}
		repo, err = store.Repo()
		if err != nil {
			return err
		}
		report, err := Build(store, repo.ID, r.Root, BuildOptions{})
		if err != nil {
			return err
		}
		if report.NeedsReindex > 0 {
			return fmt.Errorf("source changed while indexing; retry queued")
		}
		docs, err := store.KnowledgeDocs(repo.ID)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			if doc.Kind != "module" {
				continue
			}
			stage := "generate"
			if doc.Generator == "structural+model" {
				stage = "audit"
			}
			if doc.Evidence.State == "audited_supported" || doc.Evidence.State == "audited_unclear" || doc.Evidence.State == "claims_removed" {
				continue
			}
			if err := m.Enqueue(r.Root, doc.Slug, doc.Fingerprint, stage); err != nil {
				return err
			}
		}
		if err := m.Indexed(r.Root, snapshot, now.Unix()); err != nil {
			return err
		}
		return nil
	}
	if now.Unix()-r.Changed < 60 {
		return nil
	}
	// One job per repository per round prevents a large repo starving others.
	job, err := m.Next(r.Root)
	id, slug, fingerprint, stage, attempts := job.ID, job.Slug, job.Fingerprint, job.Stage, job.Attempts
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	repo, err := store.Repo()
	if err != nil {
		return err
	}
	doc, found, err := store.KnowledgeDoc(repo.ID, slug)
	if err != nil {
		return err
	}
	if !found || doc.Fingerprint != fingerprint {
		err = m.Complete(id, "superseded", attempts, "", now.Unix())
		return err
	}
	if err := m.Start(id, now.Unix()); err != nil {
		return err
	}
	synth := ProviderSynthesizer{RepoRoot: r.Root, Provider: r.Provider, Model: r.Model, Reasoning: r.Reasoning}
	if stage == "generate" {
		report, e := Build(store, repo.ID, r.Root, BuildOptions{Context: ctx, Synth: synth, Only: []string{slug}, Concurrency: 1})
		err = e
		if err == nil && len(report.Failures) > 0 {
			err = errors.New(report.Failures[0].Reason)
		}
		if err == nil {
			err = m.Enqueue(r.Root, slug, fingerprint, "audit")
		}
	} else {
		report, e := Audit(store, repo.ID, r.Root, AuditOptions{Context: ctx, Synth: synth, Only: []string{slug}, Concurrency: 1})
		err = e
		if err == nil && len(report.Failures) > 0 {
			err = errors.New(report.Failures[0].Reason)
		}
	}
	status := "done"
	message := ""
	if err != nil {
		message = err.Error()
		status = "failed"
		lower := strings.ToLower(message)
		if strings.Contains(lower, "budget exhausted") || strings.Contains(lower, "slots busy") {
			status = "queued"
			attempts--
		} else if strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication") || strings.Contains(lower, "quota") || strings.Contains(lower, "rate limit") {
			if e := m.Enable(r.Root, false); e != nil {
				return e
			}
		} else if attempts < 1 && (strings.Contains(lower, "timeout") || strings.Contains(lower, "temporar") || strings.Contains(lower, "connection")) {
			status = "queued"
		}
	}
	if len(message) > 400 {
		message = message[:400]
	}
	saveErr := m.Complete(id, status, attempts+1, message, time.Now().Unix())
	if saveErr != nil {
		return saveErr
	}
	if err != nil {
		return err
	}
	err = m.SetError(r.Root, "", time.Now().Unix())
	return err
}
