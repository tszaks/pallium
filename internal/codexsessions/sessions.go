package codexsessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	psCommand                 = "ps"
	lsofCommand               = "lsof"
	sqlite3Command            = "sqlite3"
	codexStateDBFile          = "state_5.sqlite"
	claudeProjectsDir         = "projects"
	sessionLogPreviewLimit    = 5
	codexToolLogTarget        = "codex_core::stream_events_utils"
	providerCodex             = "codex"
	providerClaude            = "claude"
	activeSessionStatus       = "active"
	inactiveSessionStatus     = "inactive"
	startingSessionTitle      = "(starting up)"
	defaultRecentActionSuffix = "ToolCall: "
)

var changeDirCommandRegex = regexp.MustCompile(`^\s*cd\s+(?:"([^"]+)"|'([^']+)'|([^&|;]+))\s*&&`)

var (
	nowFunc                    = time.Now
	hostnameFunc               = os.Hostname
	codexHomeDirFunc           = codexHomeDir
	claudeHomeDirFunc          = claudeHomeDir
	listLiveCodexProcessesVar  = listLiveCodexProcesses
	listLiveClaudeProcessesVar = listLiveClaudeProcesses
	processCWDVar              = processCWD
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
	Provider         string    `json:"provider,omitempty"`
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

type liveAgentProcess struct {
	Provider   string
	PID        int
	TTY        string
	AgeSeconds int64
	CWD        string
}

type processThreadRow struct {
	ProcessUUID string `json:"process_uuid"`
	ThreadID    string `json:"thread_id"`
}

type threadRow struct {
	ID               string `json:"id"`
	RolloutPath      string `json:"rollout_path"`
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

type sqliteTableRow struct {
	Name string `json:"name"`
}

type codexRolloutEntry struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type codexResponseItem struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	Name string
	Args map[string]any
}

func CollectSessions(ctx context.Context, opts SessionCollectOptions) (*SessionSnapshot, error) {
	generatedAt := nowFunc().UTC()
	host, _ := hostnameFunc()

	var sessions []SessionSummary
	codexSessions, err := collectCodexSessions(ctx, opts, generatedAt)
	if err != nil {
		return nil, err
	}
	sessions = append(sessions, codexSessions...)

	claudeSessions, err := collectClaudeSessions(ctx, opts, generatedAt)
	if err != nil {
		return nil, err
	}
	sessions = append(sessions, claudeSessions...)

	sortSessions(sessions)
	return &SessionSnapshot{GeneratedAt: generatedAt, Host: host, Sessions: sessions}, nil
}

func collectCodexSessions(ctx context.Context, opts SessionCollectOptions, generatedAt time.Time) ([]SessionSummary, error) {
	codexHome, err := codexHomeDirFunc()
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(codexHome, codexStateDBFile)

	liveProcesses, err := listLiveCodexProcessesVar(ctx)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			sessions := make([]SessionSummary, 0, len(liveProcesses))
			for _, proc := range liveProcesses {
				sessions = append(sessions, startingCodexSession(proc, generatedAt))
			}
			sortSessions(sessions)
			return sessions, nil
		}
		return nil, fmt.Errorf("failed to access codex state database: %w", err)
	}

	hasLogs, err := sqliteTableExists(ctx, dbPath, "logs")
	if err != nil {
		return nil, err
	}
	if !hasLogs {
		return collectCodexSessionsWithoutLogs(ctx, dbPath, opts, generatedAt, liveProcesses)
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
			active := startingCodexSession(proc, generatedAt)
			active.ThreadID = strconv.Itoa(proc.PID)
			activeByThread[active.ThreadID] = active
			continue
		}

		if _, exists := activeByThread[mapping.ThreadID]; exists {
			continue
		}

		activeByThread[mapping.ThreadID] = SessionSummary{
			Provider:   providerCodex,
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
			session := SessionSummary{Provider: providerCodex, ThreadID: row.ID, Status: inactiveSessionStatus}
			enrichSession(&session, row, logsByThread[row.ID], opts.IncludeDetails)
			sessions = append(sessions, session)
		}
	}

	sortSessions(sessions)
	return sessions, nil
}

func collectCodexSessionsWithoutLogs(ctx context.Context, dbPath string, opts SessionCollectOptions, generatedAt time.Time, liveProcesses []liveAgentProcess) ([]SessionSummary, error) {
	threads, err := queryThreads(ctx, dbPath, true, nil)
	if err != nil {
		return nil, err
	}

	summaries := make([]SessionSummary, 0, len(threads))
	rolloutPathsByID := make(map[string]string, len(threads))
	for _, row := range threads {
		session := SessionSummary{Provider: providerCodex, ThreadID: row.ID, Status: inactiveSessionStatus}
		enrichSession(&session, row, nil, opts.IncludeDetails)
		summaries = append(summaries, session)
		rolloutPathsByID[row.ID] = row.RolloutPath
	}

	activeByID := matchActiveSessions(summaries, liveProcesses, generatedAt, startingCodexSession)
	sessions := make([]SessionSummary, 0, len(summaries)+len(liveProcesses))
	seen := make(map[string]bool, len(summaries))

	for _, session := range summaries {
		if active, ok := activeByID[session.ThreadID]; ok {
			session = active
		} else if !opts.IncludeAll {
			continue
		}
		if session.Status == activeSessionStatus || opts.IncludeDetails {
			enrichCodexSessionFromRollout(&session, rolloutPathsByID[session.ThreadID], opts.IncludeDetails)
		}
		sessions = append(sessions, session)
		seen[session.ThreadID] = true
	}

	for _, session := range activeByID {
		if !seen[session.ThreadID] {
			sessions = append(sessions, session)
		}
	}

	sortSessions(sessions)
	return sessions, nil
}

func startingCodexSession(proc liveAgentProcess, generatedAt time.Time) SessionSummary {
	return SessionSummary{
		Provider:         providerCodex,
		PID:              proc.PID,
		TTY:              proc.TTY,
		AgeSeconds:       proc.AgeSeconds,
		Title:            startingSessionTitle,
		SessionCWD:       proc.CWD,
		EffectiveWorkdir: proc.CWD,
		LastActiveAt:     generatedAt,
		Status:           activeSessionStatus,
	}
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

func enrichCodexSessionFromRollout(session *SessionSummary, rolloutPath string, includeDetails bool) {
	if rolloutPath == "" {
		return
	}
	workdir, recentAction, err := readCodexRolloutDetails(rolloutPath)
	if err != nil {
		return
	}
	if workdir != "" {
		session.EffectiveWorkdir = workdir
	}
	if includeDetails && recentAction != "" {
		session.RecentAction = recentAction
	}
}

func matchActiveSessions(sessions []SessionSummary, liveProcesses []liveAgentProcess, generatedAt time.Time, startSession func(liveAgentProcess, time.Time) SessionSummary) map[string]SessionSummary {
	activeByID := make(map[string]SessionSummary)
	used := make(map[string]bool)

	for _, proc := range liveProcesses {
		match, ok := findSessionForProcess(sessions, proc, generatedAt, used)
		if !ok {
			active := startSession(proc, generatedAt)
			active.ThreadID = strconv.Itoa(proc.PID)
			activeByID[active.ThreadID] = active
			continue
		}

		match.PID = proc.PID
		match.TTY = proc.TTY
		match.AgeSeconds = proc.AgeSeconds
		match.Status = activeSessionStatus
		activeByID[match.ThreadID] = match
		used[match.ThreadID] = true
	}

	return activeByID
}

func findSessionForProcess(sessions []SessionSummary, proc liveAgentProcess, generatedAt time.Time, used map[string]bool) (SessionSummary, bool) {
	startCutoff := generatedAt.Unix() - proc.AgeSeconds - 30
	candidates := make([]SessionSummary, 0)
	for _, session := range sessions {
		if used[session.ThreadID] {
			continue
		}
		if proc.CWD != "" && session.EffectiveWorkdir != proc.CWD && session.SessionCWD != proc.CWD {
			continue
		}
		candidates = append(candidates, session)
	}

	if len(candidates) == 0 {
		return SessionSummary{}, false
	}

	sortSessions(candidates)
	for _, session := range candidates {
		if !session.LastActiveAt.IsZero() && session.LastActiveAt.Unix() >= startCutoff {
			return session, true
		}
	}
	return candidates[0], true
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
	rolloutPathColumn := "'' AS rollout_path"
	hasRolloutPath, err := sqliteColumnExists(ctx, dbPath, "threads", "rollout_path")
	if err != nil {
		return nil, err
	}
	if hasRolloutPath {
		rolloutPathColumn = "rollout_path"
	}

	query := "SELECT id, " + rolloutPathColumn + ", title, first_user_message, cwd, updated_at, git_branch, git_origin_url FROM threads WHERE archived = 0"
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

func readCodexRolloutDetails(path string) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()

	var workdir string
	var recentAction string
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && bytes.Contains(trimmed, []byte(`"type":"response_item"`)) && bytes.Contains(trimmed, []byte(`"function_call"`)) {
			call, ok := parseCodexFunctionCall(trimmed)
			if ok {
				if nextWorkdir := extractWorkdir(call); nextWorkdir != "" {
					workdir = nextWorkdir
				}
				if nextAction := summarizeToolCall(call); nextAction != "" {
					recentAction = nextAction
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
	}
	return workdir, recentAction, nil
}

func parseCodexFunctionCall(line []byte) (toolCall, bool) {
	var entry codexRolloutEntry
	if err := json.Unmarshal(line, &entry); err != nil || entry.Type != "response_item" {
		return toolCall{}, false
	}

	var item codexResponseItem
	if err := json.Unmarshal(entry.Payload, &item); err != nil || item.Type != "function_call" || item.Name == "" {
		return toolCall{}, false
	}

	args := make(map[string]any)
	_ = json.Unmarshal([]byte(item.Arguments), &args)
	return toolCall{Name: item.Name, Args: args}, true
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

func sqliteTableExists(ctx context.Context, dbPath, tableName string) (bool, error) {
	rows, err := querySQLiteRows[sqliteTableRow](
		ctx,
		dbPath,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = "+sqliteQuote(tableName)+" LIMIT 1;",
	)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

func sqliteColumnExists(ctx context.Context, dbPath, tableName, columnName string) (bool, error) {
	rows, err := querySQLiteRows[sqliteTableRow](ctx, dbPath, "PRAGMA table_info("+tableName+");")
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Name == columnName {
			return true, nil
		}
	}
	return false, nil
}

func listLiveCodexProcesses(ctx context.Context) ([]liveAgentProcess, error) {
	return listLiveAgentProcesses(ctx, providerCodex, looksLikeCodexCommand, true)
}

func listLiveClaudeProcesses(ctx context.Context) ([]liveAgentProcess, error) {
	return listLiveAgentProcesses(ctx, providerClaude, looksLikeClaudeCommand, true)
}

func listLiveAgentProcesses(ctx context.Context, provider string, predicate func(string) bool, includeCWD bool) ([]liveAgentProcess, error) {
	cmd := exec.CommandContext(ctx, psCommand, "-axo", "pid=,tty=,etime=,comm=")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list processes: %w", err)
	}

	var processes []liveAgentProcess
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if !isInteractiveTTY(fields[1]) {
			continue
		}
		command := strings.Join(fields[3:], " ")
		if !predicate(command) {
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

		process := liveAgentProcess{
			Provider:   provider,
			PID:        pid,
			TTY:        fields[1],
			AgeSeconds: ageSeconds,
		}
		if includeCWD {
			process.CWD, _ = processCWDVar(ctx, pid)
		}

		processes = append(processes, process)
	}

	return processes, nil
}

func isInteractiveTTY(tty string) bool {
	return tty != "" && tty != "??" && tty != "?"
}

func looksLikeCodexCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	base := filepath.Base(command)
	return base == "codex" || strings.Contains(command, "/codex/")
}

func looksLikeClaudeCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	base := filepath.Base(command)
	return base == "claude" || base == "claude.exe" || strings.HasSuffix(command, "/claude")
}

func processCWD(ctx context.Context, pid int) (string, error) {
	switch runtime.GOOS {
	case "linux":
		return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	case "darwin":
		cmd := exec.CommandContext(ctx, lsofCommand, "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
		output, err := cmd.Output()
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "n") && len(line) > 1 {
				return strings.TrimSpace(strings.TrimPrefix(line, "n")), nil
			}
		}
	}
	return "", nil
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
		if summary := summarizeToolCall(call); summary != "" {
			return summary
		}
	}
	return ""
}

func summarizeToolCall(call toolCall) string {
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

func claudeHomeDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".claude"), nil
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
