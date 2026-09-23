package cmd

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/tszaks/pallium/internal/gitlog"
	"github.com/tszaks/pallium/internal/knowledge"
	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/workflow"
)

func runKnowledgeMaintain(out io.Writer, args []string, jsonOutput bool) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
		args = args[1:]
	}
	if action == "run" || action == "tick" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return knowledge.Maintain(ctx, action == "tick")
	}
	if action == "install" || action == "uninstall" {
		return knowledgeService(out, action == "install")
	}
	m, err := knowledge.OpenMaintenance()
	if err != nil {
		return err
	}
	defer m.Close()
	if action == "status" {
		s, err := m.Status()
		if err != nil {
			return err
		}
		return output.Write(out, s, jsonOutput, func() string {
			return fmt.Sprintf("%d/40 model calls used in 24 hours; %d queued, %d failed. Cost: unknown.\n", s.Used, s.Queued, s.Failed)
		})
	}
	root, err := gitlog.RepoRoot(optionalRepoArg(args, 0))
	if err != nil {
		return err
	}
	switch action {
	case "enable":
		runner := workflow.Runner{Run: workflow.Run{CWD: root}}
		opts, err := runner.ResolveTextOptions()
		if err != nil {
			return err
		}
		err = m.Register(knowledge.Registration{Root: root, Provider: workflow.ResolveProvider("", opts.Provider), Model: opts.Model, Reasoning: opts.ReasoningEffort})
		if err != nil {
			return err
		}
	case "pause", "disable":
		if err := m.Enable(root, false); err != nil {
			return err
		}
	case "resume":
		if err := m.Enable(root, true); err != nil {
			return err
		}
	case "retry":
		if err := m.Retry(root); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown maintenance action %q", action)
	}
	return output.Write(out, map[string]string{"action": action, "root": root}, jsonOutput, func() string { return fmt.Sprintf("Knowledge maintenance %s: %s\n", action, root) })
}

func knowledgeService(out io.Writer, install bool) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service installation currently supports macOS; run `pallium knowledge maintain run` under your process supervisor")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	label := "com.pallium.knowledge"
	dir := filepath.Join(home, "Library", "LaunchAgents")
	path := filepath.Join(dir, label+".plist")
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if !install {
		cmd := exec.Command("launchctl", "bootout", domain, path)
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("stop service: %w: %s", err, b)
		}
		return os.Remove(path)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	escape := func(s string) string { var b strings.Builder; xml.EscapeText(&b, []byte(s)); return b.String() }
	logdir := filepath.Join(home, ".pallium")
	if err := os.MkdirAll(logdir, 0700); err != nil {
		return err
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>%s</string><string>knowledge</string><string>maintain</string><string>run</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>30</integer><key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, label, escape(exe), escape(os.Getenv("PATH")), escape(filepath.Join(logdir, "knowledge.log")), escape(filepath.Join(logdir, "knowledge-error.log")))
	// Reinstallation replaces this service only; other Pallium services are untouched.
	if _, err := os.Stat(path); err == nil {
		_ = exec.Command("launchctl", "bootout", domain, path).Run()
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return err
	}
	if b, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("start service: %w: %s", err, b)
	}
	_, err = fmt.Fprintln(out, "Installed and started "+label)
	return err
}
