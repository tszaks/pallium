package sessionmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type EmbeddingConfig struct {
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	CredentialStore string `json:"credential_store,omitempty"`
}

type EmbeddingStatus struct {
	ConfigPath       string `json:"config_path"`
	Configured       bool   `json:"configured"`
	Provider         string `json:"provider"`
	ProviderSource   string `json:"provider_source"`
	BaseURL          string `json:"base_url"`
	BaseURLSource    string `json:"base_url_source"`
	Model            string `json:"model"`
	ModelSource      string `json:"model_source"`
	APIKeyConfigured bool   `json:"api_key_configured"`
	APIKeySource     string `json:"api_key_source,omitempty"`
	ConfigError      string `json:"config_error,omitempty"`
	CredentialError  string `json:"credential_error,omitempty"`
}

type EmbeddingProbeResult struct {
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	Dimension int    `json:"dimension"`
	Duration  string `json:"duration"`
	Healthy   bool   `json:"healthy"`
}

type embeddingSettings struct {
	provider        string
	baseURL         string
	model           string
	apiKey          string
	apiKeySource    string
	providerSource  string
	baseURLSource   string
	modelSource     string
	configPath      string
	configured      bool
	configError     error
	credentialError error
}

var embeddingProviderNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func DefaultEmbeddingConfigPath() string {
	if override := strings.TrimSpace(os.Getenv("PALLIUM_EMBED_CONFIG")); override != "" {
		if absolute, err := filepath.Abs(override); err == nil {
			return absolute
		}
		return override
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".pallium", "embedding.json")
	}
	return ".pallium-embedding.json"
}

func ConfigureEmbedding(config EmbeddingConfig) (EmbeddingStatus, error) {
	var err error
	config, err = normalizeEmbeddingConfig(config)
	if err != nil {
		return EmbeddingStatus{}, err
	}
	path := DefaultEmbeddingConfigPath()
	if err := writeEmbeddingConfig(path, config); err != nil {
		return EmbeddingStatus{}, err
	}
	status := ReadEmbeddingStatus()
	if status.ConfigError != "" {
		return status, fmt.Errorf("read saved embedding configuration: %s", status.ConfigError)
	}
	return status, nil
}

func ReadEmbeddingStatus() EmbeddingStatus {
	settings := resolveEmbeddingSettings()
	status := EmbeddingStatus{
		ConfigPath:       settings.configPath,
		Configured:       settings.configured,
		Provider:         settings.provider,
		ProviderSource:   settings.providerSource,
		BaseURL:          settings.baseURL,
		BaseURLSource:    settings.baseURLSource,
		Model:            settings.model,
		ModelSource:      settings.modelSource,
		APIKeyConfigured: settings.apiKey != "",
		APIKeySource:     settings.apiKeySource,
	}
	if settings.configError != nil {
		status.ConfigError = settings.configError.Error()
	}
	if settings.credentialError != nil {
		status.CredentialError = settings.credentialError.Error()
	}
	return status
}

func ProbeEmbedding(ctx context.Context) (EmbeddingProbeResult, error) {
	settings := resolveEmbeddingSettings()
	result := EmbeddingProbeResult{Provider: settings.provider, BaseURL: settings.baseURL, Model: settings.model}
	if settings.configError != nil {
		return result, settings.configError
	}
	if settings.credentialError != nil {
		return result, settings.credentialError
	}
	started := time.Now()
	vectors, err := embedTexts(ctx, settings.model, []string{"Pallium continuity search health check"})
	result.Duration = time.Since(started).Round(time.Millisecond).String()
	if err != nil {
		return result, err
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		return result, fmt.Errorf("embedding provider returned no vector")
	}
	result.Dimension = len(vectors[0])
	result.Healthy = true
	return result, nil
}

func ActiveEmbeddingModel() string { return resolveEmbeddingModel("") }

func resolveEmbeddingSettings() embeddingSettings {
	path := DefaultEmbeddingConfigPath()
	config, configured, configErr := readEmbeddingConfig(path)
	settings := embeddingSettings{configPath: path, configured: configured, configError: configErr}

	settings.provider, settings.providerSource = resolvedEmbeddingValue("PALLIUM_EMBED_PROVIDER", config.Provider, "openai")
	settings.provider = strings.ToLower(settings.provider)
	settings.model, settings.modelSource = resolvedEmbeddingValue("PALLIUM_EMBED_MODEL", config.Model, DefaultEmbeddingModel)
	settings.baseURL, settings.baseURLSource = resolvedEmbeddingValue("PALLIUM_EMBED_BASE_URL", config.BaseURL, defaultEmbeddingBaseURL(settings.provider))
	settings.baseURL = strings.TrimRight(settings.baseURL, "/")

	settings.apiKey = strings.TrimSpace(os.Getenv("PALLIUM_EMBED_API_KEY"))
	if settings.apiKey != "" {
		settings.apiKeySource = "environment:PALLIUM_EMBED_API_KEY"
	} else if config.CredentialStore == "keychain" {
		settings.apiKey, settings.credentialError = embeddingCredentialLookup(settings.provider)
		if settings.apiKey != "" {
			settings.apiKeySource = "keychain"
		}
	} else if settings.provider == "openai" {
		settings.apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		if settings.apiKey != "" {
			settings.apiKeySource = "environment:OPENAI_API_KEY"
		} else {
			settings.apiKey = strings.TrimSpace(os.Getenv("OPENAI_ADMIN_API_KEY"))
			if settings.apiKey != "" {
				settings.apiKeySource = "environment:OPENAI_ADMIN_API_KEY"
			}
		}
	}
	return settings
}

func resolvedEmbeddingValue(envName, configured, fallback string) (string, string) {
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return value, "environment"
	}
	if value := strings.TrimSpace(configured); value != "" {
		return value, "config"
	}
	return fallback, "default"
}

func defaultEmbeddingBaseURL(provider string) string {
	if strings.EqualFold(strings.TrimSpace(provider), "ollama") {
		return "http://127.0.0.1:11434/v1"
	}
	return "https://api.openai.com/v1"
}

func resolveEmbeddingModel(model string) string {
	if strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model)
	}
	return resolveEmbeddingSettings().model
}

func embeddingProvider() string { return resolveEmbeddingSettings().provider }

func readEmbeddingConfig(path string) (EmbeddingConfig, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return EmbeddingConfig{}, false, nil
		}
		return EmbeddingConfig{}, false, fmt.Errorf("stat embedding configuration: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return EmbeddingConfig{}, true, fmt.Errorf("embedding configuration must be a regular file: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return EmbeddingConfig{}, true, fmt.Errorf("read embedding configuration: %w", err)
	}
	var config EmbeddingConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return EmbeddingConfig{}, true, fmt.Errorf("decode embedding configuration: %w", err)
	}
	config, err = normalizeEmbeddingConfig(config)
	if err != nil {
		return EmbeddingConfig{}, true, fmt.Errorf("validate embedding configuration: %w", err)
	}
	return config, true, nil
}

func writeEmbeddingConfig(path string, config EmbeddingConfig) error {
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	directoryCreated := os.IsNotExist(statErr)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create embedding configuration directory: %w", err)
	}
	if directoryCreated || isPalliumConfigDirectory(dir) {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure embedding configuration directory: %w", err)
		}
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("embedding configuration must be a regular file: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat embedding configuration: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".embedding-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary embedding configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary embedding configuration: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(config); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write embedding configuration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync embedding configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close embedding configuration: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install embedding configuration: %w", err)
	}
	removeTemporary = false
	return nil
}

func isPalliumConfigDirectory(dir string) bool {
	home, err := os.UserHomeDir()
	return err == nil && filepath.Clean(dir) == filepath.Join(home, ".pallium")
}

func normalizeEmbeddingConfig(config EmbeddingConfig) (EmbeddingConfig, error) {
	config.Provider = strings.ToLower(strings.TrimSpace(config.Provider))
	config.Model = strings.TrimSpace(config.Model)
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.CredentialStore = strings.ToLower(strings.TrimSpace(config.CredentialStore))
	if config.Provider == "" {
		return EmbeddingConfig{}, fmt.Errorf("embedding provider is required")
	}
	if !embeddingProviderNamePattern.MatchString(config.Provider) {
		return EmbeddingConfig{}, fmt.Errorf("invalid embedding provider %q", config.Provider)
	}
	if config.Model == "" {
		return EmbeddingConfig{}, fmt.Errorf("embedding model is required")
	}
	if config.CredentialStore != "" && config.CredentialStore != "keychain" {
		return EmbeddingConfig{}, fmt.Errorf("unsupported embedding credential store %q; use keychain", config.CredentialStore)
	}
	if config.BaseURL == "" {
		config.BaseURL = defaultEmbeddingBaseURL(config.Provider)
	}
	if err := validateEmbeddingBaseURL(config.BaseURL); err != nil {
		return EmbeddingConfig{}, err
	}
	return config, nil
}

func validateEmbeddingBaseURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("invalid embedding base URL %q; use an http or https URL", value)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("embedding base URL cannot contain credentials, a query, or a fragment")
	}
	return nil
}
