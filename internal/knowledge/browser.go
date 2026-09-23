package knowledge

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/index"
)

//go:embed web/index.html
var browserHTML string

type Browser struct{ Root, Token, Host string }

func NewBrowser(root, host string) (*Browser, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Browser{root, hex.EncodeToString(b), host}, nil
}
func (b *Browser) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", b.home)
	mux.HandleFunc("/api/status", b.status)
	mux.HandleFunc("/api/search", b.search)
	mux.HandleFunc("/api/doc", b.doc)
	mux.HandleFunc("/api/source", b.source)
	mux.HandleFunc("/api/control", b.control)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != b.Host {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+b.Host {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'nonce-"+b.Token+"'; style-src 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		if r.Method != "GET" && r.URL.Path != "/api/control" {
			http.Error(w, "method not allowed", 405)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (b *Browser) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, err := template.New("page").Parse(browserHTML)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = t.Execute(w, b)
}
func writeBrowserJSON(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
func (b *Browser) withStore(w http.ResponseWriter, fn func(*db.Store, int64) (any, error)) {
	s, err := index.OpenStore(b.Root)
	if err != nil {
		writeBrowserJSON(w, nil, err)
		return
	}
	defer s.Close()
	repo, err := s.Repo()
	if err != nil {
		writeBrowserJSON(w, nil, err)
		return
	}
	v, err := fn(s, repo.ID)
	writeBrowserJSON(w, v, err)
}
func (b *Browser) status(w http.ResponseWriter, r *http.Request) {
	b.withStore(w, func(s *db.Store, id int64) (any, error) {
		docs, err := s.KnowledgeDocs(id)
		if err != nil {
			return nil, err
		}
		if err := Assess(s, docs); err != nil {
			return nil, err
		}
		hits := []Hit{}
		for _, d := range docs {
			hits = append(hits, Hit{Slug: d.Slug, Title: d.Title, Kind: d.Kind, Snippet: truncate(d.Summary, 240), Freshness: d.Freshness, State: d.Evidence.State, Paths: capStrings(d.CitedPaths, 4), SourceCommit: d.SourceCommit})
		}
		m, err := OpenMaintenance()
		if err != nil {
			return nil, err
		}
		defer m.Close()
		status, err := m.Status()
		if err != nil {
			return nil, err
		}
		return map[string]any{"root": b.Root, "documents": hits, "maintenance": status, "version": ContractVersion}, nil
	})
}
func (b *Browser) search(w http.ResponseWriter, r *http.Request) {
	b.withStore(w, func(s *db.Store, id int64) (any, error) { return Search(s, id, r.URL.Query().Get("q"), 5, 8192) })
}
func (b *Browser) doc(w http.ResponseWriter, r *http.Request) {
	b.withStore(w, func(s *db.Store, id int64) (any, error) {
		d, found, err := s.KnowledgeDoc(id, r.URL.Query().Get("slug"))
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("document not found")
		}
		docs := []db.KnowledgeDoc{d}
		if err := Assess(s, docs); err != nil {
			return nil, err
		}
		return docs[0], nil
	})
}
func (b *Browser) source(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	full, err := filepath.EvalSymlinks(filepath.Join(b.Root, path))
	if err != nil {
		writeBrowserJSON(w, nil, err)
		return
	}
	root, err := filepath.EvalSymlinks(b.Root)
	if err != nil {
		writeBrowserJSON(w, nil, err)
		return
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(path) {
		http.Error(w, "outside repository", 403)
		return
	}
	// Only indexed source paths may be read, never arbitrary local credentials.
	b.withStore(w, func(s *db.Store, id int64) (any, error) {
		paths, err := s.CodeFileSHAs(id)
		if err != nil {
			return nil, err
		}
		if _, ok := paths[filepath.ToSlash(rel)]; !ok {
			return nil, fmt.Errorf("source is not indexed")
		}
		info, err := os.Stat(full)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 512*1024 {
			return nil, fmt.Errorf("source unavailable or too large")
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		if len(data) > 64000 {
			data = append(data[:64000], []byte("\n[source truncated at 64 KB]")...)
		}
		return map[string]string{"path": rel, "text": string(data)}, nil
	})
}
func (b *Browser) control(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.Header.Get("X-Pallium-Token") != b.Token || r.Header.Get("Origin") != "http://"+b.Host {
		http.Error(w, "invalid token or origin", 403)
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	m, err := OpenMaintenance()
	if err != nil {
		writeBrowserJSON(w, nil, err)
		return
	}
	defer m.Close()
	switch body.Action {
	case "pause":
		err = m.Enable(b.Root, false)
	case "resume":
		err = m.Enable(b.Root, true)
	case "retry":
		err = m.Retry(b.Root)
	default:
		http.Error(w, "unknown action", 400)
		return
	}
	writeBrowserJSON(w, map[string]string{"action": body.Action}, err)
}
func ServeBrowser(root string, port int) (net.Listener, *http.Server, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, nil, err
	}
	b, err := NewBrowser(root, listener.Addr().String())
	if err != nil {
		listener.Close()
		return nil, nil, err
	}
	server := &http.Server{Handler: b.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	return listener, server, nil
}
