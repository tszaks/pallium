package knowledge

import (
	"net/http/httptest"
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
