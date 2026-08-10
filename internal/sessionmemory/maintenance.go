package sessionmemory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SessionDoctorOptions struct {
	DBPath         string
	Repair         bool
	PruneRawEvents bool
	Vacuum         bool
}

type SessionRepairSummary struct {
	OrphanEmbeddingsRemoved int  `json:"orphan_embeddings_removed"`
	StaleEmbeddingsRemoved  int  `json:"stale_embeddings_removed"`
	RawEventsRemoved        int  `json:"raw_events_removed"`
	Vacuumed                bool `json:"vacuumed"`
}

type SessionDoctorReport struct {
	DBPath                 string               `json:"db_path"`
	DBExists               bool                 `json:"db_exists"`
	DBSizeBytes            int64                `json:"db_size_bytes"`
	DirectoryMode          string               `json:"directory_mode,omitempty"`
	FileMode               string               `json:"file_mode,omitempty"`
	Stats                  Stats                `json:"stats"`
	Embedding              EmbeddingStatus      `json:"embedding"`
	EmbeddingBacklog       int                  `json:"embedding_backlog"`
	OrphanEmbeddings       int                  `json:"orphan_embeddings"`
	StaleEmbeddings        int                  `json:"stale_embeddings"`
	StoredRawEvents        int                  `json:"stored_raw_events"`
	NoisyTitles            int                  `json:"noisy_titles"`
	OversizedFirstMessages int                  `json:"oversized_first_messages"`
	SkippedLargeSessions   int                  `json:"skipped_large_sessions"`
	MissingCapsules        int                  `json:"missing_capsules"`
	OutdatedCapsules       int                  `json:"outdated_capsules"`
	LatestIndexedAt        string               `json:"latest_indexed_at,omitempty"`
	LatestSessionUpdate    string               `json:"latest_session_update,omitempty"`
	Healthy                bool                 `json:"healthy"`
	Issues                 []string             `json:"issues"`
	Repair                 SessionRepairSummary `json:"repair"`
}

type ForgetResult struct {
	SessionID  string `json:"session_id"`
	Title      string `json:"title"`
	Messages   int    `json:"messages"`
	Chunks     int    `json:"chunks"`
	Embeddings int    `json:"embeddings"`
	Confirmed  bool   `json:"confirmed"`
	Deleted    bool   `json:"deleted"`
}

type SessionPruneResult struct {
	Cutoff    string `json:"cutoff"`
	Matched   int    `json:"matched"`
	Confirmed bool   `json:"confirmed"`
	Deleted   int    `json:"deleted"`
}

type sessionDeletionTarget struct {
	ID          string
	Source      string
	RolloutPath string
}

func DoctorSessions(opts SessionDoctorOptions) (SessionDoctorReport, error) {
	path := opts.DBPath
	if path == "" {
		path = DefaultDBPath()
	}
	report := SessionDoctorReport{DBPath: path}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			report.Issues = []string{"Session database does not exist. Run `pallium sessions sync`."}
			return report, nil
		}
		return report, err
	}
	report.DBExists = true

	store, err := Open(path)
	if err != nil {
		return report, err
	}
	defer store.Close()

	if opts.Repair {
		repair, err := store.repairSessions(opts)
		if err != nil {
			return report, err
		}
		report.Repair = repair
	}
	if opts.Vacuum {
		if !opts.Repair {
			return report, fmt.Errorf("--vacuum requires --repair")
		}
		if _, err := store.db.Exec(`VACUUM`); err != nil {
			return report, fmt.Errorf("vacuum session database: %w", err)
		}
		report.Repair.Vacuumed = true
	}

	if err := populateSessionDoctorReport(store, &report); err != nil {
		return report, err
	}
	return report, nil
}

func (s *Store) repairSessions(opts SessionDoctorOptions) (SessionRepairSummary, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return SessionRepairSummary{}, err
	}
	defer tx.Rollback()
	repair := SessionRepairSummary{}

	result, err := tx.Exec(`DELETE FROM codex_session_embeddings
		WHERE NOT EXISTS (SELECT 1 FROM codex_session_chunks c WHERE c.id=codex_session_embeddings.chunk_id)`)
	if err != nil {
		return repair, fmt.Errorf("remove orphan embeddings: %w", err)
	}
	repair.OrphanEmbeddingsRemoved = rowsAffected(result)

	result, err = tx.Exec(`DELETE FROM codex_session_embeddings
		WHERE EXISTS (SELECT 1 FROM codex_session_chunks c
			WHERE c.id=codex_session_embeddings.chunk_id
			AND c.text_sha256 != codex_session_embeddings.text_sha256)`)
	if err != nil {
		return repair, fmt.Errorf("remove stale embeddings: %w", err)
	}
	repair.StaleEmbeddingsRemoved = rowsAffected(result)

	if opts.PruneRawEvents {
		result, err = tx.Exec(`DELETE FROM codex_session_events`)
		if err != nil {
			return repair, fmt.Errorf("remove duplicated raw events: %w", err)
		}
		repair.RawEventsRemoved = rowsAffected(result)
	}

	if err := tx.Commit(); err != nil {
		return repair, err
	}
	if err := hardenDatabaseFiles(opts.DBPath); err != nil && opts.DBPath != "" {
		return repair, err
	}
	return repair, nil
}

func populateSessionDoctorReport(store *Store, report *SessionDoctorReport) error {
	path := report.DBPath
	if info, err := os.Stat(filepath.Dir(path)); err == nil {
		report.DirectoryMode = info.Mode().Perm().String()
	}
	if info, err := os.Stat(path); err == nil {
		report.FileMode = info.Mode().Perm().String()
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(candidate); err == nil {
			report.DBSizeBytes += info.Size()
		}
	}

	stats, err := statsFromStore(store)
	if err != nil {
		return err
	}
	report.Stats = stats
	report.Embedding = ReadEmbeddingStatus()
	report.EmbeddingBacklog, err = store.embeddingBacklog(report.Embedding.Model)
	if err != nil {
		return err
	}
	queries := []struct {
		dest  *int
		query string
	}{
		{&report.OrphanEmbeddings, `SELECT COUNT(*) FROM codex_session_embeddings e LEFT JOIN codex_session_chunks c ON c.id=e.chunk_id WHERE c.id IS NULL`},
		{&report.StaleEmbeddings, `SELECT COUNT(*) FROM codex_session_embeddings e JOIN codex_session_chunks c ON c.id=e.chunk_id WHERE e.text_sha256 != c.text_sha256`},
		{&report.StoredRawEvents, `SELECT COUNT(*) FROM codex_session_events`},
		{&report.NoisyTitles, `SELECT COUNT(*) FROM codex_sessions WHERE title LIKE '<recommended_plugins>%' OR title LIKE '<!-- pallium:agents:begin -->%' OR title LIKE '# AGENTS.md instructions%' OR lower(title) LIKE 'the following is the codex agent history whose request action you are assessing.%'`},
		{&report.OversizedFirstMessages, `SELECT COUNT(*) FROM codex_sessions WHERE length(first_user_message) > 10000`},
		{&report.SkippedLargeSessions, `SELECT COUNT(*) FROM codex_sessions WHERE status='skipped_large_rollout'`},
		{&report.MissingCapsules, `SELECT COUNT(*) FROM codex_sessions s LEFT JOIN codex_session_capsules c ON c.session_id=s.id WHERE c.session_id IS NULL`},
	}
	for _, item := range queries {
		if err := store.db.QueryRow(item.query).Scan(item.dest); err != nil {
			return err
		}
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_capsules WHERE schema_version < ?`, sessionCapsuleSchemaVersion).Scan(&report.OutdatedCapsules); err != nil {
		return err
	}
	_ = store.db.QueryRow(`SELECT COALESCE(MAX(indexed_at),'') FROM codex_sessions`).Scan(&report.LatestIndexedAt)
	_ = store.db.QueryRow(`SELECT COALESCE(MAX(COALESCE(updated_at,created_at)),'') FROM codex_sessions`).Scan(&report.LatestSessionUpdate)

	if report.DirectoryMode != "-rwx------" && report.DirectoryMode != "drwx------" {
		report.Issues = append(report.Issues, "Session database directory permissions are broader than 0700.")
	}
	if report.FileMode != "-rw-------" {
		report.Issues = append(report.Issues, "Session database file permissions are broader than 0600.")
	}
	if report.OrphanEmbeddings > 0 {
		report.Issues = append(report.Issues, "Orphan embeddings consume storage and cannot contribute valid results.")
	}
	if report.StaleEmbeddings > 0 {
		report.Issues = append(report.Issues, "Stale embeddings do not match their current chunk text.")
	}
	if report.EmbeddingBacklog > 0 {
		report.Issues = append(report.Issues, "Session embeddings are not fully caught up.")
	}
	if report.Embedding.ConfigError != "" {
		report.Issues = append(report.Issues, "Embedding configuration is invalid: "+report.Embedding.ConfigError)
	}
	if report.NoisyTitles > 0 || report.OversizedFirstMessages > 0 {
		report.Issues = append(report.Issues, "Injected context still pollutes searchable session metadata; run a forced sync after upgrading.")
	}
	if report.SkippedLargeSessions > 0 {
		report.Issues = append(report.Issues, "Some legacy oversized transcripts have only metadata coverage; run a forced sync to replace them with sampled continuity.")
	}
	if report.MissingCapsules > 0 {
		report.Issues = append(report.Issues, "Some sessions do not have continuity capsules; run a forced sync.")
	}
	if report.OutdatedCapsules > 0 {
		report.Issues = append(report.Issues, "Some continuity capsules use an older schema; run `pallium sessions sync`.")
	}
	report.Healthy = len(report.Issues) == 0
	return nil
}

func statsFromStore(store *Store) (Stats, error) {
	var stats Stats
	queries := []struct {
		dest  *int
		query string
	}{
		{&stats.Sessions, `SELECT COUNT(*) FROM codex_sessions`},
		{&stats.Events, `SELECT COUNT(*) FROM codex_session_events`},
		{&stats.Messages, `SELECT COUNT(*) FROM codex_session_messages`},
		{&stats.Chunks, `SELECT COUNT(*) FROM codex_session_chunks`},
		{&stats.Embeddings, `SELECT COUNT(*) FROM codex_session_embeddings`},
		{&stats.Capsules, `SELECT COUNT(*) FROM codex_session_capsules`},
	}
	for _, item := range queries {
		if err := store.db.QueryRow(item.query).Scan(item.dest); err != nil {
			return stats, err
		}
	}
	rows, err := store.db.Query(`SELECT provider, model, dim, COUNT(*) FROM codex_session_embeddings GROUP BY provider, model, dim ORDER BY COUNT(*) DESC`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var model EmbeddingModel
		if err := rows.Scan(&model.Provider, &model.Model, &model.Dim, &model.Count); err != nil {
			return stats, err
		}
		stats.Models = append(stats.Models, model)
	}
	return stats, rows.Err()
}

func ForgetSession(dbPath, prefix string, confirm bool) (ForgetResult, error) {
	store, err := Open(dbPath)
	if err != nil {
		return ForgetResult{}, err
	}
	defer store.Close()
	sessionID, err := store.resolveID(prefix)
	if err != nil {
		return ForgetResult{}, err
	}
	result := ForgetResult{SessionID: sessionID, Confirmed: confirm}
	target := sessionDeletionTarget{ID: sessionID}
	if err := store.db.QueryRow(`SELECT title,COALESCE(source,''),COALESCE(rollout_path,'') FROM codex_sessions WHERE id=?`, sessionID).Scan(&result.Title, &target.Source, &target.RolloutPath); err != nil {
		return result, err
	}
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_messages WHERE session_id=?`, sessionID).Scan(&result.Messages)
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_chunks WHERE session_id=?`, sessionID).Scan(&result.Chunks)
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM codex_session_embeddings e JOIN codex_session_chunks c ON c.id=e.chunk_id WHERE c.session_id=?`, sessionID).Scan(&result.Embeddings)
	if !confirm {
		return result, nil
	}

	tx, err := store.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := deleteSessionTx(tx, target, "forget"); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.Deleted = true
	return result, nil
}

func PruneSessions(dbPath string, olderThan time.Duration, confirm bool) (SessionPruneResult, error) {
	if olderThan <= 0 {
		return SessionPruneResult{}, fmt.Errorf("retention age must be greater than zero")
	}
	store, err := Open(dbPath)
	if err != nil {
		return SessionPruneResult{}, err
	}
	defer store.Close()
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339Nano)
	result := SessionPruneResult{Cutoff: cutoff, Confirmed: confirm}
	rows, err := store.db.Query(`SELECT id,COALESCE(source,''),COALESCE(rollout_path,'') FROM codex_sessions WHERE COALESCE(NULLIF(updated_at,''), NULLIF(created_at,''), indexed_at) < ? ORDER BY COALESCE(NULLIF(updated_at,''), NULLIF(created_at,''), indexed_at)`, cutoff)
	if err != nil {
		return result, err
	}
	var targets []sessionDeletionTarget
	for rows.Next() {
		var target sessionDeletionTarget
		if err := rows.Scan(&target.ID, &target.Source, &target.RolloutPath); err != nil {
			_ = rows.Close()
			return result, err
		}
		targets = append(targets, target)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	result.Matched = len(targets)
	if !confirm || len(targets) == 0 {
		return result, nil
	}
	tx, err := store.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for _, target := range targets {
		if err := deleteSessionTx(tx, target, "retention"); err != nil {
			return result, err
		}
		result.Deleted++
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func deleteSessionTx(tx *sql.Tx, target sessionDeletionTarget, reason string) error {
	rolloutPath := target.RolloutPath
	if strings.TrimSpace(rolloutPath) != "" {
		rolloutPath = filepath.Clean(rolloutPath)
	}
	if _, err := tx.Exec(`INSERT INTO codex_session_tombstones(session_id,source,rollout_path,deleted_at,reason) VALUES(?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET source=excluded.source,rollout_path=excluded.rollout_path,deleted_at=excluded.deleted_at,reason=excluded.reason`,
		target.ID, target.Source, rolloutPath, time.Now().UTC().Format(time.RFC3339Nano), reason); err != nil {
		return fmt.Errorf("record session deletion tombstone: %w", err)
	}
	statements := []string{
		`DELETE FROM codex_session_embeddings WHERE chunk_id IN (SELECT id FROM codex_session_chunks WHERE session_id=?)`,
		`DELETE FROM codex_session_chunks WHERE session_id=?`,
		`DELETE FROM codex_session_events WHERE session_id=?`,
		`DELETE FROM codex_session_messages WHERE session_id=?`,
		`DELETE FROM codex_session_fts WHERE session_id=?`,
		`DELETE FROM codex_message_fts WHERE session_id=?`,
		`DELETE FROM codex_session_capsules WHERE session_id=?`,
		`DELETE FROM codex_sessions WHERE id=?`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement, target.ID); err != nil {
			return err
		}
	}
	return nil
}

func rowsAffected(result sql.Result) int {
	count, err := result.RowsAffected()
	if err != nil {
		return 0
	}
	return int(count)
}
