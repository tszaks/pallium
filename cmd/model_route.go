package cmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tszaks/pallium/internal/routing"
	"github.com/tszaks/pallium/internal/workflow"
	"io"
	"os"
	"path/filepath"
)

func runModelRoute(out io.Writer, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pallium route models <init|explain|catalog|history> [--config path] [--task-class class] [--model model] [--reasoning-effort effort]")
	}
	fs := flag.NewFlagSet("route", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := fs.String("config", routing.ConfigPath("."), "Routing policy path")
	class := fs.String("task-class", "", "Task class declared by steering agent")
	provider := fs.String("provider", "", "Explicit provider pin")
	model := fs.String("model", "", "Explicit model pin")
	effort := fs.String("reasoning-effort", "", "Explicit reasoning effort pin")
	mode := fs.String("mode", "read-only", "Worker permission mode")
	runID := fs.String("run", "", "Workflow or team ID for invocation history")
	db := fs.String("db", "", "Pallium database path")
	network := fs.Bool("network", false, "Require network capability")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected route arguments: %v", fs.Args())
	}
	write := func(v any) error { e := json.NewEncoder(out); e.SetIndent("", "  "); return e.Encode(v) }
	switch args[0] {
	case "history":
		if *runID == "" {
			return fmt.Errorf("route models history requires --run")
		}
		store, err := openPalliumStore(*db)
		if err != nil {
			return err
		}
		defer store.Close()
		rows, err := store.ListInvocations(*runID)
		if err != nil {
			return err
		}
		return write(rows)
	case "init":
		c := routing.Starter()
		raw, _ := json.MarshalIndent(c, "", "  ")
		if err := os.MkdirAll(filepath.Dir(*config), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(*config, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(append(raw, '\n'))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return write(map[string]any{"path": *config, "mode": "shadow", "note": "Verify provider access before execution. No model has been launched."})
	case "catalog", "explain":
		c, err := routing.Load(*config)
		if err != nil {
			return err
		}
		for _, candidate := range c.Candidates {
			if err := workflow.ValidateReasoningEffort(candidate.Provider, candidate.Model, candidate.Effort); err != nil {
				return err
			}
		}
		if args[0] == "catalog" {
			return write(c)
		}
		d, err := c.Choose(routing.Request{Provider: *provider, Model: *model, Effort: *effort, TaskClass: *class, Mode: *mode, Network: *network}, func(provider string) bool { return workflow.ProviderAvailableWithNetwork(provider, "", *network) })
		if err != nil {
			return err
		}
		return write(d)
	default:
		return fmt.Errorf("unknown route command %q", args[0])
	}
}
