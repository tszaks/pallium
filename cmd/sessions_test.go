package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tszaks/pallium/internal/codexsessions"
	"github.com/tszaks/pallium/internal/sessionmemory"
)

func TestSessionsIndexHelpDoesNotStartIndex(t *testing.T) {
	var out bytes.Buffer
	if err := runSessionsIndex(&out, []string{"--help"}, false); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium sessions") {
		t.Fatalf("expected sessions help, got %q", out.String())
	}
}

func TestRunningSessionSummariesExcludeFinishedAndInactive(t *testing.T) {
	sessions := []codexsessions.SessionSummary{
		{ThreadID: "active", Status: "active"},
		{ThreadID: "waiting", Status: "waiting"},
		{ThreadID: "blocked", Status: "blocked"},
		{ThreadID: "idle", Status: "idle"},
		{ThreadID: "finished", Status: "finished"},
		{ThreadID: "inactive", Status: "inactive"},
	}
	got := runningSessionSummaries(sessions)
	if len(got) != 4 {
		t.Fatalf("running sessions=%v", got)
	}
	for _, session := range got {
		if session.Status == "finished" || session.Status == "inactive" {
			t.Fatalf("non-running session leaked into result: %+v", session)
		}
	}
}

func TestSessionsEmbedHelpDoesNotStartEmbedding(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_ADMIN_API_KEY", "")

	var out bytes.Buffer
	if err := runSessionsEmbed(&out, []string{"--help"}, false); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium sessions") {
		t.Fatalf("expected sessions help, got %q", out.String())
	}
}

func TestSessionsEmbeddingConfigurePersistsAndBecomesEmbedDefault(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".pallium", "embedding.json")
	t.Setenv("PALLIUM_EMBED_CONFIG", configPath)
	for _, key := range []string{"PALLIUM_EMBED_PROVIDER", "PALLIUM_EMBED_BASE_URL", "PALLIUM_EMBED_MODEL", "PALLIUM_EMBED_API_KEY", "OPENAI_API_KEY", "OPENAI_ADMIN_API_KEY"} {
		t.Setenv(key, "")
	}
	var out bytes.Buffer
	if err := runSessions(&out, []string{"embedding", "configure", "--provider", "ollama", "--model", "embeddinggemma"}, true); err != nil {
		t.Fatal(err)
	}
	var status sessionmemory.EmbeddingStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Provider != "ollama" || status.Model != "embeddinggemma" || status.ProviderSource != "config" {
		t.Fatalf("unexpected configured status: %+v", status)
	}

	out.Reset()
	dbPath := filepath.Join(t.TempDir(), "sessions.sqlite")
	if err := runSessions(&out, []string{"embed", "--db", dbPath, "--limit", "1"}, true); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "embeddinggemma" || payload["embedded"] != float64(0) {
		t.Fatalf("saved model did not become embed default: %v", payload)
	}
}

func TestSessionsEmbeddingConfigureAcceptsKeychain(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".pallium", "embedding.json")
	t.Setenv("PALLIUM_EMBED_CONFIG", configPath)
	for _, key := range []string{"PALLIUM_EMBED_PROVIDER", "PALLIUM_EMBED_BASE_URL", "PALLIUM_EMBED_MODEL", "PALLIUM_EMBED_API_KEY", "OPENAI_API_KEY", "OPENAI_ADMIN_API_KEY"} {
		t.Setenv(key, "")
	}

	var out bytes.Buffer
	err := runSessions(&out, []string{"embedding", "configure", "--provider", "openai", "--model", "text-embedding-3-small", "--credential-store", "keychain"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var status sessionmemory.EmbeddingStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Provider != "openai" || status.Model != "text-embedding-3-small" || !status.Configured {
		t.Fatalf("unexpected embedding status: %+v", status)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"credential_store": "keychain"`)) {
		t.Fatalf("keychain selection not persisted: %s", raw)
	}
}

func TestSessionsIndexRejectsUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	err := runSessionsIndex(&out, []string{"--bogus"}, false)
	if err == nil {
		t.Fatal("expected unknown flag error")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("expected unknown flag error, got %v", err)
	}
}

func TestSessionsIndexRejectsPositionalIncludePath(t *testing.T) {
	var out bytes.Buffer
	err := runSessionsIndex(&out, []string{"/tmp/sessions"}, false)
	if err == nil {
		t.Fatal("expected positional include error")
	}
	if !strings.Contains(err.Error(), "use --include") {
		t.Fatalf("expected --include guidance, got %v", err)
	}
}

func TestSessionsIndexAcceptsForceFlag(t *testing.T) {
	tmp := t.TempDir()
	codexHome := filepath.Join(tmp, ".codex")
	if err := os.MkdirAll(filepath.Join(codexHome, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runSessionsIndex(&out, []string{"--provider", "codex", "--codex-home", codexHome, "--db", filepath.Join(tmp, "sessions.sqlite"), "--force"}, false)
	if err != nil {
		t.Fatalf("force flag returned error: %v", err)
	}
	if !strings.Contains(out.String(), "Indexed 0") {
		t.Fatalf("expected index output, got %q", out.String())
	}
}

func TestSessionsStatsHelpDoesNotReadStats(t *testing.T) {
	var out bytes.Buffer
	if err := runSessions(&out, []string{"stats", "--help"}, false); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium sessions") {
		t.Fatalf("expected sessions help, got %q", out.String())
	}
}

func TestTrimTextKeepsMultiByteRunesIntact(t *testing.T) {
	got := trimText(strings.Repeat("é", 60), 90)
	if !utf8.ValidString(got) {
		t.Fatalf("trimText produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 90 {
		t.Fatalf("rune count = %d, want <= 90", n)
	}
	truncated := trimText(strings.Repeat("é", 120), 90)
	if !utf8.ValidString(truncated) {
		t.Fatalf("trimText produced invalid UTF-8: %q", truncated)
	}
	if n := utf8.RuneCountInString(truncated); n != 90 {
		t.Fatalf("rune count = %d, want 90", n)
	}
	if !strings.HasSuffix(truncated, "…") {
		t.Fatalf("truncated result missing ellipsis: %q", truncated)
	}
	if got := trimText("hello world", 90); got != "hello world" {
		t.Fatalf("ASCII input changed: %q", got)
	}
}

func TestSessionsSearchQueryContainingHelpRunsSearch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	if err := runSessions(&out, []string{"search", "help", "with", "auth"}, true); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if strings.Contains(out.String(), "pallium sessions") {
		t.Fatalf("expected search results, got help output: %q", out.String())
	}
}

func TestSessionsSearchBareHelpShowsHelp(t *testing.T) {
	var out bytes.Buffer
	if err := runSessions(&out, []string{"search", "help"}, false); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium sessions") {
		t.Fatalf("expected sessions help, got %q", out.String())
	}
	out.Reset()
	if err := runSessions(&out, []string{"search", "-h"}, false); err != nil {
		t.Fatalf("-h returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium sessions") {
		t.Fatalf("expected sessions help, got %q", out.String())
	}
}

func TestFindGoalAttachmentLastOccurrenceWins(t *testing.T) {
	tmp := t.TempDir()
	rollout := filepath.Join(tmp, "rollout-test.jsonl")
	content := `{"text":"see /Users/x/.codex/attachments/aaaaaaaa-1111-1111-1111-111111111111/goal-objective.md"}
{"text":"unrelated line"}
{"text":"now /Users/x/.codex/attachments/bbbbbbbb-2222-2222-2222-222222222222/goal-objective.md and also /Users/x/.codex/attachments/cccccccc-3333-3333-3333-333333333333/other.md"}
`
	if err := os.WriteFile(rollout, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, others, err := findGoalAttachment(rollout)
	if err != nil {
		t.Fatalf("findGoalAttachment: %v", err)
	}
	want := "/Users/x/.codex/attachments/cccccccc-3333-3333-3333-333333333333/other.md"
	if got != want {
		t.Fatalf("goalPath = %q, want %q", got, want)
	}
	if len(others) != 2 {
		t.Fatalf("otherCandidates = %v, want 2 entries", others)
	}
}

func TestExpandTildeExpandsHomeRelativePath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	got := expandTilde("~/.codex/attachments/x/goal-objective.md")
	want := filepath.Join(tmp, ".codex/attachments/x/goal-objective.md")
	if got != want {
		t.Fatalf("expandTilde = %q, want %q", got, want)
	}
	if got := expandTilde("/already/absolute/path.md"); got != "/already/absolute/path.md" {
		t.Fatalf("expandTilde changed absolute path: %q", got)
	}
}

func TestSessionsGoalResolvesLastReferenceAndExpandsTilde(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	sessionID := "11112222-3333-4444-5555-666677778888"
	dir := filepath.Join(tmp, ".codex", "sessions", "2026", "02", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-2026-02-02T00-00-00-"+sessionID+".jsonl")
	lines := []string{
		`{"text":"first ~/.codex/attachments/1111aaaa-1111-1111-1111-111111111111/old-goal.md"}`,
		`{"text":"noise"}`,
		`{"text":"latest ~/.codex/attachments/2222bbbb-2222-2222-2222-222222222222/goal-objective.md"}`,
	}
	if err := os.WriteFile(rollout, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runSessionsGoal(&out, []string{sessionID}, true); err != nil {
		t.Fatalf("runSessionsGoal: %v", err)
	}
	var result sessionGoalResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := filepath.Join(tmp, ".codex/attachments/2222bbbb-2222-2222-2222-222222222222/goal-objective.md")
	if result.GoalPath != want {
		t.Fatalf("goalPath = %q, want %q", result.GoalPath, want)
	}
	if len(result.OtherCandidates) != 1 {
		t.Fatalf("otherCandidates = %v, want 1 entry", result.OtherCandidates)
	}
}

func TestSessionsGoalNoAttachmentReferenceErrors(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	sessionID := "abcdefab-1234-1234-1234-abcdefabcdef"
	dir := filepath.Join(tmp, ".codex", "sessions", "2026", "01", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-2026-01-01T00-00-00-"+sessionID+".jsonl")
	if err := os.WriteFile(rollout, []byte(`{"type":"other"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runSessionsGoal(&out, []string{sessionID}, false)
	if err == nil {
		t.Fatal("expected error for missing goal reference")
	}
	want := "no goal file reference found in session " + sessionID
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestSessionFlagsCanFollowPositionals(t *testing.T) {
	fs := newSessionFlagSet("test")
	limit := fs.Int("limit", 10, "")
	if err := parseSessionFlags(fs, []string{"repo", "--limit", "3"}, map[string]struct{}{"limit": {}}, nil); err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if *limit != 3 {
		t.Fatalf("limit=%d, want 3", *limit)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "repo" {
		t.Fatalf("positionals=%v, want repo", fs.Args())
	}
}

func TestSessionsDoctorReportsMissingDatabaseAsJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	var out bytes.Buffer
	if err := runSessions(&out, []string{"doctor", "--db", path}, true); err != nil {
		t.Fatal(err)
	}
	var report sessionmemory.SessionDoctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.DBExists || len(report.Issues) == 0 {
		t.Fatalf("unexpected doctor report: %+v", report)
	}
}

func TestSessionsForgetPreviewsBeforeConfirmedDeletion(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	projectDir := filepath.Join(claudeHome, "projects", "-tmp-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"remember this"},"timestamp":"2026-06-10T12:00:00Z","sessionId":"forget-command","cwd":"/tmp/repo"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"remembered"},"timestamp":"2026-06-10T12:01:00Z","sessionId":"forget-command","cwd":"/tmp/repo"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(projectDir, "forget-command.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tmp, "sessions.sqlite")
	if _, err := sessionmemory.Index(context.Background(), sessionmemory.Options{DBPath: path, ClaudeHome: claudeHome, Provider: "claude", Force: true}, nil); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runSessions(&out, []string{"forget", "forget-command", "--db", path}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Preview only") {
		t.Fatalf("expected safe preview, got %q", out.String())
	}
	out.Reset()
	if err := runSessions(&out, []string{"forget", "forget-command", "--db", path, "--confirm"}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Forgot session") {
		t.Fatalf("expected deletion result, got %q", out.String())
	}
}

func TestParseSessionRetentionAge(t *testing.T) {
	for input, want := range map[string]string{
		"180d": "4320h0m0s",
		"12w":  "2016h0m0s",
		"720h": "720h0m0s",
	} {
		got, err := parseSessionRetentionAge(input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if got.String() != want {
			t.Fatalf("%s parsed as %s, want %s", input, got, want)
		}
	}
	if _, err := parseSessionRetentionAge("0d"); err == nil {
		t.Fatal("expected zero age to fail")
	}
}

func TestSessionsContinuityCommandsReturnStructuredJSON(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	projectDir := filepath.Join(claudeHome, "projects", "-tmp-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(projectDir, "continuity-command.jsonl")
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"checkout continuity"},"timestamp":"2026-06-10T12:00:00Z","sessionId":"continuity-command","cwd":"/tmp/repo"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"Next action: run the smoke test"},"timestamp":"2026-06-10T12:01:00Z","sessionId":"continuity-command","cwd":"/tmp/repo"}`,
		"",
	}, "\n")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(transcriptPath, old, old); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(tmp, "sessions.sqlite")
	if _, err := sessionmemory.Index(context.Background(), sessionmemory.Options{DBPath: dbPath, ClaudeHome: claudeHome, Provider: "claude", Force: true}, nil); err != nil {
		t.Fatal(err)
	}

	var recallOut bytes.Buffer
	if err := runSessions(&recallOut, []string{"recall", "checkout", "continuity", "--db", dbPath, "--lexical-only"}, true); err != nil {
		t.Fatal(err)
	}
	var recall sessionmemory.RecallReport
	if err := json.Unmarshal(recallOut.Bytes(), &recall); err != nil {
		t.Fatalf("invalid recall JSON: %v: %s", err, recallOut.String())
	}
	if recall.SessionID != "continuity-command" || recall.NextAction != "run the smoke test" {
		t.Fatalf("unexpected recall: %+v", recall)
	}

	var showOut bytes.Buffer
	if err := runSessions(&showOut, []string{"show", "continuity-command", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var show map[string]any
	if err := json.Unmarshal(showOut.Bytes(), &show); err != nil || show["capsule"] == nil {
		t.Fatalf("invalid show JSON: %v: %s", err, showOut.String())
	}

	var readOut bytes.Buffer
	if err := runSessions(&readOut, []string{"read", "continuity-command", "--db", dbPath, "--limit", "1"}, true); err != nil {
		t.Fatal(err)
	}
	var read map[string]any
	if err := json.Unmarshal(readOut.Bytes(), &read); err != nil || read["messages"] == nil {
		t.Fatalf("invalid read JSON: %v: %s", err, readOut.String())
	}

	var openOut bytes.Buffer
	if err := runSessions(&openOut, []string{"open", "continuity-command", "--db", dbPath}, true); err != nil {
		t.Fatal(err)
	}
	var location sessionmemory.SessionLocation
	if err := json.Unmarshal(openOut.Bytes(), &location); err != nil || location.RolloutPath != transcriptPath {
		t.Fatalf("invalid open JSON: %v: %s", err, openOut.String())
	}
}
