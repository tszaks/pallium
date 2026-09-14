package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tszaks/pallium/internal/routing"
)

func writeModelRouteConfig(t *testing.T, config routing.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.json")
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestModelRouteCatalogAllowsInactivePortableWrapper(t *testing.T) {
	config := routing.Starter()
	config.Candidates = append(config.Candidates, routing.Candidate{
		ID: "portable", Provider: "portable", Model: "custom", Effort: "provider-defined",
		Enabled: false, Modes: []string{"read-only"},
	})
	var out bytes.Buffer
	if err := runModelRoute(&out, []string{"catalog", "--config", writeModelRouteConfig(t, config)}, true); err != nil {
		t.Fatalf("inactive portable candidate blocked catalog: %v", err)
	}
}

func TestModelRouteExplainRejectsInvalidExplicitEffort(t *testing.T) {
	config := routing.Starter()
	var out bytes.Buffer
	err := runModelRoute(&out, []string{"explain", "--config", writeModelRouteConfig(t, config), "--provider", "codex", "--model", "gpt-5.5", "--reasoning-effort", "max"}, true)
	if err == nil {
		t.Fatal("explain accepted an unsupported explicit model/effort pair")
	}
}
