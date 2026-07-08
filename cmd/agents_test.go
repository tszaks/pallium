package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tszaks/pallium/internal/agentdocs"
)

func TestAgentsBlockPrintsMarkers(t *testing.T) {
	var out bytes.Buffer
	if err := runAgents(&out, []string{"block"}, false); err != nil {
		t.Fatalf("agents block failed: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, agentsBlockBegin) || !strings.Contains(text, agentsBlockEnd) {
		t.Fatalf("expected block markers, got %q", text)
	}
}

func TestAgentsGuidePrintsGuide(t *testing.T) {
	var out bytes.Buffer
	if err := runAgents(&out, []string{"guide"}, false); err != nil {
		t.Fatalf("agents guide failed: %v", err)
	}
	if !strings.Contains(out.String(), "When to use Pallium") {
		t.Fatalf("expected guide content, got %q", out.String())
	}
}

func TestAgentsGuideMatchesRepoRootPalliumMD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "PALLIUM.md"))
	if err != nil {
		t.Fatalf("read repo-root PALLIUM.md: %v", err)
	}
	if string(raw) != agentdocs.Guide {
		t.Fatal("internal/agentdocs/PALLIUM.md is out of sync with repo-root PALLIUM.md; copy the root file over it")
	}
}

func TestAgentsInstallCreatesAgentsMD(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runAgents(&out, []string{"install", "--dir", dir}, false); err != nil {
		t.Fatalf("agents install failed: %v", err)
	}
	if !strings.Contains(out.String(), "AGENTS.md: created") {
		t.Fatalf("expected created output, got %q", out.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(raw) != agentsBlock+"\n" {
		t.Fatalf("unexpected AGENTS.md content: %q", string(raw))
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("CLAUDE.md should not be created when it did not exist")
	}
}

func TestAgentsInstallAppendsToExistingFiles(t *testing.T) {
	dir := t.TempDir()
	agentsPath := filepath.Join(dir, "AGENTS.md")
	claudePath := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(agentsPath, []byte("# Existing agents notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte("# Existing claude notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAgents(&out, []string{"install", "--dir", dir}, false); err != nil {
		t.Fatalf("agents install failed: %v", err)
	}
	if !strings.Contains(out.String(), "AGENTS.md: appended") || !strings.Contains(out.String(), "CLAUDE.md: appended") {
		t.Fatalf("expected appended output for both files, got %q", out.String())
	}
	for path, heading := range map[string]string{agentsPath: "# Existing agents notes", claudePath: "# Existing claude notes"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		content := string(raw)
		if !strings.HasPrefix(content, heading+"\n\n") {
			t.Fatalf("expected existing content preserved with blank-line separator in %s, got %q", path, content)
		}
		if !strings.Contains(content, agentsBlock) {
			t.Fatalf("expected block appended in %s, got %q", path, content)
		}
	}
}

func TestAgentsInstallIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte("# Claude rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAgents(&out, []string{"install", "--dir", dir}, false); err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	first, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runAgents(&out, []string{"install", "--dir", dir}, false); err != nil {
		t.Fatalf("second install failed: %v", err)
	}
	if !strings.Contains(out.String(), "CLAUDE.md: unchanged") {
		t.Fatalf("expected unchanged output, got %q", out.String())
	}
	second, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("install is not idempotent:\nfirst:  %q\nsecond: %q", string(first), string(second))
	}
}
