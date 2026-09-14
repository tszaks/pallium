package routing

import (
	"testing"
)

func TestPolicyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		req       Request
		available bool
		wantModel string
		wantErr   bool
	}{
		{"auto rule", "auto", Request{TaskClass: "bounded-edit", Mode: "edit"}, true, "gpt-5.6-luna", false},
		{"unknown conservative", "auto", Request{TaskClass: "unknown", Mode: "edit"}, true, "gpt-6-astra", false},
		{"shadow unchanged", "shadow", Request{TaskClass: "bounded-edit", Mode: "edit"}, true, "", false},
		{"explicit model", "auto", Request{Model: "gpt-5.5", Mode: "edit"}, true, "gpt-5.5", false},
		{"explicit effort", "auto", Request{Effort: "high", Mode: "edit"}, true, "", false},
		{"lab boundary", "auto", Request{Provider: "claude", Mode: "edit"}, true, "", true},
		{"unavailable", "auto", Request{Mode: "edit"}, false, "", true},
		{"network boundary", "auto", Request{Mode: "edit", Network: true}, true, "", true},
		{"invalid permission", "auto", Request{Mode: "admin"}, true, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Starter()
			c.Mode = tc.mode
			c.Rules["bounded-edit"] = "luna-xhigh"
			d, err := c.Choose(tc.req, func(string) bool { return tc.available })
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v", err)
			}
			if err == nil && d.Selected.Model != tc.wantModel {
				t.Fatalf("decision %+v", d)
			}
			if tc.mode == "shadow" && d.Recommended == nil {
				t.Fatal("missing shadow recommendation")
			}
		})
	}
}

func TestUnavailableRuleFallsBackWithoutCrossingProvider(t *testing.T) {
	c := Starter()
	c.Mode = "auto"
	c.Rules["edit"] = "luna-xhigh"
	c.Candidates[len(c.Candidates)-1].Enabled = false
	d, err := c.Choose(Request{TaskClass: "edit", Mode: "edit"}, func(string) bool { return true })
	if err != nil || d.Selected.Model != "gpt-6-astra" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestPolicyHashChangesWithEffortAndRules(t *testing.T) {
	c := Starter()
	before := c.Hash()
	c.Candidates[0].Effort = "low"
	if before == c.Hash() {
		t.Fatal("effort ignored")
	}
	before = c.Hash()
	c.Rules["x"] = "luna-medium"
	if before == c.Hash() {
		t.Fatal("rule ignored")
	}
}

func TestVerificationEscalationIsBoundedAndPreservesPins(t *testing.T) {
	c := Starter()
	c.Mode = "auto"
	c.Rules["fix"] = "luna-xhigh"
	c.Escalations = map[string][]string{"fix": {"terra-medium", "astra-high"}}
	for retry, want := range []string{"gpt-5.6-luna", "gpt-5.6-terra", "gpt-6-astra"} {
		d, err := c.Choose(Request{TaskClass: "fix", Mode: "edit", VerificationRetry: retry}, func(string) bool { return true })
		if err != nil || d.Selected.Model != want {
			t.Fatalf("retry %d: %+v %v", retry, d, err)
		}
	}
	if _, err := c.Choose(Request{TaskClass: "fix", Mode: "edit", VerificationRetry: 3}, func(string) bool { return true }); err == nil {
		t.Fatal("unbounded escalation")
	}
	d, err := c.Choose(Request{TaskClass: "fix", Mode: "edit", Model: "gpt-5.5", Effort: "high", VerificationRetry: 1}, func(string) bool { return true })
	if err != nil || d.Selected.Model != "gpt-5.5" || d.Selected.Effort != "high" {
		t.Fatalf("pin changed: %+v %v", d, err)
	}
}
