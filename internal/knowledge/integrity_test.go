package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tszaks/pallium/internal/db"
)

func TestReadRejectsStaleAfterEdit(t *testing.T) {
	store, id, root := indexedRepo(t)
	if _, err := Build(store, id, root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	docs, _ := store.KnowledgeDocs(id)
	if err := Assess(store, docs); err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.Freshness != "current" {
			t.Fatalf("fresh doc: %+v", d)
		}
	}
	write(t, filepath.Join(root, "storage/store.go"), "package storage\nfunc Refresh() {}\n")
	if err := Assess(store, docs); err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.Freshness != "stale" || d.Verified {
			t.Fatalf("stale document trusted: %+v", d)
		}
	}
}

func TestModuleIdentityIsInjective(t *testing.T) {
	seen := map[string]string{}
	for _, dir := range []string{".", "root", "foo-bar", "foo/bar", "foo_bar", "Foo/bar", "foo%2Fbar"} {
		slug := slugForDir(dir)
		if prev, ok := seen[slug]; ok {
			t.Fatalf("%q collides with %q", dir, prev)
		}
		seen[slug] = dir
		if filepath.Base(slug) != slug {
			t.Fatal("slug is a path")
		}
	}
}

func TestWrongModuleAndReceiverCitationsRejected(t *testing.T) {
	idx := citationIndex{declarations: []db.CodeSymbol{{Path: "billing/a.go", Name: "ChargeCustomer", Receiver: "Billing"}}}
	files := map[string]struct{}{"cache/a.go": {}}
	if idx.hasModuleSymbol("Cache.ChargeCustomer", files) || idx.hasModuleSymbol("ChargeCustomer", files) {
		t.Fatal("wrong module accepted")
	}
	files["billing/a.go"] = struct{}{}
	if idx.hasModuleSymbol("Cache.ChargeCustomer", files) {
		t.Fatal("wrong receiver accepted")
	}
	if !idx.hasModuleSymbol("Billing.ChargeCustomer", files) {
		t.Fatal("correct identity rejected")
	}
}

type modifyingSynth struct{ root string }

func (s modifyingSynth) Synthesize(context.Context, string) (string, error) {
	return "{}", os.WriteFile(filepath.Join(s.root, "storage/store.go"), []byte("package storage\nfunc Changed() {}"), 0644)
}
func TestBuildRejectsSourceChangeDuringGeneration(t *testing.T) {
	store, id, root := indexedRepo(t)
	_, err := Build(store, id, root, BuildOptions{Synth: modifyingSynth{root}, Only: []string{"storage"}})
	if err == nil {
		t.Fatal("published changed source")
	}
	if _, found, _ := store.KnowledgeDoc(id, "storage"); found {
		t.Fatal("candidate published")
	}
}

func TestMaterializePreservesAuthoredPages(t *testing.T) {
	store, id, root := indexedRepo(t)
	dir := filepath.Join(root, "docs-out")
	os.MkdirAll(dir, 0755)
	p := filepath.Join(dir, "notes.md")
	os.WriteFile(p, []byte("my notes"), 0644)
	if _, err := Build(store, id, root, BuildOptions{Materialize: true, MaterializeTo: dir}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "my notes" {
		t.Fatal("authored content lost")
	}
}

func TestUntrackedSourcesRequireExplicitOptIn(t *testing.T) {
	store, id, root := indexedRepo(t)
	write(t, filepath.Join(root, "storage/private.go"), "package storage\nfunc PrivateUntracked() {}\n")
	if _, err := Build(store, id, root, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := Snapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "storage/private.go"), "package storage\nfunc ChangedUntracked() {}\n")
	after, err := Snapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("untracked file affected default snapshot")
	}
	write(t, filepath.Join(root, ".pallium/modules.json"), `{"include_untracked":["storage/private.go"]}`)
	after, err = Snapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("explicit opt-in not included")
	}
}
