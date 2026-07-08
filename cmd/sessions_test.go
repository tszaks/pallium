package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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
