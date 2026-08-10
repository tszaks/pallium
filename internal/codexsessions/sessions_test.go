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
}
