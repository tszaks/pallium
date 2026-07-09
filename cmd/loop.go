package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/output"
	"github.com/tszaks/pallium/internal/workflow"
)

func runLoop(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pallium loop <start|tick|status|list|stop|reset> [--json]")
	}
	switch args[0] {
	case "start":
		return runLoopStart(out, args[1:], jsonOutput)
	case "tick":
		return runLoopTick(out, args[1:], jsonOutput)
	case "status":
		return runLoopStatus(out, args[1:], jsonOutput)
	case "list":
		return runLoopList(out, args[1:], jsonOutput)
	case "stop":
		return runLoopStop(out, args[1:], jsonOutput)
	case "reset":
		return runLoopReset(out, args[1:], jsonOutput)
	default:
		return fmt.Errorf("unknown loop subcommand: %s", args[0])
	}
}

// currentBinaryIdentity reports what future ticks compare against to warn
// on binary staleness (cron ticks run whatever binary is on disk — if
// Pallium gets upgraded after a loop started, ticks should say so rather
// than silently running new code under an old loop's identity). Deliberately
// prefers the VCS revision over the semver Version field: a dev build
// (buildVersion=="dev", the common case outside an official release
// binary) would otherwise report the same literal string "dev" forever
// regardless of which commit it was actually built from, masking exactly
// the staleness this exists to catch. An official release binary's
// ldflags-injected Version is still available as a fallback when no VCS
// info is present at all (e.g. built outside a git checkout).
func currentBinaryIdentity() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return buildVersion
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			modified := ""
			for _, s2 := range info.Settings {
				if s2.Key == "vcs.modified" && s2.Value == "true" {
					modified = "+dirty"
				}
			}
			return setting.Value + modified
		}
	}
	if buildVersion != "" && buildVersion != "dev" {
		return buildVersion
	}
	return "unknown"
}

// LoopTickExitError carries the specific process exit code a `loop tick`
// outcome maps to (see workflow.LoopExitCode) — main.go checks for this via
// errors.As and exits with Code instead of the generic 1 every other
// command error produces. A typed error (not a direct os.Exit call inside
// runLoopTick) keeps this testable: a test can inspect Code without
// actually terminating the test binary.
type LoopTickExitError struct {
	Code  int
	State string
}

func (e *LoopTickExitError) Error() string {
	return fmt.Sprintf("loop tick ended in state %q (exit %d)", e.State, e.Code)
}

func runLoopStart(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("loop start")
	dbPath := fs.String("db", "", "")
	cwd := fs.String("cwd", "", "")
	scriptPath := fs.String("script", "", "")
	argsJSON := fs.String("args", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "cwd": {}, "script": {}, "args": {}}, nil); err != nil {
		return err
	}
	name, err := requireArg(fs.Args(), "loop-name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(*scriptPath) == "" {
		return fmt.Errorf("usage: pallium loop start <name> --script f.js [--cwd path] [--args json] [--json]")
	}
	root := strings.TrimSpace(*cwd)
	if root == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*scriptPath)
	if err != nil {
		return err
	}
	script := string(raw)
	cfg, ok, err := workflow.ExtractLoopMeta(script)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("script %s does not declare meta.kind==\"loop\" — pass a loop script (see the review-until-clean template)", *scriptPath)
	}
	absScript, err := filepath.Abs(*scriptPath)
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	loop, err := store.CreateLoop(workflow.Loop{
		Name:                 name,
		ScriptPath:           absScript,
		CWD:                  absRoot,
		ArgsJSON:             strings.TrimSpace(*argsJSON),
		StagnationThreshold:  cfg.StagnationThreshold,
		CycleBudgetUSD:       cfg.CycleBudgetUSD,
		LifetimeBudgetUSD:    cfg.LifetimeBudgetUSD,
		StartedBinaryVersion: currentBinaryIdentity(),
		StartedScriptHash:    workflow.StableHash(script),
	})
	if err != nil {
		return err
	}
	return output.Write(out, loop, jsonOutput, func() string {
		msg := fmt.Sprintf("Loop started: %s\n  script: %s\n  cwd:    %s\n  stagnation threshold: %d cycles", loop.Name, loop.ScriptPath, loop.CWD, loop.StagnationThreshold)
		if loop.CycleBudgetUSD > 0 || loop.LifetimeBudgetUSD > 0 {
			msg += fmt.Sprintf("\n  budget: cycle $%.4f / lifetime $%.4f (only self-enforces for cost-tracked providers — claude,"+
				" or a wrapper that reports PALLIUM_WORKFLOW_USAGE_FILE; codex reports no usage/cost at all"+
				" and won't count toward either ceiling — see `loop status` after ticking)", loop.CycleBudgetUSD, loop.LifetimeBudgetUSD)
		}
		return msg
	})
}

// runLoopTick advances a loop by exactly one bounded cycle. It composes
// through the SAME front door `pallium workflow run` uses (runWorkflowRun)
// to spawn the tick's child run — a loop never gets its own workflow_runs
// row, see workflow.Loop's doc comment — rather than duplicating any of
// that run-creation/execution logic here.
func runLoopTick(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("loop tick")
	dbPath := fs.String("db", "", "")
	agentTimeout := fs.Int("agent-timeout", 600, "")
	staleAfterMinutes := fs.Int("stale-after-minutes", 15, "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}, "agent-timeout": {}, "stale-after-minutes": {}}, nil); err != nil {
		return err
	}
	name, err := requireArg(fs.Args(), "loop-name")
	if err != nil {
		return err
	}
	// Resolved ONCE and reused for both this call's own store AND the child
	// run's --db below — see resolvePalliumDBPath's doc comment for the
	// cross-database split bug this avoids.
	resolvedDBPath := resolvePalliumDBPath(*dbPath)
	store, err := workflow.Open(resolvedDBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	staleAfter := time.Now().Add(-time.Duration(*staleAfterMinutes) * time.Minute).UTC().Format(time.RFC3339Nano)
	lease, err := store.BeginLoopTick(name, staleAfter)
	if err != nil {
		result := map[string]any{"loop": name, "state": workflow.LoopStateAlreadyRunning, "error": err.Error()}
		// already_running is exit-code-only: zero state mutation. No
		// FinishLoopTick call — no lease was ever acquired, so there is
		// nothing to release and no cycle/signature to touch.
		_ = output.Write(out, result, jsonOutput, func() string {
			return fmt.Sprintf("loop %s: %v", name, err)
		})
		return &LoopTickExitError{Code: workflow.LoopExitCode(workflow.LoopStateAlreadyRunning), State: workflow.LoopStateAlreadyRunning}
	}

	loop, err := store.GetLoop(name)
	if err != nil {
		return err
	}

	if currentID := currentBinaryIdentity(); loop.StartedBinaryVersion != "" && currentID != loop.StartedBinaryVersion {
		fmt.Fprintf(os.Stderr, "[loop:%s] binary changed since this loop started (was %s, now %s) — if this is unexpected, confirm the installed pallium binary is what you think it is\n", name, loop.StartedBinaryVersion, currentID)
	}
	scriptRaw, rerr := os.ReadFile(loop.ScriptPath)
	if rerr == nil {
		if currentHash := workflow.StableHash(string(scriptRaw)); loop.StartedScriptHash != "" && currentHash != loop.StartedScriptHash {
			fmt.Fprintf(os.Stderr, "[loop:%s] script content changed since `loop start` (%s) — this tick runs the CURRENT file; config (budgets/thresholds) still reflects what was captured at start\n", name, loop.ScriptPath)
		}
	}

	childID := workflow.NewID("loop-" + name)
	runArgs := []string{
		"run",
		"--id", childID,
		"--db", resolvedDBPath,
		"--cwd", loop.CWD,
		"--script", loop.ScriptPath,
		"--loop-name", name,
		"--agent-timeout", strconv.Itoa(*agentTimeout),
	}
	if loop.ArgsJSON != "" {
		runArgs = append(runArgs, "--args", loop.ArgsJSON)
	}
	runArgs = append(runArgs, "tick "+strconv.Itoa(loop.Cycle+1)+" of loop "+name)

	// In JSON mode, the child run's own JSON is suppressed — this command
	// emits exactly ONE JSON object covering the whole tick (child run
	// details included), not two concatenated documents on one stream. In
	// text mode the child run's own human-readable progress is left
	// visible; genuinely useful context for a human watching cron logs,
	// with this command's own summary appended after it.
	childOut := out
	if jsonOutput {
		childOut = io.Discard
	}
	runErr := runWorkflowRun(childOut, runArgs, jsonOutput)

	childRun, gerr := store.Run(childID)
	if gerr != nil {
		_ = store.FinishLoopTick(name, lease, workflow.LoopStateError, loop.LastSignature, loop.StagnationCount, 0)
		return gerr
	}
	_, cycleCostUSD, uerr := store.AgentUsage(childID)
	if uerr != nil {
		cycleCostUSD = 0
	}

	scriptState, signature, resultErr := parseLoopTickResult(childRun.Result)
	state := scriptState
	switch {
	case runErr != nil || childRun.Status == "failed" || resultErr != nil:
		state = workflow.LoopStateError
	case state == "":
		state = workflow.LoopStateNoOp
	}

	newSignature, newStagnationCount, stagnated := workflow.AdvanceLoopStagnation(state, loop.LastSignature, signature, loop.StagnationCount, loop.StagnationThreshold)
	if stagnated {
		state = workflow.LoopStateStagnated
	}
	state = workflow.EnforceLoopBudget(state, cycleCostUSD, loop.LifetimeSpendUSD, loop.CycleBudgetUSD, loop.LifetimeBudgetUSD)

	if err := store.FinishLoopTick(name, lease, state, newSignature, newStagnationCount, cycleCostUSD); err != nil {
		return err
	}

	result := map[string]any{
		"loop":       name,
		"cycle":      loop.Cycle + 1,
		"state":      state,
		"run_id":     childID,
		"cost_usd":   cycleCostUSD,
		"stagnation": newStagnationCount,
	}
	if writeErr := output.Write(out, result, jsonOutput, func() string {
		return fmt.Sprintf("loop %s: tick %d -> %s (run %s, $%.4f)", name, loop.Cycle+1, state, childID, cycleCostUSD)
	}); writeErr != nil {
		return writeErr
	}
	if state != workflow.LoopStateSuccess {
		return &LoopTickExitError{Code: workflow.LoopExitCode(state), State: state}
	}
	return nil
}

// parseLoopTickResult reads a loop script's returned {state, signature}
// contract out of the child run's stored Result (see Run.Result — already
// populated by the regular workflow-run completion path, no new plumbing
// needed). A script that returns nothing usable is not itself an error —
// state comes back "" and runLoopTick treats that as no_op — but a run
// that never completed at all (killed, crashed) reports resultErr so the
// caller can distinguish "ran and said nothing" from "never finished".
func parseLoopTickResult(resultText string) (state, signature string, err error) {
	resultText = strings.TrimSpace(resultText)
	if resultText == "" {
		return "", "", nil
	}
	var parsed map[string]any
	if jsonErr := json.Unmarshal([]byte(resultText), &parsed); jsonErr != nil {
		return "", "", nil
	}
	state, _ = parsed["state"].(string)
	signature, _ = parsed["signature"].(string)
	return state, signature, nil
}

func runLoopStatus(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("loop status")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	name, err := requireArg(fs.Args(), "loop-name")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	loop, err := store.GetLoop(name)
	if err != nil {
		return err
	}
	runs, err := store.RunsByLoop(name, 10)
	if err != nil {
		return err
	}
	payload := map[string]any{"loop": loop, "recent_runs": runs}
	return output.Write(out, payload, jsonOutput, func() string {
		var b strings.Builder
		fmt.Fprintf(&b, "Loop %s [%s] — cycle %d, last=%s, stagnation=%d/%d\n", loop.Name, loop.Status, loop.Cycle, firstNonEmptyLoop(loop.LastTerminalState, "(none yet)"), loop.StagnationCount, loop.StagnationThreshold)
		fmt.Fprintf(&b, "  spend: $%.4f", loop.LifetimeSpendUSD)
		if loop.LifetimeBudgetUSD > 0 {
			fmt.Fprintf(&b, " / $%.4f lifetime", loop.LifetimeBudgetUSD)
		}
		b.WriteString("\n")
		b.WriteString("  cost not tracked for: codex (reports no usage/cost at all; the spend above is incomplete if this loop's script uses codex agents)\n")
		if loop.TickStartedAt != "" {
			fmt.Fprintf(&b, "  tick in flight since %s\n", loop.TickStartedAt)
		}
		for _, r := range runs {
			fmt.Fprintf(&b, "  run %s [%s]\n", r.ID, r.Status)
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

func firstNonEmptyLoop(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func runLoopList(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("loop list")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	loops, err := store.ListLoops()
	if err != nil {
		return err
	}
	return output.Write(out, loops, jsonOutput, func() string {
		var b strings.Builder
		for _, l := range loops {
			fmt.Fprintf(&b, "%-24s [%-8s] cycle=%-4d last=%s\n", l.Name, l.Status, l.Cycle, firstNonEmptyLoop(l.LastTerminalState, "(none yet)"))
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

func runLoopStop(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("loop stop")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	name, err := requireArg(fs.Args(), "loop-name")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetLoopStatus(name, "stopped"); err != nil {
		return err
	}
	return output.Write(out, map[string]any{"loop": name, "status": "stopped"}, jsonOutput, func() string {
		return fmt.Sprintf("Loop %s stopped", name)
	})
}

func runLoopReset(out io.Writer, args []string, jsonOutput bool) error {
	fs := newSessionFlagSet("loop reset")
	dbPath := fs.String("db", "", "")
	if err := parseSessionFlags(fs, args, map[string]struct{}{"db": {}}, nil); err != nil {
		return err
	}
	name, err := requireArg(fs.Args(), "loop-name")
	if err != nil {
		return err
	}
	store, err := openPalliumStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.ResetLoop(name); err != nil {
		return err
	}
	return output.Write(out, map[string]any{"loop": name, "reset": true}, jsonOutput, func() string {
		return fmt.Sprintf("Loop %s reset (stagnation cleared, cycle history preserved)", name)
	})
}
