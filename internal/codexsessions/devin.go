package codexsessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Devin CLI session discovery. Sessions live in a SQLite database
// (~/.local/share/devin/cli/sessions.db), not per-session transcript files
// like codex rollouts or claude project JSONLs:
//
//	sessions      — id, working_directory, model, agent_mode, title,
//	                created_at/last_activity_at (unix seconds), hidden
//	message_nodes — one row per committed message; chat_message is a JSON
//	                {message_id, role, content, tool_calls, tool_call_id,
//	                metadata:{is_user_input, finish_reason, ...}}. A message
//	                can occupy several rows (it is re-committed as it
//	                updates), so tail scans order by row_id and dedupe by
//	                message_id, keeping the newest row.
//
// Lifecycle mapping onto sessionSignals: a user row with
// metadata.is_user_input marks a turn start (running); an assistant row
// with metadata.finish_reason "stop"/"max_tokens" marks it finished;
// finish_reason "tool_calls" leaves its tool_calls pending until a matching
// role="tool" row (tool_call_id) arrives — the same pending-tool →
// blocked/waiting/active classification codex and claude get.
const (
	devinSessionDBEnv          = "CHISEL_SESSION_DB"
	devinSessionDBRelativePath = ".local/share/devin/cli/sessions.db"
	devinSessionTailLimit      = 300
)

// devinDBPathFunc is a var so tests can point discovery at a fixture
// database; listLiveDevinProcessesVar lives with the other test seams in
// sessions.go.
var devinDBPathFunc = devinDBPath

// devinDBPath resolves the sessions database. CHISEL_SESSION_DB (set by the
// CLI itself for its subprocesses) wins so a nonstandard data dir is
// honored; otherwise the documented default location.
func devinDBPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(devinSessionDBEnv)); p != "" {
		return p, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, devinSessionDBRelativePath), nil
}

type devinSessionRow struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Workdir      string `json:"working_directory"`
	Model        string `json:"model"`
	AgentMode    string `json:"agent_mode"`
	CreatedAt    int64  `json:"created_at"`
	LastActivity int64  `json:"last_activity_at"`
}

type devinMessageRow struct {
	RowID       int64  `json:"row_id"`
	CreatedAt   int64  `json:"created_at"`
	ChatMessage string `json:"chat_message"`
}

type devinChatMessage struct {
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
		IsUserInput  *bool  `json:"is_user_input"`
		FinishReason string `json:"finish_reason"`
	} `json:"metadata"`
}

func collectDevinSessions(ctx context.Context, opts SessionCollectOptions, generatedAt time.Time) ([]SessionSummary, error) {
	dbPath, err := devinDBPathFunc()
	if err != nil {
		return nil, err
	}

	liveProcesses, err := listLiveDevinProcessesVar(ctx)
	if err != nil {
		return nil, err
	}
	if len(liveProcesses) == 0 && !opts.IncludeAll {
		return []SessionSummary{}, nil
	}

	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			sessions := make([]SessionSummary, 0, len(liveProcesses))
			for _, proc := range liveProcesses {
				sessions = append(sessions, startingDevinSession(proc, generatedAt))
			}
			return sessions, nil
		}
		return nil, fmt.Errorf("failed to access devin sessions database: %w", err)
	}

	hasSessions, err := sqliteTableExists(ctx, dbPath, "sessions")
	if err != nil {
		return nil, err
	}
	if !hasSessions {
		sessions := make([]SessionSummary, 0, len(liveProcesses))
		for _, proc := range liveProcesses {
			sessions = append(sessions, startingDevinSession(proc, generatedAt))
		}
		return sessions, nil
	}

	rows, err := querySQLiteRows[devinSessionRow](ctx, dbPath,
		"SELECT id, substr(COALESCE(title,''),1,500) AS title, working_directory, COALESCE(model,'') AS model, COALESCE(agent_mode,'') AS agent_mode, created_at, last_activity_at FROM sessions WHERE hidden = 0 ORDER BY last_activity_at DESC;")
	if err != nil {
		return nil, err
	}

	summaries := make([]SessionSummary, 0, len(rows))
	for _, row := range rows {
		workdir := normalizeDevinWorkdir(row.Workdir)
		summaries = append(summaries, SessionSummary{
			Provider:         providerDevin,
			ThreadID:         row.ID,
			Title:            compactSummaryText(firstNonEmpty(row.Title, row.ID), 240),
			SessionCWD:       workdir,
			EffectiveWorkdir: workdir,
			LastActiveAt:     unixSecondsToTime(row.LastActivity),
			Status:           inactiveSessionStatus,
		})
	}

	activeByID := matchActiveSessions(summaries, liveProcesses, generatedAt, startingDevinSession)
	sessions := make([]SessionSummary, 0, len(summaries)+len(liveProcesses))
	seen := make(map[string]bool, len(summaries))

	for _, session := range summaries {
		if active, ok := activeByID[session.ThreadID]; ok {
			session = active
			enrichDevinSessionFromTail(ctx, &session, dbPath, opts.IncludeDetails, generatedAt)
		} else if !opts.IncludeAll {
			continue
		} else if opts.IncludeCompletion {
			enrichDevinSessionFromTail(ctx, &session, dbPath, false, generatedAt)
		}
		sessions = append(sessions, session)
		seen[session.ThreadID] = true
	}

	for _, session := range activeByID {
		if !seen[session.ThreadID] {
			sessions = append(sessions, session)
		}
	}

	return sessions, nil
}

// normalizeDevinWorkdir resolves symlinks (macOS /tmp → /private/tmp) so a
// stored working_directory compares equal to a live process's CWD, which
// lsof already returns resolved — findSessionForProcess matches on exact
// string equality.
func normalizeDevinWorkdir(dir string) string {
	if dir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

func startingDevinSession(proc liveAgentProcess, generatedAt time.Time) SessionSummary {
	session := SessionSummary{
		Provider:         providerDevin,
		PID:              proc.PID,
		PPID:             proc.PPID,
		TTY:              proc.TTY,
		ProcessState:     proc.State,
		AgeSeconds:       proc.AgeSeconds,
		Title:            startingSessionTitle,
		SessionCWD:       proc.CWD,
		EffectiveWorkdir: proc.CWD,
	}
	applySessionState(&session, sessionSignals{}, generatedAt)
	return session
}

// enrichDevinSessionFromTail scans the session's most recent message_nodes
// rows (chronologically) for lifecycle and pending-tool signals, plus a
// recent-action string when includeDetails is set. Read-only against a live
// database via the sqlite3 CLI, matching the codex collector's approach.
func enrichDevinSessionFromTail(ctx context.Context, session *SessionSummary, dbPath string, includeDetails bool, generatedAt time.Time) {
	signals := sessionSignals{Source: "devin-transcript"}
	rows, err := querySQLiteRows[devinMessageRow](ctx, dbPath, fmt.Sprintf(
		"SELECT row_id, created_at, chat_message FROM message_nodes WHERE session_id = %s ORDER BY row_id DESC LIMIT %d;",
		sqliteQuote(session.ThreadID), devinSessionTailLimit))
	if err != nil {
		applySessionState(session, signals, generatedAt)
		return
	}
	// Rows arrive newest-first. A message is re-committed as it updates
	// (same message_id, later row_id), so the FIRST row seen on this
	// newest→oldest pass is the final state — dedupe here, then replay the
	// survivors oldest→newest for the same forward scan the transcript-file
	// collectors do.
	type timedMessage struct {
		msg devinChatMessage
		at  time.Time
	}
	deduped := make([]timedMessage, 0, len(rows))
	seenMessages := map[string]bool{}
	for i := range rows {
		var msg devinChatMessage
		if json.Unmarshal([]byte(rows[i].ChatMessage), &msg) != nil {
			continue
		}
		if msg.MessageID != "" {
			if seenMessages[msg.MessageID] {
				continue
			}
			seenMessages[msg.MessageID] = true
		}
		deduped = append(deduped, timedMessage{msg: msg, at: unixSecondsToTime(rows[i].CreatedAt)})
	}
	if includeDetails && session.FirstUserMessage == "" {
		session.FirstUserMessage = devinFirstUserMessage(ctx, dbPath, session.ThreadID)
	}
	pending := map[string]struct {
		name string
		at   time.Time
	}{}
	for i := len(deduped) - 1; i >= 0; i-- {
		msg := deduped[i].msg
		at := deduped[i].at
		if at.After(signals.LatestAt) {
			signals.LatestAt = at
		}
		switch msg.Role {
		case "user":
			if msg.Metadata.IsUserInput != nil && *msg.Metadata.IsUserInput {
				signals.Lifecycle = lifecycleRunning
				signals.LifecycleAt = at
				clear(pending)
			}
		case "assistant":
			for _, call := range msg.ToolCalls {
				if call.ID != "" {
					pending[call.ID] = struct {
						name string
						at   time.Time
					}{name: call.Name, at: at}
				}
			}
			if msg.Metadata.FinishReason == "stop" || msg.Metadata.FinishReason == "max_tokens" {
				signals.Lifecycle = lifecycleFinished
				signals.LifecycleAt = at
				clear(pending)
			}
			if includeDetails {
				if action := devinRecentAction(msg); action != "" {
					session.RecentAction = action
				}
			}
		case "tool":
			if msg.ToolCallID != "" {
				delete(pending, msg.ToolCallID)
			}
		}
	}
	for _, call := range pending {
		if call.at.After(signals.PendingSince) {
			signals.PendingTool = call.name
			signals.PendingSince = call.at
		}
	}
	applySessionState(session, signals, generatedAt)
}

// devinFirstUserMessage pulls the session's first user prompt from
// prompt_history (the sessions table carries no first_message column, so
// the --details field needs this extra lookup).
func devinFirstUserMessage(ctx context.Context, dbPath, sessionID string) string {
	type promptRow struct {
		Content string `json:"content"`
	}
	rows, err := querySQLiteRows[promptRow](ctx, dbPath, fmt.Sprintf(
		"SELECT substr(content,1,2000) AS content FROM prompt_history WHERE session_id = %s AND is_shell = 0 ORDER BY id ASC LIMIT 1;",
		sqliteQuote(sessionID)))
	if err != nil || len(rows) == 0 {
		return ""
	}
	return compactSummaryText(rows[0].Content, 500)
}

// devinRecentAction renders an assistant message's tool call for the
// `sessions live --details` recent-action field. Devin's exec tool takes
// {"command": "..."} (codex's is "cmd"), so both keys are tried.
func devinRecentAction(msg devinChatMessage) string {
	for _, call := range msg.ToolCalls {
		if call.Name == "" {
			continue
		}
		if cmd := compactWhitespace(firstNonEmpty(stringArg(call.Arguments, "command"), stringArg(call.Arguments, "cmd"))); cmd != "" {
			return call.Name + ": " + cmd
		}
		if detail := compactWhitespace(firstNonEmpty(stringArg(call.Arguments, "message"), stringArg(call.Arguments, "query"), stringArg(call.Arguments, "path"), stringArg(call.Arguments, "file_path"))); detail != "" {
			return call.Name + ": " + detail
		}
		return call.Name
	}
	return ""
}

func listLiveDevinProcesses(ctx context.Context) ([]liveAgentProcess, error) {
	return listLiveAgentProcesses(ctx, providerDevin, looksLikeDevinCommand, true)
}

func looksLikeDevinCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	base := filepath.Base(strings.ReplaceAll(command, "\\", "/"))
	return base == "devin" || base == "devin.exe"
}
