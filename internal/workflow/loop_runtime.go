package workflow

// Loop terminal states — the vocabulary a `loop tick` can end in. Each maps
// to a distinct process exit code (see LoopExitCode) so a calling cron/
// agent can branch on exit status alone, without parsing JSON, per the
// settled design. These are pure values with no I/O — the actual per-tick
// orchestration (acquire the lease, spawn the child run through the
// `workflow run` front door, read its result back) lives in cmd/loop.go,
// which is the only place with access to that front door; this file holds
// only the decision RULES loops' own service applies to what comes back.
const (
	LoopStateSuccess        = "success"
	LoopStateNoOp           = "no_op"
	LoopStateBlocked        = "blocked"
	LoopStateExhausted      = "exhausted"
	LoopStateStagnated      = "stagnated"
	LoopStateError          = "error"
	LoopStateAlreadyRunning = "already_running"
)

// LoopExitCode maps a terminal state to a distinct process exit code.
// Unknown states fall back to 1 (same bucket as "error") rather than 0, so
// a caller that only checks "did this exit clean" never mistakes an
// unrecognized state for success.
func LoopExitCode(state string) int {
	switch state {
	case LoopStateSuccess:
		return 0
	case LoopStateError:
		return 1
	case LoopStateNoOp:
		return 2
	case LoopStateBlocked:
		return 3
	case LoopStateExhausted:
		return 4
	case LoopStateStagnated:
		return 5
	case LoopStateAlreadyRunning:
		return 6
	default:
		return 1
	}
}

// defaultStagnationThreshold matches CreateLoop's own fallback — a loop
// with no threshold configured still needs SOME bound rather than spinning
// on an identical signature forever.
const defaultStagnationThreshold = 3

// AdvanceLoopStagnation applies the observe/verify hash+counter rule to one
// tick's result. newSignature is a script-AUTHORED contract field (the
// script must set it explicitly to opt into stagnation detection — this is
// deliberately NOT an implicit hash of the script's whole return value,
// which would trip on incidental noise like a run id or timestamp embedded
// anywhere in it), not something Pallium infers.
//
// An error tick never participates: it neither advances nor resets the
// counter, because an error isn't a legitimate observation to compare
// against prior cycles — comparing error noise would either falsely
// trigger stagnation (a transient provider hiccup looking like "stuck") or
// falsely reset real progress tracking (masking genuine stagnation behind
// an unrelated failure).
//
// A script that never sets signature (or one whose current observation is
// genuinely empty) gets NO stagnation detection rather than a false
// trigger: two empty strings never count as "still stuck on the same
// thing" (the `newSignature != ""` guard below), so the counter simply
// never advances for such a script — the right degrade for opting out of
// the contract, not an accidental false positive.
func AdvanceLoopStagnation(state, lastSignature, newSignature string, count, threshold int) (signature string, newCount int, stagnated bool) {
	if state == LoopStateError {
		return lastSignature, count, false
	}
	if newSignature != "" && newSignature == lastSignature {
		count++
	} else {
		count = 0
	}
	if threshold <= 0 {
		threshold = defaultStagnationThreshold
	}
	return newSignature, count, count >= threshold
}

// EnforceLoopBudget overrides a script's self-reported state to "exhausted"
// when this cycle's actual reported cost blows either budget ceiling,
// regardless of what the script itself claimed — the same hardening
// principle already accepted for Agent Teams (a script can't talk its way
// past a budget by self-reporting "success"). A ceiling of 0 or below means
// "no limit", the same convention team/workflow budgets already use.
//
// Budget honesty carries over from teams: cycleCostUSD is whatever the
// child run's provider(s) actually reported, and not every provider reports
// real cost — claude and a usage-reporting wrapper are tracked, codex is
// not (it has no usage envelope at all, true of the regular non-loop worker
// path too). A codex-only loop's budget therefore cannot self-enforce; see
// PALLIUM.md and `loop status`'s own note for where this is surfaced to an
// operator rather than silently pretending enforcement exists where it
// doesn't.
func EnforceLoopBudget(state string, cycleCostUSD, lifetimeSpendUSD, cycleBudgetUSD, lifetimeBudgetUSD float64) string {
	if cycleBudgetUSD > 0 && cycleCostUSD > cycleBudgetUSD {
		return LoopStateExhausted
	}
	if lifetimeBudgetUSD > 0 && lifetimeSpendUSD+cycleCostUSD > lifetimeBudgetUSD {
		return LoopStateExhausted
	}
	return state
}
