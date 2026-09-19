package sessionmemory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Devin CLI session indexing. Unlike codex (per-session rollout JSONL) and
// claude (per-session transcript JSONL), Devin stores every session in one
// SQLite database at ~/.local/share/devin/cli/sessions.db (CHISEL_SESSION_DB
// overrides it). sessions carries the row-level metadata; message_nodes
// carries conversation history where each row's chat_message is a JSON
// {message_id, role, content, tool_calls, tool_call_id, metadata}.
//
// Because there is no per-session file, the normalized record uses a
// synthetic rollout_path ("devin-cli://<session-id>") as the stable
// identity for skip detection and tombstones, and rollout_sha256 holds a
// fingerprint of the session's message state (last activity + row count +
// max row_id) rather than a file hash — a re-committed message bumps
// row_id, so the fingerprint changes exactly when the content does.
const (
	devinSessionDBEnv        = "CHISEL_SESSION_DB"
	devinSessionDBRelative   = ".local/share/devin/cli/sessions.db"
	devinRolloutPathPrefix   = "devin-cli://"
	devinIndexQuietPeriod    = activeRolloutQuietPeriod
	devinMaxMessagesPerQuery = 100_000
)

var devinToolErrorPattern = regexp.MustCompile(`(?i)(error|traceback|failed|exception|permission denied)`)

// DefaultDevinDBPath resolves the Devin sessions database. CHISEL_SESSION_DB
// (the same env the CLI itself sets) wins so nonstandard data dirs work;
// otherwise the documented default location. Returns "" when the platform
// layout can't place it (no home dir).
func DefaultDevinDBPath() string {
	if p := strings.TrimSpace(os.Getenv(devinSessionDBEnv)); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, devinSessionDBRelative)
	}
	return ""
}

func devinRolloutPath(sessionID string) string {
	return devinRolloutPathPrefix + sessionID
}

type devinIndexRow struct {
	id           string
	title        string
	workdir      string
	model        string
	agentMode    string
	createdAt    int64
	lastActivity int64
	sessionMeta  string
}

type devinNodeRow struct {
	nodeID      int64
	rowID       int64
	createdAt   int64
	chatMessage string
}

// indexDevinSessions is the devin branch of Index: enumerate sessions in the
// CLI database, apply the same tombstone/active/unchanged skip semantics the
// file-backed providers get, then parse + upsert. A missing database is not
// an error — the provider simply has nothing to index (same as findRollouts
// on a missing root).
func indexDevinSessions(ctx context.Context, store *Store, opts Options, cutoff time.Time) (int, error) {
	dbPath := opts.DevinDBPath
	if dbPath == "" {
		dbPath = DefaultDevinDBPath()
	}
	if dbPath == "" {
		return 0, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("access devin sessions database: %w", err)
	}
	// mode=ro: indexing must never write to (or even create WAL files
	// alongside) the live CLI database.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()

	cutoffUnix := int64(0)
	if !cutoff.IsZero() {
		cutoffUnix = cutoff.Unix()
	}
	rows, err := db.Query(`SELECT id, COALESCE(title,''), COALESCE(working_directory,''), COALESCE(model,''), COALESCE(agent_mode,''), created_at, last_activity_at, COALESCE(metadata,'')
		FROM sessions WHERE hidden = 0 AND last_activity_at >= ? ORDER BY last_activity_at`, cutoffUnix)
	if err != nil {
		return 0, fmt.Errorf("query devin sessions: %w", err)
	}
	defer rows.Close()
	var sessions []devinIndexRow
	for rows.Next() {
		var row devinIndexRow
		if err := rows.Scan(&row.id, &row.title, &row.workdir, &row.model, &row.agentMode, &row.createdAt, &row.lastActivity, &row.sessionMeta); err != nil {
			return 0, err
		}
		sessions = append(sessions, row)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, row := range sessions {
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		default:
		}
		rolloutPath := devinRolloutPath(row.id)
		tombstoned, err := store.sessionTombstoned(row.id, rolloutPath)
		if err != nil {
			return count, err
		}
		if tombstoned {
			continue
		}
		// Sessions touched inside the quiet period are still being written —
		// same skip the file-backed providers apply to recently-modified
		// transcripts.
		if !opts.Force && time.Since(time.Unix(row.lastActivity, 0)) < devinIndexQuietPeriod {
			continue
		}
		fingerprint, err := devinSessionFingerprint(db, row)
		if err != nil {
			return count, err
		}
		if !opts.Force {
			unchanged, err := store.devinSessionUnchanged(rolloutPath, fingerprint)
			if err != nil {
				return count, err
			}
			if unchanged {
				continue
			}
		}
		parsed, err := parseDevinSession(db, row, fingerprint)
		if err != nil {
			return count, fmt.Errorf("parse devin session %s: %w", row.id, err)
		}
		parsed.Session.Machine = opts.Machine
		metadata := map[string]any{"provider": "devin"}
		if row.agentMode != "" {
			metadata["agent_mode"] = row.agentMode
		}
		if acu := devinSessionACU(row.sessionMeta); acu != 0 {
			metadata["acu_cost"] = acu
		}
		if !opts.StoreRawEvents {
			parsed.RawEvents = nil
		}
		if err := store.upsert(parsed, metadata); err != nil {
			if errors.Is(err, errSessionTombstoned) {
				continue
			}
			return count, err
		}
		count++
	}
	return count, nil
}

// devinSessionUnchanged reports whether the stored fingerprint for this
// session's synthetic rollout_path already matches the live one.
func (s *Store) devinSessionUnchanged(rolloutPath, fingerprint string) (bool, error) {
	row := s.db.QueryRow(`SELECT rollout_sha256 FROM codex_sessions WHERE rollout_path=? LIMIT 1`, rolloutPath)
	var stored string
	switch err := row.Scan(&stored); err {
	case nil:
		return stored != "" && stored == fingerprint, nil
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, fmt.Errorf("lookup indexed devin session %s: %w", rolloutPath, err)
	}
}

// devinSessionFingerprint hashes the cheap change signals for a session —
// last activity timestamp, message row count, and the newest row_id (which
// advances on every commit, including re-commits of an updating message) —
// so unchanged sessions skip without re-reading their full history.
func devinSessionFingerprint(db *sql.DB, row devinIndexRow) (string, error) {
	var maxRowID, messageCount int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(row_id),0), COUNT(*) FROM message_nodes WHERE session_id=?`, row.id).Scan(&maxRowID, &messageCount); err != nil {
		return "", fmt.Errorf("fingerprint devin session %s: %w", row.id, err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("devin|%s|%d|%d|%d", row.id, row.lastActivity, messageCount, maxRowID)))
	return fmt.Sprintf("%x", sum), nil
}

// devinSessionACU pulls total_acu_cost out of the sessions.metadata JSON —
// recorded as session metadata so budget/accounting surfaces can report ACU
// without pretending it maps to USD.
func devinSessionACU(raw string) float64 {
	if raw == "" {
		return 0
	}
	var meta struct {
		TotalACUCost float64 `json:"total_acu_cost"`
	}
	if json.Unmarshal([]byte(raw), &meta) != nil {
		return 0
	}
	return meta.TotalACUCost
}

type devinChatMessageJSON struct {
	MessageID  string `json:"message_id"`
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"tool_calls"`
	Metadata struct {
		IsUserInput  *bool          `json:"is_user_input"`
		FinishReason string         `json:"finish_reason"`
		Extensions   map[string]any `json:"extensions"`
	} `json:"metadata"`
}

func parseDevinSession(db *sql.DB, row devinIndexRow, fingerprint string) (ParsedSession, error) {
	p := newParsedSession(0)
	p.Session.ID = row.id
	p.Session.RolloutPath = devinRolloutPath(row.id)
	p.Session.RolloutSHA256 = fingerprint
	p.Session.Source = "devin"
	p.Session.ModelProvider = "devin"
	p.Session.Model = row.model
	p.Session.CWD = row.workdir
	p.Session.Title = normalizeUserText(row.title)
	if row.createdAt > 0 {
		p.Session.CreatedAt = time.Unix(row.createdAt, 0).UTC().Format(time.RFC3339Nano)
	}
	if row.lastActivity > 0 {
		p.Session.UpdatedAt = time.Unix(row.lastActivity, 0).UTC().Format(time.RFC3339Nano)
	}
	p.Session.Status = "seen"

	rows, err := db.Query(`SELECT node_id, row_id, created_at, chat_message FROM message_nodes WHERE session_id=? ORDER BY node_id, row_id LIMIT ?`, row.id, devinMaxMessagesPerQuery)
	if err != nil {
		return ParsedSession{}, err
	}
	defer rows.Close()

	// A message can be committed repeatedly while streaming (same
	// message_id, later row_id). Keep the latest row per message_id, then
	// emit in node order — sequential line numbers so message FTS primary
	// keys never collide even if node_ids did.
	latest := map[string]int{} // message key -> index into nodes
	var nodes []devinNodeRow
	var rawBytes int64
	for rows.Next() {
		var node devinNodeRow
		if err := rows.Scan(&node.nodeID, &node.rowID, &node.createdAt, &node.chatMessage); err != nil {
			return ParsedSession{}, err
		}
		rawBytes += int64(len(node.chatMessage))
		var msg devinChatMessageJSON
		if json.Unmarshal([]byte(node.chatMessage), &msg) != nil {
			continue
		}
		key := msg.MessageID
		if key == "" {
			key = fmt.Sprintf("row:%d", node.rowID)
		}
		if idx, ok := latest[key]; ok {
			nodes[idx] = node
			continue
		}
		latest[key] = len(nodes)
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return ParsedSession{}, err
	}
	p.Coverage.RawBytes = rawBytes

	files := map[string]bool{}
	tools := map[string]bool{}
	lineNo := 0
	for _, node := range nodes {
		var msg devinChatMessageJSON
		if json.Unmarshal([]byte(node.chatMessage), &msg) != nil {
			continue
		}
		lineNo++
		ts := ""
		if node.createdAt > 0 {
			ts = time.Unix(node.createdAt, 0).UTC().Format(time.RFC3339Nano)
		}
		p.EventCounts["devin:"+msg.Role]++
		if len(p.RawEvents) < maxStoredRawEventsPerSession {
			rawJSON := node.chatMessage
			if len(rawJSON) > maxStoredRawEventJSON {
				rawJSON = truncate(rawJSON, maxStoredRawEventJSON) + fmt.Sprintf("\n...[truncated raw event from %d bytes]", len(node.chatMessage))
			}
			p.RawEvents = append(p.RawEvents, RawEvent{lineNo, ts, "devin:" + msg.Role, "", redact(rawJSON)})
		}
		// A single node can emit several stored messages (assistant text +
		// each tool call); sub-line numbering keeps (session_id, line_no)
		// unique, the same trick handleClaudeMessage uses for content items.
		subLine := 0
		emit := func(role, kind, text string) {
			subLine++
			appendParsedMessage(&p, Message{lineNo*1000 + subLine, ts, role, kind, text})
		}
		switch msg.Role {
		case "user":
			// Only genuine user input is indexed — non-input user-role rows
			// are injected context/system material, and indexing them as
			// user messages would pollute first_user_message and FTS.
			if msg.Metadata.IsUserInput == nil || !*msg.Metadata.IsUserInput {
				continue
			}
			text := capMessageText(normalizeUserText(msg.Content))
			if text == "" {
				continue
			}
			if p.Session.FirstUserMessage == "" {
				p.Session.FirstUserMessage = short(text, maxStoredFirstUserText)
			}
			emit("user", "message", text)
		case "assistant":
			if text := capMessageText(msg.Content); text != "" {
				p.Session.LastAgentMessage = short(text, maxStoredMessageText)
				emit("assistant", "message", text)
			}
			for _, call := range msg.ToolCalls {
				if call.Name == "" {
					continue
				}
				tools[call.Name] = true
				cmd := first(str(call.Arguments["command"]), str(call.Arguments["cmd"]))
				path := first(str(call.Arguments["file_path"]), str(call.Arguments["path"]), str(call.Arguments["notebook_path"]))
				if cmd != "" {
					appendSessionCommand(&p, cmd)
					addPaths(files, cmd)
				}
				if path != "" {
					files[path] = true
				}
				if text := capMessageText(first(cmd, path, compactJSON(call.Arguments))); text != "" {
					emit("tool", call.Name, text)
				}
			}
		case "tool":
			text := capMessageText(msg.Content)
			addPaths(files, text)
			if devinToolErrorPattern.MatchString(text) || devinToolFailed(msg.Metadata.Extensions) {
				appendSessionError(&p, short(text, 500))
			}
			if text != "" {
				emit("tool", "tool_result", text)
			}
		}
	}
	finalizeParsedMessages(&p)
	for f := range files {
		p.Session.FilesTouched = append(p.Session.FilesTouched, f)
	}
	sort.Strings(p.Session.FilesTouched)
	for t := range tools {
		p.Session.ToolNames = append(p.Session.ToolNames, t)
	}
	sort.Strings(p.Session.ToolNames)
	p.Session.Title = short(first(p.Session.Title, p.Session.FirstUserMessage, p.Session.ID), 240)
	p.SearchBlob = truncate(strings.Join([]string{p.Session.Title, p.Session.CWD, strings.Join(p.Session.Commands, "\n"), strings.Join(p.Session.FilesTouched, "\n"), messagesText(p.Messages)}, "\n"), maxSearchBlobText)
	return p, nil
}

// devinToolFailed reports whether the tool result's chisel/tool_result_meta
// extension recorded success=false — a structured failure signal the
// free-text error regex misses.
func devinToolFailed(extensions map[string]any) bool {
	raw, ok := extensions["chisel/tool_result_meta"].(map[string]any)
	if !ok {
		return false
	}
	success, ok := raw["success"].(bool)
	return ok && !success
}
