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

// 0.9.15: the adoption trigger installs itself — see agents.go's
// maybeOfferAdoptionInstall / version.go's adoptionHintLine. shouldPromptAdoptionOffer
// and isAffirmativeResponse are pulled out of the actual I/O so offer/decline/
// suppress logic is testable without faking a real pty.

func TestShouldPromptAdoptionOfferOnlyWhenAllConditionsAllow(t *testing.T) {
	cases := []struct {
		name                                               string
		jsonOutput, suppressed, hasBlock, isTerminal, want bool
	}{
		{"offers when interactive and missing", false, false, false, true, true},
		{"suppressed by json mode (machine caller)", true, false, false, true, false},
		{"suppressed by env var", false, true, false, true, false},
		{"suppressed because block already exists", false, false, true, true, false},
		{"suppressed by non-interactive stdin (CI/script)", false, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldPromptAdoptionOffer(tc.jsonOutput, tc.suppressed, tc.hasBlock, tc.isTerminal)
			if got != tc.want {
				t.Fatalf("shouldPromptAdoptionOffer(%v,%v,%v,%v) = %v, want %v", tc.jsonOutput, tc.suppressed, tc.hasBlock, tc.isTerminal, got, tc.want)
			}
		})
	}
}

func TestIsAffirmativeResponse(t *testing.T) {
	yes := []string{"y\n", "Y\n", "yes\n", "  y  \n", "Yes please\n"}
	for _, line := range yes {
		if !isAffirmativeResponse(line) {
			t.Fatalf("expected %q to be affirmative", line)
		}
	}
	no := []string{"n\n", "no\n", "\n", "", "   \n", "nah\n"}
	for _, line := range no {
		if isAffirmativeResponse(line) {
			t.Fatalf("expected %q to be a decline, not affirmative — an ambiguous or empty answer must never read as consent", line)
		}
	}
}

func TestHasAgentsBlockDetectsEitherFile(t *testing.T) {
	dir := t.TempDir()
	if hasAgentsBlock(dir) {
		t.Fatal("expected no block detected in an empty dir")
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(agentsBlock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasAgentsBlock(dir) {
		t.Fatal("expected block detected once CLAUDE.md carries it")
	}
}

func TestAdoptionHintSuppressedHonorsEnvVar(t *testing.T) {
	if adoptionHintSuppressed() {
		t.Fatal("expected not suppressed with env var unset")
	}
	t.Setenv("PALLIUM_SKIP_ADOPTION_HINT", "1")
	if !adoptionHintSuppressed() {
		t.Fatal("expected suppressed once PALLIUM_SKIP_ADOPTION_HINT is set")
	}
}

func TestVersionPrintsAdoptionHintWhenBlockMissing(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var out bytes.Buffer
	if err := runVersion(&out, false); err != nil {
		t.Fatalf("version failed: %v", err)
	}
	if !strings.Contains(out.String(), "pallium agents install") {
		t.Fatalf("expected adoption hint in version output, got %q", out.String())
	}
}

func TestVersionSuppressesAdoptionHintWhenEnvSet(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	t.Setenv("PALLIUM_SKIP_ADOPTION_HINT", "1")

	var out bytes.Buffer
	if err := runVersion(&out, false); err != nil {
		t.Fatalf("version failed: %v", err)
	}
	if strings.Contains(out.String(), "pallium agents install") {
		t.Fatalf("expected adoption hint suppressed, got %q", out.String())
	}
}

func TestVersionOmitsAdoptionHintWhenBlockAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(agentsBlock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var out bytes.Buffer
	if err := runVersion(&out, false); err != nil {
		t.Fatalf("version failed: %v", err)
	}
	if strings.Contains(out.String(), "pallium agents install") {
		t.Fatalf("expected no adoption hint once the block already exists, got %q", out.String())
	}
}
