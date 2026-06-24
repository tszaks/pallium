package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/codexsessions"
	"github.com/tszaks/pallium/internal/console"
	"github.com/tszaks/pallium/internal/gitlog"
	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/sessionmemory"
)

type consoleSnapshot struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Host        string           `json:"host"`
	Sessions    []consoleSession `json:"sessions"`
}

type consoleSession struct {
	codexsessions.SessionSummary
	Manifest      *console.Manifest `json:"manifest,omitempty"`
	Claims        []console.Claim   `json:"claims,omitempty"`
	ManifestStale bool              `json:"manifest_stale,omitempty"`
	ConflictCount int               `json:"conflict_count,omitempty"`
}

func runConsole(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	switch args[0] {
	case "ls", "list":
		return runConsoleList(out, args[1:], jsonOutput, false)
	case "watch":
		return runConsoleList(out, args[1:], jsonOutput, true)
	case "show":
		return runConsoleShow(out, args[1:], jsonOutput)
	case "manifest":
		return runConsoleManifest(out, args[1:], jsonOutput)
	case "handoff":
		return runConsoleHandoff(out, args[1:], jsonOutput)
	case "claim":
		return runConsoleClaim(out, args[1:], jsonOutput)
	case "action":
		return runConsoleAction(out, args[1:], jsonOutput)
	case "authority":
		return runConsoleAuthority(out, args[1:], jsonOutput)
	case "gate":
		return runConsoleGate(out, args[1:], jsonOutput)
	case "review":
		return runConsoleReview(out, args[1:], jsonOutput)
	default:
		printConsoleHelp(out)
		return fmt.Errorf("unknown console subcommand: %s", args[0])
	}
}

func runConsoleList(out io.Writer, args []string, jsonOutput, watch bool) error {
	if hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	fs := newSessionFlagSet("console ls")
	includeAll := fs.Bool("all", false, "")
	details := fs.Bool("details", false, "")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, map[string]struct{}{"all": {}, "details": {}}); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected console ls argument: %s", fs.Arg(0))
	}
	if watch && jsonOutput {
		return fmt.Errorf("console watch cannot be used with --json")
	}
	render := func() error {
		snapshot, err := buildConsoleSnapshot(*includeAll, *details, *dbPath)
		if err != nil {
			return err
		}
		return output.Write(out, snapshot, jsonOutput, func() string {
			return renderConsoleSnapshot(snapshot, *includeAll, *details)
		})
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

func runConsoleShow(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	fs := newSessionFlagSet("console show")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console show <session-id>")
	}
	snapshot, err := buildConsoleSnapshot(true, true, *dbPath)
	if err != nil {
		return err
	}
	session, err := findConsoleSession(snapshot.Sessions, fs.Arg(0))
	if err != nil {
		return err
	}
	return output.Write(out, session, jsonOutput, func() string {
		return renderConsoleSessionDetail(session)
	})
}

func runConsoleManifest(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	switch args[0] {
	case "set":
		return runConsoleManifestSet(out, args[1:], jsonOutput)
	case "show":
		return runConsoleManifestShow(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown console manifest subcommand: %s", args[0])
	}
}

func runConsoleManifestSet(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console manifest set")
	sessionID := fs.String("session", "", "")
	provider := fs.String("provider", "", "")
	machine := fs.String("machine", "", "")
	dbPath := fs.String("db", "", "")
	goal := fs.String("goal", "", "")
	step := fs.String("step", "", "")
	cwd := fs.String("cwd", "", "")
	stop := fs.String("stop", "", "")
	sourcePID := fs.Int("source-pid", 0, "")
	var files, next, risks, blockers multiStringFlag
	fs.Var(&files, "file", "")
	fs.Var(&next, "next", "")
	fs.Var(&risks, "risk", "")
	fs.Var(&blockers, "blocker", "")
	valueFlags := map[string]struct{}{"session": {}, "provider": {}, "machine": {}, "db": {}, "goal": {}, "step": {}, "cwd": {}, "stop": {}, "source-pid": {}, "file": {}, "next": {}, "risk": {}, "blocker": {}}
	if err := parseSessionFlags(fs, args, valueFlags, nil); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected console manifest set argument: %s", fs.Arg(0))
	}
	key, summary, err := resolveConsoleSessionKey(*sessionID, *provider, *machine)
	if err != nil {
		return err
	}
	if *cwd == "" && summary != nil {
		*cwd = summary.EffectiveWorkdir
	}
	if *sourcePID == 0 && summary != nil {
		*sourcePID = summary.PID
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	manifest, err := store.UpsertManifest(console.Manifest{
		SessionKey:  key,
		CWD:         *cwd,
		Goal:        *goal,
		CurrentStep: *step,
		Files:       []string(files),
		NextActions: []string(next),
		Risks:       []string(risks),
		Blockers:    []string(blockers),
		Stop:        *stop,
		SourcePID:   *sourcePID,
	})
	if err != nil {
		return err
	}
	return output.Write(out, manifest, jsonOutput, func() string {
		return fmt.Sprintf("Manifest saved for %s %s", manifest.Provider, shortID(manifest.SessionID))
	})
}

func runConsoleManifestShow(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console manifest show")
	sessionID := fs.String("session", "", "")
	provider := fs.String("provider", "", "")
	machine := fs.String("machine", "", "")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"session": {}, "provider": {}, "machine": {}, "db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected console manifest show argument: %s", fs.Arg(0))
	}
	key, _, err := resolveConsoleSessionKey(*sessionID, *provider, *machine)
	if err != nil {
		return err
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	manifest, err := store.Manifest(key)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("no manifest found for session %s", key.SessionID)
		}
		return err
	}
	return output.Write(out, manifest, jsonOutput, func() string {
		return renderManifest(manifest)
	})
}

func runConsoleHandoff(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	switch args[0] {
	case "write":
		return runConsoleHandoffWrite(out, args[1:], jsonOutput)
	case "list":
		return runConsoleHandoffList(out, args[1:], jsonOutput)
	case "accept":
		return runConsoleHandoffAccept(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown console handoff subcommand: %s", args[0])
	}
}

func runConsoleHandoffWrite(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console handoff write")
	sessionID := fs.String("session", "", "")
	toSessionID := fs.String("to-session", "", "")
	provider := fs.String("provider", "", "")
	machine := fs.String("machine", "", "")
	dbPath := fs.String("db", "", "")
	summary := fs.String("summary", "", "")
	var next, blockers multiStringFlag
	fs.Var(&next, "next", "")
	fs.Var(&blockers, "blocker", "")
	valueFlags := map[string]struct{}{"session": {}, "to-session": {}, "provider": {}, "machine": {}, "db": {}, "summary": {}, "next": {}, "blocker": {}}
	if err := parseSessionFlags(fs, args, valueFlags, nil); err != nil {
		return err
	}
	fromKey, _, err := resolveConsoleSessionKey(*sessionID, *provider, *machine)
	if err != nil {
		return err
	}
	toKey := console.SessionKey{}
	if strings.TrimSpace(*toSessionID) != "" {
		toKey, _, err = resolveConsoleSessionKey(*toSessionID, "", "")
		if err != nil {
			return err
		}
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	handoff, err := store.WriteHandoff(console.Handoff{
		FromProvider:   fromKey.Provider,
		FromSessionID:  fromKey.SessionID,
		FromMachine:    fromKey.Machine,
		ToProvider:     toKey.Provider,
		ToSessionID:    toKey.SessionID,
		ToMachine:      toKey.Machine,
		Summary:        *summary,
		PendingActions: []string(next),
		Blockers:       []string(blockers),
	})
	if err != nil {
		return err
	}
	return output.Write(out, handoff, jsonOutput, func() string {
		return fmt.Sprintf("Handoff saved: %s", handoff.ID)
	})
}

func runConsoleHandoffList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console handoff list")
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
	handoffs, err := store.ListHandoffs(*limit)
	if err != nil {
		return err
	}
	return output.Write(out, handoffs, jsonOutput, func() string {
		return renderHandoffs(handoffs)
	})
}

func runConsoleHandoffAccept(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console handoff accept")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console handoff accept <handoff-id>")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.AcceptHandoff(fs.Arg(0)); err != nil {
		return err
	}
	return output.Write(out, map[string]string{"status": "accepted", "id": fs.Arg(0)}, jsonOutput, func() string {
		return "Handoff accepted: " + fs.Arg(0)
	})
}

func runConsoleClaim(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	switch args[0] {
	case "acquire":
		return runConsoleClaimAcquire(out, args[1:], jsonOutput)
	case "release":
		return runConsoleClaimRelease(out, args[1:], jsonOutput)
	case "list":
		return runConsoleClaimList(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown console claim subcommand: %s", args[0])
	}
}

func runConsoleClaimAcquire(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console claim acquire")
	sessionID := fs.String("session", "", "")
	provider := fs.String("provider", "", "")
	machine := fs.String("machine", "", "")
	dbPath := fs.String("db", "", "")
	file := fs.String("file", "", "")
	repo := fs.String("repo", "", "")
	intent := fs.String("intent", "", "")
	ttl := fs.Duration("ttl", 30*time.Minute, "")
	valueFlags := map[string]struct{}{"session": {}, "provider": {}, "machine": {}, "db": {}, "file": {}, "repo": {}, "intent": {}, "ttl": {}}
	if err := parseSessionFlags(fs, args, valueFlags, nil); err != nil {
		return err
	}
	key, _, err := resolveConsoleSessionKey(*sessionID, *provider, *machine)
	if err != nil {
		return err
	}
	repoRoot, target, err := normalizeClaimTarget(*repo, *file)
	if err != nil {
		return err
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	claim, err := store.AcquireClaim(console.Claim{
		Provider:   key.Provider,
		SessionID:  key.SessionID,
		Machine:    key.Machine,
		RepoRoot:   repoRoot,
		TargetType: "file",
		Target:     target,
		Intent:     *intent,
		LeaseUntil: time.Now().UTC().Add(*ttl).Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	return output.Write(out, claim, jsonOutput, func() string {
		text := fmt.Sprintf("Claim acquired: %s %s", claim.ID, claim.Target)
		if claim.Conflict {
			text += "\nConflict with: " + strings.Join(claim.ConflictIDs, ", ")
		}
		return text
	})
}

func runConsoleClaimRelease(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console claim release")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console claim release <claim-id>")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.ReleaseClaim(fs.Arg(0)); err != nil {
		return err
	}
	return output.Write(out, map[string]string{"status": "released", "id": fs.Arg(0)}, jsonOutput, func() string {
		return "Claim released: " + fs.Arg(0)
	})
}

func runConsoleClaimList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console claim list")
	dbPath := fs.String("db", "", "")
	repo := fs.String("repo", "", "")
	includeReleased := fs.Bool("all", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "repo": {}}, map[string]struct{}{"all": {}}); err != nil {
		return err
	}
	repoRoot := ""
	if strings.TrimSpace(*repo) != "" {
		repoRoot, _ = resolveRepoRoot(*repo)
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	claims, err := store.ListClaims(repoRoot, *includeReleased)
	if err != nil {
		return err
	}
	return output.Write(out, claims, jsonOutput, func() string {
		return renderClaims(claims)
	})
}

func runConsoleAction(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	if args[0] == "request" {
		return requestConsoleAuthority(out, args[1:], jsonOutput, "message")
	}
	if args[0] == "list" {
		return runConsoleAuthorityList(out, args[1:], jsonOutput)
	}
	return fmt.Errorf("unknown console action subcommand: %s", args[0])
}

func runConsoleAuthority(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	switch args[0] {
	case "request":
		return requestConsoleAuthority(out, args[1:], jsonOutput, "")
	case "decide":
		return runConsoleAuthorityDecide(out, args[1:], jsonOutput)
	case "list":
		return runConsoleAuthorityList(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown console authority subcommand: %s", args[0])
	}
}

func requestConsoleAuthority(out io.Writer, args []string, jsonOutput bool, defaultLevel string) error {
	fs := newSessionFlagSet("console authority request")
	sessionID := fs.String("session", "", "")
	provider := fs.String("provider", "", "")
	machine := fs.String("machine", "", "")
	dbPath := fs.String("db", "", "")
	level := fs.String("level", defaultLevel, "")
	action := fs.String("action", "", "")
	target := fs.String("target", "", "")
	details := fs.String("details", "", "")
	actor := fs.String("actor", "agent", "")
	idempotency := fs.String("idempotency-key", "", "")
	reason := fs.String("reason", "", "")
	valueFlags := map[string]struct{}{"session": {}, "provider": {}, "machine": {}, "db": {}, "level": {}, "action": {}, "target": {}, "details": {}, "actor": {}, "idempotency-key": {}, "reason": {}}
	if err := parseSessionFlags(fs, args, valueFlags, nil); err != nil {
		return err
	}
	if *details == "" && *reason != "" {
		*details = *reason
	}
	key, _, err := resolveConsoleSessionKey(*sessionID, *provider, *machine)
	if err != nil {
		return err
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	event, err := store.RequestAuthority(console.AuthorityEvent{
		Provider:       key.Provider,
		SessionID:      key.SessionID,
		Machine:        key.Machine,
		Actor:          *actor,
		Level:          *level,
		Action:         *action,
		TargetRef:      *target,
		DetailsJSON:    *details,
		IdempotencyKey: *idempotency,
	})
	if err != nil {
		return err
	}
	return output.Write(out, event, jsonOutput, func() string {
		return fmt.Sprintf("Authority event recorded: %s", event.ID)
	})
}

func runConsoleAuthorityDecide(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console authority decide")
	dbPath := fs.String("db", "", "")
	by := fs.String("by", "human", "")
	approve := fs.Bool("approve", false, "")
	deny := fs.Bool("deny", false, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "by": {}}, map[string]struct{}{"approve": {}, "deny": {}}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console authority decide <event-id> --approve|--deny")
	}
	status := ""
	if *approve {
		status = "approved"
	}
	if *deny {
		if status != "" {
			return fmt.Errorf("choose only one of --approve or --deny")
		}
		status = "denied"
	}
	if status == "" {
		return fmt.Errorf("choose --approve or --deny")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.DecideAuthority(fs.Arg(0), status, *by); err != nil {
		return err
	}
	return output.Write(out, map[string]string{"id": fs.Arg(0), "status": status}, jsonOutput, func() string {
		return fmt.Sprintf("Authority event %s: %s", fs.Arg(0), status)
	})
}

func runConsoleAuthorityList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console authority list")
	dbPath := fs.String("db", "", "")
	sessionID := fs.String("session", "", "")
	limit := fs.Int("limit", 50, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "session": {}, "limit": {}}, nil); err != nil {
		return err
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	events, err := store.ListAuthority(*sessionID, *limit)
	if err != nil {
		return err
	}
	return output.Write(out, events, jsonOutput, func() string {
		return renderAuthorityEvents(events)
	})
}

func runConsoleGate(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	switch args[0] {
	case "open":
		return runConsoleGateOpen(out, args[1:], jsonOutput)
	case "attest":
		return runConsoleGateAttest(out, args[1:], jsonOutput)
	case "list":
		return runConsoleGateList(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown console gate subcommand: %s", args[0])
	}
}

func runConsoleGateOpen(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console gate open")
	dbPath := fs.String("db", "", "")
	eventID := fs.String("event", "", "")
	gateType := fs.String("type", "human", "")
	required := fs.Int("required", 1, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "event": {}, "type": {}, "required": {}}, nil); err != nil {
		return err
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	gate, err := store.OpenGate(console.RiskGate{AuthorityEventID: *eventID, GateType: *gateType, RequiredAttestations: *required})
	if err != nil {
		return err
	}
	return output.Write(out, gate, jsonOutput, func() string {
		return fmt.Sprintf("Risk gate opened: %s", gate.ID)
	})
}

func runConsoleGateAttest(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console gate attest")
	dbPath := fs.String("db", "", "")
	gateID := fs.String("gate", "", "")
	sessionID := fs.String("session", "", "")
	provider := fs.String("provider", "", "")
	machine := fs.String("machine", "", "")
	verdict := fs.String("verdict", "", "")
	evidence := fs.String("evidence", "", "")
	valueFlags := map[string]struct{}{"db": {}, "gate": {}, "session": {}, "provider": {}, "machine": {}, "verdict": {}, "evidence": {}}
	if err := parseSessionFlags(fs, args, valueFlags, nil); err != nil {
		return err
	}
	key := console.SessionKey{}
	var err error
	if *sessionID != "" || *provider != "" || *machine != "" {
		key, _, err = resolveConsoleSessionKey(*sessionID, *provider, *machine)
		if err != nil {
			return err
		}
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	attestation, err := store.AddAttestation(console.Attestation{
		GateID:    *gateID,
		Provider:  key.Provider,
		SessionID: key.SessionID,
		Machine:   key.Machine,
		Verdict:   *verdict,
		Evidence:  *evidence,
	})
	if err != nil {
		return err
	}
	return output.Write(out, attestation, jsonOutput, func() string {
		return fmt.Sprintf("Attestation recorded: %s", attestation.ID)
	})
}

func runConsoleGateList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console gate list")
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
	gates, err := store.ListGates(*limit)
	if err != nil {
		return err
	}
	return output.Write(out, gates, jsonOutput, func() string {
		return renderGates(gates)
	})
}

func runConsoleReview(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 || hasHelpArg(args) {
		printConsoleHelp(out)
		return nil
	}
	switch args[0] {
	case "create":
		return runConsoleReviewCreate(out, args[1:], jsonOutput)
	case "show":
		return runConsoleReviewShow(out, args[1:], jsonOutput)
	case "close":
		return runConsoleReviewClose(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown console review subcommand: %s", args[0])
	}
}

func runConsoleReviewCreate(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console review create")
	dbPath := fs.String("db", "", "")
	topic := fs.String("topic", "", "")
	sessionID := fs.String("session", "", "")
	reviewer := fs.String("reviewer", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "topic": {}, "session": {}, "reviewer": {}}, nil); err != nil {
		return err
	}
	openedBy := "human"
	if *sessionID != "" {
		openedBy = *sessionID
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	review, err := store.CreateReview(console.ReviewCase{Topic: *topic, OpenedBy: openedBy, Reviewer: *reviewer})
	if err != nil {
		return err
	}
	return output.Write(out, review, jsonOutput, func() string {
		return fmt.Sprintf("Agent review opened: %s", review.ID)
	})
}

func runConsoleReviewShow(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console review show")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console review show <review-id>")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	review, err := store.Review(fs.Arg(0))
	if err != nil {
		return err
	}
	return output.Write(out, review, jsonOutput, func() string {
		return fmt.Sprintf("%s %s %s\n%s", review.ID, review.Status, review.Decision, review.Topic)
	})
}

func runConsoleReviewClose(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("console review close")
	dbPath := fs.String("db", "", "")
	decision := fs.String("decision", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "decision": {}}, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: pallium console review close <review-id> --decision proceed|revise|decline")
	}
	store, err := console.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.CloseReview(fs.Arg(0), *decision); err != nil {
		return err
	}
	return output.Write(out, map[string]string{"id": fs.Arg(0), "decision": *decision}, jsonOutput, func() string {
		return fmt.Sprintf("Agent review closed: %s", fs.Arg(0))
	})
}

func buildConsoleSnapshot(includeAll, details bool, dbPath string) (consoleSnapshot, error) {
	base, err := codexsessions.CollectSessions(context.Background(), codexsessions.SessionCollectOptions{IncludeAll: includeAll, IncludeDetails: details})
	if err != nil {
		return consoleSnapshot{}, err
	}
	snapshot := consoleSnapshot{GeneratedAt: base.GeneratedAt, Host: base.Host, Sessions: make([]consoleSession, 0, len(base.Sessions))}
	var manifests map[string]console.Manifest
	var claims []console.Claim
	if store, err := openExistingConsoleStore(dbPath); err == nil && store != nil {
		defer store.Close()
		if ms, err := store.ListManifests(); err == nil {
			manifests = make(map[string]console.Manifest, len(ms))
			for _, manifest := range ms {
				manifests[consoleKey(manifest.Provider, manifest.SessionID, manifest.Machine)] = manifest
			}
		}
		claims, _ = store.ListClaims("", false)
	}
	for _, s := range base.Sessions {
		item := consoleSession{SessionSummary: s}
		key := consoleKey(s.Provider, s.ThreadID, base.Host)
		if manifest, ok := manifests[key]; ok {
			item.Manifest = &manifest
			item.ManifestStale = manifestIsStale(manifest, s)
		}
		item.Claims = claimsForSession(claims, s.Provider, s.ThreadID, base.Host)
		for _, claim := range item.Claims {
			if claim.Conflict {
				item.ConflictCount++
			}
		}
		snapshot.Sessions = append(snapshot.Sessions, item)
	}
	return snapshot, nil
}

func openExistingConsoleStore(dbPath string) (*console.Store, error) {
	path := dbPath
	if path == "" {
		path = sessionmemory.DefaultDBPath()
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return console.Open(path)
}

func resolveConsoleSessionKey(sessionID, provider, machine string) (console.SessionKey, *codexsessions.SessionSummary, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return console.SessionKey{}, nil, fmt.Errorf("session id is required")
	}
	snapshot, err := codexsessions.CollectSessions(context.Background(), codexsessions.SessionCollectOptions{IncludeAll: true, IncludeDetails: false})
	if err == nil {
		matches := make([]codexsessions.SessionSummary, 0)
		for _, session := range snapshot.Sessions {
			if session.ThreadID == sessionID || strings.HasPrefix(session.ThreadID, sessionID) {
				if provider != "" && !strings.EqualFold(session.Provider, provider) {
					continue
				}
				matches = append(matches, session)
			}
		}
		if len(matches) == 1 {
			match := matches[0]
			key := console.SessionKey{Provider: match.Provider, SessionID: match.ThreadID, Machine: firstNonEmpty(machine, snapshot.Host)}
			return key, &match, nil
		}
		if len(matches) > 1 {
			return console.SessionKey{}, nil, fmt.Errorf("session id %q is ambiguous", sessionID)
		}
		if machine == "" {
			machine = snapshot.Host
		}
	}
	if provider == "" {
		provider = "codex"
	}
	if machine == "" {
		host, _ := os.Hostname()
		machine = host
	}
	return console.SessionKey{Provider: strings.ToLower(provider), SessionID: sessionID, Machine: machine}, nil, nil
}

func findConsoleSession(sessions []consoleSession, id string) (consoleSession, error) {
	matches := make([]consoleSession, 0)
	for _, session := range sessions {
		if session.ThreadID == id || strings.HasPrefix(session.ThreadID, id) {
			matches = append(matches, session)
		}
	}
	if len(matches) == 0 {
		return consoleSession{}, fmt.Errorf("session not found: %s", id)
	}
	if len(matches) > 1 {
		return consoleSession{}, fmt.Errorf("session id %q is ambiguous", id)
	}
	return matches[0], nil
}

func normalizeClaimTarget(repoArg, fileArg string) (string, string, error) {
	if strings.TrimSpace(fileArg) == "" {
		return "", "", fmt.Errorf("--file is required")
	}
	repoRoot, err := resolveRepoRoot(firstNonEmpty(repoArg, "."))
	if err != nil {
		return "", "", err
	}
	target := fileArg
	if filepath.IsAbs(target) {
		rel, err := filepath.Rel(repoRoot, target)
		if err != nil {
			return "", "", err
		}
		target = rel
	}
	target = filepath.ToSlash(filepath.Clean(target))
	target = strings.TrimPrefix(target, "./")
	return repoRoot, target, nil
}

func resolveRepoRoot(path string) (string, error) {
	root, err := gitlog.RepoRoot(path)
	if err == nil {
		return root, nil
	}
	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		return "", err
	}
	return abs, nil
}

func manifestIsStale(manifest console.Manifest, session codexsessions.SessionSummary) bool {
	if session.Status != "active" {
		return false
	}
	updated, err := time.Parse(time.RFC3339Nano, manifest.UpdatedAt)
	if err != nil {
		return true
	}
	return session.LastActiveAt.After(updated)
}

func claimsForSession(claims []console.Claim, provider, sessionID, machine string) []console.Claim {
	out := make([]console.Claim, 0)
	for _, claim := range claims {
		if strings.EqualFold(claim.Provider, provider) && claim.SessionID == sessionID && claim.Machine == machine {
			out = append(out, claim)
		}
	}
	return out
}

func consoleKey(provider, sessionID, machine string) string {
	return strings.ToLower(provider) + "\x00" + sessionID + "\x00" + machine
}

func renderConsoleSnapshot(snapshot consoleSnapshot, includeAll, details bool) string {
	active, inactive := 0, 0
	for _, s := range snapshot.Sessions {
		if s.Status == "active" {
			active++
		} else {
			inactive++
		}
	}
	lines := []string{}
	if includeAll {
		lines = append(lines, fmt.Sprintf("%d active, %d inactive agent sessions", active, inactive))
	} else {
		lines = append(lines, fmt.Sprintf("%d active agent sessions", active))
	}
	lines = append(lines, "updated "+snapshot.GeneratedAt.Local().Format(time.Kitchen), "")
	if len(snapshot.Sessions) == 0 {
		lines = append(lines, "No Codex or Claude sessions found.")
		return strings.Join(lines, "\n")
	}
	for _, session := range snapshot.Sessions {
		pid := "-"
		if session.PID > 0 {
			pid = strconv.Itoa(session.PID)
		}
		label := trimText(firstNonEmpty(manifestGoal(session.Manifest), session.Title), 90)
		stale := ""
		if session.ManifestStale {
			stale = " stale-manifest"
		}
		conflicts := ""
		if session.ConflictCount > 0 {
			conflicts = fmt.Sprintf(" conflicts=%d", session.ConflictCount)
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s %s %s%s%s", pid, firstNonEmpty(session.TTY, "-"), session.Status, shortID(session.ThreadID), compactPath(session.EffectiveWorkdir), label, stale, conflicts))
		if details {
			lines = append(lines, renderConsoleSessionDetails(session)...)
		}
	}
	return strings.Join(lines, "\n")
}

func renderConsoleSessionDetail(session consoleSession) string {
	lines := []string{fmt.Sprintf("%s %s %s", session.Provider, session.Status, session.ThreadID)}
	lines = append(lines, "cwd: "+firstNonEmpty(session.EffectiveWorkdir, session.SessionCWD, "-"))
	if session.Title != "" {
		lines = append(lines, "title: "+session.Title)
	}
	lines = append(lines, renderConsoleSessionDetails(session)...)
	return strings.Join(lines, "\n")
}

func renderConsoleSessionDetails(session consoleSession) []string {
	lines := make([]string, 0)
	if session.Manifest != nil {
		m := session.Manifest
		if m.Goal != "" {
			lines = append(lines, "  goal: "+m.Goal)
		}
		if m.CurrentStep != "" {
			lines = append(lines, "  step: "+m.CurrentStep)
		}
		if len(m.Files) > 0 {
			lines = append(lines, "  files: "+strings.Join(limitStrings(m.Files, 8), ", "))
		}
		if len(m.NextActions) > 0 {
			lines = append(lines, "  next: "+strings.Join(limitStrings(m.NextActions, 4), "; "))
		}
		if len(m.Risks) > 0 {
			lines = append(lines, "  risks: "+strings.Join(limitStrings(m.Risks, 4), ", "))
		}
		if len(m.Blockers) > 0 {
			lines = append(lines, "  blockers: "+strings.Join(limitStrings(m.Blockers, 4), ", "))
		}
		if m.Stop != "" {
			lines = append(lines, "  stop: "+m.Stop)
		}
	}
	if session.RecentAction != "" {
		lines = append(lines, "  recent: "+session.RecentAction)
	}
	if len(session.Claims) > 0 {
		targets := make([]string, 0, len(session.Claims))
		for _, claim := range session.Claims {
			targets = append(targets, claim.Target)
		}
		lines = append(lines, "  claims: "+strings.Join(limitStrings(targets, 8), ", "))
	}
	return lines
}

func renderManifest(m console.Manifest) string {
	lines := []string{fmt.Sprintf("%s %s updated %s", m.Provider, shortID(m.SessionID), m.UpdatedAt)}
	if m.Goal != "" {
		lines = append(lines, "goal: "+m.Goal)
	}
	if m.CurrentStep != "" {
		lines = append(lines, "step: "+m.CurrentStep)
	}
	if len(m.Files) > 0 {
		lines = append(lines, "files: "+strings.Join(m.Files, ", "))
	}
	if len(m.NextActions) > 0 {
		lines = append(lines, "next: "+strings.Join(m.NextActions, "; "))
	}
	if len(m.Risks) > 0 {
		lines = append(lines, "risks: "+strings.Join(m.Risks, ", "))
	}
	if len(m.Blockers) > 0 {
		lines = append(lines, "blockers: "+strings.Join(m.Blockers, ", "))
	}
	if m.Stop != "" {
		lines = append(lines, "stop: "+m.Stop)
	}
	return strings.Join(lines, "\n")
}

func renderHandoffs(handoffs []console.Handoff) string {
	if len(handoffs) == 0 {
		return "No handoffs found."
	}
	lines := []string{"Handoffs:"}
	for _, h := range handoffs {
		lines = append(lines, fmt.Sprintf("- %s %s %s: %s", h.ID, h.Status, shortID(h.FromSessionID), h.Summary))
	}
	return strings.Join(lines, "\n")
}

func renderClaims(claims []console.Claim) string {
	if len(claims) == 0 {
		return "No claims found."
	}
	lines := []string{"Claims:"}
	for _, c := range claims {
		lines = append(lines, fmt.Sprintf("- %s %s %s %s until %s", c.ID, c.Status, shortID(c.SessionID), c.Target, c.LeaseUntil))
	}
	return strings.Join(lines, "\n")
}

func renderAuthorityEvents(events []console.AuthorityEvent) string {
	if len(events) == 0 {
		return "No authority events found."
	}
	lines := []string{"Authority events:"}
	for _, e := range events {
		lines = append(lines, fmt.Sprintf("- %s %s %s %s/%s %s", e.ID, e.Status, shortID(e.SessionID), e.Level, e.Action, e.TargetRef))
	}
	return strings.Join(lines, "\n")
}

func renderGates(gates []console.RiskGate) string {
	if len(gates) == 0 {
		return "No risk gates found."
	}
	lines := []string{"Risk gates:"}
	for _, g := range gates {
		lines = append(lines, fmt.Sprintf("- %s %s %s required=%d event=%s", g.ID, g.Status, g.GateType, g.RequiredAttestations, g.AuthorityEventID))
	}
	return strings.Join(lines, "\n")
}

func manifestGoal(m *console.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Goal
}

func printConsoleHelp(out io.Writer) {
	fmt.Fprintln(out, `pallium console

Usage:
  pallium console ls [--all] [--details] [--db path] [--json]
  pallium console watch [--all] [--details] [--db path]
  pallium console show <session-id> [--db path] [--json]
  pallium console manifest set --session id [--goal text] [--step text] [--file path] [--next text] [--risk text] [--blocker text] [--stop text] [--db path] [--json]
  pallium console manifest show --session id [--db path] [--json]
  pallium console handoff write --session id --summary text [--next text] [--blocker text] [--db path] [--json]
  pallium console handoff list [--limit n] [--db path] [--json]
  pallium console handoff accept <handoff-id> [--db path] [--json]
  pallium console claim acquire --session id --file path [--repo path] [--ttl 30m] [--intent text] [--db path] [--json]
  pallium console claim release <claim-id> [--db path] [--json]
  pallium console claim list [--repo path] [--all] [--db path] [--json]
  pallium console action request --session id --action pause [--reason text] [--db path] [--json]
  pallium console action list [--session id] [--db path] [--json]
  pallium console authority request --session id --level execute --action "go test ./..." [--target ref] [--db path] [--json]
  pallium console authority decide <event-id> --approve|--deny [--by human] [--db path] [--json]
  pallium console authority list [--session id] [--db path] [--json]
  pallium console gate open --event id [--type human] [--required n] [--db path] [--json]
  pallium console gate attest --gate id --verdict allow [--session id] [--evidence ref] [--db path] [--json]
  pallium console gate list [--db path] [--json]
  pallium console review create --topic text [--session id] [--reviewer claude] [--db path] [--json]
  pallium console review show <review-id> [--db path] [--json]
  pallium console review close <review-id> --decision proceed|revise|decline [--db path] [--json]`)
}
