package workflow

import "testing"

func TestAdvanceLoopStagnationCountsRepeatedSignature(t *testing.T) {
	sig, count, stagnated := AdvanceLoopStagnation(LoopStateNoOp, "", "abc", 0, 3)
	if sig != "abc" || count != 0 || stagnated {
		t.Fatalf("first observation should reset the counter, got sig=%q count=%d stagnated=%v", sig, count, stagnated)
	}
	sig, count, stagnated = AdvanceLoopStagnation(LoopStateNoOp, "abc", "abc", 0, 3)
	if sig != "abc" || count != 1 || stagnated {
		t.Fatalf("expected count=1 not yet stagnated, got sig=%q count=%d stagnated=%v", sig, count, stagnated)
	}
	sig, count, stagnated = AdvanceLoopStagnation(LoopStateNoOp, "abc", "abc", 1, 3)
	if count != 2 || stagnated {
		t.Fatalf("expected count=2 not yet stagnated, got count=%d stagnated=%v", count, stagnated)
	}
	sig, count, stagnated = AdvanceLoopStagnation(LoopStateNoOp, "abc", "abc", 2, 3)
	if sig != "abc" || count != 3 || !stagnated {
		t.Fatalf("expected count=3 and stagnated at threshold, got sig=%q count=%d stagnated=%v", sig, count, stagnated)
	}
}

func TestAdvanceLoopStagnationResetsOnChangedSignature(t *testing.T) {
	_, count, stagnated := AdvanceLoopStagnation(LoopStateNoOp, "abc", "xyz", 5, 3)
	if count != 0 || stagnated {
		t.Fatalf("a changed signature must reset the counter, got count=%d stagnated=%v", count, stagnated)
	}
}

// TestAdvanceLoopStagnationErrorTickDoesNotChangeCounter is one of the
// three regression tests re-pinned explicitly for this PR: an errored tick
// must neither advance NOR reset the stagnation counter — an error isn't a
// legitimate observation to compare against prior cycles.
func TestAdvanceLoopStagnationErrorTickDoesNotChangeCounter(t *testing.T) {
	sig, count, stagnated := AdvanceLoopStagnation(LoopStateError, "abc", "whatever-this-error-run-computed", 2, 3)
	if sig != "abc" {
		t.Fatalf("expected the error tick to leave last_signature UNCHANGED, got %q", sig)
	}
	if count != 2 {
		t.Fatalf("expected the error tick to leave stagnation_count UNCHANGED at 2, got %d", count)
	}
	if stagnated {
		t.Fatalf("an error tick must never itself report stagnated")
	}
}

func TestAdvanceLoopStagnationEmptySignatureNeverStagnates(t *testing.T) {
	count := 0
	for i := 0; i < 10; i++ {
		_, count, _ = AdvanceLoopStagnation(LoopStateNoOp, "", "", count, 3)
	}
	if count != 0 {
		t.Fatalf("a script that never opts into the signature contract must never accumulate stagnation, got count=%d", count)
	}
}

func TestEnforceLoopBudgetOverridesSelfReportedSuccess(t *testing.T) {
	got := EnforceLoopBudget(LoopStateSuccess, 5.00, 0, 1.00, 0)
	if got != LoopStateExhausted {
		t.Fatalf("expected a blown cycle budget to override self-reported success, got %q", got)
	}
	got = EnforceLoopBudget(LoopStateSuccess, 0.10, 9.95, 0, 10.00)
	if got != LoopStateExhausted {
		t.Fatalf("expected a blown lifetime budget to override self-reported success, got %q", got)
	}
}

func TestEnforceLoopBudgetPassesThroughWhenWithinLimits(t *testing.T) {
	got := EnforceLoopBudget(LoopStateSuccess, 0.10, 1.00, 5.00, 10.00)
	if got != LoopStateSuccess {
		t.Fatalf("expected the script's own state to pass through when within budget, got %q", got)
	}
}

func TestEnforceLoopBudgetZeroCeilingMeansNoLimit(t *testing.T) {
	got := EnforceLoopBudget(LoopStateSuccess, 999.0, 999.0, 0, 0)
	if got != LoopStateSuccess {
		t.Fatalf("expected a zero ceiling to mean no limit, got %q", got)
	}
}

func TestLoopExitCodeDistinctPerState(t *testing.T) {
	seen := map[int]string{}
	for _, state := range []string{
		LoopStateSuccess, LoopStateError, LoopStateNoOp, LoopStateBlocked,
		LoopStateExhausted, LoopStateStagnated, LoopStateAlreadyRunning,
	} {
		code := LoopExitCode(state)
		if other, ok := seen[code]; ok {
			t.Fatalf("exit code %d used by both %q and %q — must be distinct", code, other, state)
		}
		seen[code] = state
	}
	if LoopExitCode(LoopStateSuccess) != 0 {
		t.Fatalf("expected success to exit 0")
	}
	if LoopExitCode("some-unknown-state") == 0 {
		t.Fatalf("expected an unrecognized state to never report success (exit 0)")
	}
}
