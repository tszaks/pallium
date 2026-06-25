package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/tszaks/pallium/internal/console"
	"github.com/tszaks/pallium/internal/output"
	"golang.org/x/term"
)

type ownedReadResult struct {
	Session console.OwnedSession `json:"session"`
	Output  string               `json:"output"`
}

func runConsoleRun(out io.Writer, args []string, jsonOutput bool) error {
	flagArgs, commandArgs, err := splitConsoleRunArgs(args)
	if err != nil {
		return err
	}
	fs := newSessionFlagSet("console run")
	id := fs.String("id", "", "")
	dbPath := fs.String("db", "", "")
	cwd := fs.String("cwd", "", "")
	logPath := fs.String("log", "", "")
	background := fs.Bool("background", false, "")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(commandArgs) == 0 {
		return fmt.Errorf("usage: pallium console run [flags] -- command [args...]")
	}
	if *id == "" {
		*id = ownedSessionID()
	}
	if *cwd == "" {
		*cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	if *logPath == "" {
		*logPath, err = defaultOwnedLogPath(*id)
		if err != nil {
			return err
		}
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	session, err := store.CreateOwnedSession(console.OwnedSession{
		ID:      *id,
		Command: commandArgs,
		CWD:     *cwd,
		LogPath: *logPath,
		Status:  "starting",
	})
	if err != nil {
		return err
	}
	if *background {
		return startOwnedBackground(out, jsonOutput, *dbPath, session)
	}
	exitCode, err := runOwnedProcess(store, session, out, true)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("owned session %s exited with code %d", session.ID, exitCode)
	}
	return nil
}

func runConsoleRunner(out io.Writer, args []string) error {
	flagArgs, commandArgs, err := splitConsoleRunArgs(args)
	if err != nil {
		return err
	}
	fs := newSessionFlagSet("console _runner")
	id := fs.String("id", "", "")
	dbPath := fs.String("db", "", "")
	cwd := fs.String("cwd", "", "")
	logPath := fs.String("log", "", "")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if *id == "" || *cwd == "" || *logPath == "" || len(commandArgs) == 0 {
		return fmt.Errorf("invalid owned runner invocation")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	session := console.OwnedSession{ID: *id, Command: commandArgs, CWD: *cwd, LogPath: *logPath}
	exitCode, err := runOwnedProcess(store, session, io.Discard, false)
	if err != nil {
		fmt.Fprintln(out, err)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return err
}

func runConsoleRead(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console read")
	dbPath := fs.String("db", "", "")
	tail := fs.Int("tail", 200, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "tail": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console read <owned-session-id>")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	session, err := store.OwnedSession(fs.Arg(0))
	if err != nil {
		return err
	}
	text, err := readOwnedLog(session.LogPath, *tail)
	if err != nil {
		return err
	}
	return output.Write(out, ownedReadResult{Session: session, Output: text}, jsonOutput, func() string {
		return text
	})
}

func runConsoleInterrupt(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console interrupt")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console interrupt <owned-session-id>")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	session, err := store.OwnedSession(fs.Arg(0))
	if err != nil {
		return err
	}
	if session.ChildPID <= 0 {
		return fmt.Errorf("owned session %s has no child pid", session.ID)
	}
	if err := interruptProcess(session.ChildPID); err != nil {
		return err
	}
	event, _ := store.RequestAuthority(console.AuthorityEvent{
		Provider:  "pallium",
		SessionID: session.ID,
		Machine:   "local",
		Actor:     "human",
		Level:     "execute",
		Action:    "interrupt",
		TargetRef: fmt.Sprintf("pid:%d", session.ChildPID),
		Status:    "dispatched",
	})
	return output.Write(out, map[string]any{"status": "interrupted", "session": session.ID, "event": event.ID}, jsonOutput, func() string {
		return fmt.Sprintf("Interrupt sent to %s pid=%d", session.ID, session.ChildPID)
	})
}

func runConsoleOwned(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	switch args[0] {
	case "list":
		return runConsoleOwnedList(out, args[1:], jsonOutput)
	case "show":
		return runConsoleOwnedShow(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown console owned subcommand: %s", args[0])
	}
}

func runConsoleOwnedList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console owned list")
	dbPath := fs.String("db", "", "")
	limit := fs.Int("limit", 50, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "limit": {}}, nil); err != nil {
		return err
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	sessions, err := store.ListOwnedSessions(*limit)
	if err != nil {
		return err
	}
	return output.Write(out, sessions, jsonOutput, func() string {
		return renderOwnedSessions(sessions)
	})
}

func runConsoleOwnedShow(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console owned show")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console owned show <owned-session-id>")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	session, err := store.OwnedSession(fs.Arg(0))
	if err != nil {
		return err
	}
	return output.Write(out, session, jsonOutput, func() string {
		return renderOwnedSession(session)
	})
}

func runOwnedProcess(store *console.Store, session console.OwnedSession, out io.Writer, attach bool) (int, error) {
	if err := os.MkdirAll(filepath.Dir(session.LogPath), 0o755); err != nil {
		_ = store.FailOwnedSession(session.ID, 1)
		return 1, err
	}
	logFile, err := os.OpenFile(session.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = store.FailOwnedSession(session.ID, 1)
		return 1, err
	}
	defer logFile.Close()
	cmd := exec.Command(session.Command[0], session.Command[1:]...)
	cmd.Dir = session.CWD
	cmd.Env = os.Environ()
	ptmx, err := pty.Start(cmd)
	if err != nil {
		_ = store.FailOwnedSession(session.ID, 1)
		return 1, err
	}
	defer ptmx.Close()
	_ = store.UpdateOwnedSessionStarted(session.ID, os.Getpid(), cmd.Process.Pid)
	writer := io.Writer(logFile)
	if attach {
		writer = io.MultiWriter(logFile, out)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(writer, ptmx)
		close(done)
	}()
	var restore func()
	if attach {
		restore = attachInput(ptmx)
	}
	err = cmd.Wait()
	if restore != nil {
		restore()
	}
	_ = ptmx.Close()
	<-done
	exitCode := exitCodeFromError(err)
	_ = store.FinishOwnedSession(session.ID, exitCode)
	return exitCode, err
}

func attachInput(ptmx *os.File) func() {
	stdin := int(os.Stdin.Fd())
	if term.IsTerminal(stdin) {
		oldState, err := term.MakeRaw(stdin)
		if err == nil {
			go func() { _, _ = io.Copy(ptmx, os.Stdin) }()
			return func() { _ = term.Restore(stdin, oldState) }
		}
	}
	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()
	return nil
}

func startOwnedBackground(out io.Writer, jsonOutput bool, dbPath string, session console.OwnedSession) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"console", "_runner", "--id", session.ID, "--cwd", session.CWD, "--log", session.LogPath}
	if dbPath != "" {
		args = append(args, "--db", dbPath)
	}
	args = append(args, "--")
	args = append(args, session.Command...)
	cmd := exec.Command(exe, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if cmd.Process != nil {
		if store, err := console.Open(dbPath); err == nil {
			_ = store.UpdateOwnedSessionStarted(session.ID, cmd.Process.Pid, 0)
			_ = store.Close()
		}
	}
	_ = cmd.Process.Release()
	return output.Write(out, session, jsonOutput, func() string {
		return fmt.Sprintf("Owned session started: %s\nlog: %s", session.ID, session.LogPath)
	})
}

func splitConsoleRunArgs(args []string) ([]string, []string, error) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:], nil
		}
	}
	return nil, nil, fmt.Errorf("missing -- before command")
}

func defaultOwnedLogPath(id string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pallium", "owned-sessions", id+".log"), nil
}

func ownedSessionID() string {
	return fmt.Sprintf("owned-%d", time.Now().UTC().UnixNano())
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func interruptProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(os.Interrupt)
}

func readOwnedLog(path string, tail int) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if tail <= 0 {
		return string(raw), nil
	}
	lines := bytes.Split(raw, []byte("\n"))
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return string(bytes.Join(lines, []byte("\n"))), nil
}

func renderOwnedSessions(sessions []console.OwnedSession) string {
	if len(sessions) == 0 {
		return "No owned sessions found."
	}
	lines := []string{"Owned sessions:"}
	for _, session := range sessions {
		lines = append(lines, "- "+renderOwnedSession(session))
	}
	return strings.Join(lines, "\n")
}

func renderOwnedSession(session console.OwnedSession) string {
	return fmt.Sprintf("%s %s pid=%d exit=%d cwd=%s cmd=%s", session.ID, session.Status, session.ChildPID, session.ExitCode, session.CWD, strings.Join(session.Command, " "))
}
