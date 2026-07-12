package cmd

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tszaks/pallium/internal/agentdocs"
	"github.com/tszaks/pallium/internal/output"
	"golang.org/x/term"
)

const (
	agentsBlockBegin = "<!-- pallium:agents:begin -->"
	agentsBlockEnd   = "<!-- pallium:agents:end -->"
)

const agentsBlock = agentsBlockBegin + "\n" +
	"## Pallium\n" +
	"\n" +
	"This machine has Pallium: a local control plane for coding agents (workflows, repo memory, verification, session state — kept outside your context window).\n" +
	"\n" +
	"Reach for it when a task is multi-step, needs tests objectively green, wants parallel workers, must survive the session, or needs isolated reviewable edits. Skip it for one-shot edits.\n" +
	"\n" +
	"- Scope first: `pallium workflow preflight \"<task>\"` (files to inspect, risk, test commands)\n" +
	"- Orchestrate: write an async-JS workflow, then `pallium workflow validate f.js && pallium workflow run --script f.js \"<task>\" --json`\n" +
	"- Primitives: `agent()` (schema-validated workers), `pipeline()` (streaming stages), `parallel()` (barrier), `verify.untilGreen()`, `gate()` — discover all with `pallium workflow tools list --json`\n" +
	"- Resume and inspect: `pallium workflow resume|inspect|report <run-id>`\n" +
	"\n" +
	"Full agent guide: `pallium agents guide`\n" +
	agentsBlockEnd

type AgentsInstallReport struct {
	Dir   string              `json:"dir"`
	Files []AgentsInstallFile `json:"files"`
}

type AgentsInstallFile struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

func runAgents(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printAgentsHelp(out)
		return nil
	}
	switch args[0] {
	case "guide":
		return output.Write(out, map[string]string{"guide": agentdocs.Guide}, jsonOutput, func() string {
			return agentdocs.Guide
		})
	case "block":
		return output.Write(out, map[string]string{"block": agentsBlock}, jsonOutput, func() string {
			return agentsBlock
		})
	case "install":
		return runAgentsInstall(out, args[1:], jsonOutput)
	default:
		printAgentsHelp(out)
		return fmt.Errorf("unknown agents subcommand: %s", args[0])
	}
}

func runAgentsInstall(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("agents install")
	dir := fs.String("dir", ".", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"dir": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: pallium agents install [--dir path] [--json]")
	}

	report := AgentsInstallReport{Dir: *dir}
	targets := []string{"AGENTS.md", "CLAUDE.md"}
	anyExists := false
	for _, name := range targets {
		path := filepath.Join(*dir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		anyExists = true
		action, err := installAgentsBlock(path)
		if err != nil {
			return err
		}
		report.Files = append(report.Files, AgentsInstallFile{Path: name, Action: action})
	}
	if !anyExists {
		path := filepath.Join(*dir, "AGENTS.md")
		if err := os.WriteFile(path, []byte(agentsBlock+"\n"), 0o644); err != nil {
			return err
		}
		report.Files = append(report.Files, AgentsInstallFile{Path: "AGENTS.md", Action: "created"})
	}

	return output.Write(out, report, jsonOutput, func() string {
		lines := make([]string, 0, len(report.Files))
		for _, file := range report.Files {
			lines = append(lines, fmt.Sprintf("%s: %s", file.Path, file.Action))
		}
		return strings.Join(lines, "\n")
	})
}

// hasAgentsBlock reports whether dir's AGENTS.md or CLAUDE.md already carries
// the adoption block — the shared check both maybeOfferAdoptionInstall and
// the `version` hint use to decide whether there's anything to offer.
func hasAgentsBlock(dir string) bool {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), agentsBlockBegin) {
			return true
		}
	}
	return false
}

// adoptionHintSuppressed is the one suppression knob both the `start` y/n
// offer and the `version` one-line hint honor, so a CI runner or a user who
// has already made a deliberate choice sets it once instead of each surface
// inventing its own flag.
func adoptionHintSuppressed() bool {
	return strings.TrimSpace(os.Getenv("PALLIUM_SKIP_ADOPTION_HINT")) != ""
}

// shouldPromptAdoptionOffer is maybeOfferAdoptionInstall's decision logic,
// split out as a pure function so offer/decline/suppress behavior is unit
// testable without needing a real pty to fake a terminal.
func shouldPromptAdoptionOffer(jsonOutput, suppressed, hasBlock, isTerminal bool) bool {
	return !jsonOutput && !suppressed && !hasBlock && isTerminal
}

// isAffirmativeResponse reports whether a y/n prompt's raw stdin line counts
// as "yes". Anything other than an explicit y/yes (case-insensitive) — an
// empty line, "n", garbage — is a decline, matching the prompt's [y/N]
// default-no framing and the "never silently write" rule: an ambiguous
// answer must not be read as consent.
func isAffirmativeResponse(line string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
}

// maybeOfferAdoptionInstall runs at the top of `pallium start`. Fix for the
// exact gap found 2026-07-12: `pallium agents install` existed since PR #14
// but sat unused on a real machine for 4 days because nothing ever prompted
// for it — a mechanism nobody runs is the same failure as a mechanism that
// doesn't exist. Never writes silently: skipped entirely (no prompt, no
// write) whenever a human isn't actually available to answer — --json mode
// (a machine caller), non-interactive stdin (script/CI), or the explicit
// suppression env var — and even when asked, only installs on an explicit
// "y" (see isAffirmativeResponse).
func maybeOfferAdoptionInstall(cwd string, jsonOutput bool) {
	if !shouldPromptAdoptionOffer(jsonOutput, adoptionHintSuppressed(), hasAgentsBlock(cwd), term.IsTerminal(int(os.Stdin.Fd()))) {
		return
	}
	fmt.Fprint(os.Stderr, "Pallium found no adoption block in this repo's AGENTS.md/CLAUDE.md. Add one so future agents here reach for `pallium start`/workflows automatically? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if !isAffirmativeResponse(line) {
		return
	}
	var installOut bytes.Buffer
	if err := runAgentsInstall(&installOut, []string{"--dir", cwd}, false); err != nil {
		fmt.Fprintf(os.Stderr, "pallium: adoption install failed: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "Installed:\n"+strings.TrimSpace(installOut.String()))
}

func installAgentsBlock(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(raw)
	updated, action := upsertAgentsBlock(content)
	if updated == content {
		return "unchanged", nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return action, nil
}

func upsertAgentsBlock(content string) (string, string) {
	begin := strings.Index(content, agentsBlockBegin)
	end := strings.Index(content, agentsBlockEnd)
	if begin >= 0 && end > begin {
		return content[:begin] + agentsBlock + content[end+len(agentsBlockEnd):], "updated"
	}
	if strings.TrimSpace(content) == "" {
		return agentsBlock + "\n", "appended"
	}
	return strings.TrimRight(content, "\n") + "\n\n" + agentsBlock + "\n", "appended"
}

func printAgentsHelp(out io.Writer) {
	fmt.Fprintln(out, `pallium agents

Usage:
  pallium agents guide
  pallium agents block
  pallium agents install [--dir path] [--json]

guide    print the full agent guide (PALLIUM.md)
block    print the compact instruction block for AGENTS.md / CLAUDE.md
install  add or refresh the block in AGENTS.md and CLAUDE.md in the target repo`)
}
