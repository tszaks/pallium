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
	waitingSessionStatus      = "waiting"
	blockedSessionStatus      = "blocked"
	stuckSessionStatus        = "stuck"
	finishedSessionStatus     = "finished"
	inactiveSessionStatus     = "inactive"
	idleSessionStatus         = "idle"
	startingSessionTitle      = "(starting up)"
	defaultRecentActionSuffix = "ToolCall: "
	liveActivityWindow        = 2 * time.Minute
	stuckActivityWindow       = 10 * time.Minute
	openDesktopSessionWindow  = 24 * time.Hour
)

var changeDirCommandRegex = regexp.MustCompile(`^\s*cd\s+(?:"([^"]+)"|'([^']+)'|([^&|;]+))\s*&&`)
var rolloutThreadIDRegex = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

var (
	nowFunc                    = time.Now
	hostnameFunc               = os.Hostname
	codexHomeDirFunc           = codexHomeDir
	claudeHomeDirFunc          = claudeHomeDir
	listLiveCodexProcessesVar  = listLiveCodexProcesses
	listLiveClaudeProcessesVar = listLiveClaudeProcesses
	listOpenCodexRolloutsVar   = listOpenCodexRollouts
	processCWDVar              = processCWD
)

type SessionCollectOptions struct {
	IncludeAll        bool
	IncludeDetails    bool
	IncludeCompletion bool
}

type SessionSnapshot struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Host        string            `json:"host"`
	Coverage    DiscoveryCoverage `json:"coverage"`
	Sessions    []SessionSummary  `json:"sessions"`
	Warnings    []string          `json:"warnings"`
}

type DiscoveryCoverage struct {
	Scope        string             `json:"scope"`
	Completeness string             `json:"completeness"`
	Providers    []ProviderCoverage `json:"providers"`
	Includes     []string           `json:"includes"`
	Excludes     []string           `json:"excludes"`
}

type ProviderCoverage struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Sessions int    `json:"sessions"`
	Evidence string `json:"evidence"`
}

type SessionSummary struct {
	Provider         string     `json:"provider,omitempty"`
	PID              int        `json:"pid,omitempty"`
	PPID             int        `json:"ppid,omitempty"`
	TTY              string     `json:"tty,omitempty"`
	ProcessState     string     `json:"process_state,omitempty"`
	AgeSeconds       int64      `json:"age_seconds,omitempty"`
	ThreadID         string     `json:"thread_id,omitempty"`
	Title            string     `json:"title,omitempty"`
	FirstUserMessage string     `json:"first_user_message,omitempty"`
	SessionCWD       string     `json:"session_cwd,omitempty"`
	EffectiveWorkdir string     `json:"effective_workdir,omitempty"`
	LastActiveAt     time.Time  `json:"last_active_at,omitempty"`
	GitBranch        string     `json:"git_branch,omitempty"`
	GitOriginURL     string     `json:"git_origin_url,omitempty"`
	Status           string     `json:"status"`
	StatusReason     string     `json:"status_reason,omitempty"`
	StatusSource     string     `json:"status_source,omitempty"`
	StatusConfidence string     `json:"status_confidence,omitempty"`
	StatusSince      time.Time  `json:"status_since,omitempty"`
	CompletionStatus string     `json:"completion_status,omitempty"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	RecentAction     string     `json:"recent_action,omitempty"`
}

type liveAgentProcess struct {
	Provider   string
	PID        int
	PPID       int
	TTY        string
	State      string
	AgeSeconds int64
	CWD        string
}

type openCodexRollout struct {
	PID      int
	ThreadID string
	Path     string
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
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexResponseItem struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`
	CallID    string `json:"call_id"`
	Phase     string `json:"phase"`
}

type codexEventMessage struct {
	Type string `json:"type"`
}

type toolCall struct {
	ID       string
	Name     string
	Args     map[string]any
	RawInput string
}

type rolloutDetails struct {
	Workdir      string
	RecentAction string
	Signals      sessionSignals
}

func CollectSessions(ctx context.Context, opts SessionCollectOptions) (*SessionSnapshot, error) {
	generatedAt := nowFunc().UTC()
	host, _ := hostnameFunc()
	type providerResult struct {
		provider string
		sessions []SessionSummary
		err      error
	}
	results := make(chan providerResult, 2)
	go func() {
		sessions, err := collectCodexSessions(ctx, opts, generatedAt)
		results <- providerResult{provider: providerCodex, sessions: sessions, err: err}
	}()
	go func() {
		sessions, err := collectClaudeSessions(ctx, opts, generatedAt)
		results <- providerResult{provider: providerClaude, sessions: sessions, err: err}
	}()
	sessions := []SessionSummary{}
	warnings := []string{}
	coverage := defaultDiscoveryCoverage()
	for range 2 {
		result := <-results
		if result.err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			warnings = append(warnings, fmt.Sprintf("%s live discovery failed: %s", result.provider, result.err))
			coverage.Providers = append(coverage.Providers, ProviderCoverage{
				Provider: result.provider,
				Status:   "unavailable",
				Evidence: providerEvidence(result.provider),
			})
			continue
		}
		sessions = append(sessions, result.sessions...)
		coverage.Providers = append(coverage.Providers, ProviderCoverage{
			Provider: result.provider,
			Status:   "available",
			Sessions: len(result.sessions),
			Evidence: providerEvidence(result.provider),
		})
	}

	sortSessions(sessions)
	sort.Slice(coverage.Providers, func(i, j int) bool {
		return coverage.Providers[i].Provider < coverage.Providers[j].Provider
	})
	return &SessionSnapshot{GeneratedAt: generatedAt, Host: host, Coverage: coverage, Sessions: sessions, Warnings: warnings}, nil
}

func providerEvidence(provider string) string {
	switch provider {
	case providerCodex:
		return "exact executable identity plus Codex state database and open transcript files"
	case providerClaude:
		return "exact executable identity plus Claude history and transcript files"
	default:
		return "registered provider detector"
	}
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
			Provider:     providerCodex,
			PID:          proc.PID,
			PPID:         proc.PPID,
			TTY:          proc.TTY,
			ProcessState: proc.State,
			AgeSeconds:   proc.AgeSeconds,
			ThreadID:     mapping.ThreadID,
			Status:       activeSessionStatus,
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
			enrichCodexSessionFromRollout(&session, row.RolloutPath, opts.IncludeDetails, generatedAt)
		} else {
			applySessionState(&session, sessionSignals{}, generatedAt)
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
			if opts.IncludeCompletion {
				enrichCodexSessionCompletion(&session, row.RolloutPath, generatedAt)
			}
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
	openByID := map[string]openCodexRollout{}
	if openRollouts, openErr := listOpenCodexRolloutsVar(ctx, filepath.Dir(dbPath)); openErr == nil {
		for _, rollout := range openRollouts {
			openByID[rollout.ThreadID] = rollout
		}
	}
	sessions := make([]SessionSummary, 0, len(summaries)+len(liveProcesses))
	seen := make(map[string]bool, len(summaries))

	for _, session := range summaries {
		if active, ok := activeByID[session.ThreadID]; ok {
			session = active
		} else if opened, ok := openByID[session.ThreadID]; ok {
			if info, statErr := os.Stat(opened.Path); statErr == nil && info.ModTime().After(session.LastActiveAt) {
				session.LastActiveAt = info.ModTime().UTC()
			}
			if !opts.IncludeAll && (session.LastActiveAt.IsZero() || generatedAt.Sub(session.LastActiveAt) > openDesktopSessionWindow) {
				continue
			}
			session.PID = opened.PID
			session.TTY = "desktop"
			applySessionState(&session, sessionSignals{}, generatedAt)
		} else if !opts.IncludeAll {
			continue
		}
		if session.PID > 0 {
			enrichCodexSessionFromRollout(&session, rolloutPathsByID[session.ThreadID], opts.IncludeDetails, generatedAt)
		} else if opts.IncludeCompletion {
			enrichCodexSessionCompletion(&session, rolloutPathsByID[session.ThreadID], generatedAt)
		}
		sessions = append(sessions, session)
		seen[session.ThreadID] = true
	}

	for _, session := range activeByID {
		if !seen[session.ThreadID] {
			sessions = append(sessions, session)
		}
	}
	for threadID, opened := range openByID {
		if seen[threadID] {
			continue
		}
		info, err := os.Stat(opened.Path)
		if err != nil || (!opts.IncludeAll && generatedAt.Sub(info.ModTime()) > openDesktopSessionWindow) {
			continue
		}
		session := SessionSummary{
			Provider:     providerCodex,
			PID:          opened.PID,
			TTY:          "desktop",
			ThreadID:     threadID,
			Title:        startingSessionTitle,
			LastActiveAt: info.ModTime().UTC(),
		}
		enrichCodexSessionFromRollout(&session, opened.Path, opts.IncludeDetails, generatedAt)
		sessions = append(sessions, session)
	}

	sortSessions(sessions)
	return sessions, nil
}

func startingCodexSession(proc liveAgentProcess, generatedAt time.Time) SessionSummary {
	session := SessionSummary{
		Provider:         providerCodex,
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

func enrichSession(session *SessionSummary, row threadRow, logs []threadLogRow, includeDetails bool) {
	session.Title = compactSummaryText(firstNonEmpty(row.Title, row.FirstUserMessage, startingSessionTitle), 240)
	if includeDetails {
		session.FirstUserMessage = compactSummaryText(row.FirstUserMessage, 500)
	}
	session.SessionCWD = row.CWD
	session.EffectiveWorkdir = inferEffectiveWorkdir(logs, row.CWD)
	session.LastActiveAt = unixSecondsToTime(row.UpdatedAt)
	session.GitBranch = row.GitBranch
	session.GitOriginURL = row.GitOriginURL
	if includeDetails {
		session.RecentAction = compactSummaryText(summarizeRecentAction(logs), 500)
	}
}

func enrichCodexSessionFromRollout(session *SessionSummary, rolloutPath string, includeDetails bool, generatedAt time.Time) {
	if rolloutPath == "" {
		applySessionState(session, sessionSignals{}, generatedAt)
		return
	}
	details, err := readCodexRolloutDetails(rolloutPath)
	if err != nil {
		applySessionState(session, sessionSignals{}, generatedAt)
		return
	}
	if details.Workdir != "" {
		session.EffectiveWorkdir = details.Workdir
	}
	if includeDetails && details.RecentAction != "" {
		session.RecentAction = details.RecentAction
	}
	applySessionState(session, details.Signals, generatedAt)
}

func enrichCodexSessionCompletion(session *SessionSummary, rolloutPath string, generatedAt time.Time) {
	if rolloutPath == "" {
		applySessionState(session, sessionSignals{}, generatedAt)
		return
	}
	details, err := readCodexRolloutTailDetails(rolloutPath, 128*1024)
	if err != nil {
		applySessionState(session, sessionSignals{}, generatedAt)
		return
	}
	applySessionState(session, details.Signals, generatedAt)
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
		match.PPID = proc.PPID
		match.TTY = proc.TTY
		match.ProcessState = proc.State
		match.AgeSeconds = proc.AgeSeconds
		applySessionState(&match, sessionSignals{}, generatedAt)
		activeByID[match.ThreadID] = match
		used[match.ThreadID] = true
	}

	return activeByID
}

func liveSessionStatus(lastActiveAt, generatedAt time.Time, processAgeSeconds int64) string {
	session := SessionSummary{PID: 1, AgeSeconds: processAgeSeconds, LastActiveAt: lastActiveAt}
	applySessionState(&session, sessionSignals{}, generatedAt)
	return session.Status
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

	query := "SELECT id, " + rolloutPathColumn + ", substr(title,1,500) AS title, substr(first_user_message,1,1000) AS first_user_message, cwd, updated_at, git_branch, git_origin_url FROM threads WHERE archived = 0"
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

func readCodexRolloutDetails(path string) (rolloutDetails, error) {
	file, err := os.Open(path)
	if err != nil {
		return rolloutDetails{}, err
	}
	defer file.Close()
	return parseCodexRolloutDetails(bufio.NewReader(file))
}

func readCodexRolloutTailDetails(path string, tailBytes int64) (rolloutDetails, error) {
	file, err := os.Open(path)
	if err != nil {
		return rolloutDetails{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return rolloutDetails{}, err
	}
	start := max(int64(0), info.Size()-tailBytes)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return rolloutDetails{}, err
	}
	reader := bufio.NewReader(file)
	if start > 0 {
		_, _ = reader.ReadBytes('\n')
	}
	return parseCodexRolloutDetails(reader)
}

func parseCodexRolloutDetails(reader *bufio.Reader) (rolloutDetails, error) {
	details := rolloutDetails{Signals: sessionSignals{Source: "codex-transcript"}}
	pending := map[string]struct {
		name string
		at   time.Time
	}{}
	for {
		line, readErr := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var entry codexRolloutEntry
			if json.Unmarshal(trimmed, &entry) == nil {
				at, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
				at = at.UTC()
				if at.After(details.Signals.LatestAt) {
					details.Signals.LatestAt = at
				}
				switch entry.Type {
				case "event_msg":
					var event codexEventMessage
					if json.Unmarshal(entry.Payload, &event) == nil {
						switch event.Type {
						case "task_started":
							details.Signals.Lifecycle = lifecycleRunning
							details.Signals.LifecycleAt = at
							clear(pending)
						case "task_complete":
							details.Signals.Lifecycle = lifecycleFinished
							details.Signals.LifecycleAt = at
							clear(pending)
						}
					}
				case "response_item":
					var item codexResponseItem
					if json.Unmarshal(entry.Payload, &item) == nil {
						switch item.Type {
						case "function_call", "custom_tool_call":
							call, ok := parseCodexToolCallItem(item)
							if ok {
								if nextWorkdir := extractWorkdir(call); nextWorkdir != "" {
									details.Workdir = nextWorkdir
								}
								if nextAction := summarizeToolCall(call); nextAction != "" {
									details.RecentAction = nextAction
								}
								if call.ID != "" {
									pending[call.ID] = struct {
										name string
										at   time.Time
									}{name: inferNestedToolName(call.Name, call.RawInput), at: at}
								}
							}
						case "function_call_output", "custom_tool_call_output":
							delete(pending, item.CallID)
						}
					}
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return rolloutDetails{}, readErr
		}
	}
	for _, call := range pending {
		if call.at.After(details.Signals.PendingSince) {
			details.Signals.PendingTool = call.name
			details.Signals.PendingSince = call.at
		}
	}
	return details, nil
}

func parseCodexFunctionCall(line []byte) (toolCall, bool) {
	var entry codexRolloutEntry
	if err := json.Unmarshal(line, &entry); err != nil || entry.Type != "response_item" {
		return toolCall{}, false
	}

	var item codexResponseItem
	if err := json.Unmarshal(entry.Payload, &item); err != nil {
		return toolCall{}, false
	}
	return parseCodexToolCallItem(item)
}

func parseCodexToolCallItem(item codexResponseItem) (toolCall, bool) {
	if (item.Type != "function_call" && item.Type != "custom_tool_call") || item.Name == "" {
		return toolCall{}, false
	}
	args := make(map[string]any)
	rawInput := firstNonEmpty(item.Arguments, item.Input)
	_ = json.Unmarshal([]byte(rawInput), &args)
	return toolCall{ID: item.CallID, Name: item.Name, Args: args, RawInput: rawInput}, true
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

func listOpenCodexRollouts(ctx context.Context, codexHome string) ([]openCodexRollout, error) {
	processName := providerCodex
	cmd := exec.CommandContext(ctx, lsofCommand, "-n", "-Fpn", "-c", processName)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to inspect open Codex transcripts: %w", err)
	}
	return parseOpenCodexRollouts(output, codexHome), nil
}

func parseOpenCodexRollouts(output []byte, codexHome string) []openCodexRollout {
	sessionsRoot := filepath.Clean(filepath.Join(codexHome, "sessions")) + string(filepath.Separator)
	currentPID := 0
	seen := map[string]bool{}
	rollouts := []openCodexRollout{}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "p") {
			currentPID, _ = strconv.Atoi(strings.TrimPrefix(line, "p"))
			continue
		}
		if !strings.HasPrefix(line, "n") || currentPID <= 0 {
			continue
		}
		path := filepath.Clean(strings.TrimPrefix(line, "n"))
		if !strings.HasPrefix(path, sessionsRoot) {
			continue
		}
		match := rolloutThreadIDRegex.FindStringSubmatch(filepath.Base(path))
		if len(match) != 2 || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		rollouts = append(rollouts, openCodexRollout{PID: currentPID, ThreadID: match[1], Path: path})
	}
	return rollouts
}

func listLiveAgentProcesses(ctx context.Context, provider string, predicate func(string) bool, includeCWD bool) ([]liveAgentProcess, error) {
	cmd := exec.CommandContext(ctx, psCommand, "-axo", "pid=,ppid=,stat=,tty=,etime=,comm=")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list processes: %w", err)
	}

	processes, err := parseLiveAgentProcesses(output, provider, predicate)
	if err != nil {
		return nil, err
	}
	if includeCWD {
		for index := range processes {
			processes[index].CWD, _ = processCWDVar(ctx, processes[index].PID)
		}
	}
	return processes, nil
}

func parseLiveAgentProcesses(output []byte, provider string, predicate func(string) bool) ([]liveAgentProcess, error) {
	var processes []liveAgentProcess
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if !isInteractiveTTY(fields[3]) {
			continue
		}
		command := strings.Join(fields[5:], " ")
		if !predicate(command) {
			continue
		}

		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, err
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		ageSeconds, err := parseElapsedTime(fields[4])
		if err != nil {
			return nil, err
		}

		process := liveAgentProcess{
			Provider:   provider,
			PID:        pid,
			PPID:       ppid,
			State:      fields[2],
			TTY:        fields[3],
			AgeSeconds: ageSeconds,
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
	base := filepath.Base(strings.ReplaceAll(command, "\\", "/"))
	return base == "codex" || base == "codex.exe"
}

func looksLikeClaudeCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	base := filepath.Base(strings.ReplaceAll(command, "\\", "/"))
	return base == "claude" || base == "claude.exe"
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

func compactSummaryText(value string, limit int) string {
	value = compactWhitespace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
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
			return liveStatusRank(sessions[i].Status) < liveStatusRank(sessions[j].Status)
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
