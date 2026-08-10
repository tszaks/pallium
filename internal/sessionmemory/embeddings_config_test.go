package sessionmemory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
			wantBaseURL:  "http://127.0.0.1:11434/v1",
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
			t.Setenv("PALLIUM_EMBED_CONFIG", filepath.Join(t.TempDir(), "embedding.json"))
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
	t.Setenv("PALLIUM_EMBED_CONFIG", filepath.Join(t.TempDir(), "embedding.json"))
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

func TestConfigureEmbeddingPersistsSecureConfigWithEnvironmentPrecedence(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".pallium", "embedding.json")
	t.Setenv("PALLIUM_EMBED_CONFIG", configPath)
	for _, key := range []string{"PALLIUM_EMBED_PROVIDER", "PALLIUM_EMBED_BASE_URL", "PALLIUM_EMBED_MODEL", "PALLIUM_EMBED_API_KEY", "OPENAI_API_KEY", "OPENAI_ADMIN_API_KEY"} {
		t.Setenv(key, "")
	}
	status, err := ConfigureEmbedding(EmbeddingConfig{Provider: "ollama", Model: "embeddinggemma"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Provider != "ollama" || status.Model != "embeddinggemma" || status.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Fatalf("unexpected saved status: %+v", status)
	}
	if status.ProviderSource != "config" || status.ModelSource != "config" || status.BaseURLSource != "config" {
		t.Fatalf("saved config did not become active: %+v", status)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(configPath))
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config directory is not 0700: info=%v err=%v", dirInfo, err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "key") || strings.Contains(string(raw), "token") {
		t.Fatalf("config persisted a credential field: %s", raw)
	}

	t.Setenv("PALLIUM_EMBED_PROVIDER", "openai")
	t.Setenv("PALLIUM_EMBED_MODEL", "text-embedding-3-small")
	t.Setenv("PALLIUM_EMBED_BASE_URL", "https://example.test/v1")
	overridden := ReadEmbeddingStatus()
	if overridden.Provider != "openai" || overridden.ProviderSource != "environment" || overridden.ModelSource != "environment" || overridden.BaseURLSource != "environment" {
		t.Fatalf("environment did not override saved config: %+v", overridden)
	}
}

func TestKeychainCredentialOverridesAmbientOpenAIKey(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".pallium", "embedding.json")
	t.Setenv("PALLIUM_EMBED_CONFIG", configPath)
	t.Setenv("PALLIUM_EMBED_PROVIDER", "")
	t.Setenv("PALLIUM_EMBED_BASE_URL", "")
	t.Setenv("PALLIUM_EMBED_MODEL", "")
	t.Setenv("PALLIUM_EMBED_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "ambient-old-key")
	t.Setenv("OPENAI_ADMIN_API_KEY", "")

	originalLookup := embeddingCredentialLookup
	t.Cleanup(func() { embeddingCredentialLookup = originalLookup })
	embeddingCredentialLookup = func(provider string) (string, error) {
		if provider != "openai" {
			t.Fatalf("provider=%q, want openai", provider)
		}
		return "keychain-current-key", nil
	}

	status, err := ConfigureEmbedding(EmbeddingConfig{
		Provider:        "openai",
		Model:           DefaultEmbeddingModel,
		CredentialStore: "keychain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !status.APIKeyConfigured || status.APIKeySource != "keychain" {
		t.Fatalf("unexpected credential status: %+v", status)
	}
	settings := resolveEmbeddingSettings()
	if settings.apiKey != "keychain-current-key" {
		t.Fatal("resolved ambient key instead of keychain credential")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "keychain-current-key") || strings.Contains(string(raw), "ambient-old-key") {
		t.Fatal("embedding configuration persisted a credential")
	}
}

func TestExplicitPalliumKeyOverridesConfiguredKeychain(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "embedding.json")
	t.Setenv("PALLIUM_EMBED_CONFIG", configPath)
	t.Setenv("PALLIUM_EMBED_API_KEY", "one-run-key")
	if err := writeEmbeddingConfig(configPath, EmbeddingConfig{
		Provider:        "openai",
		Model:           DefaultEmbeddingModel,
		BaseURL:         defaultEmbeddingBaseURL("openai"),
		CredentialStore: "keychain",
	}); err != nil {
		t.Fatal(err)
	}

	originalLookup := embeddingCredentialLookup
	t.Cleanup(func() { embeddingCredentialLookup = originalLookup })
	embeddingCredentialLookup = func(string) (string, error) {
		t.Fatal("keychain should not be read when PALLIUM_EMBED_API_KEY is set")
		return "", nil
	}

	settings := resolveEmbeddingSettings()
	if settings.apiKey != "one-run-key" || settings.apiKeySource != "environment:PALLIUM_EMBED_API_KEY" {
		t.Fatalf("unexpected explicit override: key=%q source=%q", settings.apiKey, settings.apiKeySource)
	}
}

func TestEmbeddingCredentialLookupFailureStopsBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	t.Cleanup(server.Close)

	configPath := filepath.Join(t.TempDir(), "embedding.json")
	t.Setenv("PALLIUM_EMBED_CONFIG", configPath)
	for _, key := range []string{"PALLIUM_EMBED_PROVIDER", "PALLIUM_EMBED_BASE_URL", "PALLIUM_EMBED_MODEL", "PALLIUM_EMBED_API_KEY", "OPENAI_API_KEY", "OPENAI_ADMIN_API_KEY"} {
		t.Setenv(key, "")
	}
	if err := writeEmbeddingConfig(configPath, EmbeddingConfig{
		Provider:        "openai",
		Model:           DefaultEmbeddingModel,
		BaseURL:         server.URL + "/v1",
		CredentialStore: "keychain",
	}); err != nil {
		t.Fatal(err)
	}
	originalLookup := embeddingCredentialLookup
	t.Cleanup(func() { embeddingCredentialLookup = originalLookup })
	embeddingCredentialLookup = func(string) (string, error) {
		return "", errors.New("keychain credential unavailable")
	}

	_, err := openAICompatibleEmbeddings(context.Background(), DefaultEmbeddingModel, []string{"probe"})
	if err == nil || !strings.Contains(err.Error(), "keychain credential unavailable") {
		t.Fatalf("error=%v, want keychain lookup failure", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requests=%d, want no unauthenticated request", requests.Load())
	}
}

func TestEmbeddingReadyReportsCredentialLookupFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "embedding.json")
	t.Setenv("PALLIUM_EMBED_CONFIG", configPath)
	for _, key := range []string{"PALLIUM_EMBED_PROVIDER", "PALLIUM_EMBED_BASE_URL", "PALLIUM_EMBED_MODEL", "PALLIUM_EMBED_API_KEY", "OPENAI_API_KEY", "OPENAI_ADMIN_API_KEY"} {
		t.Setenv(key, "")
	}
	if err := writeEmbeddingConfig(configPath, EmbeddingConfig{
		Provider:        "openai",
		Model:           DefaultEmbeddingModel,
		CredentialStore: "keychain",
	}); err != nil {
		t.Fatal(err)
	}
	originalLookup := embeddingCredentialLookup
	t.Cleanup(func() { embeddingCredentialLookup = originalLookup })
	embeddingCredentialLookup = func(string) (string, error) {
		return "", errors.New("keychain credential unavailable")
	}

	ready, reason := embeddingReady()
	if ready || !strings.Contains(reason, "keychain credential unavailable") || strings.Contains(reason, "no OpenAI key") {
		t.Fatalf("ready=%v reason=%q, want the credential lookup failure", ready, reason)
	}
}

func TestConfigureEmbeddingRejectsUnsafeBaseURL(t *testing.T) {
	t.Setenv("PALLIUM_EMBED_CONFIG", filepath.Join(t.TempDir(), "embedding.json"))
	_, err := ConfigureEmbedding(EmbeddingConfig{Provider: "custom", Model: "model", BaseURL: "https://user:secret@example.test/v1"})
	if err == nil || !strings.Contains(err.Error(), "cannot contain credentials") {
		t.Fatalf("error=%v, want credential rejection", err)
	}
}

func TestConfigureEmbeddingRejectsUnknownCredentialStore(t *testing.T) {
	t.Setenv("PALLIUM_EMBED_CONFIG", filepath.Join(t.TempDir(), "embedding.json"))
	_, err := ConfigureEmbedding(EmbeddingConfig{Provider: "openai", Model: DefaultEmbeddingModel, CredentialStore: "plaintext"})
	if err == nil || !strings.Contains(err.Error(), "unsupported embedding credential store") {
		t.Fatalf("error=%v, want credential store rejection", err)
	}
}

func TestConfigureEmbeddingDoesNotChmodUnrelatedExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALLIUM_EMBED_CONFIG", filepath.Join(dir, "embedding.json"))
	if _, err := ConfigureEmbedding(EmbeddingConfig{Provider: "ollama", Model: "embeddinggemma"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("unrelated directory mode=%o, want unchanged 755", info.Mode().Perm())
	}
}

func TestProbeEmbeddingReportsResolvedVectorSpace(t *testing.T) {
	t.Setenv("PALLIUM_EMBED_CONFIG", filepath.Join(t.TempDir(), "embedding.json"))
	t.Setenv("PALLIUM_EMBED_PROVIDER", "test-local")
	t.Setenv("PALLIUM_EMBED_BASE_URL", "http://127.0.0.1:9999/v1")
	t.Setenv("PALLIUM_EMBED_MODEL", "test-model")
	originalEmbedTexts := embedTexts
	t.Cleanup(func() { embedTexts = originalEmbedTexts })
	embedTexts = func(context.Context, string, []string) ([][]float64, error) {
		return [][]float64{{1, 2, 3}}, nil
	}
	result, err := ProbeEmbedding(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Healthy || result.Provider != "test-local" || result.Model != "test-model" || result.Dimension != 3 {
		t.Fatalf("unexpected probe: %+v", result)
	}
}

func TestEmbeddingCreditExhaustionFailsWithoutRetrying(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"insufficient_quota","code":"credit_balance_exhausted","message":"Add credits."}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("PALLIUM_EMBED_PROVIDER", "openai")
	t.Setenv("PALLIUM_EMBED_BASE_URL", server.URL)
	t.Setenv("PALLIUM_EMBED_API_KEY", "test-key")

	_, err := openAICompatibleEmbeddings(context.Background(), DefaultEmbeddingModel, []string{"probe"})
	if err == nil || !strings.Contains(err.Error(), "credit_balance_exhausted") {
		t.Fatalf("error=%v, want actionable credit exhaustion", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d, want one non-retried request", requests.Load())
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
