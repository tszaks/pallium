package codexsessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type claudeLogEntry struct {
	Type       string        `json:"type"`
	Timestamp  string        `json:"timestamp"`
	SessionID  string        `json:"sessionId"`
	CWD        string        `json:"cwd"`
	GitBranch  string        `json:"gitBranch"`
	AITitle    string        `json:"aiTitle"`
	AgentName  string        `json:"agentName"`
	LastPrompt string        `json:"lastPrompt"`
	Message    claudeMessage `json:"message"`
}

type claudeMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stop_reason"`
}

type claudeContentItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type claudeToolInput struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type claudeHistoryEntry struct {
	Display   string `json:"display"`
	Timestamp int64  `json:"timestamp"`
	Project   string `json:"project"`
	SessionID string `json:"sessionId"`
}

type claudeHistorySummary struct {
	FirstPrompt string
	LastPrompt  string
	Project     string
	UpdatedAt   time.Time
}

func collectClaudeSessions(ctx context.Context, opts SessionCollectOptions, generatedAt time.Time) ([]SessionSummary, error) {
	claudeHome, err := claudeHomeDirFunc()
	if err != nil {
		return nil, err
	}

	liveProcesses, err := listLiveClaudeProcessesVar(ctx)
	if err != nil {
		return nil, err
	}
	if len(liveProcesses) == 0 && !opts.IncludeAll {
		return []SessionSummary{}, nil
	}

	projectsRoot := filepath.Join(claudeHome, claudeProjectsDir)
	if _, err := os.Stat(projectsRoot); err != nil {
		if os.IsNotExist(err) {
			sessions := make([]SessionSummary, 0, len(liveProcesses))
			for _, proc := range liveProcesses {
				sessions = append(sessions, startingClaudeSession(proc, generatedAt))
			}
			return sessions, nil
		}
		return nil, fmt.Errorf("failed to access claude projects directory: %w", err)
	}

	sessionFiles, err := listClaudeSessionFiles(projectsRoot)
	if err != nil {
		return nil, err
	}

	history, err := readClaudeHistory(filepath.Join(claudeHome, "history.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	summaries := make([]SessionSummary, 0, len(sessionFiles))
	pathsByID := make(map[string]string, len(sessionFiles))
	for _, path := range sessionFiles {
		id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		pathsByID[id] = path
		summary := SessionSummary{Provider: providerClaude, ThreadID: id, Status: inactiveSessionStatus}
		item, hasHistory := history[id]
		if hasHistory {
			summary.Title = compactSummaryText(firstNonEmpty(item.FirstPrompt, item.LastPrompt, id), 240)
			if opts.IncludeDetails {
				summary.FirstUserMessage = compactSummaryText(item.FirstPrompt, 500)
			}
			summary.SessionCWD = item.Project
			summary.EffectiveWorkdir = item.Project
			summary.LastActiveAt = item.UpdatedAt
		}
		needsTranscript := !hasHistory || summary.Title == "" || summary.SessionCWD == "" || summary.EffectiveWorkdir == "" || summary.LastActiveAt.IsZero() || (opts.IncludeDetails && summary.FirstUserMessage == "")
		if needsTranscript {
			if transcriptSummary, readErr := readClaudeSessionFile(path, opts.IncludeDetails); readErr == nil {
				if hasHistory {
					mergeMissingClaudeSessionSummary(&summary, transcriptSummary)
				} else {
					summary = transcriptSummary
				}
			} else if !hasHistory {
				if info, statErr := os.Stat(path); statErr == nil {
					summary.Title = id
					summary.LastActiveAt = info.ModTime().UTC()
				}
			}
		}
		pathsByID[summary.ThreadID] = path
		summaries = append(summaries, summary)
	}

	activeByID := matchActiveSessions(summaries, liveProcesses, generatedAt, startingClaudeSession)
	sessions := make([]SessionSummary, 0, len(summaries)+len(liveProcesses))
	seen := make(map[string]bool, len(summaries))

	for _, session := range summaries {
		if active, ok := activeByID[session.ThreadID]; ok {
			session = active
			enrichClaudeSessionFromTail(&session, pathsByID[session.ThreadID], opts.IncludeDetails, generatedAt)
		} else if !opts.IncludeAll {
			continue
		} else if opts.IncludeCompletion {
			enrichClaudeSessionFromTail(&session, pathsByID[session.ThreadID], false, generatedAt)
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

func mergeMissingClaudeSessionSummary(summary *SessionSummary, transcript SessionSummary) {
	if summary.Title == "" {
		summary.Title = transcript.Title
	}
	if summary.FirstUserMessage == "" {
		summary.FirstUserMessage = transcript.FirstUserMessage
	}
	if summary.SessionCWD == "" {
		summary.SessionCWD = transcript.SessionCWD
	}
	if summary.EffectiveWorkdir == "" {
		summary.EffectiveWorkdir = transcript.EffectiveWorkdir
	}
	if summary.LastActiveAt.IsZero() {
		summary.LastActiveAt = transcript.LastActiveAt
	}
	if summary.GitBranch == "" {
		summary.GitBranch = transcript.GitBranch
	}
	if summary.RecentAction == "" {
		summary.RecentAction = transcript.RecentAction
	}
}

func startingClaudeSession(proc liveAgentProcess, generatedAt time.Time) SessionSummary {
	session := SessionSummary{
		Provider:         providerClaude,
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

func readClaudeHistory(path string) (map[string]claudeHistorySummary, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	summaries := map[string]claudeHistorySummary{}
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var entry claudeHistoryEntry
			if json.Unmarshal(bytes.TrimSpace(line), &entry) == nil && entry.SessionID != "" {
				summary := summaries[entry.SessionID]
				if summary.FirstPrompt == "" && strings.TrimSpace(entry.Display) != "" {
					summary.FirstPrompt = compactWhitespace(entry.Display)
				}
				if strings.TrimSpace(entry.Display) != "" {
					summary.LastPrompt = compactWhitespace(entry.Display)
				}
				if entry.Project != "" {
					summary.Project = entry.Project
				}
				if entry.Timestamp > 0 {
					updatedAt := time.UnixMilli(entry.Timestamp).UTC()
					if updatedAt.After(summary.UpdatedAt) {
						summary.UpdatedAt = updatedAt
					}
				}
				summaries[entry.SessionID] = summary
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return summaries, nil
}

func enrichClaudeSessionFromTail(session *SessionSummary, path string, includeDetails bool, generatedAt time.Time) {
	signals := sessionSignals{Source: "claude-transcript"}
	if path == "" {
		applySessionState(session, signals, generatedAt)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		applySessionState(session, signals, generatedAt)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		applySessionState(session, signals, generatedAt)
		return
	}
	const tailBytes = int64(512 * 1024)
	start := max(int64(0), info.Size()-tailBytes)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		applySessionState(session, signals, generatedAt)
		return
	}
	reader := bufio.NewReader(file)
	if start > 0 {
		_, _ = reader.ReadBytes('\n')
	}
	pending := map[string]struct {
		name string
		at   time.Time
	}{}
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var entry claudeLogEntry
			if json.Unmarshal(bytes.TrimSpace(line), &entry) == nil {
				at, _ := parseClaudeTimestamp(entry.Timestamp)
				if at.After(signals.LatestAt) {
					signals.LatestAt = at
				}
				if entry.CWD != "" {
					session.EffectiveWorkdir = entry.CWD
				}
				if entry.GitBranch != "" {
					session.GitBranch = entry.GitBranch
				}
				items := parseClaudeContentItems(entry.Message.Content)
				if entry.Type == "user" {
					signals.Lifecycle = lifecycleRunning
					signals.LifecycleAt = at
					for _, item := range items {
						if item.Type == "tool_result" && item.ToolUseID != "" {
							delete(pending, item.ToolUseID)
						}
					}
				}
				if entry.Type == "assistant" {
					for _, item := range items {
						if item.Type == "tool_use" && item.ID != "" {
							pending[item.ID] = struct {
								name string
								at   time.Time
							}{name: item.Name, at: at}
						}
					}
					if entry.Message.StopReason == "end_turn" {
						signals.Lifecycle = lifecycleFinished
						signals.LifecycleAt = at
						clear(pending)
					}
					if includeDetails {
						if action := claudeRecentAction(entry.Message.Content); action != "" {
							session.RecentAction = action
						}
					}
				}
			}
		}
		if readErr != nil {
			break
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

func listClaudeSessionFiles(projectsRoot string) ([]string, error) {
	projectDirs, err := os.ReadDir(projectsRoot)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, projectDir := range projectDirs {
		if !projectDir.IsDir() {
			continue
		}
		dirPath := filepath.Join(projectsRoot, projectDir.Name())
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			files = append(files, filepath.Join(dirPath, entry.Name()))
		}
	}
	return files, nil
}

func readClaudeSessionFile(path string, includeDetails bool) (SessionSummary, error) {
	file, err := os.Open(path)
	if err != nil {
		return SessionSummary{}, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return SessionSummary{}, err
	}

	session := SessionSummary{
		Provider: providerClaude,
		ThreadID: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Status:   inactiveSessionStatus,
	}

	var firstUserMessage string
	var title string
	var agentName string
	var lastPrompt string
	var recentAction string

	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var entry claudeLogEntry
			if jsonErr := json.Unmarshal(bytes.TrimSpace(line), &entry); jsonErr != nil {
				return SessionSummary{}, fmt.Errorf("failed to parse claude session %s: %w", path, jsonErr)
			}

			if entry.SessionID != "" {
				session.ThreadID = entry.SessionID
			}
			if entry.CWD != "" {
				session.SessionCWD = entry.CWD
				session.EffectiveWorkdir = entry.CWD
			}
			if entry.GitBranch != "" {
				session.GitBranch = entry.GitBranch
			}
			if parsed, ok := parseClaudeTimestamp(entry.Timestamp); ok && parsed.After(session.LastActiveAt) {
				session.LastActiveAt = parsed
			}
			if entry.AITitle != "" {
				title = entry.AITitle
			}
			if entry.AgentName != "" {
				agentName = entry.AgentName
			}
			if entry.LastPrompt != "" {
				lastPrompt = entry.LastPrompt
			}
			if firstUserMessage == "" && entry.Type == "user" && entry.Message.Role == "user" {
				firstUserMessage = claudeMessageText(entry.Message.Content)
			}
			if includeDetails && entry.Type == "assistant" {
				if action := claudeRecentAction(entry.Message.Content); action != "" {
					recentAction = action
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return SessionSummary{}, err
		}
	}

	session.FirstUserMessage = compactSummaryText(firstUserMessage, 500)
	session.Title = compactSummaryText(firstNonEmpty(title, agentName, firstUserMessage, lastPrompt, startingSessionTitle), 240)
	if session.LastActiveAt.IsZero() {
		session.LastActiveAt = info.ModTime().UTC()
	}
	if session.EffectiveWorkdir == "" {
		session.EffectiveWorkdir = session.SessionCWD
	}
	if includeDetails {
		session.RecentAction = compactSummaryText(recentAction, 500)
	}
	return session, nil
}

func parseClaudeTimestamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func claudeMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return compactWhitespace(text)
	}

	var items []claudeContentItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	for _, item := range items {
		if item.Type == "text" && item.Text != "" {
			return compactWhitespace(item.Text)
		}
	}
	return ""
}

func claudeRecentAction(raw json.RawMessage) string {
	items := parseClaudeContentItems(raw)
	for _, item := range items {
		if item.Type != "tool_use" || item.Name == "" {
			continue
		}
		var input claudeToolInput
		_ = json.Unmarshal(item.Input, &input)
		detail := compactWhitespace(firstNonEmpty(input.Description, input.Command))
		if detail == "" {
			return item.Name
		}
		return item.Name + ": " + detail
	}
	return ""
}

func parseClaudeContentItems(raw json.RawMessage) []claudeContentItem {
	var items []claudeContentItem
	_ = json.Unmarshal(raw, &items)
	return items
}
