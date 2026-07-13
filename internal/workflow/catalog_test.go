package workflow

import (
	"strings"
	"testing"
)

// TestTeamTemplatesHaveValidShape is the shape contract for M3's team
// templates: every template must have real member content (a template with
// an empty role or mode would spawn a member with a useless or invalid
// mode, silently).
func TestTeamTemplatesHaveValidShape(t *testing.T) {
	templates := TeamTemplates()
	if len(templates) < 2 {
		t.Fatalf("expected at least the two M3-scoped templates (parallel-review, adversarial-debate), got %d", len(templates))
	}
	for _, tmpl := range templates {
		if tmpl.Name == "" || tmpl.Description == "" || tmpl.WhenToUse == "" {
			t.Fatalf("template missing required descriptive fields: %+v", tmpl)
		}
		if len(tmpl.Members) < 2 {
			t.Fatalf("template %q: expected at least 2 members, got %d", tmpl.Name, len(tmpl.Members))
		}
		seen := map[string]bool{}
		for _, m := range tmpl.Members {
			if m.Name == "" || m.Role == "" {
				t.Fatalf("template %q: member missing name or role: %+v", tmpl.Name, m)
			}
			if m.Mode != "read-only" && m.Mode != "edit" {
				t.Fatalf("template %q: member %q has invalid mode %q", tmpl.Name, m.Name, m.Mode)
			}
			if seen[m.Name] {
				t.Fatalf("template %q: duplicate member name %q would collide on spawn", tmpl.Name, m.Name)
			}
			seen[m.Name] = true
		}
	}
}

func TestTeamTemplateLookupByName(t *testing.T) {
	tmpl, ok := TeamTemplate("parallel-review")
	if !ok {
		t.Fatal("expected parallel-review template to exist")
	}
	if tmpl.Name != "parallel-review" {
		t.Fatalf("unexpected template: %+v", tmpl)
	}
	if _, ok := TeamTemplate("does-not-exist"); ok {
		t.Fatal("expected lookup of an unknown template to fail")
	}
}

func TestUnknownTeamTemplateErrorListsNames(t *testing.T) {
	err := UnknownTeamTemplateError("bogus")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range TeamTemplateNames() {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("expected error to list available template %q, got %q", name, err.Error())
		}
	}
}
