package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"time"

	"github.com/tszaks/pallium/internal/codexsessions"
	"github.com/tszaks/pallium/internal/sessionmemory"
)

func runSessions(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printSessionsHelp(out)
		return nil
	}
	switch args[0] {
	case "live":
		return runSessionsLive(out, args[1:], jsonOutput, false)
	case "watch":
		return runSessionsLive(out, args[1:], jsonOutput, true)
	case "index":
		return runSessionsIndex(out, args[1:], jsonOutput)
	case "list":
		return runSessionsList(out, args[1:], jsonOutput)
	case "search":
		return runSessionsSearch(out, args[1:], jsonOutput)
	case "grep":
		return runSessionsGrep(out, args[1:], jsonOutput)
	case "show":
		return runSessionsShow(out, args[1:], jsonOutput)
	case "embed":
		return runSessionsEmbed(out, args[1:], jsonOutput)
	case "semantic":
		return runSessionsSemantic(out, args[1:], jsonOutput)
	case "stats":
		return runSessionsStats(out, jsonOutput)
	default:
		printSessionsHelp(out)
		return fmt.Errorf("unknown sessions command: %s", args[0])
	}
}

func runSessionsLive(out io.Writer, args []string, jsonOutput bool, watch bool) error {
	includeAll := hasArg(args, "--all")
	details := hasArg(args, "--details")
	if watch && jsonOutput {
		return fmt.Errorf("sessions watch cannot be used with --json")
	}
	render := func() error {
		snapshot, err := codexsessions.CollectSessions(context.Background(), codexsessions.SessionCollectOptions{IncludeAll: includeAll, IncludeDetails: details})
		if err != nil {
			return err
		}
		if jsonOutput {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(snapshot)
		}
		renderLiveSessions(out, snapshot, includeAll, details)
		return nil
	}
	if !watch {
		return render()
	}
	for {
		if _, ok := out.(interface{ Fd() uintptr }); ok {
			fmt.Fprint(out, "\033[H\033[2J")
		}
		if err := render(); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
	}
}

func renderLiveSessions(out io.Writer, snapshot *codexsessions.SessionSnapshot, includeAll, details bool) {
	active, inactive := 0, 0
	for _, s := range snapshot.Sessions {
		if s.Status == "active" {
			active++
		} else {
			inactive++
		}
	}
	if includeAll {
		fmt.Fprintf(out, "%d active, %d inactive agent sessions\n", active, inactive)
	} else {
		fmt.Fprintf(out, "%d active agent sessions\n", active)
	}
	fmt.Fprintf(out, "updated %s\n\n", snapshot.GeneratedAt.Local().Format(time.Kitchen))
	if len(snapshot.Sessions) == 0 {
		fmt.Fprintln(out, "No Codex sessions found.")
		return
	}
	for _, s := range snapshot.Sessions {
		pid := "-"
		if s.PID > 0 {
			pid = strconv.Itoa(s.PID)
		}
		fmt.Fprintf(out, "%s %s %s %s %s %s\n", pid, firstNonEmpty(s.TTY, "-"), s.Status, shortID(s.ThreadID), compactPath(s.EffectiveWorkdir), trimText(s.Title, 90))
		if details && s.RecentAction != "" {
			fmt.Fprintf(out, "  recent: %s\n", s.RecentAction)
		}
	}
	fmt.Fprintln(out)
}

func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func trimText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func compactPath(s string) string {
	if s == "" {
		return "-"
	}
	parts := strings.Split(strings.TrimRight(s, "/"), "/")
	if len(parts) <= 2 {
		return s
	}
	return "…/" + strings.Join(parts[len(parts)-2:], "/")
}

func runSessionsIndex(out io.Writer, args []string, jsonOutput bool) error {
	opts := sessionmemory.Options{}
	var include []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--codex-home":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing value for --codex-home")
			}
			opts.CodexHome = args[i]
		case "--claude-home":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing value for --claude-home")
			}
			opts.ClaudeHome = args[i]
		case "--provider":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing value for --provider")
			}
			opts.Provider = args[i]
		case "--db":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing value for --db")
			}
			opts.DBPath = args[i]
		case "--machine":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing value for --machine")
			}
			opts.Machine = args[i]
		case "--include":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing value for --include")
			}
			include = append(include, args[i])
		default:
			include = append(include, args[i])
		}
	}
	count, err := sessionmemory.Index(context.Background(), opts, include)
	if err != nil {
		return err
	}
	return writeMaybeJSON(out, jsonOutput, map[string]any{"indexed": count}, fmt.Sprintf("Indexed %d agent sessions", count))
}

func runSessionsList(out io.Writer, args []string, jsonOutput bool) error {
	limit := intArg(args, "--limit", 20)
	sessions, err := sessionmemory.List(limit)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(sessions)
	}
	for _, s := range sessions {
		printSessionBrief(out, s)
	}
	return nil
}

func runSessionsSearch(out io.Writer, args []string, jsonOutput bool) error {
	query := strings.TrimSpace(strings.Join(nonFlagArgs(args), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions search <query>")
	}
	limit := intArg(args, "--limit", 10)
	results, err := sessionmemory.Search(query, limit)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(results)
	}
	for _, r := range results {
		printSessionBrief(out, r.Session)
	}
	return nil
}

func runSessionsGrep(out io.Writer, args []string, jsonOutput bool) error {
	query := strings.TrimSpace(strings.Join(nonFlagArgs(args), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions grep <query>")
	}
	limit := intArg(args, "--limit", 20)
	results, err := sessionmemory.Grep(query, limit)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(results)
	}
	for _, r := range results {
		fmt.Fprintf(out, "%s:%v %s/%s — %s\n  %s\n\n", r["session_id"], r["line_no"], r["role"], r["kind"], r["title"], r["snippet"])
	}
	return nil
}

func runSessionsShow(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pallium sessions show <session-id>")
	}
	transcript := hasArg(args, "--transcript")
	id := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			id = a
			break
		}
	}
	s, messages, err := sessionmemory.Show(id, transcript)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(map[string]any{"session": s, "messages": messages})
	}
	printSessionDetail(out, s)
	if transcript {
		fmt.Fprintln(out, "\nTranscript")
		for _, m := range messages {
			fmt.Fprintf(out, "\n[%d] %s/%s %s\n%s\n", m.LineNo, m.Role, m.Kind, m.Timestamp, m.Text)
		}
	}
	return nil
}

func runSessionsEmbed(out io.Writer, args []string, jsonOutput bool) error {
	model := stringArg(args, "--model", sessionmemory.DefaultEmbeddingModel)
	limit := intArg(args, "--limit", 1000000)
	batch := intArg(args, "--batch-size", 64)
	sessionID := stringArg(args, "--session", "")
	count, err := sessionmemory.EmbedSession(context.Background(), sessionID, model, limit, batch)
	if err != nil {
		return err
	}
	payload := map[string]any{"embedded": count, "model": model}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}
	return writeMaybeJSON(out, jsonOutput, payload, fmt.Sprintf("Embedded %d session chunks with %s", count, model))
}

func runSessionsSemantic(out io.Writer, args []string, jsonOutput bool) error {
	query := strings.TrimSpace(strings.Join(nonFlagArgs(args), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions semantic <query>")
	}
	model := stringArg(args, "--model", sessionmemory.DefaultEmbeddingModel)
	limit := intArg(args, "--limit", 10)
	results, err := sessionmemory.Semantic(context.Background(), query, model, limit, true)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(results)
	}
	for _, r := range results {
		fmt.Fprintf(out, "%.4f %s %s — %s\n  cwd: %s updated: %s\n  %s\n\n", r.Score, r.SessionID, r.Kind, r.Title, r.CWD, r.UpdatedAt, r.Snippet)
	}
	return nil
}

func runSessionsStats(out io.Writer, jsonOutput bool) error {
	stats, err := sessionmemory.StatsRead()
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(stats)
	}
	fmt.Fprintf(out, "sessions: %d\nevents: %d\nmessages: %d\nchunks: %d\nembeddings: %d\n", stats.Sessions, stats.Events, stats.Messages, stats.Chunks, stats.Embeddings)
	for _, m := range stats.Models {
		fmt.Fprintf(out, "- %s/%s dim=%d count=%d\n", m.Provider, m.Model, m.Dim, m.Count)
	}
	return nil
}

func printSessionsHelp(out io.Writer) {
	fmt.Fprintln(out, `pallium sessions

Usage:
  pallium sessions live [--all] [--details] [--json]
  pallium sessions watch [--all] [--details]
  pallium sessions index [--provider all|codex|claude] [--codex-home ~/.codex] [--claude-home ~/.claude] [--include path] [--machine name] [--json]
  pallium sessions list [--limit 20] [--json]
  pallium sessions search <query> [--limit 10] [--json]
  pallium sessions grep <query> [--limit 20] [--json]
  pallium sessions show <session-id> [--transcript] [--json]
  pallium sessions embed [--session id] [--model text-embedding-3-small] [--limit n] [--batch-size n] [--json]
  pallium sessions semantic <query> [--model text-embedding-3-small] [--limit 10] [--json]
  pallium sessions stats [--json]`)
}

func printSessionBrief(out io.Writer, s sessionmemory.Session) {
	fmt.Fprintf(out, "%s  %s  %s\n", firstNonEmpty(s.UpdatedAt, s.CreatedAt), s.ID, s.Title)
	fmt.Fprintf(out, "  cwd: %s\n", s.CWD)
	if len(s.FilesTouched) > 0 {
		fmt.Fprintf(out, "  files: %s\n", strings.Join(limitStrings(s.FilesTouched, 8), ", "))
	}
	fmt.Fprintln(out)
}

func printSessionDetail(out io.Writer, s sessionmemory.Session) {
	printSessionBrief(out, s)
	if s.FirstUserMessage != "" {
		fmt.Fprintf(out, "First ask:\n%s\n\n", s.FirstUserMessage)
	}
	if s.LastAgentMessage != "" {
		fmt.Fprintf(out, "Last agent message:\n%s\n\n", s.LastAgentMessage)
	}
	if len(s.Commands) > 0 {
		fmt.Fprintln(out, "Commands:")
		for _, c := range limitStrings(s.Commands, 40) {
			fmt.Fprintf(out, "- %s\n", c)
		}
	}
}

func writeMaybeJSON(out io.Writer, jsonOutput bool, payload any, text string) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(payload)
	}
	_, err := fmt.Fprintln(out, text)
	return err
}
func intArg(args []string, name string, fallback int) int {
	s := stringArg(args, name, "")
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}
func stringArg(args []string, name, fallback string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return fallback
}
func hasArg(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}
func nonFlagArgs(args []string) []string {
	out := []string{}
	skip := false
	for i, a := range args {
		if skip {
			skip = false
			continue
		}
		if strings.HasPrefix(a, "--") {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				skip = true
			}
			continue
		}
		out = append(out, a)
	}
	return out
}
func limitStrings(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	out := append([]string{}, in[:n]...)
	out = append(out, "...")
	return out
}
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
