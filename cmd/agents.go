package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tszaks/pallium/internal/agentdocs"
	"github.com/tszaks/pallium/internal/output"
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
