package knowledge

import (
	"context"
	"os"
	"strings"

	"github.com/tszaks/pallium/internal/workflow"
)

// ProviderSynthesizer runs synthesis through Pallium's existing provider
// layer, so a knowledge build uses whatever agent CLI the user already has
// configured, with the same resolution a workflow agent gets. No new
// credential, no new SDK, no network code in this package.
type ProviderSynthesizer struct {
	CodexBinary string
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
	return runner.RunProviderText(ctx, prompt)
}
