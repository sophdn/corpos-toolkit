// Package anthropic provides an HTTP client for the Anthropic Messages API.
package anthropic

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

const defaultBaseURL = "https://api.anthropic.com"
const apiKeyEnv = "ANTHROPIC_API_KEY"
const defaultModel = "claude-sonnet-4-6"
const defaultTimeout = 60 * time.Second
const apiVersion = "2023-06-01"

// Message is one chat message in a Messages API request.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// MessagesRequest is the body sent to /v1/messages.
//
// CacheSystem opts the System prompt into Anthropic's prompt-caching surface
// (5-minute ephemeral TTL by default). When true and System is non-empty,
// the request body marshals `system` as a single-element content-block list
// with `cache_control: {type: "ephemeral"}` instead of a bare string. The
// first call within 5 minutes pays a ~25% cache-write surcharge; subsequent
// calls hit the cache and pay ~10% of the cached portion's cost. Net win for
// any usage pattern that repeats the same system prompt within 5 minutes
// (the typical classify_* path, where the rubric system prompt stays stable
// across many input classifications). For Anthropic's cache to engage at
// all, the cached prefix must exceed the model-specific minimum (~1024
// tokens on Sonnet); below that, the marker is ignored and there's no
// extra cost. See vault `learnings/mcp-servers/2026-05-14_rust-to-go-migration.md`
// for the post-migration follow-up context this lands as.
type MessagesRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	System      string    `json:"-"` // serialized by MarshalJSON; see CacheSystem
	Messages    []Message `json:"messages"`
	CacheSystem bool      `json:"-"`
}

// MarshalJSON emits the API wire format, encoding `system` as either a
// bare string (default) or a single-element content-block list with
// cache_control when CacheSystem is true. Each branch marshals through
// its own fully-typed wire struct so the JSON-encoding seam stays
// concrete — no `any`-typed system field at this layer.
func (r MessagesRequest) MarshalJSON() ([]byte, error) {
	if r.CacheSystem && r.System != "" {
		return json.Marshal(wireRequestCached{
			Model:     r.Model,
			MaxTokens: r.MaxTokens,
			System: []systemBlock{{
				Type:         "text",
				Text:         r.System,
				CacheControl: &cacheControl{Type: "ephemeral"},
			}},
			Messages: r.Messages,
		})
	}
	return json.Marshal(wireRequestPlain{
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
		System:    r.System,
		Messages:  r.Messages,
	})
}

// wireRequestPlain is the legacy / non-cached wire shape: `system` is a
// bare string and omitted via omitempty when empty.
type wireRequestPlain struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
}

// wireRequestCached is the cache-engaged wire shape: `system` is a
// content-block list whose last element carries cache_control. Anthropic
// requires the block-list form to attach cache_control markers.
type wireRequestCached struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    []systemBlock `json:"system"`
	Messages  []Message     `json:"messages"`
}

// systemBlock is the wire shape Anthropic accepts for a system content
// block when prompt-caching is engaged.
type systemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl mirrors Anthropic's cache_control marker. Type is always
// "ephemeral" today; if Anthropic adds 1h TTL beyond ephemeral, extend
// with a TTL field.
type cacheControl struct {
	Type string `json:"type"`
}

// ContentBlock is one block in a Messages API response.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// UsageInfo holds token counts returned by the API. CacheCreationInputTokens
// and CacheReadInputTokens are populated when prompt caching is engaged
// (system block carried a cache_control marker that Anthropic honored);
// both are omitted from non-caching responses.
type UsageInfo struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// MessagesResponse is the body returned by /v1/messages.
type MessagesResponse struct {
	Content []ContentBlock `json:"content"`
	Usage   UsageInfo      `json:"usage"`
}

// HTTPError is returned when the server replies with a non-200 status.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("anthropic complete: HTTP %d: %s", e.StatusCode, e.Body)
}

// Client calls the Anthropic Messages API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// New constructs a Client, reading the API key from ANTHROPIC_API_KEY.
// Returns an error if the key is absent so callers can handle the absence
// explicitly rather than discovering it at first inference call.
func New() (*Client, error) {
	return NewWithBaseURL(defaultBaseURL)
}

// NewWithBaseURL constructs a Client targeting a custom base URL (used in
// tests to point at a mock server).
func NewWithBaseURL(baseURL string) (*Client, error) {
	key := os.Getenv(apiKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("anthropic: %s is not set", apiKeyEnv)
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  key,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}, nil
}

// Complete sends req to /v1/messages and returns the response.
func (c *Client) Complete(ctx context.Context, req MessagesRequest) (MessagesResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return MessagesResponse{}, fmt.Errorf("anthropic complete: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return MessagesResponse{}, fmt.Errorf("anthropic complete: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return MessagesResponse{}, fmt.Errorf("anthropic complete: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return MessagesResponse{}, fmt.Errorf("anthropic complete: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return MessagesResponse{}, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	var out MessagesResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return MessagesResponse{}, fmt.Errorf("anthropic complete: unmarshal: %w", err)
	}
	return out, nil
}
