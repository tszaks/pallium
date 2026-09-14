package workflow

import (
	"fmt"
	"os"
	"strings"
)

// ValidateReasoningEffort validates the CLI surface, not the provider's API.
// An empty effort deliberately preserves the provider's configured default.
func ValidateReasoningEffort(provider, model, effort string) error {
	if effort == "" {
		return nil
	}
	var allowed []string
	switch provider {
	case "codex":
		allowed = []string{"low", "medium", "high", "xhigh"}
		if model == "gpt-6-astra" || strings.HasPrefix(model, "gpt-5.6-") || model == "gpt-daybreak-blue-latest" {
			allowed = append(allowed, "max")
		}
	case "claude":
		if strings.Contains(model, "haiku") {
			return fmt.Errorf("model %q does not support reasoning effort", model)
		}
		allowed = []string{"low", "medium", "high"}
		if strings.Contains(model, "-5") || strings.Contains(model, "opus-4-7") || strings.Contains(model, "opus-4-8") {
			allowed = append(allowed, "xhigh", "max")
		} else if strings.Contains(model, "4-6") {
			allowed = append(allowed, "max")
		}
	default:
		if strings.TrimSpace(os.Getenv(providerCommandEnvName(provider))) == "" {
			return fmt.Errorf("provider %q has no verified reasoning-effort adapter", provider)
		}
		if strings.TrimSpace(effort) != effort || strings.ContainsAny(effort, " \t\r\n") {
			return fmt.Errorf("configured provider %q reasoning effort must be one non-whitespace token", provider)
		}
		return nil
	}
	for _, value := range allowed {
		if effort == value {
			return nil
		}
	}
	return fmt.Errorf("unsupported reasoning effort %q for %s/%s (supported: %s)", effort, provider, model, strings.Join(allowed, ", "))
}
