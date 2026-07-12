package workflow

import "testing"

func TestValidateScriptWarnsOnThenChainedOnAgent(t *testing.T) {
	result := ValidateScript(`
const x = agent("do something", {label: "x"}).then(r => r.summary)
return x
`)
	if !result.Valid {
		t.Fatalf("expected script to still be syntactically valid, got error: %s", result.Error)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", result.Warnings)
	}
}

func TestValidateScriptWarnsOnCatchChainedOnCheck(t *testing.T) {
	result := ValidateScript(`
check("npm test").catch(e => log(e))
return "done"
`)
	if !result.Valid {
		t.Fatalf("expected script to still be syntactically valid, got error: %s", result.Error)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", result.Warnings)
	}
}

func TestValidateScriptNoWarningOnDirectAgentReturn(t *testing.T) {
	result := ValidateScript(`
const r = agent("do something", {label: "x"})
return r
`)
	if !result.Valid {
		t.Fatalf("expected valid script, got error: %s", result.Error)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expected no warnings for the documented direct-return shape, got %v", result.Warnings)
	}
}

func TestValidateScriptNoWarningOnUnrelatedThenUsage(t *testing.T) {
	result := ValidateScript(`
const p = Promise.resolve(42).then(v => v + 1)
const agentResult = agent("do something", {label: "x"})
return {p, agentResult}
`)
	if !result.Valid {
		t.Fatalf("expected valid script, got error: %s", result.Error)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expected no false-positive warning on an unrelated .then(), got %v", result.Warnings)
	}
}

func TestValidateScriptNoWarningOnAgentInsideArgumentsOfAnotherCall(t *testing.T) {
	result := ValidateScript(`
const results = await pipeline([1,2,3], item => agent("process " + item, {label: "x"}))
return results
`)
	if !result.Valid {
		t.Fatalf("expected valid script, got error: %s", result.Error)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expected no warning when agent() isn't itself chained with .then(), got %v", result.Warnings)
	}
}
