package cmd

import (
	"bytes"
	"strings"
	"testing"
)

const validStubWorkflow = `export const meta = { name: "stubwf", description: "stub", phases: ["p"] };
phase("p");
return { ok: true };`

func TestStartRequiresTask(t *testing.T) {
	var buf bytes.Buffer
	if err := runStart(&buf, nil, false); err == nil {
		t.Fatal("expected an error when no task is given")
	}
}

func TestStartDryRunUsesGeneratedWhenValid(t *testing.T) {
	// The stub short-circuits provider generation with a known-valid script, so
	// start should adopt it and report a generated source. --dry-run means no run.
	t.Setenv("PALLIUM_WORKFLOW_GENERATE_STUB", validStubWorkflow)
	var buf bytes.Buffer
	if err := runStart(&buf, []string{"--dry-run", "do a thing"}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "source: generated") {
		t.Fatalf("expected a generated source, got:\n%s", out)
	}
	if !strings.Contains(out, "stubwf") {
		t.Fatalf("expected the generated script in the output, got:\n%s", out)
	}
}

func TestStartDryRunFallsBackToTemplateWhenInvalid(t *testing.T) {
	// A provider that returns an invalid script must not fail start: it falls
	// back to the deterministic template (always valid) and says so.
	t.Setenv("PALLIUM_WORKFLOW_GENERATE_STUB", "this is not valid javascript !!!")
	var buf bytes.Buffer
	if err := runStart(&buf, []string{"--dry-run", "review the auth code"}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "source: template") {
		t.Fatalf("expected a template fallback source, got:\n%s", out)
	}
	if !strings.Contains(out, "invalid script") {
		t.Fatalf("expected a note explaining the fallback, got:\n%s", out)
	}
	// The fallback must itself be a real, valid workflow.
	if !strings.Contains(out, "export const meta") {
		t.Fatalf("template fallback is not a workflow script:\n%s", out)
	}
}
