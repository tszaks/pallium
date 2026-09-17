package cmd

import (
	"testing"
)

func TestKnowledgeBuildParsesAllowStale(t *testing.T) {
	opts, useModel, positional, err := parseKnowledgeBuildArgs([]string{"--allow-stale", "--no-model", "--only", "storage", "/repo"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.AllowStale || useModel || len(positional) != 1 || positional[0] != "/repo" {
		t.Fatalf("unexpected parsed build options: opts=%+v useModel=%t positional=%v", opts, useModel, positional)
	}
}
