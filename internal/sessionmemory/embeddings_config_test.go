package sessionmemory

import (
	"context"
	"testing"
)

func TestResolveEmbeddingSettings(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		wantProvider string
		wantBaseURL  string
		wantKey      string
	}{
		{
			name:         "defaults to openai",
			env:          map[string]string{},
			wantProvider: "openai",
			wantBaseURL:  "https://api.openai.com/v1",
			wantKey:      "",
		},
		{
			name:         "openai picks up OPENAI_API_KEY fallback",
			env:          map[string]string{"OPENAI_API_KEY": "sk-test-key"},
			wantProvider: "openai",
			wantBaseURL:  "https://api.openai.com/v1",
			wantKey:      "sk-test-key",
		},
		{
			name:         "ollama defaults to local endpoint with no key",
			env:          map[string]string{"PALLIUM_EMBED_PROVIDER": "ollama", "OPENAI_API_KEY": "sk-should-be-ignored"},
			wantProvider: "ollama",
			wantBaseURL:  "http://localhost:11434/v1",
			wantKey:      "",
		},
		{
			name: "custom OpenAI-compatible host with explicit key, trailing slash trimmed",
			env: map[string]string{
				"PALLIUM_EMBED_PROVIDER": "voyage",
				"PALLIUM_EMBED_BASE_URL": "https://api.voyageai.com/v1/",
				"PALLIUM_EMBED_API_KEY":  "pa-byo-key",
			},
			wantProvider: "voyage",
			wantBaseURL:  "https://api.voyageai.com/v1",
			wantKey:      "pa-byo-key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear the keys this test reasons about so the host environment can't leak in.
			for _, k := range []string{"PALLIUM_EMBED_PROVIDER", "PALLIUM_EMBED_BASE_URL", "PALLIUM_EMBED_API_KEY", "OPENAI_API_KEY", "OPENAI_ADMIN_API_KEY", "PALLIUM_EMBED_MODEL"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := resolveEmbeddingSettings()
			if got.provider != tc.wantProvider {
				t.Errorf("provider=%q want %q", got.provider, tc.wantProvider)
			}
			if got.baseURL != tc.wantBaseURL {
				t.Errorf("baseURL=%q want %q", got.baseURL, tc.wantBaseURL)
			}
			if got.apiKey != tc.wantKey {
				t.Errorf("apiKey=%q want %q", got.apiKey, tc.wantKey)
			}
		})
	}
}

func TestResolveEmbeddingModel(t *testing.T) {
	t.Setenv("PALLIUM_EMBED_MODEL", "")
	if got := resolveEmbeddingModel(""); got != DefaultEmbeddingModel {
		t.Errorf("empty model = %q, want default %q", got, DefaultEmbeddingModel)
	}

	t.Setenv("PALLIUM_EMBED_MODEL", "bge-m3")
	if got := resolveEmbeddingModel(""); got != "bge-m3" {
		t.Errorf("env model = %q, want bge-m3", got)
	}

	// An explicit override always wins over the environment default.
	if got := resolveEmbeddingModel("nomic-embed-text"); got != "nomic-embed-text" {
		t.Errorf("override model = %q, want nomic-embed-text", got)
	}
}

// Embeddings from different (provider, model) spaces must never be mixed in one similarity
// search: a query under provider B must not match vectors stored under provider A.
func TestSemanticSearchIsPartitionedByProvider(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("PALLIUM_EMBED_PROVIDER", "ollama")
	t.Setenv("PALLIUM_EMBED_MODEL", "bge-m3")

	originalEmbedTexts := embedTexts
	t.Cleanup(func() { embedTexts = originalEmbedTexts })
	embedTexts = func(ctx context.Context, model string, texts []string) ([][]float64, error) {
		vecs := make([][]float64, len(texts))
		for i := range texts {
			vecs[i] = []float64{1, 0.5}
		}
		return vecs, nil
	}

	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	now := "2026-06-10T12:00:00Z"
	if _, err := store.db.Exec(`INSERT INTO codex_sessions(id,machine,title,cwd,indexed_at,updated_at) VALUES(?,?,?,?,?,?)`, "s1", "test", "Title", "/tmp/repo", now, now); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO codex_session_chunks(id,session_id,chunk_index,kind,text,text_sha256) VALUES(?,?,?,?,?,?)`, "c1", "s1", 0, "message", "a relevant chunk", "sha-c1"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Embed under the ollama/bge-m3 space.
	embedded, err := Embed(context.Background(), "", 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if embedded != 1 {
		t.Fatalf("embedded=%d, want 1", embedded)
	}

	// Same space -> the chunk is found.
	hits, err := Semantic(context.Background(), "query", "", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("same-space search hits=%d, want 1", len(hits))
	}

	// Switch to the openai space -> the ollama-stored vector must not match.
	t.Setenv("PALLIUM_EMBED_PROVIDER", "openai")
	t.Setenv("PALLIUM_EMBED_MODEL", "text-embedding-3-small")
	crossSpace, err := Semantic(context.Background(), "query", "", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(crossSpace) != 0 {
		t.Fatalf("cross-space search hits=%d, want 0 (vectors must not mix across provider/model spaces)", len(crossSpace))
	}
}
