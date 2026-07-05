package workflowclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      string
}

type RunRequest struct {
	ID           string         `json:"id,omitempty"`
	Task         string         `json:"task"`
	CWD          string         `json:"cwd,omitempty"`
	ScriptPath   string         `json:"script_path,omitempty"`
	WorkflowName string         `json:"workflow_name,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	ArgsJSON     string         `json:"args_json,omitempty"`
}

type LibraryInstallRequest struct {
	Pack  string `json:"pack"`
	CWD   string `json:"cwd,omitempty"`
	Name  string `json:"name,omitempty"`
	Force bool   `json:"force,omitempty"`
}

func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTPClient: http.DefaultClient}
}

func NewWithToken(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTPClient: http.DefaultClient, Token: strings.TrimSpace(token)}
}

func (c *Client) Health(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/healthz")
}

func (c *Client) Fleet(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/workflows/fleet")
}

func (c *Client) Analytics(ctx context.Context, limit int) (json.RawMessage, error) {
	path := "/workflows/analytics"
	if limit > 0 {
		values := url.Values{}
		values.Set("limit", fmt.Sprintf("%d", limit))
		path += "?" + values.Encode()
	}
	return c.get(ctx, path)
}

func (c *Client) Library(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/workflows/library")
}

func (c *Client) LibraryPack(ctx context.Context, name string) (json.RawMessage, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("workflow library pack name is required")
	}
	return c.get(ctx, "/workflows/library/"+url.PathEscape(name))
}

func (c *Client) InstallLibrary(ctx context.Context, req LibraryInstallRequest) (json.RawMessage, error) {
	if strings.TrimSpace(req.Pack) == "" {
		return nil, fmt.Errorf("workflow library pack is required")
	}
	return c.post(ctx, "/workflows/library/install", req)
}

func (c *Client) Run(ctx context.Context, req RunRequest) (json.RawMessage, error) {
	if strings.TrimSpace(req.Task) == "" {
		return nil, fmt.Errorf("workflow task is required")
	}
	return c.post(ctx, "/workflows/run", req)
}

func (c *Client) RunSnapshot(ctx context.Context, id string) (json.RawMessage, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("workflow run id is required")
	}
	return c.get(ctx, "/workflows/runs/"+url.PathEscape(id))
}

func (c *Client) get(ctx context.Context, path string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, path, bytes.NewReader(raw))
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (json.RawMessage, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("workflow API base URL is required")
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("workflow API %s %s failed: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	return json.RawMessage(bytes.TrimSpace(raw)), nil
}
