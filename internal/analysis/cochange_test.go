package analysis

import (
	"testing"

	"github.com/tszaks/pallium/internal/index"
)

func TestNeighbors(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	neighbors, err := Neighbors(store, "main.go", 5)
	if err != nil {
		t.Fatalf("Neighbors failed: %v", err)
	}

	if len(neighbors) == 0 {
		t.Fatalf("expected neighbors for main.go")
	}
}

func indexRepo(t *testing.T) string {
	t.Helper()
	return indexRepoHelper(t)
}
