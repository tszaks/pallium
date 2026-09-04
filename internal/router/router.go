package router

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	AuthorityObserve  = "observe"
	AuthorityExecute  = "execute"
	AuthorityEdit     = "edit"
	AuthorityExternal = "external"
)

type Options struct {
	Task      string
	CWD       string
	Authority string
}

type Report struct {
	Task               string          `json:"task"`
	CWD                string          `json:"cwd"`
	CapabilityID       string          `json:"capability_id"`
	Capability         Capability      `json:"capability"`
	Service            string          `json:"service"`
	Action             string          `json:"action"`
	Command            string          `json:"command,omitempty"`
	CommandArgs        []string        `json:"command_args,omitempty"`
	Why                string          `json:"why"`
	DecisionConfidence string          `json:"decision_confidence"`
	Signals            []string        `json:"signals"`
	RequiredAuthority  string          `json:"required_authority"`
	AuthorityCeiling   string          `json:"authority_ceiling"`
	Allowed            bool            `json:"allowed"`
	BlockedReason      string          `json:"blocked_reason,omitempty"`
	Alternatives       []Alternative   `json:"alternatives"`
	Repository         RepositoryState `json:"repository"`
	Caveats            []string        `json:"caveats"`
	Execution          *Execution      `json:"execution,omitempty"`
}

type Execution struct {
	Attempted  bool       `json:"attempted"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   int        `json:"exit_code"`
	Success    bool       `json:"success"`
	Result     any        `json:"result,omitempty"`
	Output     string     `json:"output,omitempty"`
	Stderr     string     `json:"stderr,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type Alternative struct {
	Service string `json:"service"`
	Command string `json:"command,omitempty"`
	WhyNot  string `json:"why_not"`
}

type RepositoryState struct {
	IsGit      bool   `json:"is_git"`
	Branch     string `json:"branch,omitempty"`
	Dirty      bool   `json:"dirty"`
	Head       string `json:"head,omitempty"`
	Confidence string `json:"confidence"`
}

type recommendation struct {
	capabilityID string
	action       string
	commandArgs  []string
	why          string
	confidence   string
	signals      []string
	alternatives []Alternative
	caveats      []string
}

func Route(ctx context.Context, opts Options) (Report, error) {
	task := strings.TrimSpace(opts.Task)
	if task == "" {
		return Report{}, fmt.Errorf("task cannot be empty")
	}
	authority := strings.ToLower(strings.TrimSpace(opts.Authority))
	if authority == "" {
		authority = AuthorityObserve
	}
	if _, ok := authorityRank(authority); !ok {
		return Report{}, fmt.Errorf("invalid authority %q; use observe, execute, edit, or external", authority)
	}
	cwd := strings.TrimSpace(opts.CWD)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return Report{}, err
		}
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return Report{}, err
	}
	if info, statErr := os.Stat(absCWD); statErr != nil {
		return Report{}, fmt.Errorf("inspect cwd: %w", statErr)
	} else if !info.IsDir() {
		return Report{}, fmt.Errorf("cwd is not a directory: %s", absCWD)
	}

	repo := inspectRepository(ctx, absCWD)
	rec := recommend(task, absCWD, repo)
	capability, ok := CapabilityByID(rec.capabilityID)
	if !ok {
		return Report{}, fmt.Errorf("router selected unknown capability %q", rec.capabilityID)
	}
	ceilingRank, _ := authorityRank(authority)
	requiredRank, _ := authorityRank(capability.RequiredAuthority)
	allowed := requiredRank <= ceilingRank
	report := Report{
		Task:               task,
		CWD:                absCWD,
		CapabilityID:       capability.ID,
		Capability:         capability,
		Service:            capability.Service,
		Action:             rec.action,
		Command:            renderCommand(rec.commandArgs),
		CommandArgs:        append([]string(nil), rec.commandArgs...),
		Why:                rec.why,
		DecisionConfidence: rec.confidence,
		Signals:            rec.signals,
		RequiredAuthority:  capability.RequiredAuthority,
		AuthorityCeiling:   authority,
		Allowed:            allowed,
		Alternatives:       rec.alternatives,
		Repository:         repo,
		Caveats:            rec.caveats,
	}
	if !allowed {
		report.BlockedReason = fmt.Sprintf("recommendation requires %s authority, above the caller's %s ceiling", capability.RequiredAuthority, authority)
	}
	return report, nil
}

func recommend(task, cwd string, repo RepositoryState) recommendation {
	lower := strings.ToLower(task)
	quotedTask := shellQuote(task)
	quotedCWD := shellQuote(cwd)
	editRequested := containsAny(lower, "implement", "fix", "build", "refactor", "migrate", "change the code", "add a feature") ||
		strings.HasPrefix(lower, "add ") || strings.HasPrefix(lower, "create ") || strings.Contains(lower, " then release")

	if containsAny(lower, "running session", "active session", "live session", "other codex", "other claude", "agent session", "session overlap") {
		return recommendation{
			capabilityID: "sessions-live",
			action:       "inspect-live-sessions",
			commandArgs:  []string{"sessions", "live", "--running-only", "--details", "--json"},
			why:          "The request is about current local agent activity; live session discovery has process and transcript evidence and reports its coverage limits.",
			confidence:   "high",
			signals:      matchedSignals(lower, map[string]string{"session": "mentions sessions", "running": "asks about current activity", "active": "asks about current activity", "overlap": "asks about possible concurrent work"}),
			alternatives: []Alternative{
				{Service: "session-memory", Command: "pallium sessions recall " + quotedTask + " --repo " + quotedCWD + " --json", WhyNot: "Recall searches prior work; it does not prove what is running now."},
				{Service: "workflow", Command: "pallium workflow preflight " + quotedTask + " --cwd " + quotedCWD + " --json", WhyNot: "A workflow adds orchestration overhead before the overlap question is answered."},
			},
			caveats: []string{"Discovery is local and best-effort; inspect the returned coverage object before treating the list as exhaustive."},
		}
	}

	if strings.Contains(lower, "session") && containsAny(lower, "wrapped", "finished", "completed", "touched", "inactive", "recent", "latest", "updated") {
		return recommendation{
			capabilityID: "sessions-find",
			action:       "find-sessions-by-state-and-time",
			commandArgs:  []string{"sessions", "find", task, "--details", "--json"},
			why:          "The request describes session completion, inactivity, or recency in natural language; session find translates it into explicit metadata filters and returns the interpretation.",
			confidence:   "high",
			signals:      []string{"mentions sessions", "asks about completion, activity age, or recency"},
			alternatives: []Alternative{
				{Service: "session-awareness", Command: "pallium sessions live --details --json", WhyNot: "Live discovery is best for current process activity but does not natively apply the requested time or completion predicate."},
				{Service: "session-memory", Command: "pallium sessions search " + quotedTask + " --json", WhyNot: "Transcript search matches conversation content, not session-state metadata."},
			},
			caveats: []string{"Natural time phrases are returned with their parsed interpretation; unknown completion evidence is never treated as unfinished."},
		}
	}

	if containsAny(lower, "where did", "last time", "previous session", "prior session", "recall", "what happened before", "continue yesterday", "resume context") {
		return recommendation{
			capabilityID: "sessions-recall",
			action:       "recall-prior-work",
			commandArgs:  []string{"sessions", "recall", task, "--repo", cwd, "--json"},
			why:          "The task depends on evidence from prior agent runs, so session recall is the narrowest durable source of context.",
			confidence:   "high",
			signals:      []string{"asks for prior-session context"},
			alternatives: []Alternative{
				{Service: "session-awareness", Command: "pallium sessions live --details --json", WhyNot: "Live discovery shows current activity, not the contents of prior work."},
				{Service: "repo-intelligence", Command: "pallium decisions " + quotedTask + " " + quotedCWD + " --json", WhyNot: "Decision search is narrower and may omit unfinished execution details."},
			},
		}
	}

	if containsAny(lower, "again and again", "keep retrying", "until green", "until clean", "recurring", "repeat until", "every time") {
		return recommendation{
			capabilityID: "loop-design",
			action:       "design-bounded-loop",
			commandArgs:  []string{"workflow", "preflight", task, "--cwd", cwd, "--json"},
			why:          "The request describes repeated bounded execution; preflight should define the loop's script, success condition, and stop conditions before it is started.",
			confidence:   "high",
			signals:      []string{"requests repeated execution with a terminal condition"},
			alternatives: []Alternative{
				{Service: "workflow", Command: "pallium start " + quotedTask + " --cwd " + quotedCWD + " --json", WhyNot: "A single workflow finishes one pass and does not persist a cross-invocation cycle."},
				{Service: "verification", Command: "pallium verify safe " + quotedCWD + " --json", WhyNot: "Verification checks once; it does not own retry state or stop policy."},
			},
			caveats: []string{"Starting or ticking the eventual loop still requires the authority appropriate to its actions."},
		}
	}

	if containsAny(lower, "debate", "independent reviewers", "peer agents", "agent team", "agents coordinate", "parallel reviewers") {
		template := "parallel-review"
		if strings.Contains(lower, "debate") || strings.Contains(lower, "argue") {
			template = "adversarial-debate"
		}
		return recommendation{
			capabilityID: "team-start",
			action:       "start-independent-team",
			commandArgs:  []string{"team", "start", task, "--cwd", cwd, "--template", template, "--json"},
			why:          "The task explicitly benefits from peers reasoning independently and exchanging findings.",
			confidence:   "high",
			signals:      []string{"requests independent or peer-to-peer agent work"},
			alternatives: []Alternative{
				{Service: "workflow", Command: "pallium start " + quotedTask + " --cwd " + quotedCWD + " --json", WhyNot: "Workflow fan-out is enough only when workers do not need peer-to-peer coordination."},
				{Service: "repo-intelligence", Command: "pallium review " + quotedCWD + " --json", WhyNot: "Static review is cheaper but does not supply independent deliberation."},
			},
			caveats: []string{"External authority is required because this launches autonomous peers; their edits and external actions remain subject to their own gates."},
		}
	}

	if containsAny(lower, "run the tests", "test suite", "verify the build", "build is green", "tests are green", "verify everything") {
		return recommendation{
			capabilityID: "verify-safe",
			action:       "run-safe-verification",
			commandArgs:  []string{"verify", "safe", cwd, "--json"},
			why:          "The requested outcome is objective local verification, which the verification service can discover, run, and report consistently.",
			confidence:   "high",
			signals:      []string{"requests objective test or build evidence"},
			alternatives: []Alternative{
				{Service: "workflow", Command: "pallium start " + quotedTask + " --cwd " + quotedCWD + " --json", WhyNot: "A workflow is unnecessary unless failures must be diagnosed and fixed."},
				{Service: "repo-intelligence", Command: "pallium workflow preflight " + quotedTask + " --cwd " + quotedCWD + " --json", WhyNot: "Preflight proposes tests but does not execute them."},
			},
		}
	}

	if containsAny(lower, "review the diff", "review this change", "review the current", "code review", "risk of this", "what does this change touch") || (strings.Contains(lower, "review") && !editRequested) {
		return recommendation{
			capabilityID: "repo-review",
			action:       "review-current-change",
			commandArgs:  []string{"review", cwd, "--json"},
			why:          "The request is about the current change surface, which repo intelligence can inspect without launching an editing workflow.",
			confidence:   "high",
			signals:      []string{"asks for change review or blast-radius evidence"},
			alternatives: []Alternative{
				{Service: "workflow", Command: "pallium start " + quotedTask + " --style review --cwd " + quotedCWD + " --json", WhyNot: "Use this only when the review needs multiple staged or adversarial workers."},
				{Service: "verification", Command: "pallium verify safe " + quotedCWD + " --json", WhyNot: "Tests can disprove some defects but do not explain the whole change surface."},
			},
		}
	}

	if editRequested {
		signals := []string{"requests source-changing implementation"}
		if repo.IsGit {
			signals = append(signals, "cwd is a Git repository")
		}
		if repo.Dirty {
			signals = append(signals, "working tree already has changes")
		}
		return recommendation{
			capabilityID: "workflow-start",
			action:       "run-repo-scoped-workflow",
			commandArgs:  []string{"start", task, "--cwd", cwd, "--json"},
			why:          "The task asks for an implementation with inspection, edits, and verification; a resumable repo-scoped workflow is the strongest fit.",
			confidence:   "medium",
			signals:      signals,
			alternatives: []Alternative{
				{Service: "repo-intelligence", Command: "pallium workflow preflight " + quotedTask + " --cwd " + quotedCWD + " --json", WhyNot: "Preflight is safer when the caller wants a plan only, but it will not implement the change."},
				{Service: "plain-agent", WhyNot: "Direct editing is lower overhead for a truly one-shot change, but this wording implies a broader implementation lifecycle."},
			},
			caveats: []string{"The route authorizes no edits by itself; execution remains blocked unless the caller's authority ceiling includes edit."},
		}
	}

	return recommendation{
		capabilityID: "repo-preflight",
		action:       "preflight-task",
		commandArgs:  []string{"workflow", "preflight", task, "--cwd", cwd, "--json"},
		why:          "The task does not map confidently to a specialized service, so a read-only preflight is the safest way to collect scope, risk, and verification evidence before choosing.",
		confidence:   "low",
		signals:      []string{"no high-confidence specialized trigger"},
		alternatives: []Alternative{
			{Service: "plain-agent", WhyNot: "Prefer this only if preflight confirms the task is a one-shot question or edit."},
			{Service: "workflow", Command: "pallium start " + quotedTask + " --cwd " + quotedCWD + " --json", WhyNot: "Starting orchestration before scope is known may add unnecessary cost."},
		},
		caveats: []string{"This is a conservative fallback, not a claim that a workflow is required."},
	}
}

func inspectRepository(ctx context.Context, cwd string) RepositoryState {
	state := RepositoryState{Confidence: "high"}
	inside, err := gitOutput(ctx, cwd, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		state.Confidence = "high"
		return state
	}
	state.IsGit = true
	if branch, branchErr := gitOutput(ctx, cwd, "branch", "--show-current"); branchErr == nil {
		state.Branch = strings.TrimSpace(branch)
	}
	if head, headErr := gitOutput(ctx, cwd, "rev-parse", "--short=12", "HEAD"); headErr == nil {
		state.Head = strings.TrimSpace(head)
	}
	if status, statusErr := gitOutput(ctx, cwd, "status", "--porcelain"); statusErr == nil {
		state.Dirty = strings.TrimSpace(status) != ""
	} else {
		state.Confidence = "partial"
	}
	return state
}

func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = cwd
	raw, err := command.Output()
	return string(raw), err
}

func authorityRank(authority string) (int, bool) {
	switch authority {
	case AuthorityObserve:
		return 0, true
	case AuthorityExecute:
		return 1, true
	case AuthorityEdit:
		return 2, true
	case AuthorityExternal:
		return 3, true
	default:
		return 0, false
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func matchedSignals(value string, candidates map[string]string) []string {
	unique := map[string]bool{}
	for needle, signal := range candidates {
		if strings.Contains(value, needle) {
			unique[signal] = true
		}
	}
	result := make([]string, 0, len(unique))
	for signal := range unique {
		result = append(result, signal)
	}
	sort.Strings(result)
	return result
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func renderCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "pallium")
	for _, arg := range args {
		if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
			return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("-._/:=@,+", r)
		}) == -1 {
			parts = append(parts, arg)
			continue
		}
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}
