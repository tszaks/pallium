package workflow

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/tszaks/pallium/internal/sessionmemory"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrCallBudget = errors.New("knowledge model budget exhausted: 40 attempts per rolling 24 hours")
var ErrCallSlots = errors.New("knowledge model slots busy: two calls already running")

type KnowledgeMaintenanceStore struct{ DB *sql.DB }

func OpenKnowledgeMaintenance() (*KnowledgeMaintenanceStore, error) {
	path := ResolveStorePath("")
	if path == "" {
		path = sessionmemory.DefaultDBPath()
	}
	return OpenKnowledgeMaintenancePath(path)
}
func OpenKnowledgeMaintenancePath(path string) (*KnowledgeMaintenanceStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	for _, q := range []string{`PRAGMA busy_timeout=5000`, `PRAGMA journal_mode=WAL`, maintenanceSchema} {
		if _, err := conn.Exec(q); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return &KnowledgeMaintenanceStore{conn}, nil
}

const maintenanceSchema = `
CREATE TABLE IF NOT EXISTS knowledge_calls (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, started INTEGER NOT NULL, finished INTEGER, status TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, cost REAL);
CREATE TABLE IF NOT EXISTS knowledge_registrations (root TEXT PRIMARY KEY, enabled INTEGER NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, reasoning TEXT NOT NULL, observed TEXT NOT NULL DEFAULT '', changed INTEGER NOT NULL DEFAULT 0, indexed TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '', updated INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS knowledge_jobs (id INTEGER PRIMARY KEY, root TEXT NOT NULL, slug TEXT NOT NULL, fingerprint TEXT NOT NULL, stage TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'queued', attempts INTEGER NOT NULL DEFAULT 0, updated INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', UNIQUE(root,slug,fingerprint,stage));
CREATE TABLE IF NOT EXISTS knowledge_context_requests(root TEXT NOT NULL, slug TEXT NOT NULL, requested INTEGER NOT NULL, PRIMARY KEY(root,slug));
CREATE INDEX IF NOT EXISTS knowledge_calls_started ON knowledge_calls(started);
`

func (m *KnowledgeMaintenanceStore) Reserve(repo, provider, model string, now time.Time) (int64, error) {
	// One atomic conditional write provides the same hard cap across processes.
	result, err := m.DB.Exec(`INSERT INTO knowledge_calls(repo,started,status,provider,model)
 SELECT ?,?,'running',?,? WHERE
 (SELECT COUNT(*) FROM knowledge_calls WHERE started>?)<40 AND
 (SELECT COUNT(*) FROM knowledge_calls WHERE status='running' AND started>?)<2`, repo, now.Unix(), provider, model, now.Add(-24*time.Hour).Unix(), now.Add(-4*time.Minute).Unix())
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		var used int
		if err := m.DB.QueryRow(`SELECT COUNT(*) FROM knowledge_calls WHERE started>?`, now.Add(-24*time.Hour).Unix()).Scan(&used); err != nil {
			return 0, err
		}
		if used >= 40 {
			return 0, ErrCallBudget
		}
		return 0, ErrCallSlots
	}
	return result.LastInsertId()
}
func (m *KnowledgeMaintenanceStore) Finish(id int64, status string) error {
	_, err := m.DB.Exec(`UPDATE knowledge_calls SET finished=?,status=? WHERE id=?`, time.Now().Unix(), status, id)
	return err
}

type MaintenanceStatus struct {
	Limit         int            `json:"limit"`
	Used          int            `json:"used_24h"`
	Remaining     int            `json:"remaining"`
	Running       int            `json:"running"`
	Queued        int            `json:"queued"`
	Failed        int            `json:"failed"`
	Cost          *float64       `json:"cost"`
	Registrations []Registration `json:"repositories"`
}
type Registration struct {
	Root      string `json:"root"`
	Enabled   bool   `json:"enabled"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Reasoning string `json:"reasoning"`
	Observed  string `json:"observed"`
	Changed   int64  `json:"changed_at"`
	Indexed   string `json:"indexed"`
	Error     string `json:"error,omitempty"`
	Updated   int64  `json:"updated_at"`
}

func (m *KnowledgeMaintenanceStore) Registrations() ([]Registration, error) {
	rows, err := m.DB.Query(`SELECT root,enabled,provider,model,reasoning,observed,changed,indexed,last_error,updated FROM knowledge_registrations ORDER BY updated,root`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registration{}
	for rows.Next() {
		var r Registration
		if err := rows.Scan(&r.Root, &r.Enabled, &r.Provider, &r.Model, &r.Reasoning, &r.Observed, &r.Changed, &r.Indexed, &r.Error, &r.Updated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (m *KnowledgeMaintenanceStore) Register(r Registration) error {
	if r.Root == "" {
		return fmt.Errorf("repository required")
	}
	_, err := m.DB.Exec(`INSERT INTO knowledge_registrations(root,enabled,provider,model,reasoning) VALUES(?,1,?,?,?) ON CONFLICT(root) DO UPDATE SET enabled=1,provider=excluded.provider,model=excluded.model,reasoning=excluded.reasoning,last_error=''`, r.Root, r.Provider, r.Model, r.Reasoning)
	return err
}
func (m *KnowledgeMaintenanceStore) Enable(root string, enabled bool) error {
	_, err := m.DB.Exec(`UPDATE knowledge_registrations SET enabled=?,last_error='' WHERE root=?`, enabled, root)
	return err
}
func (m *KnowledgeMaintenanceStore) Status() (MaintenanceStatus, error) {
	s := MaintenanceStatus{Limit: 40}
	now := time.Now()
	for _, q := range []struct {
		query  string
		target *int
		args   []any
	}{
		{`SELECT COUNT(*) FROM knowledge_calls WHERE started>?`, &s.Used, []any{now.Add(-24 * time.Hour).Unix()}},
		{`SELECT COUNT(*) FROM knowledge_calls WHERE status='running' AND started>?`, &s.Running, []any{now.Add(-4 * time.Minute).Unix()}},
		{`SELECT COUNT(*) FROM knowledge_jobs WHERE status='queued'`, &s.Queued, nil},
		{`SELECT COUNT(*) FROM knowledge_jobs WHERE status='failed'`, &s.Failed, nil},
	} {
		if err := m.DB.QueryRow(q.query, q.args...).Scan(q.target); err != nil {
			return s, err
		}
	}
	s.Remaining = max(0, 40-s.Used)
	var err error
	s.Registrations, err = m.Registrations()
	return s, err
}

// Prioritize records intent only for already enabled repositories.
func (m *KnowledgeMaintenanceStore) Prioritize(root string, slugs []string) error {
	for _, slug := range slugs {
		if _, err := m.DB.Exec(`INSERT INTO knowledge_context_requests(root,slug,requested) SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM knowledge_registrations WHERE root=? AND enabled=1) ON CONFLICT(root,slug) DO UPDATE SET requested=excluded.requested`, root, slug, time.Now().Unix(), root); err != nil {
			return err
		}
	}
	return nil
}

// ResolveStorePath is the shared safety redirect for every global-store service.
func ResolveStorePath(path string) string {
	if path == "" {
		if testDB := strings.TrimSpace(os.Getenv("PALLIUM_TEST_DB")); testDB != "" {
			return testDB
		}
	}
	return path
}
func (m *KnowledgeMaintenanceStore) Close() error { return m.DB.Close() }
func (m *KnowledgeMaintenanceStore) Retry(root string) error {
	_, err := m.DB.Exec(`UPDATE knowledge_jobs SET status='queued',attempts=0,error='' WHERE root=? AND status='failed'`, root)
	return err
}
func (m *KnowledgeMaintenanceStore) Recover() error {
	_, err := m.DB.Exec(`UPDATE knowledge_jobs SET status='queued' WHERE status='running'`)
	return err
}
func (m *KnowledgeMaintenanceStore) Observe(root, snapshot string, now int64) error {
	_, err := m.DB.Exec(`UPDATE knowledge_registrations SET observed=?,changed=?,updated=? WHERE root=?`, snapshot, now, now, root)
	return err
}
func (m *KnowledgeMaintenanceStore) SetError(root, message string, now int64) error {
	_, err := m.DB.Exec(`UPDATE knowledge_registrations SET last_error=?,updated=? WHERE root=?`, message, now, root)
	return err
}
func (m *KnowledgeMaintenanceStore) Indexed(root, snapshot string, now int64) error {
	_, err := m.DB.Exec(`UPDATE knowledge_registrations SET indexed=?,last_error='',updated=? WHERE root=?`, snapshot, now, root)
	return err
}
func (m *KnowledgeMaintenanceStore) Enqueue(root, slug, fingerprint, stage string) error {
	_, err := m.DB.Exec(`INSERT OR IGNORE INTO knowledge_jobs(root,slug,fingerprint,stage) VALUES(?,?,?,?)`, root, slug, fingerprint, stage)
	return err
}

type KnowledgeJob struct {
	ID                       int64
	Slug, Fingerprint, Stage string
	Attempts                 int
}

func (m *KnowledgeMaintenanceStore) Next(root string) (KnowledgeJob, error) {
	var j KnowledgeJob
	err := m.DB.QueryRow(`SELECT id,slug,fingerprint,stage,attempts FROM knowledge_jobs WHERE root=? AND status='queued' ORDER BY COALESCE((SELECT requested FROM knowledge_context_requests r WHERE r.root=knowledge_jobs.root AND r.slug=knowledge_jobs.slug),0) DESC, CASE stage WHEN 'audit' THEN 0 ELSE 1 END,id LIMIT 1`, root).Scan(&j.ID, &j.Slug, &j.Fingerprint, &j.Stage, &j.Attempts)
	return j, err
}
func (m *KnowledgeMaintenanceStore) Start(id int64, now int64) error {
	_, err := m.DB.Exec(`UPDATE knowledge_jobs SET status='running',attempts=attempts+1,updated=? WHERE id=?`, now, id)
	return err
}
func (m *KnowledgeMaintenanceStore) Complete(id int64, status string, attempts int, message string, now int64) error {
	_, err := m.DB.Exec(`UPDATE knowledge_jobs SET status=?,attempts=?,error=?,updated=? WHERE id=?`, status, attempts, message, now, id)
	return err
}
