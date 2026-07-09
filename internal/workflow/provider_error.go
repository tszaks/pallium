package workflow

import (
	"fmt"
	"strings"
)

// providerErrorPatterns are case-insensitive substrings that mark a line of
// provider stdout/stderr as the real reason for a failure. A worker process
// that hits a quota wall or an auth failure often keeps running afterward
// (e.g. waiting on stdin) until Pallium's own timeout kills it, so the exec
// error ends up a generic "signal: killed" with the actual cause buried
// earlier in the output.
var providerErrorPatterns = []string{
	"usage limit",
	"rate limit",
	"quota",
	"error:",
	"not authorized",
	"unauthorized",
	"try again",
}

// meaningfulProviderErrorLine scans a provider's combined stdout+stderr for
// the first line matching providerErrorPatterns.
func meaningfulProviderErrorLine(combinedOutput string) (string, bool) {
	for _, line := range strings.Split(combinedOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		for _, pattern := range providerErrorPatterns {
			if strings.Contains(lower, pattern) {
				return line, true
			}
		}
	}
	return "", false
}

// wrapProviderCommandError leads a provider command failure with the most
// meaningful line found in its combined stdout+stderr, if any, so a real
// cause like a quota wall surfaces instead of being hidden behind a generic
// exec error. The original error (with whatever output it already embeds)
// is kept via %w.
func wrapProviderCommandError(err error, combinedOutput string) error {
	if err == nil {
		return nil
	}
	if line, ok := meaningfulProviderErrorLine(combinedOutput); ok {
		return fmt.Errorf("%s: %w", line, err)
	}
	return err
}
