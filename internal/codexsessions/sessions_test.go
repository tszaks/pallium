package codexsessions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseOpenCodexRolloutsFindsDesktopThreads(t *testing.T) {
	home := "/Users/test/.codex"
	output := []byte(strings.Join([]string{
		"p123",
		"n" + home + "/sessions/2026/08/07/rollout-2026-08-07T12-00-00-019fde59-4d6b-7f71-8cc0-06b953ec2ece.jsonl",
		"n/tmp/unrelated.jsonl",
		"p456",
		"n" + home + "/sessions/2026/08/07/rollout-2026-08-07T12-01-00-019fde44-5d4d-74d1-bdaa-55de0181eddc.jsonl",
		"",
	}, "\n"))
	rollouts := parseOpenCodexRollouts(output, home)
	if len(rollouts) != 2 || rollouts[0].PID != 123 || rollouts[0].ThreadID != "019fde59-4d6b-7f71-8cc0-06b953ec2ece" || rollouts[1].PID != 456 {
		t.Fatalf("unexpected open rollouts: %+v", rollouts)
	}
}

func TestLiveSessionStatusUsesIdleForInactivity(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if got := liveSessionStatus(now.Add(-30*time.Second), now, 3600); got != activeSessionStatus {
		t.Fatalf("recent session status=%q, want active", got)
	}
	if got := liveSessionStatus(now.Add(-10*time.Minute), now, 3600); got != idleSessionStatus {
		t.Fatalf("quiet live session status=%q, want idle", got)
	}
	if got := liveSessionStatus(time.Time{}, now, 30); got != activeSessionStatus {
		t.Fatalf("new process status=%q, want active", got)
	}
	if got := liveSessionStatus(time.Time{}, now, 3600); got != idleSessionStatus {
		t.Fatalf("old process without activity status=%q, want idle", got)
	}
}

func TestLooksLikeCodexCommandRejectsHelperProcesses(t *testing.T) {
	for _, command := range []string{"codex", "/opt/homebrew/bin/codex", "C:\\tools\\codex.exe"} {
		if !looksLikeCodexCommand(command) {
			t.Fatalf("expected %q to be recognized as Codex", command)
		}
	}
	for _, command := range []string{
		"/Applications/Codex.app/Contents/Resources/codex-code-mode-host",
		"/tmp/codex/helper",
		"codex-helper",
	} {
		if looksLikeCodexCommand(command) {
			t.Fatalf("helper process %q must not become a separate session", command)
		}
	}
}

func TestParseLiveAgentProcessesCapturesIdentityAndState(t *testing.T) {
	output := []byte("  101   1 S+   ttys001 01:02 /opt/homebrew/bin/codex\n  102 101 S+ ttys001 00:30 /Applications/Codex.app/Contents/Resources/codex-code-mode-host\n")
	processes, err := parseLiveAgentProcesses(output, providerCodex, looksLikeCodexCommand)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 1 {
		t.Fatalf("expected exactly one real Codex process, got %+v", processes)
	}
	if processes[0].PID != 101 || processes[0].PPID != 1 || processes[0].State != "S+" || processes[0].TTY != "ttys001" || processes[0].AgeSeconds != 62 {
		t.Fatalf("unexpected process parse: %+v", processes[0])
	}
}

func TestApplySessionStateUsesStrongEvidenceAndSafePrecedence(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		session SessionSummary
		signals sessionSignals
		want    string
	}{
		{name: "blocked", session: SessionSummary{PID: 1}, signals: sessionSignals{Source: "test", PendingTool: "request_user_input", PendingSince: now.Add(-time.Minute)}, want: blockedSessionStatus},
		{name: "waiting", session: SessionSummary{PID: 1}, signals: sessionSignals{Source: "test", PendingTool: "wait_threads", PendingSince: now.Add(-time.Minute)}, want: waitingSessionStatus},
		{name: "pending tool is active", session: SessionSummary{PID: 1}, signals: sessionSignals{Source: "test", PendingTool: "exec_command", PendingSince: now.Add(-time.Minute)}, want: activeSessionStatus},
		{name: "stuck requires process evidence", session: SessionSummary{PID: 1, ProcessState: "D", LastActiveAt: now.Add(-20 * time.Minute)}, want: stuckSessionStatus},
		{name: "silence alone is idle", session: SessionSummary{PID: 1, ProcessState: "S", LastActiveAt: now.Add(-20 * time.Minute)}, want: idleSessionStatus},
		{name: "finished lifecycle wins", session: SessionSummary{PID: 1, ProcessState: "D", LastActiveAt: now.Add(-20 * time.Minute)}, signals: sessionSignals{Source: "test", Lifecycle: lifecycleFinished, LifecycleAt: now.Add(-time.Minute)}, want: finishedSessionStatus},
		{name: "no process is inactive", session: SessionSummary{}, signals: sessionSignals{Source: "test", Lifecycle: lifecycleFinished}, want: inactiveSessionStatus},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applySessionState(&test.session, test.signals, now)
			if test.session.Status != test.want {
				t.Fatalf("status=%q, want %q; session=%+v", test.session.Status, test.want, test.session)
			}
			if test.session.StatusReason == "" || test.session.StatusSource == "" || test.session.StatusConfidence == "" {
				t.Fatalf("classification must be explainable: %+v", test.session)
			}
			if test.signals.Lifecycle == lifecycleFinished && test.session.CompletionStatus != CompletionFinished {
				t.Fatalf("finished lifecycle must be independently queryable: %+v", test.session)
			}
		})
	}
}

func TestReadCodexRolloutDetailsTracksLifecycleAndPendingCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	contents := strings.Join([]string{
		`{"timestamp":"2026-09-04T12:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`,
		`{"timestamp":"2026-09-04T12:00:01Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\",\"workdir\":\"/repo\"}","call_id":"call-1"}}`,
		`{"timestamp":"2026-09-04T12:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1"}}`,
		`{"timestamp":"2026-09-04T12:00:03Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"await tools.request_user_input({questions: []})","call_id":"call-2"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	details, err := readCodexRolloutDetails(path)
	if err != nil {
		t.Fatal(err)
	}
	if details.Workdir != "/repo" || details.RecentAction == "" || details.Signals.Lifecycle != lifecycleRunning || details.Signals.PendingTool != "request_user_input" {
		t.Fatalf("unexpected rollout details: %+v", details)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(`{"timestamp":"2026-09-04T12:00:04Z","type":"event_msg","payload":{"type":"task_complete"}}` + "\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append task completion: write=%v close=%v", writeErr, closeErr)
	}
	details, err = readCodexRolloutDetails(path)
	if err != nil {
		t.Fatal(err)
	}
	if details.Signals.Lifecycle != lifecycleFinished || details.Signals.PendingTool != "" {
		t.Fatalf("task completion must clear pending state: %+v", details.Signals)
	}
}

func TestEnrichClaudeSessionFromTailClassifiesPendingTool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.jsonl")
	contents := strings.Join([]string{
		`{"type":"user","timestamp":"2026-09-04T12:00:00Z","message":{"role":"user","content":"run tests"}}`,
		`{"type":"assistant","timestamp":"2026-09-04T12:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"AskUserQuestion","input":{}}],"stop_reason":"tool_use"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	session := SessionSummary{PID: 42, Provider: providerClaude}
	enrichClaudeSessionFromTail(&session, path, true, time.Date(2026, 9, 4, 12, 1, 0, 0, time.UTC))
	if session.Status != blockedSessionStatus || session.StatusSource != "claude-transcript" {
		t.Fatalf("unexpected Claude state: %+v", session)
	}
}

func TestCollectClaudeSessionsUsesHistoryWithoutScanningTranscript(t *testing.T) {
	tmp := t.TempDir()
	projects := filepath.Join(tmp, "projects", "-repo")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "1ee0f2e6-b212-4dad-8abd-f6fe1e2d0965"
	if err := os.WriteFile(filepath.Join(projects, id+".jsonl"), []byte("this is intentionally not valid JSON\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	history := fmt.Sprintf(`{"display":"Review latest session updates","timestamp":%d,"project":"/repo","sessionId":"%s"}`+"\n", generatedAt.Add(-30*time.Second).UnixMilli(), id)
	if err := os.WriteFile(filepath.Join(tmp, "history.jsonl"), []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	originalHome := claudeHomeDirFunc
	originalProcesses := listLiveClaudeProcessesVar
	t.Cleanup(func() {
		claudeHomeDirFunc = originalHome
		listLiveClaudeProcessesVar = originalProcesses
	})
	claudeHomeDirFunc = func() (string, error) { return tmp, nil }
	listLiveClaudeProcessesVar = func(context.Context) ([]liveAgentProcess, error) {
		return []liveAgentProcess{{Provider: providerClaude, PID: 42, TTY: "ttys001", AgeSeconds: 3600, CWD: "/repo"}}, nil
	}
	sessions, err := collectClaudeSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ThreadID != id || sessions[0].Title != "Review latest session updates" || sessions[0].Status != activeSessionStatus {
		t.Fatalf("unexpected Claude live sessions: %+v", sessions)
	}
}

func TestCollectClaudeSessionsFallsBackToTranscriptMetadataWithoutHistory(t *testing.T) {
	tmp := t.TempDir()
	projects := filepath.Join(tmp, "projects", "-repo")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "1ee0f2e6-b212-4dad-8abd-f6fe1e2d0966"
	generatedAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	transcript := fmt.Sprintf(`{"type":"user","timestamp":"%s","sessionId":"%s","cwd":"/repo","gitBranch":"main","message":{"role":"user","content":"Resume the continuity work"}}`+"\n", generatedAt.Add(-30*time.Second).Format(time.RFC3339Nano), id)
	path := filepath.Join(projects, id+".jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	old := generatedAt.Add(-30 * time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	originalHome := claudeHomeDirFunc
	originalProcesses := listLiveClaudeProcessesVar
	t.Cleanup(func() {
		claudeHomeDirFunc = originalHome
		listLiveClaudeProcessesVar = originalProcesses
	})
	claudeHomeDirFunc = func() (string, error) { return tmp, nil }
	listLiveClaudeProcessesVar = func(context.Context) ([]liveAgentProcess, error) {
		return []liveAgentProcess{{Provider: providerClaude, PID: 42, TTY: "ttys001", AgeSeconds: 3600, CWD: "/repo"}}, nil
	}

	sessions, err := collectClaudeSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ThreadID != id || sessions[0].PID != 42 || sessions[0].SessionCWD != "/repo" || sessions[0].Title != "Resume the continuity work" {
		t.Fatalf("transcript metadata did not recover the live Claude session: %+v", sessions)
	}
}

func TestCollectClaudeSessionsFillsMissingProjectFromTranscript(t *testing.T) {
	tmp := t.TempDir()
	projects := filepath.Join(tmp, "projects", "-repo")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "1ee0f2e6-b212-4dad-8abd-f6fe1e2d0967"
	generatedAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	transcript := fmt.Sprintf(`{"type":"user","timestamp":"%s","sessionId":"%s","cwd":"/repo","message":{"role":"user","content":"Fill the missing project"}}`+"\n", generatedAt.Add(-30*time.Second).Format(time.RFC3339Nano), id)
	if err := os.WriteFile(filepath.Join(projects, id+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	history := fmt.Sprintf(`{"display":"History title","timestamp":%d,"sessionId":"%s"}`+"\n", generatedAt.Add(-30*time.Second).UnixMilli(), id)
	if err := os.WriteFile(filepath.Join(tmp, "history.jsonl"), []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}

	originalHome := claudeHomeDirFunc
	originalProcesses := listLiveClaudeProcessesVar
	t.Cleanup(func() {
		claudeHomeDirFunc = originalHome
		listLiveClaudeProcessesVar = originalProcesses
	})
	claudeHomeDirFunc = func() (string, error) { return tmp, nil }
	listLiveClaudeProcessesVar = func(context.Context) ([]liveAgentProcess, error) {
		return []liveAgentProcess{{Provider: providerClaude, PID: 43, TTY: "ttys002", AgeSeconds: 3600, CWD: "/repo"}}, nil
	}

	sessions, err := collectClaudeSessions(context.Background(), SessionCollectOptions{IncludeDetails: true}, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ThreadID != id || sessions[0].PID != 43 || sessions[0].SessionCWD != "/repo" || sessions[0].EffectiveWorkdir != "/repo" || sessions[0].Title != "History title" {
		t.Fatalf("transcript metadata did not fill the incomplete history entry: %+v", sessions)
	}
}

func TestCollectClaudeSessionsWithNoProcessReturnsBeforeFilesystemScan(t *testing.T) {
	originalHome := claudeHomeDirFunc
	originalProcesses := listLiveClaudeProcessesVar
	t.Cleanup(func() {
		claudeHomeDirFunc = originalHome
		listLiveClaudeProcessesVar = originalProcesses
	})
	claudeHomeDirFunc = func() (string, error) { return filepath.Join(t.TempDir(), "missing"), nil }
	listLiveClaudeProcessesVar = func(context.Context) ([]liveAgentProcess, error) { return nil, nil }
	sessions, err := collectClaudeSessions(context.Background(), SessionCollectOptions{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
}

func TestCollectSessionsPreservesOtherProviderWhenOneFails(t *testing.T) {
	originalCodexHome := codexHomeDirFunc
	originalClaudeHome := claudeHomeDirFunc
	originalCodexProcesses := listLiveCodexProcessesVar
	originalClaudeProcesses := listLiveClaudeProcessesVar
	t.Cleanup(func() {
		codexHomeDirFunc = originalCodexHome
		claudeHomeDirFunc = originalClaudeHome
		listLiveCodexProcessesVar = originalCodexProcesses
		listLiveClaudeProcessesVar = originalClaudeProcesses
	})
	tmp := t.TempDir()
	codexHomeDirFunc = func() (string, error) { return tmp, nil }
	claudeHomeDirFunc = func() (string, error) { return tmp, nil }
	listLiveCodexProcessesVar = func(context.Context) ([]liveAgentProcess, error) { return nil, errors.New("process lookup failed") }
	listLiveClaudeProcessesVar = func(context.Context) ([]liveAgentProcess, error) { return nil, nil }
	snapshot, err := CollectSessions(context.Background(), SessionCollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Warnings) != 1 || !strings.Contains(snapshot.Warnings[0], "codex") {
		t.Fatalf("unexpected warnings: %+v", snapshot.Warnings)
	}
	if snapshot.Coverage.Scope != "local-agent-sessions" || len(snapshot.Coverage.Excludes) == 0 || len(snapshot.Coverage.Providers) != 2 {
		t.Fatalf("coverage must disclose discovery scope: %+v", snapshot.Coverage)
	}
}
