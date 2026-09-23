package knowledge

import (
	"github.com/tszaks/pallium/internal/index"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserRejectsForeignHostsOriginsAndUnauthenticatedWrites(t *testing.T) {
	b, err := NewBrowser(t.TempDir(), "127.0.0.1:8766")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, host, origin, token string
		want                              int
	}{
		{"GET", "/", "evil.test", "", "", 403},
		{"GET", "/api/status", "127.0.0.1:8766", "https://evil.test", "", 403},
		{"POST", "/api/control", "127.0.0.1:8766", "http://127.0.0.1:8766", "", 403},
		{"GET", "/api/control", "127.0.0.1:8766", "", "", 405},
		{"GET", "/", "127.0.0.1:8766", "", "", 200},
	} {
		r := httptest.NewRequest(test.method, "http://"+test.host+test.path, strings.NewReader(`{"action":"pause"}`))
		r.Host = test.host
		r.Header.Set("Origin", test.origin)
		r.Header.Set("X-Pallium-Token", test.token)
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("%+v got %d: %s", test, w.Code, w.Body.String())
		}
	}
}
func TestDecisionsSurviveIndexAndExposeConflicts(t *testing.T) {
	store, id, _ := indexedRepo(t)
	a, err := LinkDecision(store, id, "Storage", "Use SQLite", "ADR-1", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LinkDecision(store, id, "Storage", "Use Postgres", "ADR-2", "")
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ = store.KnowledgeDoc(id, a.Slug)
	if a.Evidence.State != "conflicting_intent" || b.Evidence.State != "conflicting_intent" {
		t.Fatal("conflict hidden")
	}
	c, err := LinkDecision(store, id, "Storage", "Use SQLite with WAL", "ADR-3", b.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ResetRepoData(id); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.KnowledgeDoc(id, c.Slug); !found {
		t.Fatal("authored decision lost on reindex")
	}
	b, _, _ = store.KnowledgeDoc(id, b.Slug)
	if b.Evidence.State != "superseded" {
		t.Fatal("supersession lost")
	}
}

func TestBrowserSourceConfinesIndexedPathsAtOpenTime(t *testing.T) {
	store, _, root := indexedRepo(t)
	store.Close()
	store, err := index.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.New(store).Run(); err != nil {
		t.Fatal(err)
	}
	store.Close()
	b, err := NewBrowser(root, "127.0.0.1:8766")
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://"+b.Host+"/api/source?path="+url.QueryEscape(path), nil)
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, r)
		return w
	}
	if w := read("storage/store.go"); w.Code != 200 || !strings.Contains(w.Body.String(), "Store owns") {
		t.Fatalf("indexed read: %d %s", w.Code, w.Body)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("outside-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret.txt", secret, ".git/config"} {
		if w := read(path); w.Code == 200 || strings.Contains(w.Body.String(), "outside-secret") {
			t.Fatalf("unexpected read: %s", path)
		}
	}
	// The index still names a legitimate file, but it has since become a symlink.
	target := filepath.Join(root, "storage/store.go")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, target); err != nil {
		t.Fatal(err)
	}
	if w := read("storage/store.go"); w.Code == 200 || strings.Contains(w.Body.String(), "outside-secret") {
		t.Fatalf("symlink escaped: %s", w.Body)
	}
}
