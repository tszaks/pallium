package codexsessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Cursor session discovery. Both Cursor surfaces (the IDE agent and the
// cursor-agent CLI, including headless -p runs) write the same per-chat
// transcript under the Cursor home:
//
//	projects/<slug>/agent-transcripts/<id>/<id>.jsonl
//	projects/<slug>/agent-transcripts/<id>/subagents/*.jsonl
//	projects/<slug>/terminals/<n>.txt
//
// <slug> is the workspace path with the leading "/" dropped and every "/"
// turned into "-" (lossy: a "-" in a directory name is ambiguous, so
// cursorSlugToPath resolves it against the real filesystem).
//
// Transcript lines are Claude-like {role, message:{content:[text|tool_use]}}
// and a bare {type:"turn_ended", status} closes each turn. There is NO
// per-line timestamp (user text carries a human-readable <timestamp> tag
// only), so recency is the file modification time — every status below says
// so in its reason and carries "medium" confidence at best. Assistant
// reasoning text is stored as "[REDACTED]" and is skipped.
//
// CLI chats also keep ~/.cursor/chats/<md5(cwd)>/<id>/{meta.json,store.db};
// store.db is an opaque content-addressed blob store, so it is not read —
// the transcript above already covers every chat that has one.
//
// terminals/<n>.txt files are written for commands the agent runs: a YAML-ish
// header between two "---" lines (pid, cwd, command, title, status,
// started_at) followed by captured output. They are per-workspace, not
// per-chat, so a running terminal is attributed to the workspace's most
// recently active chat.
const (
	cursorHomeRelativePath = ".cursor"
	cursorProjectsDir      = "projects"
	cursorTailBytes        = 256 * 1024
	cursorRunningWindow    = 5 * time.Minute
	cursorMaxLineBytes     = 8 * 1024 * 1024
)

// cursorHomeDirFunc is a var so tests can point discovery at a fixture tree.
var cursorHomeDirFunc = cursorHomeDir

func cursorHomeDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, cursorHomeRelativePath), nil
}

type cursorTranscriptLine struct {
	Role    string `json:"role"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type cursorContentItem struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type cursorRunningTerminal struct {
	PID     int
	Title   string
	Command string
}

type cursorTranscriptInfo struct {
	FirstUser    string
	RecentAction string
	TurnEnded    bool
	HasLines     bool
}

func collectCursorSessions(ctx context.Context, opts SessionCollectOptions, generatedAt time.Time) ([]SessionSummary, error) {
	cursorHome, err := cursorHomeDirFunc()
	if err != nil {
		return nil, err
	}
	liveProcesses, err := listLiveCursorProcessesVar(ctx)
	if err != nil {
		return nil, err
	}
	projectsRoot := filepath.Join(cursorHome, cursorProjectsDir)
	slugs, err := os.ReadDir(projectsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return startingCursorSessions(liveProcesses, generatedAt), nil
		}
		return nil, fmt.Errorf("failed to access cursor projects directory: %w", err)
	}

	// Without --all only chats touched in the last day are even opened;
	// Cursor keeps every chat forever.
	cutoff := time.Time{}
	if !opts.IncludeAll {
		cutoff = generatedAt.Add(-openDesktopSessionWindow)
	}

	var summaries []SessionSummary
	infoByID := map[string]cursorTranscriptInfo{}
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		projectDir := filepath.Join(projectsRoot, slug.Name())
		chats, err := os.ReadDir(filepath.Join(projectDir, "agent-transcripts"))
		if err != nil {
			continue
		}
		var workdir string
		var terminals []cursorRunningTerminal
		terminalsRead := false
		var projectSessions []int
		for _, chat := range chats {
			if !chat.IsDir() {
				continue
			}
			chatDir := filepath.Join(projectDir, "agent-transcripts", chat.Name())
			mainFile := filepath.Join(chatDir, chat.Name()+".jsonl")
			latest, newestFile := cursorChatActivity(chatDir, mainFile)
			if newestFile == "" || (!cutoff.IsZero() && latest.Before(cutoff)) {
				continue
			}
			if workdir == "" {
				workdir = cursorSlugToPath(slug.Name())
			}
			info := readCursorTranscript(mainFile, newestFile, opts.IncludeDetails)
			session := SessionSummary{
				Provider:         providerCursor,
				ThreadID:         chat.Name(),
				Title:            compactSummaryText(firstNonEmpty(info.FirstUser, chat.Name()), 240),
				SessionCWD:       workdir,
				EffectiveWorkdir: workdir,
				LastActiveAt:     latest,
				Status:           inactiveSessionStatus,
			}
			if opts.IncludeDetails {
				session.FirstUserMessage = compactSummaryText(info.FirstUser, 500)
				session.RecentAction = info.RecentAction
			}
			if !terminalsRead {
				terminals = readCursorRunningTerminals(filepath.Join(projectDir, "terminals"))
				terminalsRead = true
			}
			infoByID[session.ThreadID] = info
			projectSessions = append(projectSessions, len(summaries))
			summaries = append(summaries, session)
		}
		// Terminals belong to the workspace, not a chat: credit the chat
		// that moved most recently.
		if len(terminals) > 0 && len(projectSessions) > 0 {
			newest := projectSessions[0]
			for _, idx := range projectSessions[1:] {
				if summaries[idx].LastActiveAt.After(summaries[newest].LastActiveAt) {
					newest = idx
				}
			}
			terminal := terminals[len(terminals)-1]
			summaries[newest].RecentAction = "terminal running: " + compactSummaryText(firstNonEmpty(terminal.Title, terminal.Command), 80) + " (" + compactSummaryText(terminal.Command, 160) + ")"
			markCursorTerminalRunning(&summaries[newest], terminal, len(terminals), generatedAt)
		}
	}

	activeByID := matchActiveSessions(summaries, liveProcesses, generatedAt, startingCursorSession)
	sessions := make([]SessionSummary, 0, len(summaries)+len(liveProcesses))
	seen := make(map[string]bool, len(summaries))
	for _, session := range summaries {
		if active, ok := activeByID[session.ThreadID]; ok {
			// A live cursor-agent process owns this chat: classify it with
			// the process-aware rules, but keep a running terminal's action.
			action := session.RecentAction
			active.RecentAction = firstNonEmpty(action, active.RecentAction)
			if session.Status == activeSessionStatus && strings.HasPrefix(session.StatusReason, "a running terminal") {
				active.Status, active.StatusReason, active.StatusSource, active.StatusConfidence, active.StatusSince = session.Status, session.StatusReason, session.StatusSource, session.StatusConfidence, session.StatusSince
			} else {
				applyCursorTranscriptState(&active, infoByID[session.ThreadID], generatedAt)
			}
			session = active
		} else {
			if session.Status == inactiveSessionStatus {
				applyCursorTranscriptState(&session, infoByID[session.ThreadID], generatedAt)
			}
			if !opts.IncludeAll && session.Status == inactiveSessionStatus {
				continue
			}
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

func startingCursorSessions(procs []liveAgentProcess, generatedAt time.Time) []SessionSummary {
	sessions := make([]SessionSummary, 0, len(procs))
	for _, proc := range procs {
		sessions = append(sessions, startingCursorSession(proc, generatedAt))
	}
	return sessions
}

func startingCursorSession(proc liveAgentProcess, generatedAt time.Time) SessionSummary {
	session := SessionSummary{
		Provider:         providerCursor,
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

// cursorChatActivity returns the newest modification time across a chat's
// main transcript and its subagent transcripts, plus which file that was.
// Subagent work counts toward the parent chat: it is listed under the parent,
// never as a session of its own.
func cursorChatActivity(chatDir, mainFile string) (time.Time, string) {
	var latest time.Time
	newest := ""
	consider := func(path string) {
		if st, err := os.Stat(path); err == nil && !st.IsDir() && (newest == "" || st.ModTime().After(latest)) {
			latest, newest = st.ModTime(), path
		}
	}
	consider(mainFile)
	if subs, err := filepath.Glob(filepath.Join(chatDir, "subagents", "*.jsonl")); err == nil {
		for _, sub := range subs {
			consider(sub)
		}
	}
	return latest, newest
}

var cursorUserQueryRegex = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)

// readCursorTranscript pulls the first user ask from the head of the main
// transcript and the turn-end marker / last action from the tail of the
// newest file. Unknown or malformed lines are skipped.
func readCursorTranscript(mainFile, newestFile string, includeDetails bool) cursorTranscriptInfo {
	info := cursorTranscriptInfo{}
	if f, err := os.Open(mainFile); err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), cursorMaxLineBytes)
		for i := 0; i < 40 && scanner.Scan(); i++ {
			var line cursorTranscriptLine
			if json.Unmarshal(scanner.Bytes(), &line) != nil || line.Role != "user" {
				continue
			}
			if text := cursorUserText(line); text != "" {
				info.FirstUser = text
				break
			}
		}
		f.Close()
	}
	for _, line := range readCursorTail(newestFile) {
		info.HasLines = true
		if line.Type == "turn_ended" {
			info.TurnEnded = true
			continue
		}
		if line.Role == "" {
			continue
		}
		info.TurnEnded = false
		if action := cursorRecentAction(line); action != "" {
			info.RecentAction = action
		}
	}
	return info
}

func readCursorTail(path string) []cursorTranscriptLine {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	offset := int64(0)
	if st.Size() > cursorTailBytes {
		offset = st.Size() - cursorTailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	if offset > 0 {
		// Drop the partial first line of the window.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			return nil
		}
	}
	var lines []cursorTranscriptLine
	for _, raw := range bytes.Split(data, []byte("\n")) {
		var line cursorTranscriptLine
		if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &line) != nil {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func cursorContentItems(raw json.RawMessage) []cursorContentItem {
	var items []cursorContentItem
	if json.Unmarshal(raw, &items) == nil {
		return items
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return []cursorContentItem{{Type: "text", Text: text}}
	}
	return nil
}

func cursorUserText(line cursorTranscriptLine) string {
	var parts []string
	for _, item := range cursorContentItems(line.Message.Content) {
		if item.Type == "text" {
			parts = append(parts, item.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if m := cursorUserQueryRegex.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	return strings.TrimSpace(text)
}

// cursorRecentAction renders the last tool call of an assistant line, else
// its visible text. "[REDACTED]" reasoning placeholders are ignored.
func cursorRecentAction(line cursorTranscriptLine) string {
	if line.Role != "assistant" {
		return ""
	}
	items := cursorContentItems(line.Message.Content)
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Type != "tool_use" || item.Name == "" {
			continue
		}
		detail := compactWhitespace(firstNonEmpty(
			stringArg(item.Input, "command"), stringArg(item.Input, "path"), stringArg(item.Input, "target_file"),
			stringArg(item.Input, "pattern"), stringArg(item.Input, "query"), stringArg(item.Input, "description")))
		if detail == "" {
			return item.Name
		}
		return item.Name + ": " + compactSummaryText(detail, 160)
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == "text" {
			text := compactSummaryText(strings.ReplaceAll(items[i].Text, "[REDACTED]", ""), 160)
			if text != "" {
				return text
			}
		}
	}
	return ""
}

// applyCursorTranscriptState classifies a chat from transcript recency
// alone. The file mtime is the only clock Cursor gives us (see the format
// notes above), so confidence tops out at "medium".
func applyCursorTranscriptState(session *SessionSummary, info cursorTranscriptInfo, generatedAt time.Time) {
	const source = "cursor-transcript"
	age := generatedAt.Sub(session.LastActiveAt)
	switch {
	case session.PID > 0:
		signals := sessionSignals{Source: source, LatestAt: session.LastActiveAt}
		if info.TurnEnded {
			signals.Lifecycle = lifecycleFinished
			signals.LifecycleAt = session.LastActiveAt
		}
		applySessionState(session, signals, generatedAt)
	case info.HasLines && !info.TurnEnded && age <= cursorRunningWindow:
		setSessionState(session, activeSessionStatus, "the transcript changed in the last five minutes and its last turn has not ended (Cursor writes no per-line timestamps, so recency is the file modification time)", source, "medium", session.LastActiveAt)
		session.CompletionStatus = "not_finished"
	case age <= openDesktopSessionWindow:
		setSessionState(session, idleSessionStatus, "no turn is in progress; the transcript last changed "+age.Round(time.Second).String()+" ago (file modification time, Cursor writes no per-line timestamps)", source, "medium", session.LastActiveAt)
		session.CompletionStatus = "unknown"
		if info.TurnEnded {
			session.CompletionStatus = "finished"
		}
	default:
		setSessionState(session, inactiveSessionStatus, "the transcript has not changed in over a day", source, "medium", session.LastActiveAt)
	}
}

func markCursorTerminalRunning(session *SessionSummary, terminal cursorRunningTerminal, count int, generatedAt time.Time) {
	reason := "a running terminal command (pid " + strconv.Itoa(terminal.PID) + ") belongs to this workspace; terminals are per workspace, so it is credited to the most recently active chat"
	if count > 1 {
		reason += " (" + strconv.Itoa(count) + " running)"
	}
	setSessionState(session, activeSessionStatus, reason, "cursor-terminals", "medium", session.LastActiveAt)
	session.CompletionStatus = "not_finished"
}

// readCursorRunningTerminals returns terminals marked "status: running"
// whose pid is still alive; a stale file from a killed Cursor is ignored.
func readCursorRunningTerminals(dir string) []cursorRunningTerminal {
	files, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return nil
	}
	// Terminal files are numbered; the highest number is the newest.
	sort.Slice(files, func(i, j int) bool {
		a, aerr := strconv.Atoi(strings.TrimSuffix(filepath.Base(files[i]), ".txt"))
		b, berr := strconv.Atoi(strings.TrimSuffix(filepath.Base(files[j]), ".txt"))
		if aerr != nil || berr != nil {
			return files[i] < files[j]
		}
		return a < b
	})
	var running []cursorRunningTerminal
	for _, path := range files {
		if t, ok := parseCursorTerminalHeader(path); ok && cursorPIDAlive(t.PID) {
			running = append(running, t)
		}
	}
	return running
}

func parseCursorTerminalHeader(path string) (cursorRunningTerminal, bool) {
	f, err := os.Open(path)
	if err != nil {
		return cursorRunningTerminal{}, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), cursorMaxLineBytes)
	fields := map[string]string{}
	started := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			if started {
				break
			}
			started = true
			continue
		}
		if !started {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		fields[strings.TrimSpace(key)] = value
	}
	pid, err := strconv.Atoi(fields["pid"])
	if err != nil || pid <= 0 || fields["status"] != "running" {
		return cursorRunningTerminal{}, false
	}
	return cursorRunningTerminal{PID: pid, Title: fields["title"], Command: fields["command"]}, true
}

func cursorPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// cursorSlugToPath maps a project slug back to a directory by trying every
// way of regrouping its "-" separated parts into existing path segments,
// since a literal "-" in a directory name is indistinguishable from a "/".
// Falls back to the naive all-slashes reading when nothing exists on disk.
func cursorSlugToPath(slug string) string {
	parts := strings.Split(slug, "-")
	var resolve func(base string, rest []string) (string, bool)
	resolve = func(base string, rest []string) (string, bool) {
		if len(rest) == 0 {
			return base, true
		}
		for n := 1; n <= len(rest); n++ {
			candidate := filepath.Join(base, strings.Join(rest[:n], "-"))
			if st, err := os.Stat(candidate); err != nil || (!st.IsDir() && n < len(rest)) {
				continue
			}
			if path, ok := resolve(candidate, rest[n:]); ok {
				return path, true
			}
		}
		return "", false
	}
	if path, ok := resolve("/", parts); ok {
		return path
	}
	return "/" + strings.ReplaceAll(slug, "-", "/")
}

func listLiveCursorProcesses(ctx context.Context) ([]liveAgentProcess, error) {
	return listLiveAgentProcesses(ctx, providerCursor, looksLikeCursorCommand, true)
}

// looksLikeCursorCommand matches an interactive cursor-agent CLI run. On
// macOS the launcher script execs a bundled node binary under
// .../cursor-agent/versions/<v>/, so the process name is "node" (with that
// path) rather than "cursor-agent". The IDE's own background worker lives
// under anysphere.cursor-agent-worker and is not a chat, so it is excluded.
func looksLikeCursorCommand(command string) bool {
	command = strings.TrimSpace(strings.ReplaceAll(command, "\\", "/"))
	if command == "" || strings.Contains(command, "cursor-agent-worker") {
		return false
	}
	// comm= may carry the executable path; its arguments are never included,
	// but the path itself can contain spaces (Application Support).
	base := filepath.Base(command)
	if base == "cursor-agent" || base == "cursor-agent.exe" {
		return true
	}
	return base == "node" && strings.Contains(command, "/cursor-agent/versions/")
}
