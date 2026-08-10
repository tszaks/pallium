package sessionmemory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pallium-sessionmemory-tests-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("PALLIUM_EMBED_CONFIG", filepath.Join(dir, "embedding.json")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
