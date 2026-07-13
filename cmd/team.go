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
		return fmt.Errorf("usage: pallium team <start|spawn|join|member|tasks|send|inbox|nudge|status|run|approve|reject|gate|stop|attach|template> [--json]")
	}
	switch args[0] {
	case "start":
		return runTeamStart(out, args[1:], jsonOutput)
	case "template":
		return runTeamTemplate(out, args[1:], jsonOutput)
	case "join":
		return runTeamJoin(out, args[1:], jsonOutput)
	case "spawn":
		return runTeamSpawn(out, args[1:], jsonOutput)
	case "member":
		return runTeamMember(out, args[1:], jsonOutput)
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
	case "reject":
		return runTeamReject(out, args[1:], jsonOutput)
	case "gate":
		return runTeamGateCmd(out, args[1:], jsonOutput)
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
	template := fs.String("template", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "cwd": {}, "budget-usd": {}, "template": {}}, nil); err != nil {
		return err
	}
	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		return fmt.Errorf("usage: pallium team start <goal> [--cwd path] [--budget-usd N] [--template name] [--json]")
	}
	var tmpl workflow.TeamTemplateInfo
	if strings.TrimSpace(*template) != "" {
		var ok bool
		tmpl, ok = workflow.TeamTemplate(*template)
		if !ok {
			return workflow.UnknownTeamTemplateError(*template)
		}
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
	var spawned []workflow.TeamMember
	for _, m := range tmpl.Members {
		member, err := spawnTeamMember(store, team.ID, m.Name, "", "", m.Role, m.Mode, false)
		if err != nil {
			return fmt.Errorf("template %q: spawning %q: %w", tmpl.Name, m.Name, err)
		}
		spawned = append(spawned, member)
	}
	result := struct {
		workflow.Team
		TemplateMembers []workflow.TeamMember `json:"template_members,omitempty"`
	}{Team: team, TemplateMembers: spawned}
	return output.Write(out, result, jsonOutput, func() string {
		msg := "Team started: " + team.ID + "\n  goal: " + team.Goal + "\n  cwd:  " + team.CWD
		if team.BudgetUSDLimit > 0 {
			msg += fmt.Sprintf("\n  budget: $%.4f (only self-enforces for cost-tracked providers — claude,"+
				" or a wrapper that reports PALLIUM_WORKFLOW_USAGE_FILE; codex reports no usage/cost at all"+
				" and won't count toward this ceiling — see `team status` after spawning members)", team.BudgetUSDLimit)
		}
		for _, m := range spawned {
			msg += fmt.Sprintf("\n  spawned %s (provider=%s mode=%s): %s", m.Name, m.Provider, m.Mode, m.Role)
		}
		return msg
	})
}

// externalMemberStaleAfter matches the same 15-minute default convention
// `team member restart`'s --stale-after-minutes already uses elsewhere in
// this file — one honest, consistent staleness window across the teams
// surface rather than a second magic number.
const externalMemberStaleAfter = 15 * time.Minute

// parseTeamTimestamp returns the zero time on a parse failure rather than
// erroring — used only for staleness display, where a zero time reads as
// "long past any threshold" and correctly renders as stale rather than
// panicking or hiding the (already unusual) malformed value.
func parseTeamTimestamp(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// runTeamJoin is M3's external-session attach: an already-running agent
// session (a Claude Code tab, a Codex session, a human at a terminal)
// registers itself as a self-driving teammate with no provider dispatch —
// see JoinExternalMember's doc comment for the full reasoning. Re-running
// `team join` for a name that already joined is how that same session
// re-announces liveness with no other CLI activity in between.
func runTeamJoin(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team join")
	dbPath := fs.String("db", "", "")
	as := fs.String("as", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "as": {}}, nil); err != nil {
		return err
	}
	teamID, err := requireArg(fs.Args(), "team-id")
	if err != nil {
		return err
	}
	if strings.TrimSpace(*as) == "" {
		return fmt.Errorf("usage: pallium team join <team-id> --as <name> [--db path] [--json]")
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	member, err := store.JoinExternalMember(teamID, *as)
	if err != nil {
		return err
	}
	return output.Write(out, member, jsonOutput, func() string {
		return fmt.Sprintf("Joined team %s as %q (external, drives itself via inbox/send/tasks claim/complete)", teamID, member.Name)
	})
}

func runTeamTemplate(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		return runTeamTemplateList(out, nil, jsonOutput)
	}
	switch args[0] {
	case "list", "ls":
		return runTeamTemplateList(out, args[1:], jsonOutput)
	case "show":
		return runTeamTemplateShow(out, args[1:], jsonOutput)
	default:
		if tmpl, ok := workflow.TeamTemplate(args[0]); ok {
			return output.Write(out, tmpl, jsonOutput, func() string {
				return renderTeamTemplate(tmpl)
			})
		}
		return workflow.UnknownTeamTemplateError(args[0])
	}
}

func runTeamTemplateList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team template list")
	if err := parseSessionFlags(fs, args, nil, nil); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: pallium team template list")
	}
	templates := workflow.TeamTemplates()
	return output.Write(out, templates, jsonOutput, func() string {
		lines := []string{"Team templates:"}
		for _, tmpl := range templates {
			lines = append(lines, fmt.Sprintf("- %s (%d members): %s", tmpl.Name, len(tmpl.Members), tmpl.Description))
		}
		return strings.Join(lines, "\n")
	})
}

func runTeamTemplateShow(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team template show")
	if err := parseSessionFlags(fs, args, nil, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium team template show <name>")
	}
	tmpl, ok := workflow.TeamTemplate(fs.Arg(0))
	if !ok {
		return workflow.UnknownTeamTemplateError(fs.Arg(0))
	}
	return output.Write(out, tmpl, jsonOutput, func() string {
		return renderTeamTemplate(tmpl)
	})
}

func renderTeamTemplate(tmpl workflow.TeamTemplateInfo) string {
	lines := []string{
		"Team template " + tmpl.Name,
		tmpl.Description,
		"When to use: " + tmpl.WhenToUse,
		"Members:",
	}
	for _, m := range tmpl.Members {
		lines = append(lines, fmt.Sprintf("  - %s [%s]: %s", m.Name, m.Mode, m.Role))
	}
	if tmpl.Example != "" {
		lines = append(lines, "Example: "+tmpl.Example)
	}
	return strings.Join(lines, "\n")
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
	planRequired := fs.Bool("plan-required", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "provider": {}, "model": {}, "role": {}, "mode": {}}, map[string]struct{}{"plan-required": {}}); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 {
		return fmt.Errorf("usage: pallium team spawn <team-id> <name> [--provider p] [--model m] [--role r] [--mode read-only|edit] [--plan-required] [--json]")
	}
	teamID, name := positionals[0], positionals[1]
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	member, err := spawnTeamMember(store, teamID, name, *provider, *model, *role, *mode, *planRequired)
	if err != nil {
		return err
	}
	return output.Write(out, member, jsonOutput, func() string {
		return fmt.Sprintf("Spawned %s on team %s (provider=%s mode=%s)", member.Name, teamID, member.Provider, member.Mode)
	})
}

// spawnTeamMember is the shared front door behind both `team spawn` and
// `team start --template`: resolves the provider through the exact same
// chain, mints and persists a claude session immediately (matching what
// runTeamSpawn always did before this was factored out), and returns the
// final member row. Kept as one function so a template-spawned member is
// indistinguishable from one spawned by hand — no second, drifting copy of
// the claude-session-minting step.
func spawnTeamMember(store *workflow.Store, teamID, name, provider, model, role, mode string, planRequired bool) (workflow.TeamMember, error) {
	resolvedProvider := workflow.ResolveProvider("", provider)
	var member workflow.TeamMember
	var err error
	if planRequired {
		// A plan-required member is always spawned read-only regardless of
		// mode: it cannot edit anything until `team approve` flips it, so
		// mode is enforced here, not merely defaulted.
		member, err = store.SpawnPlanRequiredMember(teamID, name, resolvedProvider, model, role)
	} else {
		member, err = store.SpawnMember(teamID, name, resolvedProvider, model, role, mode)
	}
	if err != nil {
		return workflow.TeamMember{}, err
	}
	if resolvedProvider == "claude" {
		if err := store.PersistMemberSession(teamID, name, uuid.NewString()); err != nil {
			return workflow.TeamMember{}, err
		}
		member, err = store.GetMember(teamID, name)
		if err != nil {
			return workflow.TeamMember{}, err
		}
	}
	return member, nil
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
	// CreateTeamTaskWithGate (not the bare Store method) so the M2
	// task_created quality-gate hook fires from the CLI exactly the same way
	// it does from the team.tasks.create workflow primitive — one front
	// door, per the architecture ruling.
	//
	// Run.ID is set to teamID before the call — a one-off CLI invocation is
	// a single goroutine, so this has none of the concurrency risk RunTeam's
	// own r.Run.ID assignment documents (see RunTeamTurn); it just gives a
	// configured wrapper provider the same PALLIUM_WORKFLOW_RUN_ID metadata
	// during a gate call that it already gets during a real team turn.
	// Found by review: this bare Runner used to leave Run.ID empty.
	runner := &workflow.Runner{Run: workflow.Run{ID: teamID}}
	task, err := runner.CreateTeamTaskWithGate(context.Background(), store, teamID, title, *description, deps)
	if err != nil {
		return err
	}
	return output.Write(out, task, jsonOutput, func() string {
		if task.Status == "blocked" {
			return "Task added then BLOCKED by quality gate: " + task.ID + " — " + task.Title + " (" + task.Result + ")"
		}
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
	// Best-effort (see TouchMemberActivity's own doc comment): the claim
	// already succeeded and is already committed — a heartbeat-refresh
	// failure here must never surface as a claim failure to a caller
	// (a real external session) that already holds the task.
	_ = store.TouchMemberActivity(positionals[0], *as)
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
	// CompleteTaskWithGate, not the bare Store method — same front door as
	// team.tasks.complete and RunTeamTurn's own decision-application, so a
	// CLI-driven completion gets the identical task_completed quality bar.
	//
	// Run.ID is set to teamID before the call for the same reason as `team
	// tasks add` above — a configured wrapper provider gets the same
	// PALLIUM_WORKFLOW_RUN_ID metadata during this gate call that it would
	// during a real team turn. Found by review.
	runner := &workflow.Runner{Run: workflow.Run{ID: positionals[0]}}
	task, approved, err := runner.CompleteTaskWithGate(context.Background(), store, positionals[0], positionals[1], *as, *result)
	if err != nil {
		return err
	}
	// Best-effort, same reasoning as runTeamTasksClaim above: the
	// completion (and any gate verdict) already committed.
	_ = store.TouchMemberActivity(positionals[0], *as)
	// approved is folded into the JSON payload (not just the text renderer)
	// via anonymous embedding, which flattens TeamTask's own fields into the
	// same object rather than nesting under a "task" key — found by review:
	// --json on a rejected completion used to serialize only the unchanged
	// task, so automation had no reliable way to distinguish a rejection
	// from a successful no-op completion.
	payload := struct {
		workflow.TeamTask
		Approved bool `json:"approved"`
	}{TeamTask: task, Approved: approved}
	return output.Write(out, payload, jsonOutput, func() string {
		if !approved {
			return "Task " + task.ID + " completion REJECTED by quality gate; feedback sent to " + *as
		}
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
	// Best-effort liveness ping (see TouchMemberActivity's own doc comment):
	// the send already succeeded, so a heartbeat-refresh failure must never
	// surface as a send failure. Also always a safe no-op for a
	// non-external sender like the default "lead".
	_ = store.TouchMemberActivity(teamID, *from)
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
	// M3 external-session attach: an external member has no provider-driven
	// turn to mark its own mail delivered the way RunTeamTurn does, so
	// reading its inbox IS the delivery event — the CLI's own delivery
	// receipt. Scoped to provider=="external" (checked here, not just in the
	// store's own defensive WHERE clause) so a lead peeking at a real
	// provider-driven member's queued mail via `--for` never marks it
	// delivered out from under that member's own next turn. `--all` is
	// exempt for the same reason: it's a whole-team read, not one member
	// reading its own mail.
	if !*all && strings.TrimSpace(*forName) != "" {
		if member, merr := store.GetMember(teamID, *forName); merr == nil && member.Provider == "external" {
			ids := make([]string, len(msgs))
			for i, m := range msgs {
				ids[i] = m.ID
			}
			if err := store.MarkMessagesDelivered(ids, 0); err != nil {
				return err
			}
			// Best-effort (see TouchMemberActivity's own doc comment): the
			// mail above is ALREADY marked delivered in the DB at this
			// point — found by adversarial review, a touch failure here
			// used to fail the whole command and return an error, but the
			// caller's own undelivered-mail query would never surface
			// these messages again on retry. Losing a heartbeat refresh is
			// a shrug; losing mail content is not.
			_ = store.TouchMemberActivity(teamID, *forName)
			now := time.Now().UTC().Format(time.RFC3339Nano)
			for i := range msgs {
				msgs[i].DeliveredAt = now
			}
		}
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
	// untracked_cost_providers surfaces the SAME honesty signal the text
	// renderer below already prints, as a structured field too — M1 landed
	// the usage read-back and the human-readable note, but a JSON consumer
	// (a dashboard, a script polling `team status --json`) had no way to
	// see it without parsing prose. Included unconditionally (not gated on
	// whether a budget is set) since it's equally true either way; a caller
	// checking budget honesty naturally reads it alongside BudgetUSDLimit.
	payload := map[string]any{"team": team, "members": members, "tasks": tasks, "untracked_cost_providers": workflow.UntrackedCostProviders(members)}
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
			// StopRequested-but-Status-not-yet-"stopped" is the exact
			// window between `team member stop` landing mid-turn and that
			// turn actually finishing — same honest-reporting bar as the
			// turn-in-flight note above: say what's ACTUALLY true (a stop
			// is pending) rather than let status alone imply this member
			// is still making unrestricted progress.
			if m.StopRequested && m.Status != "stopped" {
				b.WriteString(" (stop requested — will not be scheduled again once this turn finishes)")
			}
			// M3 external-session attach: this member has no provider turn
			// to prove it's alive, so say plainly what its OWN CLI activity
			// last showed rather than implying it's being watched somehow.
			if m.Provider == "external" {
				switch {
				case m.LastActiveAt == "":
					b.WriteString(" (external, no activity since joining)")
				case time.Since(parseTeamTimestamp(m.LastActiveAt)) > externalMemberStaleAfter:
					fmt.Fprintf(&b, " (external, STALE — last active %s)", m.LastActiveAt)
				default:
					fmt.Fprintf(&b, " (external, last active %s)", m.LastActiveAt)
				}
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
		if untracked := workflow.UntrackedCostProviders(members); len(untracked) > 0 {
			fmt.Fprintf(&b, "  cost not tracked for: %s (reports no usage/cost at all; the spend total above is incomplete for these members)\n", strings.Join(untracked, ", "))
		}
		return strings.TrimRight(b.String(), "\n")
	})
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

// runTeamApprove is the full M2 plan-approval flow when the member is
// plan-required (ApproveMemberPlan: validates a plan is actually pending,
// flips to edit, journals the approval as a message), and falls back to the
// M1 primitive mode-flip for a plain read-only member with no plan flow at
// all — approve on a non-plan-required member has always just meant "let it
// edit now," and that keeps working unchanged.
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
	teamID, name := positionals[0], positionals[1]
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	existing, err := store.GetMember(teamID, name)
	if err != nil {
		return err
	}
	var member workflow.TeamMember
	if existing.PlanRequired {
		member, err = store.ApproveMemberPlan(teamID, name)
	} else {
		if err := store.SetMemberMode(teamID, name, "edit"); err != nil {
			return err
		}
		member, err = store.GetMember(teamID, name)
	}
	if err != nil {
		return err
	}
	return output.Write(out, member, jsonOutput, func() string {
		return member.Name + " approved for edit mode"
	})
}

// runTeamReject delivers plan-review feedback (M2) and keeps the member
// read-only — see Store.RejectMemberPlan for why this isn't terminal.
func runTeamReject(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team reject")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 3 {
		return fmt.Errorf("usage: pallium team reject <team-id> <member-name> \"<feedback>\" [--json]")
	}
	teamID, name := positionals[0], positionals[1]
	feedback := strings.Join(positionals[2:], " ")
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	member, err := store.RejectMemberPlan(teamID, name, feedback)
	if err != nil {
		return err
	}
	return output.Write(out, member, jsonOutput, func() string {
		return member.Name + "'s plan rejected; feedback delivered"
	})
}

// runTeamGateCmd configures the M2 quality-gate hooks. Called "gate set"
// (not just "gate") to leave room for a future "gate show" without an
// awkward positional-vs-subcommand ambiguity.
func runTeamGateCmd(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || args[0] != "set" {
		return fmt.Errorf("usage: pallium team gate set <team-id> --hooks task_created,task_completed,teammate_idle \"<prompt>\" [--json]")
	}
	fs := newSessionFlagSet("team gate set")
	dbPath := fs.String("db", "", "")
	hooksFlag := fs.String("hooks", "", "")
	if err := parseSessionFlags(fs, args[1:], map[string]struct{}{"db": {}, "hooks": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	// SetTeamGate already treats an empty hooks list as "disable gating"
	// (team_store.go), but this CLI used to reject an empty --hooks before
	// that path was ever reachable, leaving no supported way to turn a
	// configured gate back off. --hooks "" (explicitly passed, checked via
	// flagWasSet rather than just an empty string, which is also what an
	// omitted flag defaults to) now clears it, and a prompt is no longer
	// required in that case — there is nothing left for a prompt to
	// describe. Found by review.
	clearingHooks := flagWasSet(fs, "hooks") && strings.TrimSpace(*hooksFlag) == ""
	if len(positionals) < 1 || (!clearingHooks && (len(positionals) < 2 || strings.TrimSpace(*hooksFlag) == "")) {
		return fmt.Errorf("usage: pallium team gate set <team-id> --hooks task_created,task_completed,teammate_idle \"<prompt>\" [--json]\n   or: pallium team gate set <team-id> --hooks \"\" (clears a configured gate)")
	}
	teamID := positionals[0]
	prompt := ""
	if len(positionals) > 1 {
		prompt = strings.Join(positionals[1:], " ")
	}
	var hooks []string
	for _, h := range strings.Split(*hooksFlag, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hooks = append(hooks, h)
		}
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetTeamGate(teamID, prompt, hooks); err != nil {
		return err
	}
	team, err := store.GetTeam(teamID)
	if err != nil {
		return err
	}
	return output.Write(out, team, jsonOutput, func() string {
		if len(hooks) == 0 {
			return "Quality gate cleared for " + teamID
		}
		return fmt.Sprintf("Quality gate configured for %s: hooks=%s", teamID, strings.Join(hooks, ","))
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

// runTeamMember dispatches individual-teammate supervision (M2 item 4,
// PR B): stop/restart/steer, one specific member rather than the whole
// team (`team stop` already covers that). All three are SOFT — see
// steerDirectivePrefix's own comment and each subcommand's usage string:
// none of this kills an in-flight provider call. A turn already running
// when stop/steer lands keeps running to its own natural completion; the
// supervision takes effect starting the member's NEXT turn. A true
// mid-turn kill needs per-turn PID/liveness tracking (the same care
// 0.9.15's owner_pid/heartbeat work went through for workflow runs) and is
// a deliberately separate, bigger, not-yet-built feature — not attempted
// here.
func runTeamMember(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pallium team member <stop|restart|steer> <team-id> <member-name> [args] [--json]")
	}
	switch args[0] {
	case "stop":
		return runTeamMemberStop(out, args[1:], jsonOutput)
	case "restart":
		return runTeamMemberRestart(out, args[1:], jsonOutput)
	case "steer":
		return runTeamMemberSteer(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown team member subcommand: %s", args[0])
	}
}

func runTeamMemberStop(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team member stop")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 {
		return fmt.Errorf("usage: pallium team member stop <team-id> <member-name> [--json]\n  soft stop: takes effect once this member's current turn (if any) finishes; does not kill an in-flight call")
	}
	teamID, name := positionals[0], positionals[1]
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	member, err := store.RequestMemberStop(teamID, name)
	if err != nil {
		return err
	}
	return output.Write(out, member, jsonOutput, func() string {
		if member.TurnStartedAt != "" {
			return fmt.Sprintf("Stop requested for %s — a turn is currently in flight; will not be scheduled again once it finishes", name)
		}
		return fmt.Sprintf("%s stopped", name)
	})
}

func runTeamMemberRestart(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team member restart")
	dbPath := fs.String("db", "", "")
	staleAfterMinutes := fs.Int("stale-after-minutes", 15, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "stale-after-minutes": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 2 {
		return fmt.Errorf("usage: pallium team member restart <team-id> <member-name> [--stale-after-minutes N] [--json]")
	}
	teamID, name := positionals[0], positionals[1]
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	staleAfter := time.Now().Add(-time.Duration(*staleAfterMinutes) * time.Minute).UTC().Format(time.RFC3339Nano)
	member, err := store.RestartMember(teamID, name, staleAfter)
	if err != nil {
		return err
	}
	return output.Write(out, member, jsonOutput, func() string {
		return fmt.Sprintf("%s restarted (status=%s)", name, member.Status)
	})
}

func runTeamMemberSteer(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("team member steer")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 3 {
		return fmt.Errorf("usage: pallium team member steer <team-id> <member-name> \"<directive>\" [--json]\n  soft steer: delivered as high-priority mail for this member's NEXT turn; does not interrupt a turn already in flight")
	}
	teamID, name := positionals[0], positionals[1]
	directive := strings.Join(positionals[2:], " ")
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.GetMember(teamID, name); err != nil {
		return err
	}
	msg, err := store.SendTeamMessage(teamID, "lead", name, workflow.SteerDirectivePrefix+directive)
	if err != nil {
		return err
	}
	return output.Write(out, msg, jsonOutput, func() string {
		return "Steering directive delivered to " + name
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
