package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"runtime"
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
	case "sync":
		return runSessionsSync(out, args[1:], jsonOutput)
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
	case "read":
		return runSessionsRead(out, args[1:], jsonOutput)
	case "open":
		return runSessionsOpen(out, args[1:], jsonOutput)
	case "recall":
		return runSessionsRecall(out, args[1:], jsonOutput)
	case "embed":
		return runSessionsEmbed(out, args[1:], jsonOutput)
	case "embedding":
		return runSessionsEmbedding(out, args[1:], jsonOutput)
	case "semantic":
		return runSessionsSemantic(out, args[1:], jsonOutput)
	case "stats":
		return runSessionsStats(out, args[1:], jsonOutput)
	case "doctor":
		return runSessionsDoctor(out, args[1:], jsonOutput)
	case "forget":
		return runSessionsForget(out, args[1:], jsonOutput)
	case "prune":
		return runSessionsPrune(out, args[1:], jsonOutput)
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
	active, idle, inactive := 0, 0, 0
	for _, s := range snapshot.Sessions {
		switch s.Status {
		case "active":
			active++
		case "idle":
			idle++
		default:
			inactive++
		}
	}
	if includeAll {
		fmt.Fprintf(out, "%d active, %d idle, %d inactive agent sessions\n", active, idle, inactive)
	} else {
		fmt.Fprintf(out, "%d active, %d idle agent sessions\n", active, idle)
	}
	fmt.Fprintf(out, "updated %s\n\n", snapshot.GeneratedAt.Local().Format(time.Kitchen))
	for _, warning := range snapshot.Warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
	if len(snapshot.Warnings) > 0 {
		fmt.Fprintln(out)
	}
	if len(snapshot.Sessions) == 0 {
		fmt.Fprintln(out, "No live agent sessions found.")
		return
	}
	for _, s := range snapshot.Sessions {
		pid := "-"
		if s.PID > 0 {
			pid = strconv.Itoa(s.PID)
		}
		fmt.Fprintf(out, "%s %s %s %s %s %s %s\n", firstNonEmpty(s.Provider, "agent"), pid, firstNonEmpty(s.TTY, "-"), s.Status, shortID(s.ThreadID), compactPath(s.EffectiveWorkdir), trimText(s.Title, 90))
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
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
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
	fs.StringVar(&opts.EmbeddingModel, "model", "", "")
	since := fs.String("since", "", "")
	safetyBuffer := fs.String("safety-buffer", "30m", "")
	fs.BoolVar(&opts.Force, "force", false, "")
	fs.BoolVar(&opts.StoreRawEvents, "raw-events", false, "")
	fs.Var((*multiStringFlag)(&include), "include", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"codex-home": {}, "claude-home": {}, "provider": {}, "db": {}, "machine": {}, "include": {}, "model": {}, "since": {}, "safety-buffer": {}}, map[string]struct{}{"force": {}, "raw-events": {}}); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions index argument %q; use --include for extra session paths", fs.Arg(0))
	}
	if strings.TrimSpace(*safetyBuffer) != "" {
		duration, err := time.ParseDuration(*safetyBuffer)
		if err != nil {
			return fmt.Errorf("invalid --safety-buffer duration %q: %w", *safetyBuffer, err)
		}
		opts.SafetyBuffer = duration
	}
	if strings.TrimSpace(*since) != "" {
		duration, err := time.ParseDuration(*since)
		if err != nil {
			return fmt.Errorf("invalid --since duration %q: %w", *since, err)
		}
		opts.Since = duration
	}
	count, err := sessionmemory.Index(context.Background(), opts, include)
	if err != nil {
		return err
	}
	return writeMaybeJSON(out, jsonOutput, map[string]any{"indexed": count}, fmt.Sprintf("Indexed %d agent sessions", count))
}

func runSessionsSync(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	var opts sessionmemory.SyncOptions
	var include []string
	fs := newSessionFlagSet("sessions sync")
	fs.StringVar(&opts.Index.CodexHome, "codex-home", "", "")
	fs.StringVar(&opts.Index.ClaudeHome, "claude-home", "", "")
	fs.StringVar(&opts.Index.Provider, "provider", "", "")
	fs.StringVar(&opts.Index.DBPath, "db", "", "")
	fs.StringVar(&opts.Index.Machine, "machine", "", "")
	fs.StringVar(&opts.Index.EmbeddingModel, "model", "", "")
	since := fs.String("since", "", "")
	safetyBuffer := fs.String("safety-buffer", "30m", "")
	timeout := fs.String("timeout", "", "")
	fs.BoolVar(&opts.Index.Force, "force", false, "")
	fs.BoolVar(&opts.Index.StoreRawEvents, "raw-events", false, "")
	fs.BoolVar(&opts.NoEmbed, "no-embed", false, "")
	fs.IntVar(&opts.EmbedLimit, "embed-limit", 1_000_000, "")
	fs.IntVar(&opts.BatchSize, "batch-size", 64, "")
	fs.Var((*multiStringFlag)(&include), "include", "")
	valueFlags := map[string]struct{}{"codex-home": {}, "claude-home": {}, "provider": {}, "db": {}, "machine": {}, "include": {}, "model": {}, "since": {}, "safety-buffer": {}, "timeout": {}, "embed-limit": {}, "batch-size": {}}
	boolFlags := map[string]struct{}{"force": {}, "raw-events": {}, "no-embed": {}}
	if err := parseSessionFlags(fs, args, valueFlags, boolFlags); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions sync argument %q; use --include for extra session paths", fs.Arg(0))
	}
	opts.Include = include
	if strings.TrimSpace(*safetyBuffer) != "" {
		duration, err := time.ParseDuration(*safetyBuffer)
		if err != nil {
			return fmt.Errorf("invalid --safety-buffer duration %q: %w", *safetyBuffer, err)
		}
		opts.Index.SafetyBuffer = duration
	}
	if strings.TrimSpace(*since) != "" {
		duration, err := parseSessionRetentionAge(*since)
		if err != nil {
			return fmt.Errorf("invalid --since: %w", err)
		}
		opts.Index.Since = duration
	}
	ctx := context.Background()
	if strings.TrimSpace(*timeout) != "" {
		duration, err := time.ParseDuration(*timeout)
		if err != nil || duration <= 0 {
			return fmt.Errorf("invalid --timeout duration %q", *timeout)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	lastPhase := ""
	lastPercent := -1
	progress := func(update sessionmemory.SyncProgress) {
		if jsonOutput {
			return
		}
		if update.Phase != lastPhase {
			fmt.Fprintf(out, "%s: %s\n", update.Phase, update.Message)
			lastPhase = update.Phase
			lastPercent = -1
		}
		if update.Total > 0 {
			percent := update.Completed * 100 / update.Total
			if percent == 100 || percent >= lastPercent+10 {
				fmt.Fprintf(out, "  %d/%d (%d%%)\n", update.Completed, update.Total, percent)
				lastPercent = percent
			}
		}
	}
	report, err := sessionmemory.Sync(ctx, opts, progress)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Fprintf(out, "sync result: %d indexed, %d legacy sessions rebuilt, %d embedded, %d embedding backlog, %s\n", report.Indexed, report.LegacyBackfilled, report.Embedded, report.EmbeddingBacklog, report.Duration)
	if report.FullReindex {
		fmt.Fprintln(out, "coverage: performed a full reindex to upgrade legacy session memory")
	}
	if report.EmbeddingWarning != "" {
		fmt.Fprintf(out, "warning: %s\n", report.EmbeddingWarning)
	}
	return nil
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
	if hasHelpQuery(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions search")
	limit := fs.Int("limit", 10, "")
	hybrid := fs.Bool("hybrid", false, "")
	dbPath := fs.String("db", "", "")
	model := fs.String("model", "", "")
	source := fs.String("source", "", "")
	cwd := fs.String("cwd", "", "")
	repo := fs.String("repo", "", "")
	since := fs.String("since", "", "")
	before := fs.String("before", "", "")
	var files []string
	fs.Var((*multiStringFlag)(&files), "file", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"limit": {}, "db": {}, "model": {}, "source": {}, "cwd": {}, "repo": {}, "since": {}, "before": {}, "file": {}}, map[string]struct{}{"hybrid": {}}); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions search <query>")
	}
	opts := sessionmemory.SessionSearchOptions{
		DBPath: *dbPath,
		Query:  query,
		Limit:  *limit,
		Hybrid: *hybrid,
		Model:  *model,
		Source: *source,
		CWD:    *cwd,
		Files:  files,
	}
	if strings.TrimSpace(*repo) != "" {
		repoRoot, err := gitlog.RepoRoot(*repo)
		if err != nil {
			return err
		}
		opts.RepoRoot = repoRoot
		opts.GitOriginURL, _ = gitlog.OriginURL(repoRoot)
	}
	if strings.TrimSpace(*since) != "" {
		duration, err := parseSessionRetentionAge(*since)
		if err != nil {
			return fmt.Errorf("invalid --since: %w", err)
		}
		opts.After = time.Now().Add(-duration)
	}
	if strings.TrimSpace(*before) != "" {
		parsed, err := parseSessionSearchDate(*before)
		if err != nil {
			return err
		}
		opts.Before = parsed
	}
	results, err := sessionmemory.SearchWithOptions(context.Background(), opts)
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

func parseSessionSearchDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --before date %q; use YYYY-MM-DD or RFC3339", value)
}

func runSessionsRecall(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpQuery(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions recall")
	limit := fs.Int("limit", 5, "")
	dbPath := fs.String("db", "", "")
	model := fs.String("model", "", "")
	source := fs.String("source", "", "")
	cwd := fs.String("cwd", "", "")
	repo := fs.String("repo", "", "")
	since := fs.String("since", "", "")
	before := fs.String("before", "", "")
	lexicalOnly := fs.Bool("lexical-only", false, "")
	var files []string
	fs.Var((*multiStringFlag)(&files), "file", "")
	valueFlags := map[string]struct{}{"limit": {}, "db": {}, "model": {}, "source": {}, "cwd": {}, "repo": {}, "since": {}, "before": {}, "file": {}}
	if err := parseSessionFlags(fs, args, valueFlags, map[string]struct{}{"lexical-only": {}}); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions recall <question> [--repo path]")
	}
	search := sessionmemory.SessionSearchOptions{
		DBPath: *dbPath,
		Query:  query,
		Limit:  *limit,
		Model:  *model,
		Source: *source,
		CWD:    *cwd,
		Files:  files,
	}
	if strings.TrimSpace(*repo) != "" {
		repoRoot, err := gitlog.RepoRoot(*repo)
		if err != nil {
			return err
		}
		search.RepoRoot = repoRoot
		search.GitOriginURL, _ = gitlog.OriginURL(repoRoot)
	}
	if strings.TrimSpace(*since) != "" {
		duration, err := parseSessionRetentionAge(*since)
		if err != nil {
			return fmt.Errorf("invalid --since: %w", err)
		}
		search.After = time.Now().Add(-duration)
	}
	if strings.TrimSpace(*before) != "" {
		parsed, err := parseSessionSearchDate(*before)
		if err != nil {
			return err
		}
		search.Before = parsed
	}
	report, err := sessionmemory.Recall(context.Background(), sessionmemory.RecallOptions{Search: search, LexicalOnly: *lexicalOnly})
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderSessionRecall(out, report)
	return nil
}

func renderSessionRecall(out io.Writer, report sessionmemory.RecallReport) {
	fmt.Fprintf(out, "Recall: %s\n", report.Question)
	fmt.Fprintf(out, "confidence: %s (%.2f)\n", report.Confidence.Level, report.Confidence.Score)
	if report.SessionID == "" {
		fmt.Fprintln(out, "No matching session evidence was found.")
	} else {
		fmt.Fprintf(out, "session: %s %s\n", report.SessionID, report.Title)
	}
	if report.StoppedAt != "" {
		fmt.Fprintf(out, "\nStopped at\n%s\n", report.StoppedAt)
	}
	printRecallItems(out, "Completed", report.Completed)
	printRecallItems(out, "Remaining", report.Remaining)
	printRecallItems(out, "Blockers", report.Blockers)
	if report.NextAction != "" {
		fmt.Fprintf(out, "\nNext action\n%s\n", report.NextAction)
	}
	if len(report.Evidence) > 0 {
		fmt.Fprintln(out, "\nEvidence")
		for _, evidence := range report.Evidence {
			fmt.Fprintf(out, "- %s %s\n", sessionmemory.FormatRecallCitation(evidence.Citation), evidence.Summary)
		}
	}
	if len(report.CoverageWarnings) > 0 {
		fmt.Fprintln(out, "\nCoverage notes")
		for _, warning := range report.CoverageWarnings {
			fmt.Fprintf(out, "- %s\n", warning)
		}
	}
}

func printRecallItems(out io.Writer, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%s\n", heading)
	for _, item := range items {
		fmt.Fprintf(out, "- %s\n", item)
	}
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
	if hasHelpQuery(args) {
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
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, map[string]struct{}{"transcript": {}}); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: pallium sessions show <session-id>")
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("unexpected sessions show argument: %s", fs.Arg(1))
	}
	id := fs.Arg(0)
	s, messages, err := sessionmemory.ShowPath(*dbPath, id, *transcript)
	if err != nil {
		return err
	}
	capsule, capsuleErr := sessionmemory.ReadCapsule(*dbPath, id)
	capsuleWarning := ""
	if capsuleErr != nil {
		capsuleWarning = "Continuity capsule unavailable: " + capsuleErr.Error()
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(map[string]any{"session": s, "capsule": capsule, "capsule_warning": capsuleWarning, "messages": messages})
	}
	printSessionDetail(out, s)
	if capsuleErr == nil {
		renderSessionCapsule(out, capsule)
	} else {
		fmt.Fprintf(out, "warning: %s\n", capsuleWarning)
	}
	if *transcript {
		fmt.Fprintln(out, "\nTranscript")
		for _, m := range messages {
			fmt.Fprintf(out, "\n[%d] %s/%s %s\n%s\n", m.LineNo, m.Role, m.Kind, m.Timestamp, m.Text)
		}
	}
	return nil
}

func renderSessionCapsule(out io.Writer, capsule sessionmemory.SessionCapsule) {
	fmt.Fprintln(out, "Continuity capsule")
	if capsule.Goal != "" {
		fmt.Fprintf(out, "goal: %s\n", capsule.Goal)
	}
	if capsule.StoppedAt != "" {
		fmt.Fprintf(out, "stopped at: %s\n", capsule.StoppedAt)
	}
	printRecallItems(out, "Completed", capsule.Completed)
	printRecallItems(out, "Remaining", capsule.Remaining)
	printRecallItems(out, "Blockers", capsule.Blockers)
	if capsule.NextAction != "" {
		fmt.Fprintf(out, "\nNext action\n%s\n", capsule.NextAction)
	}
	if capsule.Coverage.Warning != "" {
		fmt.Fprintf(out, "coverage: %s\n", capsule.Coverage.Warning)
	}
}

func runSessionsRead(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions read")
	dbPath := fs.String("db", "", "")
	fromLine := fs.Int("from-line", 0, "")
	limit := fs.Int("limit", 50, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "from-line": {}, "limit": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium sessions read <session-id> [--from-line n] [--limit 50]")
	}
	sess, messages, err := sessionmemory.ReadMessages(*dbPath, fs.Arg(0), *fromLine, *limit)
	if err != nil {
		return err
	}
	nextLine := 0
	if len(messages) > 0 {
		nextLine = messages[len(messages)-1].LineNo + 1
	}
	if jsonOutput {
		payload := map[string]any{
			"session_id":     sess.ID,
			"title":          sess.Title,
			"source":         sess.Source,
			"cwd":            sess.CWD,
			"from_line":      *fromLine,
			"next_from_line": nextLine,
			"messages":       messages,
		}
		return json.NewEncoder(out).Encode(payload)
	}
	fmt.Fprintf(out, "%s %s\n", sess.ID, sess.Title)
	for _, message := range messages {
		fmt.Fprintf(out, "\n[%d] %s/%s %s\n%s\n", message.LineNo, message.Role, message.Kind, message.Timestamp, message.Text)
	}
	if nextLine > 0 {
		fmt.Fprintf(out, "\nContinue with: pallium sessions read %s --from-line %d --limit %d\n", sess.ID, nextLine, *limit)
	}
	return nil
}

func runSessionsOpen(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions open")
	dbPath := fs.String("db", "", "")
	launch := fs.Bool("launch", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, map[string]struct{}{"launch": {}}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium sessions open <session-id> [--launch]")
	}
	location, err := sessionmemory.LocateSession(*dbPath, fs.Arg(0))
	if err != nil {
		return err
	}
	if *launch {
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("--launch is currently supported on macOS only")
		}
		if strings.TrimSpace(location.RolloutPath) == "" {
			return fmt.Errorf("session %s has no source transcript path", location.SessionID)
		}
		if err := exec.Command("open", location.RolloutPath).Start(); err != nil {
			return fmt.Errorf("open source transcript: %w", err)
		}
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(location)
	}
	fmt.Fprintf(out, "%s\n", location.RolloutPath)
	if *launch {
		fmt.Fprintln(out, "Opened the source transcript.")
	}
	return nil
}

func runSessionsEmbed(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions embed")
	model := fs.String("model", "", "")
	limit := fs.Int("limit", 1000000, "")
	batch := fs.Int("batch-size", 64, "")
	sessionID := fs.String("session", "", "")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"model": {}, "limit": {}, "batch-size": {}, "session": {}, "db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions embed argument: %s", fs.Arg(0))
	}
	lastPercent := -1
	count, err := sessionmemory.EmbedSessionPath(context.Background(), *dbPath, *sessionID, *model, *limit, *batch, func(completed, total int) {
		if jsonOutput || total == 0 {
			return
		}
		percent := completed * 100 / total
		if percent == 100 || percent >= lastPercent+10 {
			fmt.Fprintf(out, "embedding: %d/%d (%d%%)\n", completed, total, percent)
			lastPercent = percent
		}
	})
	if err != nil {
		return err
	}
	resolvedModel := strings.TrimSpace(*model)
	if resolvedModel == "" {
		resolvedModel = sessionmemory.ActiveEmbeddingModel()
	}
	payload := map[string]any{"embedded": count, "model": resolvedModel}
	if *sessionID != "" {
		payload["session_id"] = *sessionID
	}
	return writeMaybeJSON(out, jsonOutput, payload, fmt.Sprintf("Embedded %d session chunks with %s", count, resolvedModel))
}

func runSessionsEmbedding(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	action := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}
	switch action {
	case "status":
		fs := newSessionFlagSet("sessions embedding status")
		if err := parseSessionFlags(fs, args, nil, nil); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected sessions embedding status argument: %s", fs.Arg(0))
		}
		return renderEmbeddingStatus(out, sessionmemory.ReadEmbeddingStatus(), jsonOutput)
	case "configure":
		fs := newSessionFlagSet("sessions embedding configure")
		provider := fs.String("provider", "", "")
		baseURL := fs.String("base-url", "", "")
		model := fs.String("model", "", "")
		credentialStore := fs.String("credential-store", "", "")
		if err := parseSessionFlags(fs, args, map[string]struct{}{"provider": {}, "base-url": {}, "model": {}, "credential-store": {}}, nil); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected sessions embedding configure argument: %s", fs.Arg(0))
		}
		status, err := sessionmemory.ConfigureEmbedding(sessionmemory.EmbeddingConfig{Provider: *provider, BaseURL: *baseURL, Model: *model, CredentialStore: *credentialStore})
		if err != nil {
			return err
		}
		return renderEmbeddingStatus(out, status, jsonOutput)
	case "check":
		fs := newSessionFlagSet("sessions embedding check")
		timeout := fs.String("timeout", "2m", "")
		if err := parseSessionFlags(fs, args, map[string]struct{}{"timeout": {}}, nil); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected sessions embedding check argument: %s", fs.Arg(0))
		}
		duration, err := time.ParseDuration(*timeout)
		if err != nil || duration <= 0 {
			return fmt.Errorf("invalid --timeout duration %q", *timeout)
		}
		ctx, cancel := context.WithTimeout(context.Background(), duration)
		defer cancel()
		result, err := sessionmemory.ProbeEmbedding(ctx)
		if err != nil {
			return err
		}
		if jsonOutput {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		}
		fmt.Fprintf(out, "healthy: %s/%s returned %d dimensions in %s\n", result.Provider, result.Model, result.Dimension, result.Duration)
		return nil
	default:
		return fmt.Errorf("unknown sessions embedding command %q; use status, configure, or check", action)
	}
}

func renderEmbeddingStatus(out io.Writer, status sessionmemory.EmbeddingStatus, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	fmt.Fprintf(out, "provider: %s (%s)\n", status.Provider, status.ProviderSource)
	fmt.Fprintf(out, "model: %s (%s)\n", status.Model, status.ModelSource)
	fmt.Fprintf(out, "base URL: %s (%s)\n", status.BaseURL, status.BaseURLSource)
	fmt.Fprintf(out, "config: %s\n", status.ConfigPath)
	fmt.Fprintf(out, "API key configured: %t\n", status.APIKeyConfigured)
	if status.APIKeySource != "" {
		fmt.Fprintf(out, "API key source: %s\n", status.APIKeySource)
	}
	if status.ConfigError != "" {
		fmt.Fprintf(out, "configuration error: %s\n", status.ConfigError)
	}
	if status.CredentialError != "" {
		fmt.Fprintf(out, "credential error: %s\n", status.CredentialError)
	}
	return nil
}

func runSessionsSemantic(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpQuery(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions semantic")
	model := fs.String("model", "", "")
	limit := fs.Int("limit", 10, "")
	timeout := fs.String("timeout", "10s", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"model": {}, "limit": {}, "timeout": {}}, nil); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: pallium sessions semantic <query>")
	}
	duration, err := time.ParseDuration(*timeout)
	if err != nil || duration <= 0 {
		return fmt.Errorf("invalid --timeout duration %q", *timeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	results, err := sessionmemory.Semantic(ctx, query, *model, *limit, true)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(results)
	}
	for _, r := range results {
		fmt.Fprintf(out, "%.4f %s %s: %s\n  cwd: %s updated: %s\n  %s\n\n", r.Score, r.SessionID, r.Kind, r.Title, r.CWD, r.UpdatedAt, r.Snippet)
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
	fmt.Fprintf(out, "sessions: %d\nevents: %d\nmessages: %d\nchunks: %d\nembeddings: %d\ncapsules: %d\n", stats.Sessions, stats.Events, stats.Messages, stats.Chunks, stats.Embeddings, stats.Capsules)
	for _, m := range stats.Models {
		fmt.Fprintf(out, "- %s/%s dim=%d count=%d\n", m.Provider, m.Model, m.Dim, m.Count)
	}
	return nil
}

func runSessionsDoctor(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	var opts sessionmemory.SessionDoctorOptions
	fs := newSessionFlagSet("sessions doctor")
	fs.StringVar(&opts.DBPath, "db", "", "")
	fs.BoolVar(&opts.Repair, "repair", false, "")
	fs.BoolVar(&opts.PruneRawEvents, "prune-raw-events", false, "")
	fs.BoolVar(&opts.Vacuum, "vacuum", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, map[string]struct{}{"repair": {}, "prune-raw-events": {}, "vacuum": {}}); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected sessions doctor argument: %s", fs.Arg(0))
	}
	if (opts.PruneRawEvents || opts.Vacuum) && !opts.Repair {
		return fmt.Errorf("--prune-raw-events and --vacuum require --repair")
	}
	report, err := sessionmemory.DoctorSessions(opts)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderSessionDoctor(out, report)
	return nil
}

func renderSessionDoctor(out io.Writer, report sessionmemory.SessionDoctorReport) {
	fmt.Fprintln(out, "Session memory doctor")
	fmt.Fprintf(out, "database: %s\n", report.DBPath)
	if !report.DBExists {
		for _, issue := range report.Issues {
			fmt.Fprintf(out, "issue: %s\n", issue)
		}
		return
	}
	fmt.Fprintf(out, "storage: %.1f MiB, directory %s, file %s\n", float64(report.DBSizeBytes)/(1024*1024), report.DirectoryMode, report.FileMode)
	fmt.Fprintf(out, "content: %d sessions, %d messages, %d chunks, %d embeddings, %d capsules\n", report.Stats.Sessions, report.Stats.Messages, report.Stats.Chunks, report.Stats.Embeddings, report.Stats.Capsules)
	fmt.Fprintf(out, "integrity: %d orphan embeddings, %d stale embeddings, %d missing embeddings\n", report.OrphanEmbeddings, report.StaleEmbeddings, report.EmbeddingBacklog)
	fmt.Fprintf(out, "coverage: %d noisy titles, %d oversized first messages, %d legacy skipped large sessions, %d missing capsules\n", report.NoisyTitles, report.OversizedFirstMessages, report.SkippedLargeSessions, report.MissingCapsules)
	if report.Repair.OrphanEmbeddingsRemoved > 0 || report.Repair.StaleEmbeddingsRemoved > 0 || report.Repair.RawEventsRemoved > 0 || report.Repair.Vacuumed {
		fmt.Fprintf(out, "repaired: %d orphan embeddings, %d stale embeddings, %d raw events", report.Repair.OrphanEmbeddingsRemoved, report.Repair.StaleEmbeddingsRemoved, report.Repair.RawEventsRemoved)
		if report.Repair.Vacuumed {
			fmt.Fprint(out, ", database vacuumed")
		}
		fmt.Fprintln(out)
	}
	if report.Healthy {
		fmt.Fprintln(out, "status: healthy")
		return
	}
	for _, issue := range report.Issues {
		fmt.Fprintf(out, "issue: %s\n", issue)
	}
}

func runSessionsForget(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions forget")
	dbPath := fs.String("db", "", "")
	confirm := fs.Bool("confirm", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, map[string]struct{}{"confirm": {}}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium sessions forget <session-id> [--confirm]")
	}
	result, err := sessionmemory.ForgetSession(*dbPath, fs.Arg(0), *confirm)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if result.Deleted {
		fmt.Fprintf(out, "Forgot session %s: %s\n", result.SessionID, trimText(result.Title, 100))
		return nil
	}
	fmt.Fprintf(out, "Forget preview for %s: %s\n", result.SessionID, trimText(result.Title, 100))
	fmt.Fprintf(out, "would delete: %d messages, %d chunks, %d embeddings\n", result.Messages, result.Chunks, result.Embeddings)
	fmt.Fprintln(out, "Preview only. Re-run with --confirm to delete this session from Pallium memory.")
	return nil
}

func runSessionsPrune(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions prune")
	dbPath := fs.String("db", "", "")
	olderThan := fs.String("older-than", "", "")
	confirm := fs.Bool("confirm", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "older-than": {}}, map[string]struct{}{"confirm": {}}); err != nil {
		return err
	}
	if fs.NArg() > 0 || strings.TrimSpace(*olderThan) == "" {
		return fmt.Errorf("usage: pallium sessions prune --older-than 180d [--confirm]")
	}
	age, err := parseSessionRetentionAge(*olderThan)
	if err != nil {
		return err
	}
	result, err := sessionmemory.PruneSessions(*dbPath, age, *confirm)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if result.Confirmed {
		fmt.Fprintf(out, "Deleted %d of %d sessions older than %s.\n", result.Deleted, result.Matched, result.Cutoff)
		return nil
	}
	fmt.Fprintf(out, "Retention preview: %d sessions older than %s.\n", result.Matched, result.Cutoff)
	fmt.Fprintln(out, "Preview only. Re-run with --confirm to delete these sessions from Pallium memory.")
	return nil
}

func parseSessionRetentionAge(value string) (time.Duration, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	multiplier := time.Duration(1)
	number := value
	if strings.HasSuffix(value, "d") {
		multiplier = 24 * time.Hour
		number = strings.TrimSuffix(value, "d")
	} else if strings.HasSuffix(value, "w") {
		multiplier = 7 * 24 * time.Hour
		number = strings.TrimSuffix(value, "w")
	} else {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return 0, fmt.Errorf("invalid --older-than duration %q; examples: 180d, 12w, 720h", value)
		}
		return duration, nil
	}
	count, err := strconv.Atoi(number)
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("invalid --older-than duration %q; examples: 180d, 12w, 720h", value)
	}
	return time.Duration(count) * multiplier, nil
}

func printSessionsHelp(out io.Writer) {
	fmt.Fprintln(out, `pallium sessions

Usage:
  pallium sessions live [--all] [--details] [--json]
  pallium sessions watch [--all] [--details]
  pallium sessions index [--provider all|codex|claude] [--codex-home ~/.codex] [--claude-home ~/.claude] [--include path] [--machine name] [--model text-embedding-3-small] [--safety-buffer 30m] [--since 24h] [--force] [--raw-events] [--json]
  pallium sessions sync [--provider all|codex|claude] [--include path] [--model name] [--force] [--no-embed] [--json]
  pallium sessions list [--limit 20] [--json]
  pallium sessions search <query> [--limit 10] [--hybrid] [--repo path] [--cwd path] [--source codex|claude] [--file path] [--since 30d] [--before YYYY-MM-DD] [--model name] [--json]
  pallium sessions recall <question> [--repo path] [--source codex|claude] [--file path] [--since 30d] [--lexical-only] [--json]
  pallium sessions related [repo-path] [--file path] [--limit 10] [--json]
  pallium sessions grep <query> [--limit 20] [--json]
  pallium sessions show <session-id> [--db path] [--transcript] [--json]
  pallium sessions read <session-id> [--db path] [--from-line n] [--limit 50] [--json]
  pallium sessions open <session-id> [--db path] [--launch] [--json]
  pallium sessions embedding <status|configure|check> [--provider name] [--model name] [--base-url URL] [--credential-store keychain] [--json]
  pallium sessions embed [--session id] [--db path] [--model text-embedding-3-small] [--limit n] [--batch-size n] [--json]
  pallium sessions semantic <query> [--model text-embedding-3-small] [--limit 10] [--timeout 10s] [--json]
  pallium sessions stats [--json]
  pallium sessions doctor [--db path] [--repair] [--prune-raw-events] [--vacuum] [--json]
  pallium sessions forget <session-id> [--db path] [--confirm] [--json]
  pallium sessions prune --older-than 180d [--db path] [--confirm] [--json]`)
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
	score := fmt.Sprintf("score=%d", r.Score)
	if r.HybridScore != 0 {
		score = fmt.Sprintf("hybrid=%.5f", r.HybridScore)
	} else if r.SemanticScore != 0 {
		score = fmt.Sprintf("semantic=%.4f", r.SemanticScore)
	} else if r.LexicalScore != 0 {
		score = fmt.Sprintf("lexical=%.4f", r.LexicalScore)
	}
	fmt.Fprintf(out, "%s %s  %s  %s\n", score, firstNonEmpty(r.UpdatedAt, r.CreatedAt), r.ID, r.Title)
	fmt.Fprintf(out, "  cwd: %s\n", r.CWD)
	if r.Snippet != "" {
		fmt.Fprintf(out, "  match: %s\n", r.Snippet)
	}
	if r.Citation.SessionID != "" {
		line := ""
		if r.Citation.LineNo > 0 {
			line = fmt.Sprintf(":%d", r.Citation.LineNo)
		}
		fmt.Fprintf(out, "  citation: %s:%s%s\n", firstNonEmpty(r.Citation.Source, "session"), r.Citation.SessionID, line)
	}
	if len(r.Signals) > 0 {
		fmt.Fprintf(out, "  signals: %s\n", strings.Join(r.Signals, ", "))
	}
	if len(r.FilesTouched) > 0 {
		fmt.Fprintf(out, "  files: %s\n", strings.Join(limitStrings(r.FilesTouched, 8), ", "))
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", warning)
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

// hasHelpQuery is for commands whose positional arguments form a free-text
// query: the literal word "help" only counts when it is the sole argument, so
// queries like `sessions search help with auth` still run a search.
func hasHelpQuery(args []string) bool {
	if len(args) == 1 && args[0] == "help" {
		return true
	}
	return slices.Contains(args, "-h") || slices.Contains(args, "--help")
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
