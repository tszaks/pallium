//go:build !darwin

package sessionmemory

import "fmt"

func readPlatformEmbeddingCredential(provider string) (string, error) {
	return "", fmt.Errorf("the keychain embedding credential store is only available on macOS; configure PALLIUM_EMBED_API_KEY for %s", provider)
}
