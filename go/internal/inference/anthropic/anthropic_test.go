package anthropic_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"toolkit/internal/inference/anthropic"
	"toolkit/internal/testutil"
)

func goodResponse() map[string]any {
	return map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "low"},
		},
		"usage": map[string]any{
			"input_tokens":  80,
			"output_tokens": 3,
		},
	}
}

func makeReq() anthropic.MessagesRequest {
	return anthropic.MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 16,
		System:    "You classify.",
		Messages:  []anthropic.Message{{Role: "user", Content: "Input: trivial doc typo"}},
	}
}

func TestNew_MissingAPIKeyReturnsError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, err := anthropic.New()
	if err == nil {
		t.Error("expected error when ANTHROPIC_API_KEY is unset, got nil")
	}
}

func TestComplete_Success(t *testing.T) {
	srv := testutil.MockAnthropic(t, map[string]json.RawMessage{
		"/v1/messages": testutil.JSON(t, goodResponse()),
	})
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	c, err := anthropic.NewWithBaseURL(srv.URL)
	if err != nil {
		t.Fatalf("NewWithBaseURL: %v", err)
	}

	resp, err := c.Complete(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("expected at least one content block")
	}
	if resp.Content[0].Text != "low" {
		t.Errorf("content text: want %q, got %q", "low", resp.Content[0].Text)
	}
	if resp.Usage.InputTokens != 80 {
		t.Errorf("input_tokens: want 80, got %d", resp.Usage.InputTokens)
	}
}

func TestComplete_RateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"too many requests"}}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	c, err := anthropic.NewWithBaseURL(srv.URL)
	if err != nil {
		t.Fatalf("NewWithBaseURL: %v", err)
	}

	_, err = c.Complete(context.Background(), makeReq())
	if err == nil {
		t.Fatal("expected error on HTTP 429, got nil")
	}
}

func TestComplete_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"internal"}}`))
	}))
	t.Cleanup(srv.Close)

	_ = os.Setenv("ANTHROPIC_API_KEY", "test-key")
	c, err := anthropic.NewWithBaseURL(srv.URL)
	if err != nil {
		t.Fatalf("NewWithBaseURL: %v", err)
	}

	_, err = c.Complete(context.Background(), makeReq())
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
}

func TestComplete_SetsRequiredHeaders(t *testing.T) {
	var gotAPIKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ANTHROPIC_API_KEY", "sk-test-sentinel")
	c, err := anthropic.NewWithBaseURL(srv.URL)
	if err != nil {
		t.Fatalf("NewWithBaseURL: %v", err)
	}
	_, _ = c.Complete(context.Background(), makeReq())

	if gotAPIKey != "sk-test-sentinel" {
		t.Errorf("x-api-key header: want %q, got %q", "sk-test-sentinel", gotAPIKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header must be set")
	}
}

// TestMessagesRequest_CacheSystemEmitsBlockList pins the wire shape: when
// CacheSystem is true and System is non-empty, the request body serializes
// `system` as a single-element list of content blocks carrying
// cache_control: {type: ephemeral}, rather than as a bare string. The
// default (CacheSystem=false) shape — `system` as a string — must stay
// unchanged for back-compat.
func TestMessagesRequest_CacheSystemEmitsBlockList(t *testing.T) {
	t.Run("default emits bare string", func(t *testing.T) {
		req := anthropic.MessagesRequest{
			Model:     "claude-sonnet-4-6",
			MaxTokens: 16,
			System:    "You classify.",
			Messages:  []anthropic.Message{{Role: "user", Content: "x"}},
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if s, ok := got["system"].(string); !ok || s != "You classify." {
			t.Errorf("default system shape: want string %q, got %T %v", "You classify.", got["system"], got["system"])
		}
	})

	t.Run("CacheSystem=true emits block list with ephemeral cache_control", func(t *testing.T) {
		req := anthropic.MessagesRequest{
			Model:       "claude-sonnet-4-6",
			MaxTokens:   16,
			System:      "You classify.",
			Messages:    []anthropic.Message{{Role: "user", Content: "x"}},
			CacheSystem: true,
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		blocks, ok := got["system"].([]any)
		if !ok || len(blocks) != 1 {
			t.Fatalf("cached system shape: want single-element list, got %T %v", got["system"], got["system"])
		}
		block, ok := blocks[0].(map[string]any)
		if !ok {
			t.Fatalf("block: want map, got %T", blocks[0])
		}
		if block["type"] != "text" {
			t.Errorf("block.type: want %q, got %v", "text", block["type"])
		}
		if block["text"] != "You classify." {
			t.Errorf("block.text: want %q, got %v", "You classify.", block["text"])
		}
		cc, ok := block["cache_control"].(map[string]any)
		if !ok {
			t.Fatalf("block.cache_control: want map, got %T %v", block["cache_control"], block["cache_control"])
		}
		if cc["type"] != "ephemeral" {
			t.Errorf("cache_control.type: want %q, got %v", "ephemeral", cc["type"])
		}
	})

	t.Run("empty System never emits cache_control even when CacheSystem=true", func(t *testing.T) {
		// Anthropic rejects an empty cache_control'd block; the omit-if-empty
		// guard prevents it.
		req := anthropic.MessagesRequest{
			Model:       "claude-sonnet-4-6",
			MaxTokens:   16,
			System:      "",
			Messages:    []anthropic.Message{{Role: "user", Content: "x"}},
			CacheSystem: true,
		}
		data, _ := json.Marshal(req)
		var got map[string]any
		_ = json.Unmarshal(data, &got)
		if _, present := got["system"]; present {
			t.Errorf("empty system should be omitted, got %v", got["system"])
		}
	})
}

// TestComplete_ParsesCacheUsageFields pins that cache_creation_input_tokens
// and cache_read_input_tokens flow from the response body into UsageInfo so
// the inference router (and downstream observability) can see cache effects.
func TestComplete_ParsesCacheUsageFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content":[{"type":"text","text":"ok"}],
			"usage":{
				"input_tokens": 10,
				"output_tokens": 5,
				"cache_creation_input_tokens": 1024,
				"cache_read_input_tokens": 2048
			}
		}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	c, err := anthropic.NewWithBaseURL(srv.URL)
	if err != nil {
		t.Fatalf("NewWithBaseURL: %v", err)
	}
	resp, err := c.Complete(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.CacheCreationInputTokens != 1024 {
		t.Errorf("CacheCreationInputTokens: want 1024, got %d", resp.Usage.CacheCreationInputTokens)
	}
	if resp.Usage.CacheReadInputTokens != 2048 {
		t.Errorf("CacheReadInputTokens: want 2048, got %d", resp.Usage.CacheReadInputTokens)
	}
}
