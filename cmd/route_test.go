package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tszaks/pallium/internal/router"
)

func TestRouteCapabilitiesAreDiscoverable(t *testing.T) {
	var out bytes.Buffer
	if err := runRoute(&out, []string{"capabilities"}, true); err != nil {
		t.Fatal(err)
	}
	var capabilities []router.Capability
	if err := json.Unmarshal(out.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if len(capabilities) < 5 || capabilities[0].ID == "" {
		t.Fatalf("unexpected capabilities: %+v", capabilities)
	}
}

func TestRouteExecuteRunsStructuredArgsAndReturnsResult(t *testing.T) {
	var received []string
	runner := func(args []string) (string, string, error) {
		received = append([]string(nil), args...)
		return `{"matched": 2}`, "", nil
	}

	var out bytes.Buffer
	if err := runRouteWithRunner(&out, []string{"find all running sessions on this computer", "--execute"}, true, runner); err != nil {
		t.Fatal(err)
	}
	var report router.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Execution == nil || !report.Execution.Attempted || !report.Execution.Success || report.Execution.ExitCode != 0 {
		t.Fatalf("unexpected execution: %+v", report.Execution)
	}
	if strings.Join(received, " ") != "sessions live --running-only --details --json" {
		t.Fatalf("unexpected executed args: %q", received)
	}
	result, ok := report.Execution.Result.(map[string]any)
	if !ok || result["matched"] != float64(2) {
		t.Fatalf("structured result was not preserved: %#v", report.Execution.Result)
	}
}

func TestRouteExecuteRefusesToRaiseAuthority(t *testing.T) {
	runner := func(args []string) (string, string, error) {
		return "", "", errors.New("must not be called")
	}

	var out bytes.Buffer
	err := runRouteWithRunner(&out, []string{"implement a new API", "--execute"}, true, runner)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked execution, got %v", err)
	}
	var report router.Report
	if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if report.Execution == nil || report.Execution.Attempted || report.Execution.Success || report.Allowed {
		t.Fatalf("blocked route should be inspectable and unattempted: %+v", report)
	}
}
