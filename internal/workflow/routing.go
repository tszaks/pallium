package workflow

import (
	"encoding/json"
	"fmt"
	"github.com/tszaks/pallium/internal/routing"
	"os"
	"os/exec"
	"strings"
)

// resolveRouting applies only an operator-provided policy. A missing default
// file preserves existing behavior; a missing explicit file is an error.
func (r *Runner) resolveRouting(opts AgentOptions, mode string) (AgentOptions, string, error) {
	path := routing.ConfigPath(r.Run.CWD)
	c, err := routing.Load(path)
	if os.IsNotExist(err) && os.Getenv("PALLIUM_ROUTING_CONFIG") == "" {
		if opts.Model == "auto" {
			return opts, "", fmt.Errorf("model auto requires a routing config; run pallium route models init")
		}
		return opts, "", nil
	}
	if err != nil {
		return opts, "", err
	}
	// Environment provider pins are operator intent, too.
	provider := normalizeProvider(opts.Provider)
	if provider == "" {
		provider = normalizeProvider(os.Getenv("PALLIUM_WORKFLOW_PROVIDER"))
	}
	networkRequired := opts.Network && r.Run.AllowNetwork
	d, err := c.Choose(routing.Request{VerificationRetry: opts.verificationRetry, Provider: provider, Model: opts.Model, Effort: opts.ReasoningEffort, TaskClass: opts.TaskClass, Mode: mode, Network: networkRequired}, func(provider string) bool {
		return provider == "codex" && r.AssumeCodexAvailable || ProviderAvailableWithNetwork(provider, r.CodexBinary, networkRequired)
	})
	if err != nil {
		return opts, "", err
	}
	if d.Recommended != nil {
		if err := ValidateReasoningEffort(d.Recommended.Provider, d.Recommended.Model, d.Recommended.Effort); err != nil {
			return opts, "", err
		}
	}
	if c.Mode != "off" {
		// Shadow execution uses the original provider but is still constrained.
		effectiveProvider := ResolveProvider("", d.Selected.Provider)
		allowed := false
		for _, p := range c.AllowedProviders {
			if p == effectiveProvider {
				allowed = true
			}
		}
		if !allowed {
			return opts, "", fmt.Errorf("effective provider %q is not allowed by routing policy", effectiveProvider)
		}
		if networkRequired && effectiveProvider == "claude" && strings.TrimSpace(os.Getenv(providerCommandEnvName("claude"))) == "" {
			return opts, "", fmt.Errorf("built-in claude provider cannot satisfy required network access; configure %s", providerCommandEnvName("claude"))
		}
	}
	opts.Provider = d.Selected.Provider
	opts.Model = d.Selected.Model
	opts.ReasoningEffort = d.Selected.Effort
	raw, _ := json.Marshal(d)
	return opts, string(raw), nil
}

// ProviderAvailable checks executable wiring only; authentication remains an
// operator declaration until an actual invocation succeeds.
func ProviderAvailable(provider, codexBinary string) bool {
	return ProviderAvailableWithNetwork(provider, codexBinary, false)
}

func ProviderAvailableWithNetwork(provider, codexBinary string, networkRequired bool) bool {
	if provider != "codex" && os.Getenv(providerCommandEnvName(provider)) != "" {
		return true
	}
	if provider == "claude" && networkRequired {
		return false
	}
	binary := provider
	if provider == "codex" && codexBinary != "" {
		binary = codexBinary
	}
	_, err := exec.LookPath(binary)
	return err == nil
}

// enforceTeamProviderPolicy rechecks a stored teammate against current policy
// without changing the model or effort of its native provider session.
func enforceTeamProviderPolicy(cwd, provider string) error {
	c, err := routing.Load(routing.ConfigPath(cwd))
	if os.IsNotExist(err) && os.Getenv("PALLIUM_ROUTING_CONFIG") == "" {
		return nil
	}
	if err != nil {
		return err
	}
	if c.Mode == "off" {
		return nil
	}
	for _, allowed := range c.AllowedProviders {
		if provider == allowed {
			return nil
		}
	}
	return fmt.Errorf("team provider %q is not allowed by routing policy", provider)
}
