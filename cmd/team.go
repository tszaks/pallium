package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/workflow"
)

func runTeam(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pallium team <start|spawn|tasks|send|inbox|nudge|status|run|approve|stop|attach> [--json]")
	}
	switch args[0] {
	case "start":
		return runTeamStart(out, args[1:], jsonOutput)
	case "spawn":
		return runTeamSpawn(out, args[1:], jsonOutput)
	case "tasks":
		return runTeamTasks(out, args[1:], jsonOutput)
	case "send":
		return runTeamSend(out, args[1:], jsonOutput)
	case "inbox":
		return runTeamInbox(out, args[1:], jsonOutput)
	case "nudge":
		return runTeamNudge(out, args[1:], jsonOutput)
	case "status":
		return runTeamStatus(out, args[1:], jsonOutput)
	case "run":
		return runTeamRun(out, args[1:], jsonOutput)
	case "approve":
		return runTeamApprove(out, args[1:], jsonOutput)
	case "stop":
		return runTeamStop(out, args[1:], jsonOutput)
	case "attach":
		return runTeamAttach(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown team subcommand: %s", args[0])
	}
}

func runTeamStart(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team start")
	dbPath := fs.String("db", "", "")
	cwd := fs.String("cwd", "", "")
	budget := fs.String("budget-usd", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "cwd": {}, "budget-usd": {}}, nil); err != nil {
		return err
	}
	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		return fmt.Errorf("usage: pallium team start <goal> [--cwd path] [--budget-usd N] [--json]")
	}
	root := strings.TrimSpace(*cwd)
	if root == "" {
		var err error
		root, err = defaultTeamCWD()
		if err != nil {
			return err
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	var limit float64
	if strings.TrimSpace(*budget) != "" {
		limit, err = strconv.ParseFloat(*budget, 64)
		if err != nil {
			return fmt.Errorf("invalid --budget-usd %q: %w", *budget, err)
		}
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	team, err := store.CreateTeam(goal, absRoot, limit)
	if err != nil {
		return err
	}
	return output.Write(out, team, jsonOutput, func() string {
		msg := "Team started: " + team.ID + "\n  goal: " + team.Goal + "\n  cwd:  " + team.CWD
		if team.BudgetUSDLimit > 0 {
			msg += fmt.Sprintf("\n  budget: $%.4f (only self-enforces for cost-tracked providers — claude,"+
				" or a wrapper that reports PALLIUM_WORKFLOW_USAGE_FILE; codex reports no usage/cost at all"+
				" and won't count toward this ceiling — see `team status` after spawning members)", team.BudgetUSDLimit)
		}
		return msg
	})
}

func defaultTeamCWD() (string, error) {
	wd, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	return wd, nil
}

// runTeamSpawn creates a teammate identity. If --provider is omitted it
// resolves through the SAME chain a worker agent does (ResolveProvider):
// explicit flag > PALLIUM_WORKFLOW_PROVIDER env > detected steering agent >
// "codex" fallback — a team spawned from inside Claude Code gets a claude
// teammate with no configuration, exactly like workflow workers do. For a
// claude teammate, the session id is MINTED here (a fresh UUID) and
// persisted immediately: claude lets the caller choose its own session id
// (--session-id on turn 1), unlike codex, which assigns its own — see
// dispatchTeamTurn in internal/workflow/team_runtime.go.
func runTeamSpawn(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team spawn")
	dbPath := fs.String("db", "", "")
	provider := fs.String("provider", "", "")
	model := fs.String("model", "", "")
	role := fs.String("role", "", "")
	mode := fs.String("mode", "read-only", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "provider": {}, "model": {}, "role": {}, "mode": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 {
		return fmt.Errorf("usage: pallium team spawn <team-id> <name> [--provider p] [--model m] [--role r] [--mode read-only|edit] [--json]")
	}
	teamID, name := positionals[0], positionals[1]
	resolvedProvider := workflow.ResolveProvider("", *provider)
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	member, err := store.SpawnMember(teamID, name, resolvedProvider, *model, *role, *mode)
	if err != nil {
		return err
	}
	if resolvedProvider == "claude" {
		if err := store.PersistMemberSession(teamID, name, uuid.NewString()); err != nil {
			return err
		}
		member, err = store.GetMember(teamID, name)
		if err != nil {
			return err
		}
	}
	return output.Write(out, member, jsonOutput, func() string {
		return fmt.Sprintf("Spawned %s on team %s (provider=%s mode=%s)", member.Name, teamID, member.Provider, member.Mode)
	})
}

func runTeamTasks(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pallium team tasks <add|list|claim|complete> ...")
	}
	switch args[0] {
	case "add":
		return runTeamTasksAdd(out, args[1:], jsonOutput)
	case "list":
		return runTeamTasksList(out, args[1:], jsonOutput)
	case "claim":
		return runTeamTasksClaim(out, args[1:], jsonOutput)
	case "complete":
		return runTeamTasksComplete(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown team tasks subcommand: %s", args[0])
	}
}

func runTeamTasksAdd(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team tasks add")
	dbPath := fs.String("db", "", "")
	description := fs.String("description", "", "")
	dependsOn := fs.String("depends-on", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "description": {}, "depends-on": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 {
		return fmt.Errorf("usage: pallium team tasks add <team-id> <title> [--description d] [--depends-on id1,id2] [--json]")
	}
	teamID := positionals[0]
	title := strings.Join(positionals[1:], " ")
	var deps []string
	if strings.TrimSpace(*dependsOn) != "" {
		for _, d := range strings.Split(*dependsOn, ",") {
			if d = strings.TrimSpace(d); d != "" {
				deps = append(deps, d)
			}
		}
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	task, err := store.CreateTeamTask(teamID, title, *description, deps)
	if err != nil {
		return err
	}
	return output.Write(out, task, jsonOutput, func() string {
		return "Task added: " + task.ID + " — " + task.Title
	})
}

func runTeamTasksList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team tasks list")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	teamID, err := requireArg(fs.Args(), "team-id")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	tasks, err := store.ListTeamTasks(teamID)
	if err != nil {
		return err
	}
	return output.Write(out, tasks, jsonOutput, func() string {
		var b strings.Builder
		for _, t := range tasks {
			fmt.Fprintf(&b, "%s [%s] owner=%s — %s\n", t.ID, t.Status, firstNonEmptyCmd(t.Owner, "-"), t.Title)
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

func runTeamTasksClaim(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team tasks claim")
	dbPath := fs.String("db", "", "")
	as := fs.String("as", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "as": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 || strings.TrimSpace(*as) == "" {
		return fmt.Errorf("usage: pallium team tasks claim <team-id> <task-id> --as <member-name> [--json]")
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	task, err := store.ClaimTask(positionals[0], positionals[1], *as)
	if err != nil {
		return err
	}
	return output.Write(out, task, jsonOutput, func() string {
		return fmt.Sprintf("Task %s claimed by %s", task.ID, task.Owner)
	})
}

func runTeamTasksComplete(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team tasks complete")
	dbPath := fs.String("db", "", "")
	as := fs.String("as", "", "")
	result := fs.String("result", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "as": {}, "result": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 || strings.TrimSpace(*as) == "" {
		return fmt.Errorf("usage: pallium team tasks complete <team-id> <task-id> --as <member-name> [--result text] [--json]")
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	task, err := store.CompleteTask(positionals[0], positionals[1], *as, *result)
	if err != nil {
		return err
	}
	return output.Write(out, task, jsonOutput, func() string {
		return "Task " + task.ID + " completed"
	})
}

// runTeamSend is a CLI-level trust boundary noted but not closed in the M1
// review: --from is an unauthenticated free-text string, not tied to any
// session or credential — anyone who can run this CLI (or write to the
// underlying DB directly) can claim to be "lead" or any teammate's name, and
// the recipient's next turn will see it wrapped as legitimate agent-origin
// mail (see teamAgentOrigin) with no way to tell the difference. The trust
// boundary this codebase actually enforces is narrower than it may look: "a
// message can never carry a human approval it didn't actually get" (the
// wrapper text's own promise), not "the sender field is verified". Same
// class of caveat as local SQLite file access — whoever can reach this CLI
// already has that level of trust in the team.
func runTeamSend(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team send")
	dbPath := fs.String("db", "", "")
	from := fs.String("from", "lead", "")
	to := fs.String("to", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "from": {}, "to": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 1 || strings.TrimSpace(*to) == "" {
		return fmt.Errorf("usage: pallium team send <team-id> --to <name> [--from lead] \"<message>\" [--json]")
	}
	teamID := positionals[0]
	body := strings.Join(positionals[1:], " ")
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("message body must not be empty")
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	msg, err := store.SendTeamMessage(teamID, *from, *to, body)
	if err != nil {
		return err
	}
	return output.Write(out, msg, jsonOutput, func() string {
		return fmt.Sprintf("Sent %s -> %s: %s", msg.From, msg.To, msg.Body)
	})
}

func runTeamInbox(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team inbox")
	dbPath := fs.String("db", "", "")
	forName := fs.String("for", "", "")
	all := fs.Bool("all", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "for": {}}, map[string]struct{}{"all": {}}); err != nil {
		return err
	}
	teamID, err := requireArg(fs.Args(), "team-id")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	var msgs []workflow.TeamMessage
	if *all {
		msgs, err = store.ListTeamMessages(teamID)
	} else {
		if strings.TrimSpace(*forName) == "" {
			return fmt.Errorf("usage: pallium team inbox <team-id> --for <name> (or --all) [--json]")
		}
		msgs, err = store.UndeliveredMessages(teamID, *forName)
	}
	if err != nil {
		return err
	}
	return output.Write(out, msgs, jsonOutput, func() string {
		var b strings.Builder
		for _, m := range msgs {
			delivered := "undelivered"
			if m.DeliveredAt != "" {
				delivered = "delivered"
			}
			fmt.Fprintf(&b, "[%s] %s -> %s (%s): %s\n", m.CreatedAt, m.From, m.To, delivered, m.Body)
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

func runTeamNudge(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team nudge")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 {
		return fmt.Errorf("usage: pallium team nudge <team-id> <member-name> [--json]")
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.NudgeMember(positionals[0], positionals[1]); err != nil {
		return err
	}
	return output.Write(out, map[string]string{"team_id": positionals[0], "member": positionals[1]}, jsonOutput, func() string {
		return "Nudged " + positionals[1]
	})
}

func runTeamStatus(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team status")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	teamID, err := requireArg(fs.Args(), "team-id")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	team, err := store.GetTeam(teamID)
	if err != nil {
		return err
	}
	members, err := store.ListMembers(teamID)
	if err != nil {
		return err
	}
	tasks, err := store.ListTeamTasks(teamID)
	if err != nil {
		return err
	}
	payload := map[string]any{"team": team, "members": members, "tasks": tasks}
	return output.Write(out, payload, jsonOutput, func() string {
		var b strings.Builder
		fmt.Fprintf(&b, "Team %s [%s] — %s (spend $%.4f", team.ID, team.Status, team.Goal, team.SpendUSD)
		if team.BudgetUSDLimit > 0 {
			fmt.Fprintf(&b, " / $%.4f", team.BudgetUSDLimit)
		}
		b.WriteString(")\n")
		for _, m := range members {
			// Honest reporting: a member with turn_started_at still set here
			// (Status would already say "interrupted" after any
			// reconciling `team run`/`attach`, but a status of "active" with
			// a stale turn in flight before that happens is shown plainly,
			// never presented as if it were making progress).
			fmt.Fprintf(&b, "  %-16s provider=%-8s mode=%-9s status=%-11s turns=%-3d spend=$%.4f",
				m.Name, m.Provider, m.Mode, m.Status, m.TurnCount, m.SpendUSD)
			if m.TurnStartedAt != "" {
				fmt.Fprintf(&b, " (turn in flight since %s)", m.TurnStartedAt)
			}
			if m.LastTurnError != "" {
				fmt.Fprintf(&b, " last_error=%q", m.LastTurnError)
			}
			b.WriteString("\n")
		}
		pending, inProgress, completed := 0, 0, 0
		for _, t := range tasks {
			switch t.Status {
			case "pending":
				pending++
			case "in_progress":
				inProgress++
			case "completed":
				completed++
			}
		}
		fmt.Fprintf(&b, "  tasks: %d pending, %d in progress, %d completed\n", pending, inProgress, completed)
		if untracked := untrackedCostProviders(members); len(untracked) > 0 {
			fmt.Fprintf(&b, "  cost not tracked for: %s (reports no usage/cost at all; the spend total above is incomplete for these members)\n", strings.Join(untracked, ", "))
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

// untrackedCostProviders reports which distinct providers among members
// never contribute real spend, so `team status`/`team start --budget-usd`
// can say plainly which providers a budget ceiling silently cannot see —
// rather than a $0.0000 line looking indistinguishable from "genuinely free
// so far". Only codex is unconditionally untracked (its exec has no usage
// envelope at all); claude round-trips a real cost, and a configured
// wrapper is tracked whenever it reports PALLIUM_WORKFLOW_USAGE_FILE (see
// runConfiguredProviderTeamTurn) — Pallium can't tell from here whether a
// given wrapper script actually does that, so wrappers are not flagged.
func untrackedCostProviders(members []workflow.TeamMember) []string {
	seen := map[string]bool{}
	var untracked []string
	for _, m := range members {
		if m.Provider == "codex" && !seen[m.Provider] {
			seen[m.Provider] = true
			untracked = append(untracked, m.Provider)
		}
	}
	return untracked
}

func runTeamRun(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team run")
	dbPath := fs.String("db", "", "")
	codexBinary := fs.String("codex", "codex", "")
	agentTimeout := fs.Int("agent-timeout", 600, "")
	staleAfterMinutes := fs.Int("stale-after-minutes", 15, "")
	maxConcurrent := fs.Int("max-concurrent", 16, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "codex": {}, "agent-timeout": {}, "stale-after-minutes": {}, "max-concurrent": {}}, nil); err != nil {
		return err
	}
	teamID, err := requireArg(fs.Args(), "team-id")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	runner := &workflow.Runner{CodexBinary: *codexBinary}
	opts := workflow.TeamTurnOptions{
		StaleTurnAfter: time.Duration(*staleAfterMinutes) * time.Minute,
		AgentTimeout:   time.Duration(*agentTimeout) * time.Second,
		MaxConcurrent:  *maxConcurrent,
	}
	summary, err := runner.RunTeam(context.Background(), store, teamID, opts)
	if err != nil {
		return err
	}
	return output.Write(out, summary, jsonOutput, func() string {
		return fmt.Sprintf("Team %s: %d round(s), %d turn(s) taken, reconciled %d interrupted member(s), stopped=%v parked=%v",
			teamID, summary.Rounds, summary.TurnsTaken, len(summary.Interrupted), summary.StoppedAtEnd, summary.ParkedAtEnd)
	})
}

func runTeamApprove(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team approve")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 {
		return fmt.Errorf("usage: pallium team approve <team-id> <member-name> [--json]")
	}
	// M1 scope: approve is the primitive mode-flip only (read-only -> edit).
	// The full plan-review artifact + reject-with-feedback loop from the
	// settled design is M2 (workflow-script primitives, quality gates).
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetMemberMode(positionals[0], positionals[1], "edit"); err != nil {
		return err
	}
	member, err := store.GetMember(positionals[0], positionals[1])
	if err != nil {
		return err
	}
	return output.Write(out, member, jsonOutput, func() string {
		return member.Name + " approved for edit mode"
	})
}

func runTeamStop(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team stop")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	teamID, err := requireArg(fs.Args(), "team-id")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetTeamStatus(teamID, "stopped"); err != nil {
		return err
	}
	return output.Write(out, map[string]string{"team_id": teamID, "status": "stopped"}, jsonOutput, func() string {
		return "Team " + teamID + " stopped"
	})
}

// runTeamAttach validates a team exists and reconciles any interrupted
// members immediately (rather than waiting for the next `team run`), so
// `pallium team attach <id>` gives an honest status right away for whoever
// — any agent, or a human — is picking this team back up.
func runTeamAttach(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team attach")
	dbPath := fs.String("db", "", "")
	staleAfterMinutes := fs.Int("stale-after-minutes", 15, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "stale-after-minutes": {}}, nil); err != nil {
		return err
	}
	teamID, err := requireArg(fs.Args(), "team-id")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	team, err := store.GetTeam(teamID)
	if err != nil {
		return err
	}
	staleAfter := time.Now().Add(-time.Duration(*staleAfterMinutes) * time.Minute).UTC().Format(time.RFC3339Nano)
	interrupted, err := store.ReconcileInterruptedMembers(teamID, staleAfter)
	if err != nil {
		return err
	}
	members, err := store.ListMembers(teamID)
	if err != nil {
		return err
	}
	payload := map[string]any{"team": team, "members": members, "reconciled_interrupted": interrupted}
	return output.Write(out, payload, jsonOutput, func() string {
		return fmt.Sprintf("Attached to team %s [%s], %d member(s), reconciled %d interrupted", team.ID, team.Status, len(members), len(interrupted))
	})
}

func firstNonEmptyCmd(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
