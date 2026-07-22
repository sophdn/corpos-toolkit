// Package llamacpp provides an HTTP client for the llama.cpp OpenAI-compatible
// /v1/chat/completions endpoint.
package llamacpp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const defaultBaseURL = "http://localhost:8081"
const defaultTimeout = 120 * time.Second
const healthTimeout = 5 * time.Second

// EnvLocalURL is the canonical env var that overrides the default base URL,
// mirroring the inference-clients Rust router's TOOLKIT_LOCAL_URL.
const EnvLocalURL = "TOOLKIT_LOCAL_URL"

// EnvLocalURLLegacy is a deprecated, back-compat alias some deployments still set.
// EnvLocalURL takes precedence when both are present.
const EnvLocalURLLegacy = "LLAMA_CPP_BASE_URL"

// BaseURLFromEnv resolves the llama-server base URL from the environment — the
// SINGLE source of truth shared by the inference client (NewFromEnv), the admin
// health probe, and the toolkit-server -llama-url flag default. Precedence:
// TOOLKIT_LOCAL_URL, then the back-compat LLAMA_CPP_BASE_URL, then
// http://localhost:8081. One resolver fixes the prior split where the health probe
// checked a DIFFERENT var than the router dispatched to (so reachability could lie).
func BaseURLFromEnv() string {
	if v := os.Getenv(EnvLocalURL); v != "" {
		return v
	}
	if v := os.Getenv(EnvLocalURLLegacy); v != "" {
		return v
	}
	return defaultBaseURL
}

// Message is one chat message in a completion request.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CompletionRequest is the body sent to /v1/chat/completions.
type CompletionRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Stream    bool      `json:"stream"`
}

// CompletionResponse is the body returned by /v1/chat/completions.
type CompletionResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// HTTPError is returned when the server replies with a non-200 status.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("llamacpp complete: HTTP %d: %s", e.StatusCode, e.Body)
}

// UnreachableError is returned by Health when the server is not
// reachable or replied with a non-2xx status. URL is the probed URL;
// Cause is the underlying reason (transport error, HTTP status).
// Callers print these typed errors directly to surface the actionable
// "where was I trying to reach, why didn't it work" answer that the
// silent qwen-unreachable failure mode swallowed before this contract
// existed.
type UnreachableError struct {
	URL   string
	Cause string
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("llamacpp unreachable at %s: %s", e.URL, e.Cause)
}

// Client calls the llama.cpp server.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New returns a Client targeting baseURL. An empty baseURL uses localhost:8081.
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// NewFromEnv returns a Client targeting BaseURLFromEnv() — TOOLKIT_LOCAL_URL, then
// the back-compat LLAMA_CPP_BASE_URL, then the default (http://localhost:8081).
// Mirrors the Rust inference-clients Config::from_env behaviour so the canonical
// llama-server URL has one source of truth across both languages.
func NewFromEnv() *Client {
	return New(BaseURLFromEnv())
}

// BaseURL returns the URL this client is targeting. Surfaced so
// callers (binaries, diagnostics) can include it in error messages
// without re-reading env vars.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// Health probes GET <baseURL>/health and returns nil iff the server
// responds with 2xx. Any other outcome (timeout, connection refused,
// non-2xx) returns a typed *UnreachableError naming URL + cause.
//
// Has its own 5-second timeout regardless of the Client's larger
// generation timeout — health checks should fail fast.
func (c *Client) Health(ctx context.Context) error {
	url := c.baseURL + "/health"

	healthClient := &http.Client{Timeout: healthTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &UnreachableError{URL: url, Cause: err.Error()}
	}

	resp, err := healthClient.Do(req)
	if err != nil {
		return &UnreachableError{URL: url, Cause: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &UnreachableError{URL: url, Cause: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return nil
}

// Complete sends req to /v1/chat/completions and returns the response.
// Context cancellation is propagated to the underlying HTTP request.
func (c *Client) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("llamacpp complete: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("llamacpp complete: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("llamacpp complete: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("llamacpp complete: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return CompletionResponse{}, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	var out CompletionResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return CompletionResponse{}, fmt.Errorf("llamacpp complete: unmarshal: %w", err)
	}
	return out, nil
}
