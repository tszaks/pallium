package knowledge

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/workflow"
)

// ProviderSynthesizer runs synthesis through Pallium's existing provider
// layer, so a knowledge build uses whatever agent CLI the user already has
// configured, with the same resolution a workflow agent gets. No new
// credential, no new SDK, no network code in this package.
type ProviderSynthesizer struct {
	CodexBinary string
	Provider    string
	Model       string
	Reasoning   string
	RepoRoot    string
}

// stubEnv lets tests and dry runs supply a canned response, matching the
// pattern workflow generation already uses for the same reason.
const stubEnv = "PALLIUM_KNOWLEDGE_SYNTH_STUB"

func (p ProviderSynthesizer) Synthesize(ctx context.Context, prompt string) (string, error) {
	if stub := strings.TrimSpace(os.Getenv(stubEnv)); stub != "" {
		return stub, nil
	}
	runner := &workflow.Runner{CodexBinary: p.CodexBinary, Run: workflow.Run{CWD: p.RepoRoot}}
	ledger, err := OpenMaintenance()
	if err != nil {
		return "", err
	}
	defer ledger.DB.Close()
	opts := workflow.AgentOptions{Provider: p.Provider, Model: p.Model, ReasoningEffort: p.Reasoning}
	if opts.Provider == "" {
		opts, err = runner.ResolveTextOptions()
		if err != nil {
			return "", err
		}
		opts.Provider = workflow.ResolveProvider("", opts.Provider)
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var id int64
	for {
		id, err = ledger.Reserve(p.RepoRoot, opts.Provider, opts.Model, time.Now())
		if !errors.Is(err, ErrCallSlots) {
			break
		}
		select {
		case <-bounded.Done():
			return "", bounded.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err != nil {
		return "", err
	}
	result, runErr := runner.RunProviderTextOptions(bounded, prompt, opts)
	status := "completed"
	if runErr != nil {
		status = "failed"
	}
	if err := ledger.Finish(id, status); err != nil {
		return "", err
	}
	return result, runErr
}
