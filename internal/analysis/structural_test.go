package analysis

import (
	"reflect"
	"testing"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/index"
)

// TestStructuralLinksIndexedMatchesScan is the contract between the two
// implementations. The indexed path is the one that runs in practice, so
// without this the fallback could rot unnoticed until someone hit a repo with
// an old index and got different answers from the same command.
func TestStructuralLinksIndexedMatchesScan(t *testing.T) {
	repo := indexRepoHelper(t)
	store, err := db.Open(repo)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index: %v", err)
	}

	repoRecord, err := store.Repo()
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	if !store.HasCodeIndex(repoRecord.ID) {
		t.Fatal("expected a content index after indexing")
	}

	targets := []string{
		"cli/app.go",
		"internalpkg/helper/helper.go",
		"main.go",
		"web/app.ts",
		"web/session.ts",
		"packages/app/src/feature.ts",
		"pkg/app.py",
		"pkg/helper.py",
	}

	indexed := make(map[string][]StructuralLink, len(targets))
	for _, target := range targets {
		links, err := StructuralLinks(store, target, 12)
		if err != nil {
			t.Fatalf("indexed links for %s: %v", target, err)
		}
		if len(links) == 0 {
			t.Fatalf("expected indexed links for %s", target)
		}
		indexed[target] = links
	}

	// Drop the content index to force the fallback, exactly as an index
	// written by an older Pallium would look.
	if _, err := store.DB().Exec(`DELETE FROM code_files WHERE repo_id = ?`, repoRecord.ID); err != nil {
		t.Fatalf("clear content index: %v", err)
	}
	if store.HasCodeIndex(repoRecord.ID) {
		t.Fatal("expected the content index to be gone")
	}

	for _, target := range targets {
		scanned, err := StructuralLinks(store, target, 12)
		if err != nil {
			t.Fatalf("scanned links for %s: %v", target, err)
		}
		if !reflect.DeepEqual(indexed[target], scanned) {
			t.Fatalf("links for %s differ.\nindexed: %#v\nscanned: %#v", target, indexed[target], scanned)
		}
	}
}
