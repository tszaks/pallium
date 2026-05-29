package codexsessions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	psCommand                 = "ps"
	sqlite3Command            = "sqlite3"
	codexStateDBFile          = "state_5.sqlite"
	sessionLogPreviewLimit    = 5
	codexToolLogTarget        = "codex_core::stream_events_utils"
	activeSessionStatus       = "active"
	inactiveSessionStatus     = "inactive"
	startingSessionTitle      = "(starting up)"
	defaultRecentActionSuffix = "ToolCall: "
)

var changeDirCommandRegex = regexp.MustCompile(`^\s*cd\s+(?:"([^"]+)"|'([^']+)'|([^&|;]+))\s*&&`)

var (
	nowFunc                   = time.Now
	hostnameFunc              = os.Hostname
	codexHomeDirFunc          = codexHomeDir
	listLiveCodexProcessesVar = listLiveCodexProcesses
)

type SessionCollectOptions struct {
	IncludeAll     bool
	IncludeDetails bool
}

type SessionSnapshot struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Host        string           `json:"host"`
	Sessions    []SessionSummary `json:"sessions"`
}

type SessionSummary struct {
	PID              int       `json:"pid,omitempty"`
	TTY              string    `json:"tty,omitempty"`
	AgeSeconds       int64     `json:"age_seconds,omitempty"`
	ThreadID         string    `json:"thread_id,omitempty"`
	Title            string    `json:"title,omitempty"`
	FirstUserMessage string    `json:"first_user_message,omitempty"`
	SessionCWD       string    `json:"session_cwd,omitempty"`
	EffectiveWorkdir string    `json:"effective_workdir,omitempty"`
	LastActiveAt     time.Time `json:"last_active_at,omitempty"`
	GitBranch        string    `json:"git_branch,omitempty"`
	GitOriginURL     string    `json:"git_origin_url,omitempty"`
	Status           string    `json:"status"`
	RecentAction     string    `json:"recent_action,omitempty"`
}

type liveCodexProcess struct {
	PID        int
	TTY        string
	AgeSeconds int64
}

type processThreadRow struct {
	ProcessUUID string `json:"process_uuid"`
	ThreadID    string `json:"thread_id"`
}

type threadRow struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	FirstUserMessage string `json:"first_user_message"`
	CWD              string `json:"cwd"`
	UpdatedAt        int64  `json:"updated_at"`
	GitBranch        string `json:"git_branch"`
	GitOriginURL     string `json:"git_origin_url"`
}

type threadLogRow struct {
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
}

type toolCall struct {
	Name string
	Args map[string]any
}

func CollectSessions(ctx context.Context, opts SessionCollectOptions) (*SessionSnapshot, error) {
	codexHome, err := codexHomeDirFunc()
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(codexHome, codexStateDBFile)
	generatedAt := nowFunc().UTC()
	host, _ := hostnameFunc()

	liveProcesses, err := listLiveCodexProcessesVar(ctx)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			sessions := make([]SessionSummary, 0, len(liveProcesses))
			for _, proc := range liveProcesses {
				sessions = append(sessions, SessionSummary{
					PID:          proc.PID,
					TTY:          proc.TTY,
					AgeSeconds:   proc.AgeSeconds,
					Title:        startingSessionTitle,
					LastActiveAt: generatedAt,
					Status:       activeSessionStatus,
				})
			}
			sortSessions(sessions)
			return &SessionSnapshot{GeneratedAt: generatedAt, Host: host, Sessions: sessions}, nil
		}
		return nil, fmt.Errorf("failed to access codex state database: %w", err)
	}

	activeByThread := make(map[string]SessionSummary)
	threadIDs := make([]string, 0, len(liveProcesses))

	for _, proc := range liveProcesses {
		processStartCutoff := generatedAt.Unix() - proc.AgeSeconds - 30
		mapping, err := queryPrimaryThreadForPID(ctx, dbPath, proc.PID, processStartCutoff)
		if err != nil {
			return nil, err
		}

		if mapping.ThreadID == "" {
			activeByThread[strconv.Itoa(proc.PID)] = SessionSummary{
				PID:          proc.PID,
				TTY:          proc.TTY,
				AgeSeconds:   proc.AgeSeconds,
				Title:        startingSessionTitle,
				LastActiveAt: generatedAt,
				Status:       activeSessionStatus,
			}
			continue
		}

		if _, exists := activeByThread[mapping.ThreadID]; exists {
			continue
		}

		activeByThread[mapping.ThreadID] = SessionSummary{
			PID:        proc.PID,
			TTY:        proc.TTY,
			AgeSeconds: proc.AgeSeconds,
			ThreadID:   mapping.ThreadID,
			Status:     activeSessionStatus,
		}
		threadIDs = append(threadIDs, mapping.ThreadID)
	}

	threads, err := queryThreads(ctx, dbPath, opts.IncludeAll, threadIDs)
	if err != nil {
		return nil, err
	}

	threadsByID := make(map[string]threadRow, len(threads))
	for _, row := range threads {
		threadsByID[row.ID] = row
	}

	logsByThread, err := queryRecentToolLogs(ctx, dbPath, threadIDsForLogs(opts.IncludeAll, threads, threadIDs))
	if err != nil {
		return nil, err
	}

	sessions := make([]SessionSummary, 0, len(activeByThread)+len(threads))
	for threadID, session := range activeByThread {
		if row, ok := threadsByID[threadID]; ok {
			enrichSession(&session, row, logsByThread[threadID], opts.IncludeDetails)
		}
		sessions = append(sessions, session)
	}

	if opts.IncludeAll {
		for _, row := range threads {
			if _, ok := activeByThread[row.ID]; ok {
				continue
			}
			session := SessionSummary{ThreadID: row.ID, Status: inactiveSessionStatus}
			enrichSession(&session, row, logsByThread[row.ID], opts.IncludeDetails)
			sessions = append(sessions, session)
		}
	}

	sortSessions(sessions)
	return &SessionSnapshot{GeneratedAt: generatedAt, Host: host, Sessions: sessions}, nil
}

func enrichSession(session *SessionSummary, row threadRow, logs []threadLogRow, includeDetails bool) {
	session.Title = firstNonEmpty(row.Title, row.FirstUserMessage, startingSessionTitle)
	session.FirstUserMessage = row.FirstUserMessage
	session.SessionCWD = row.CWD
	session.EffectiveWorkdir = inferEffectiveWorkdir(logs, row.CWD)
	session.LastActiveAt = unixSecondsToTime(row.UpdatedAt)
	session.GitBranch = row.GitBranch
	session.GitOriginURL = row.GitOriginURL
	if includeDetails {
		session.RecentAction = summarizeRecentAction(logs)
	}
}

func queryPrimaryThreadForPID(ctx context.Context, dbPath string, pid int, minTS int64) (processThreadRow, error) {
	query := fmt.Sprintf(
		`SELECT logs.process_uuid, threads.id AS thread_id
FROM threads
JOIN (
	SELECT process_uuid, thread_id
	FROM logs
	WHERE process_uuid LIKE %s
		AND thread_id IS NOT NULL
		AND thread_id != ''
		AND ts >= %d
	GROUP BY process_uuid, thread_id
) AS logs ON logs.thread_id = threads.id
WHERE threads.archived = 0
ORDER BY threads.created_at ASC, threads.updated_at DESC, threads.id ASC
LIMIT 1;`,
		sqliteQuote(fmt.Sprintf("pid:%d:%%", pid)),
		minTS,
	)

	rows, err := querySQLiteRows[processThreadRow](ctx, dbPath, query)
	if err != nil || len(rows) == 0 {
		return processThreadRow{}, err
	}
	return rows[0], nil
}

func queryThreads(ctx context.Context, dbPath string, includeAll bool, activeThreadIDs []string) ([]threadRow, error) {
	query := "SELECT id, title, first_user_message, cwd, updated_at, git_branch, git_origin_url FROM threads WHERE archived = 0"
	if !includeAll && len(activeThreadIDs) > 0 {
		query += " AND id IN (" + sqliteStringList(activeThreadIDs) + ")"
	}
	query += " ORDER BY updated_at DESC, id DESC;"
	return querySQLiteRows[threadRow](ctx, dbPath, query)
}

func queryRecentToolLogs(ctx context.Context, dbPath string, threadIDs []string) (map[string][]threadLogRow, error) {
	if len(threadIDs) == 0 {
		return map[string][]threadLogRow{}, nil
	}

	query := fmt.Sprintf(`
WITH ranked AS (
	SELECT thread_id, message, ROW_NUMBER() OVER (PARTITION BY thread_id ORDER BY id DESC) AS rn
	FROM logs
	WHERE target = %s
		AND thread_id IS NOT NULL
		AND thread_id != ''
		AND thread_id IN (%s)
)
SELECT thread_id, message
FROM ranked
WHERE rn <= %d
ORDER BY thread_id ASC, rn ASC;`,
		sqliteQuote(codexToolLogTarget),
		sqliteStringList(threadIDs),
		sessionLogPreviewLimit,
	)

	rows, err := querySQLiteRows[threadLogRow](ctx, dbPath, query)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: logs") {
			return map[string][]threadLogRow{}, nil
		}
		return nil, err
	}

	grouped := make(map[string][]threadLogRow)
	for _, row := range rows {
		grouped[row.ThreadID] = append(grouped[row.ThreadID], row)
	}
	return grouped, nil
}

func querySQLiteRows[T any](ctx context.Context, dbPath, query string) ([]T, error) {
	cmd := exec.CommandContext(ctx, sqlite3Command, "-json", dbPath, query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sqlite query failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	output = bytes.TrimSpace(output)
	if len(output) == 0 {
		return nil, nil
	}

	var rows []T
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse sqlite json output: %w", err)
	}
	return rows, nil
}

func listLiveCodexProcesses(ctx context.Context) ([]liveCodexProcess, error) {
	cmd := exec.CommandContext(ctx, psCommand, "-axo", "pid=,tty=,etime=,comm=")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list processes: %w", err)
	}

	var processes []liveCodexProcess
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		command := strings.Join(fields[3:], " ")
		if !looksLikeCodexCommand(command) {
			continue
		}

		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, err
		}
		ageSeconds, err := parseElapsedTime(fields[2])
		if err != nil {
			return nil, err
		}

		processes = append(processes, liveCodexProcess{
			PID:        pid,
			TTY:        fields[1],
			AgeSeconds: ageSeconds,
		})
	}

	return processes, nil
}

func looksLikeCodexCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	base := filepath.Base(command)
	return base == "codex" || strings.Contains(command, "/codex/")
}

func parseElapsedTime(raw string) (int64, error) {
	dayParts := strings.SplitN(raw, "-", 2)
	days := 0
	timePart := raw
	if len(dayParts) == 2 {
		parsedDays, err := strconv.Atoi(dayParts[0])
		if err != nil {
			return 0, err
		}
		days = parsedDays
		timePart = dayParts[1]
	}

	parts := strings.Split(timePart, ":")
	switch len(parts) {
	case 3:
		hours, _ := strconv.Atoi(parts[0])
		minutes, _ := strconv.Atoi(parts[1])
		seconds, _ := strconv.Atoi(parts[2])
		return int64(days*24*3600 + hours*3600 + minutes*60 + seconds), nil
	case 2:
		minutes, _ := strconv.Atoi(parts[0])
		seconds, _ := strconv.Atoi(parts[1])
		return int64(days*24*3600 + minutes*60 + seconds), nil
	case 1:
		seconds, _ := strconv.Atoi(parts[0])
		return int64(days*24*3600 + seconds), nil
	default:
		return 0, fmt.Errorf("unsupported elapsed time format")
	}
}

func inferEffectiveWorkdir(logs []threadLogRow, fallback string) string {
	for _, row := range logs {
		call, ok := parseToolCallMessage(row.Message)
		if !ok {
			continue
		}
		if workdir := extractWorkdir(call); workdir != "" {
			return workdir
		}
	}
	return fallback
}

func summarizeRecentAction(logs []threadLogRow) string {
	for _, row := range logs {
		call, ok := parseToolCallMessage(row.Message)
		if !ok {
			continue
		}
		if call.Name == "exec_command" {
			if cmd := compactWhitespace(stringArg(call.Args, "cmd")); cmd != "" {
				return "exec_command: " + cmd
			}
		}
		if message := compactWhitespace(firstNonEmpty(
			stringArg(call.Args, "message"),
			stringArg(call.Args, "query"),
			call.Name,
		)); message != "" {
			return message
		}
	}
	return ""
}

func parseToolCallMessage(message string) (toolCall, bool) {
	if !strings.HasPrefix(message, defaultRecentActionSuffix) {
		return toolCall{}, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(message, defaultRecentActionSuffix))
	idx := strings.Index(rest, " {")
	if idx == -1 {
		return toolCall{Name: rest}, true
	}
	name := strings.TrimSpace(rest[:idx])
	rawArgs := strings.TrimSpace(rest[idx+1:])

	args := make(map[string]any)
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolCall{Name: name}, true
	}
	return toolCall{Name: name, Args: args}, true
}

func extractWorkdir(call toolCall) string {
	if workdir := stringArg(call.Args, "workdir"); workdir != "" {
		return workdir
	}
	cmd := stringArg(call.Args, "cmd")
	if cmd == "" {
		return ""
	}
	matches := changeDirCommandRegex.FindStringSubmatch(cmd)
	if len(matches) == 0 {
		return ""
	}
	for i := 1; i < len(matches); i++ {
		if matches[i] != "" {
			return strings.TrimSpace(matches[i])
		}
	}
	return ""
}

func stringArg(args map[string]any, key string) string {
	value, ok := args[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

func compactWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func sqliteStringList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, sqliteQuote(value))
	}
	return strings.Join(quoted, ", ")
}

func sqliteQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func unixSecondsToTime(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func threadIDsForLogs(includeAll bool, threads []threadRow, activeThreadIDs []string) []string {
	if includeAll {
		ids := make([]string, 0, len(threads))
		for _, row := range threads {
			ids = append(ids, row.ID)
		}
		return ids
	}
	return activeThreadIDs
}

func sortSessions(sessions []SessionSummary) {
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Status != sessions[j].Status {
			return sessions[i].Status == activeSessionStatus
		}
		return sessions[i].LastActiveAt.After(sessions[j].LastActiveAt)
	})
}

func codexHomeDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".codex"), nil
}

func shortThreadID(threadID string) string {
	if len(threadID) <= 8 {
		return threadID
	}
	return threadID[:8]
}

func displayPath(path string) string {
	if path == "" {
		return "-"
	}
	homeDir, err := os.UserHomeDir()
	if err == nil && path == homeDir {
		return "~"
	}
	if err == nil && strings.HasPrefix(path, homeDir+string(os.PathSeparator)) {
		return "~/" + strings.TrimPrefix(path, homeDir+string(os.PathSeparator))
	}
	return path
}

func formatShortDuration(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
	default:
		return fmt.Sprintf("%dd%dh", seconds/86400, (seconds%86400)/3600)
	}
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
