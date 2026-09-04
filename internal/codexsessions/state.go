package codexsessions

import (
	"path/filepath"
	"strings"
	"time"
)

const (
	lifecycleRunning  = "running"
	lifecycleFinished = "finished"
)

type sessionSignals struct {
	Source       string
	LatestAt     time.Time
	Lifecycle    string
	LifecycleAt  time.Time
	PendingTool  string
	PendingSince time.Time
}

func applySessionState(session *SessionSummary, signals sessionSignals, generatedAt time.Time) {
	if signals.LatestAt.After(session.LastActiveAt) {
		session.LastActiveAt = signals.LatestAt
	}
	session.CompletionStatus = "unknown"
	session.CompletedAt = nil
	switch signals.Lifecycle {
	case lifecycleFinished:
		session.CompletionStatus = "finished"
		if !signals.LifecycleAt.IsZero() {
			completedAt := signals.LifecycleAt
			session.CompletedAt = &completedAt
		}
	case lifecycleRunning:
		session.CompletionStatus = "not_finished"
	}
	if signals.PendingTool != "" {
		session.CompletionStatus = "not_finished"
	}

	if session.PID <= 0 {
		reason := "no live process or open desktop task was found"
		source := "process"
		confidence := "high"
		if session.CompletionStatus == "finished" {
			reason = "the transcript records task completion, and no live process or open desktop task was found"
			source = firstNonEmpty(signals.Source, source)
		} else if session.CompletionStatus == "not_finished" {
			reason = "the latest transcript task has no completion event, and no live process or open desktop task was found"
			source = firstNonEmpty(signals.Source, source)
			confidence = "medium"
		}
		setSessionState(session, inactiveSessionStatus, reason, source, confidence, session.LastActiveAt)
		return
	}

	if signals.Lifecycle == lifecycleFinished {
		setSessionState(session, finishedSessionStatus, "the latest transcript lifecycle event completed the task", signals.Source, "high", signals.LifecycleAt)
		return
	}

	if signals.PendingTool != "" {
		switch {
		case isBlockedTool(signals.PendingTool):
			setSessionState(session, blockedSessionStatus, "waiting for user input or approval via "+signals.PendingTool, signals.Source, "high", signals.PendingSince)
			return
		case isWaitingTool(signals.PendingTool):
			setSessionState(session, waitingSessionStatus, "an explicit wait operation is still pending: "+signals.PendingTool, signals.Source, "high", signals.PendingSince)
			return
		case processLooksStuck(session.ProcessState) && staleFor(session, signals, generatedAt) >= stuckActivityWindow:
			setSessionState(session, stuckSessionStatus, "a pending tool has made no transcript progress while the process is stopped or uninterruptible", "process+"+signals.Source, "high", signals.PendingSince)
			return
		default:
			setSessionState(session, activeSessionStatus, "tool execution is still pending: "+signals.PendingTool, signals.Source, "high", signals.PendingSince)
			return
		}
	}

	if processLooksStuck(session.ProcessState) && staleFor(session, signals, generatedAt) >= stuckActivityWindow {
		setSessionState(session, stuckSessionStatus, "the process is stopped or uninterruptible and the transcript has not advanced", "process+recency", "high", session.LastActiveAt)
		return
	}

	lastActivity := session.LastActiveAt
	if lastActivity.IsZero() && session.AgeSeconds > 0 {
		lastActivity = generatedAt.Add(-time.Duration(session.AgeSeconds) * time.Second)
	}
	if !lastActivity.IsZero() && generatedAt.Sub(lastActivity) <= liveActivityWindow {
		setSessionState(session, activeSessionStatus, "transcript activity was observed within the last two minutes", firstNonEmpty(signals.Source, "recency"), "medium", lastActivity)
		return
	}

	setSessionState(session, idleSessionStatus, "the process is live but no stronger current-work signal was found", "process+recency", "medium", lastActivity)
}

func setSessionState(session *SessionSummary, status, reason, source, confidence string, since time.Time) {
	session.Status = status
	session.StatusReason = reason
	session.StatusSource = source
	session.StatusConfidence = confidence
	session.StatusSince = since
}

func staleFor(session *SessionSummary, signals sessionSignals, generatedAt time.Time) time.Duration {
	last := session.LastActiveAt
	if signals.LatestAt.After(last) {
		last = signals.LatestAt
	}
	if last.IsZero() {
		return time.Duration(session.AgeSeconds) * time.Second
	}
	return generatedAt.Sub(last)
}

func processLooksStuck(state string) bool {
	state = strings.ToUpper(strings.TrimSpace(state))
	if state == "" {
		return false
	}
	switch state[0] {
	case 'D', 'T', 'U', 'Z':
		return true
	default:
		return false
	}
}

func isBlockedTool(name string) bool {
	name = normalizeToolName(name)
	return strings.Contains(name, "request_user_input") ||
		strings.Contains(name, "ask_user") ||
		strings.Contains(name, "askuser") ||
		strings.Contains(name, "approval")
}

func isWaitingTool(name string) bool {
	name = normalizeToolName(name)
	return name == "wait" ||
		strings.Contains(name, "wait_agent") ||
		strings.Contains(name, "wait_threads") ||
		strings.Contains(name, "write_stdin")
}

func normalizeToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = filepath.Base(name)
	return strings.ReplaceAll(name, "-", "_")
}

func inferNestedToolName(name, rawInput string) string {
	if normalizeToolName(name) != "exec" || rawInput == "" {
		return name
	}
	lower := strings.ToLower(rawInput)
	for _, candidate := range []string{
		"request_user_input",
		"wait_threads",
		"wait_agent",
		"write_stdin",
	} {
		if strings.Contains(lower, candidate) {
			return candidate
		}
	}
	return name
}

func defaultDiscoveryCoverage() DiscoveryCoverage {
	return DiscoveryCoverage{
		Scope:        "local-agent-sessions",
		Completeness: "best-effort",
		Includes: []string{
			"Codex CLI sessions",
			"Codex desktop tasks with open transcripts",
			"Claude Code CLI sessions",
		},
		Excludes: []string{
			"generic terminal shells",
			"SSH and tmux sessions",
			"browser tabs and application windows",
			"agent providers without a registered detector",
		},
	}
}

func liveStatusRank(status string) int {
	switch status {
	case blockedSessionStatus:
		return 0
	case stuckSessionStatus:
		return 1
	case waitingSessionStatus:
		return 2
	case activeSessionStatus:
		return 3
	case idleSessionStatus:
		return 4
	case finishedSessionStatus:
		return 5
	default:
		return 6
	}
}
