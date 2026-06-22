package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"time"

	"github.com/tszaks/pallium/internal/codexsessions"
	"github.com/tszaks/pallium/internal/gitlog"
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
	case "related":
		return runSessionsRelated(out, args[1:], jsonOutput)
	case "grep":
		return runSessionsGrep(out, args[1:], jsonOutput)
	case "show":
		return runSessionsShow(out, args[1:], jsonOutput)
	case "embed":
		return runSessionsEmbed(out, args[1:], jsonOutput)
	case "semantic":
		return runSessionsSemantic(out, args[1:], jsonOutput)
	case "stats":
		return runSessionsStats(out, args[1:], jsonOutput)
	default:
		printSessionsHelp(out)
		return fmt.Errorf("unknown sessions command: %s", args[0])
	}
}

func runSessionsLive(out io.Writer, args []string, jsonOutput bool, watch bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions live")
	includeAll := fs.Bool("all", false, "")
	details := fs.Bool("details", false, "")
	if err := parseSessionFlags(fs, args, nil, map[string]struct{}{"all": {}, "details": {}}); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions live argument: %s", fs.Arg(0))
	}
	if watch && jsonOutput {
		return fmt.Errorf("sessions watch cannot be used with --json")
	}
	render := func() error {
		snapshot, err := codexsessions.CollectSessions(context.Background(), codexsessions.SessionCollectOptions{IncludeAll: *includeAll, IncludeDetails: *details})
		if err != nil {
			return err
		}
		if jsonOutput {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(snapshot)
		}
		renderLiveSessions(out, snapshot, *includeAll, *details)
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
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	opts := sessionmemory.Options{}
	var include []string
	fs := newSessionFlagSet("sessions index")
	fs.StringVar(&opts.CodexHome, "codex-home", "", "")
	fs.StringVar(&opts.ClaudeHome, "claude-home", "", "")
	fs.StringVar(&opts.Provider, "provider", "", "")
	fs.StringVar(&opts.DBPath, "db", "", "")
	fs.StringVar(&opts.Machine, "machine", "", "")
	fs.Var((*multiStringFlag)(&include), "include", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"codex-home": {}, "claude-home": {}, "provider": {}, "db": {}, "machine": {}, "include": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions index argument %q; use --include for extra session paths", fs.Arg(0))
	}
	count, err := sessionmemory.Index(context.Background(), opts, include)
	if err != nil {
		return err
	}
	return writeMaybeJSON(out, jsonOutput, map[string]any{"indexed": count}, fmt.Sprintf("Indexed %d agent sessions", count))
}

func runSessionsList(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions list")
	limit := fs.Int("limit", 20, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"limit": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions list argument: %s", fs.Arg(0))
	}
	sessions, err := sessionmemory.List(*limit)
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
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions search")
	limit := fs.Int("limit", 10, "")
	hybrid := fs.Bool("hybrid", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"limit": {}}, map[string]struct{}{"hybrid": {}}); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions search <query>")
	}
	var results []sessionmemory.SearchResult
	var err error
	if *hybrid {
		results, err = sessionmemory.SearchHybrid(query, *limit)
	} else {
		results, err = sessionmemory.Search(query, *limit)
	}
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

func runSessionsRelated(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions related")
	limit := fs.Int("limit", 10, "")
	var files []string
	fs.Var((*multiStringFlag)(&files), "file", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"limit": {}, "file": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("unexpected sessions related argument: %s", fs.Arg(1))
	}
	repoPath := "."
	if fs.NArg() == 1 {
		repoPath = fs.Arg(0)
	}
	repoRoot, err := gitlog.RepoRoot(repoPath)
	if err != nil {
		return err
	}
	origin, _ := gitlog.OriginURL(repoRoot)
	results, err := sessionmemory.Related(sessionmemory.RelatedOptions{
		RepoRoot:     repoRoot,
		GitOriginURL: origin,
		Files:        files,
		Limit:        *limit,
	})
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(results)
	}
	for _, r := range results {
		printSessionSearchResult(out, r)
	}
	return nil
}

func runSessionsGrep(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions grep")
	limit := fs.Int("limit", 20, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"limit": {}}, nil); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions grep <query>")
	}
	results, err := sessionmemory.Grep(query, *limit)
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
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions show")
	transcript := fs.Bool("transcript", false, "")
	if err := parseSessionFlags(fs, args, nil, map[string]struct{}{"transcript": {}}); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: pallium sessions show <session-id>")
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("unexpected sessions show argument: %s", fs.Arg(1))
	}
	id := fs.Arg(0)
	s, messages, err := sessionmemory.Show(id, *transcript)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(map[string]any{"session": s, "messages": messages})
	}
	printSessionDetail(out, s)
	if *transcript {
		fmt.Fprintln(out, "\nTranscript")
		for _, m := range messages {
			fmt.Fprintf(out, "\n[%d] %s/%s %s\n%s\n", m.LineNo, m.Role, m.Kind, m.Timestamp, m.Text)
		}
	}
	return nil
}

func runSessionsEmbed(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions embed")
	model := fs.String("model", sessionmemory.DefaultEmbeddingModel, "")
	limit := fs.Int("limit", 1000000, "")
	batch := fs.Int("batch-size", 64, "")
	sessionID := fs.String("session", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"model": {}, "limit": {}, "batch-size": {}, "session": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions embed argument: %s", fs.Arg(0))
	}
	count, err := sessionmemory.EmbedSession(context.Background(), *sessionID, *model, *limit, *batch)
	if err != nil {
		return err
	}
	payload := map[string]any{"embedded": count, "model": *model}
	if *sessionID != "" {
		payload["session_id"] = *sessionID
	}
	return writeMaybeJSON(out, jsonOutput, payload, fmt.Sprintf("Embedded %d session chunks with %s", count, *model))
}

func runSessionsSemantic(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions semantic")
	model := fs.String("model", sessionmemory.DefaultEmbeddingModel, "")
	limit := fs.Int("limit", 10, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"model": {}, "limit": {}}, nil); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions semantic <query>")
	}
	results, err := sessionmemory.Semantic(context.Background(), query, *model, *limit, true)
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

func runSessionsStats(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions stats")
	if err := parseSessionFlags(fs, args, nil, nil); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions stats argument: %s", fs.Arg(0))
	}
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
  pallium sessions search <query> [--limit 10] [--hybrid] [--json]
  pallium sessions related [repo-path] [--file path] [--limit 10] [--json]
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

func printSessionSearchResult(out io.Writer, r sessionmemory.SearchResult) {
	fmt.Fprintf(out, "score=%d %s  %s  %s\n", r.Score, firstNonEmpty(r.UpdatedAt, r.CreatedAt), r.ID, r.Title)
	fmt.Fprintf(out, "  cwd: %s\n", r.CWD)
	if len(r.Signals) > 0 {
		fmt.Fprintf(out, "  signals: %s\n", strings.Join(r.Signals, ", "))
	}
	if len(r.FilesTouched) > 0 {
		fmt.Fprintf(out, "  files: %s\n", strings.Join(limitStrings(r.FilesTouched, 8), ", "))
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
func newSessionFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseSessionFlags(fs *flag.FlagSet, args []string, valueFlags, boolFlags map[string]struct{}) error {
	reordered, err := reorderSessionFlags(args, valueFlags, boolFlags)
	if err != nil {
		return err
	}
	return fs.Parse(reordered)
}

func reorderSessionFlags(args []string, valueFlags, boolFlags map[string]struct{}) ([]string, error) {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if idx := strings.Index(name, "="); idx >= 0 {
			name = name[:idx]
		}
		if _, ok := boolFlags[name]; ok {
			flags = append(flags, arg)
			continue
		}
		if _, ok := valueFlags[name]; ok {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		flags = append(flags, arg)
	}
	return append(flags, positionals...), nil
}

func hasHelpArg(args []string) bool {
	return slices.Contains(args, "help") || slices.Contains(args, "-h") || slices.Contains(args, "--help")
}

type multiStringFlag []string

func (m *multiStringFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("empty value")
	}
	*m = append(*m, value)
	return nil
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
