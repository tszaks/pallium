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
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeContentItem struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type claudeToolInput struct {
	Command     string `json:"command"`
	Description string `json:"description"`
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

	summaries := make([]SessionSummary, 0, len(sessionFiles))
	for _, path := range sessionFiles {
		session, err := readClaudeSessionFile(path, opts.IncludeDetails)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, session)
	}

	activeByID := matchActiveSessions(summaries, liveProcesses, generatedAt, startingClaudeSession)
	sessions := make([]SessionSummary, 0, len(summaries)+len(liveProcesses))
	seen := make(map[string]bool, len(summaries))

	for _, session := range summaries {
		if active, ok := activeByID[session.ThreadID]; ok {
			session = active
		} else if !opts.IncludeAll {
			continue
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

func startingClaudeSession(proc liveAgentProcess, generatedAt time.Time) SessionSummary {
	return SessionSummary{
		Provider:         providerClaude,
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

	session.FirstUserMessage = firstUserMessage
	session.Title = firstNonEmpty(title, agentName, firstUserMessage, lastPrompt, startingSessionTitle)
	if session.LastActiveAt.IsZero() {
		session.LastActiveAt = info.ModTime().UTC()
	}
	if session.EffectiveWorkdir == "" {
		session.EffectiveWorkdir = session.SessionCWD
	}
	if includeDetails {
		session.RecentAction = recentAction
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
	var items []claudeContentItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
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
