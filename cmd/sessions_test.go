package cmd

import (
	"bytes"
	"strings"
	"testing"
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

func TestSessionsStatsHelpDoesNotReadStats(t *testing.T) {
	var out bytes.Buffer
	if err := runSessions(&out, []string{"stats", "--help"}, false); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "pallium sessions") {
		t.Fatalf("expected sessions help, got %q", out.String())
	}
}
