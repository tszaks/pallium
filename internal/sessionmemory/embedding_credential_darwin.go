//go:build darwin

package sessionmemory

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

func readPlatformEmbeddingCredential(provider string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	command := exec.Command("/usr/bin/security", "find-generic-password", "-a", provider, "-s", embeddingKeychainService, "-w")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("read %s embedding credential from macOS Keychain: %s", provider, detail)
	}
	credential := strings.TrimSpace(string(output))
	if credential == "" {
		return "", fmt.Errorf("macOS Keychain returned an empty %s embedding credential", provider)
	}
	return credential, nil
}
