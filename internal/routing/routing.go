// Package routing selects an execution configuration from an operator-owned
// catalog. It does not launch models, expand permissions, or infer credentials.
package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

type Candidate struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Effort   string   `json:"reasoning_effort"`
	Enabled  bool     `json:"enabled"`
	Network  bool     `json:"network"`
	Modes    []string `json:"modes"`
	Evidence []string `json:"evidence,omitempty"`
}

type Config struct {
	Escalations      map[string][]string `json:"escalations,omitempty"`
	Version          int                 `json:"version"`
	Mode             string              `json:"mode"`
	AllowedProviders []string            `json:"allowed_providers"`
	Default          string              `json:"default"`
	Candidates       []Candidate         `json:"candidates"`
	Rules            map[string]string   `json:"rules"`
}

type Request struct {
	VerificationRetry int    `json:"verification_retry,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Model             string `json:"model,omitempty"`
	Effort            string `json:"reasoning_effort,omitempty"`
	TaskClass         string `json:"task_class,omitempty"`
	Mode              string `json:"mode,omitempty"`
	Network           bool   `json:"network,omitempty"`
}

type Decision struct {
	Mode        string     `json:"mode"`
	PolicyHash  string     `json:"policy_hash"`
	Requested   Request    `json:"requested"`
	Recommended *Candidate `json:"recommended,omitempty"`
	Selected    Request    `json:"selected"`
	Reason      string     `json:"reason"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return nil, fmt.Errorf("routing config: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("routing config must contain exactly one JSON object")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ConfigPath uses an explicit operator path or a repository-local config.
func ConfigPath(repo string) string {
	if path := os.Getenv("PALLIUM_ROUTING_CONFIG"); path != "" {
		return path
	}
	return filepath.Join(repo, ".pallium", "routing.json")
}

func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported routing version %d", c.Version)
	}
	if !slices.Contains([]string{"off", "shadow", "auto"}, c.Mode) {
		return fmt.Errorf("routing mode must be off, shadow, or auto")
	}
	if len(c.AllowedProviders) == 0 {
		return fmt.Errorf("routing allowed_providers must be explicit")
	}
	seen := map[string]bool{}
	for _, candidate := range c.Candidates {
		if candidate.ID == "" || candidate.Provider == "" || candidate.Model == "" || candidate.Effort == "" || len(candidate.Modes) == 0 {
			return fmt.Errorf("routing candidates require id, provider, model, reasoning_effort, and modes")
		}
		if seen[candidate.ID] {
			return fmt.Errorf("duplicate routing candidate %q", candidate.ID)
		}
		seen[candidate.ID] = true
		for _, mode := range candidate.Modes {
			if !slices.Contains([]string{"read-only", "edit", "check", "test"}, mode) {
				return fmt.Errorf("unsupported candidate mode %q", mode)
			}
		}
	}
	if !seen[c.Default] {
		return fmt.Errorf("routing default %q is not a candidate", c.Default)
	}
	for class, id := range c.Rules {
		if class == "" || !seen[id] {
			return fmt.Errorf("invalid routing rule %q -> %q", class, id)
		}
	}
	for class, chain := range c.Escalations {
		if class == "" || len(chain) > 3 {
			return fmt.Errorf("escalation chains require a task class and at most three steps")
		}
		for _, id := range chain {
			if !seen[id] {
				return fmt.Errorf("unknown escalation candidate %q", id)
			}
		}
	}
	return nil
}

func (c Config) Hash() string {
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Choose preserves every explicit model/effort pin. The caller supplies actual
// executable availability; enabled is the operator's access declaration, not
// an authentication probe. Unknown task classes use the conservative default.
func (c Config) Choose(req Request, available func(string) bool) (Decision, error) {
	d := Decision{Mode: c.Mode, PolicyHash: c.Hash(), Requested: req, Selected: req}
	if err := c.Validate(); err != nil {
		return d, err
	}
	if c.Mode == "off" {
		if req.Model == "auto" {
			return d, fmt.Errorf("model auto requires shadow or auto routing mode")
		}
		d.Reason = "routing disabled"
		return d, nil
	}
	if req.Provider != "" && !slices.Contains(c.AllowedProviders, req.Provider) {
		return d, fmt.Errorf("provider %q is not allowed by routing policy", req.Provider)
	}
	if req.Model != "" && req.Model != "auto" || req.Effort != "" {
		if req.Model == "auto" {
			d.Selected.Model = ""
		}
		d.Reason = "explicit model or reasoning effort preserved"
		return d, nil
	}
	id := c.Default
	d.Reason = "conservative default for uncalibrated task class"
	if rule, ok := c.Rules[req.TaskClass]; ok {
		id = rule
		d.Reason = "configured rule for task class " + req.TaskClass
	}
	if req.VerificationRetry < 0 {
		return d, fmt.Errorf("verification retry cannot be negative")
	}
	if chain, ok := c.Escalations[req.TaskClass]; ok && req.VerificationRetry > 0 {
		if req.VerificationRetry > len(chain) {
			return d, fmt.Errorf("routing escalation exhausted for task class %q", req.TaskClass)
		}
		id = chain[req.VerificationRetry-1]
		d.Reason = fmt.Sprintf("configured escalation after verification retry %d", req.VerificationRetry)
	}
	eligible := func(p Candidate) bool {
		return p.Enabled && slices.Contains(c.AllowedProviders, p.Provider) && (req.Provider == "" || req.Provider == p.Provider) && slices.Contains(p.Modes, req.Mode) && (!req.Network || p.Network) && available(p.Provider)
	}
	for _, p := range c.Candidates {
		if p.ID == id && eligible(p) {
			candidate := p
			d.Recommended = &candidate
			break
		}
	}
	if d.Recommended == nil && id != c.Default {
		for _, p := range c.Candidates {
			if p.ID == c.Default && eligible(p) {
				candidate := p
				d.Recommended = &candidate
				d.Reason = "rule candidate unavailable; conservative default"
				break
			}
		}
	}
	if d.Recommended == nil {
		if c.Mode == "shadow" {
			d.Reason = "no eligible recommendation; shadow execution unchanged"
			if d.Selected.Model == "auto" {
				d.Selected.Model = ""
			}
			return d, nil
		}
		return d, fmt.Errorf("no eligible routing candidate for provider %q, mode %q, network %t", req.Provider, req.Mode, req.Network)
	}
	if c.Mode == "auto" {
		d.Selected.Provider = d.Recommended.Provider
		d.Selected.Model = d.Recommended.Model
		d.Selected.Effort = d.Recommended.Effort
	}
	if d.Selected.Model == "auto" {
		d.Selected.Model = ""
	}
	return d, nil
}

// Starter is deliberately shadow-only. Rules are empty until local evidence
// justifies them; the default never pretends a smaller model is proven better.
func Starter() Config {
	c := Config{Version: 1, Mode: "shadow", AllowedProviders: []string{"codex"}, Default: "astra-high", Rules: map[string]string{}}
	for _, row := range [][3]string{{"astra-high", "gpt-6-astra", "high"}, {"astra-low", "gpt-6-astra", "low"}, {"sol-medium", "gpt-5.6-sol", "medium"}, {"terra-medium", "gpt-5.6-terra", "medium"}, {"luna-medium", "gpt-5.6-luna", "medium"}, {"luna-xhigh", "gpt-5.6-luna", "xhigh"}} {
		c.Candidates = append(c.Candidates, Candidate{ID: row[0], Provider: "codex", Model: row[1], Effort: row[2], Enabled: true, Modes: []string{"read-only", "edit", "check", "test"}, Evidence: []string{"docs/model-routing/catalog.md"}})
	}
	return c
}
