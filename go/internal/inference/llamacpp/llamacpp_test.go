package llamacpp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/testutil"
)

func goodResponse() map[string]any {
	return map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "low",
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     100,
			"completion_tokens": 5,
		},
	}
}

func makeReq() llamacpp.CompletionRequest {
	return llamacpp.CompletionRequest{
		Model: "qwen2.5-32b",
		Messages: []llamacpp.Message{
			{Role: "system", Content: "You classify."},
			{Role: "user", Content: "Input: trivial doc typo"},
		},
		MaxTokens: 16,
	}
}

func TestComplete_Success(t *testing.T) {
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{
		"/v1/chat/completions": testutil.JSON(t, goodResponse()),
	})
	c := llamacpp.New(srv.URL)
	resp, err := c.Complete(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if resp.Choices[0].Message.Content != "low" {
		t.Errorf("content: want %q, got %q", "low", resp.Choices[0].Message.Content)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 100 {
		t.Errorf("usage: want prompt_tokens=100, got %v", resp.Usage)
	}
}

func TestComplete_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	t.Cleanup(srv.Close)

	c := llamacpp.New(srv.URL)
	_, err := c.Complete(context.Background(), makeReq())
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
	// Error message must be non-empty and must not panic.
	if msg := err.Error(); msg == "" {
		t.Error("expected non-empty error message")
	}
}

func TestComplete_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not valid json`))
	}))
	t.Cleanup(srv.Close)

	c := llamacpp.New(srv.URL)
	_, err := c.Complete(context.Background(), makeReq())
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
}

func TestComplete_ContextCancellation(t *testing.T) {
	// Server that hangs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := llamacpp.New(srv.URL)
	_, err := c.Complete(ctx, makeReq())
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
}

func TestHealth_OkOnTwoHundred(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("Health probed %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	c := llamacpp.New(srv.URL)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: want nil, got %v", err)
	}
}

func TestHealth_TypedUnreachableOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := llamacpp.New(srv.URL)
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("Health: want error on 503, got nil")
	}
	var unreachable *llamacpp.UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("Health: want *UnreachableError, got %T: %v", err, err)
	}
	if unreachable.URL != srv.URL+"/health" {
		t.Errorf("UnreachableError.URL: want %q, got %q", srv.URL+"/health", unreachable.URL)
	}
	if !strings.Contains(unreachable.Cause, "503") {
		t.Errorf("UnreachableError.Cause: want substring %q, got %q", "503", unreachable.Cause)
	}
}

func TestHealth_TypedUnreachableOnConnectionRefused(t *testing.T) {
	// No server — point at a port nothing listens on.
	c := llamacpp.New("http://127.0.0.1:1")
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("Health: want error on dead port, got nil")
	}
	var unreachable *llamacpp.UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("Health: want *UnreachableError, got %T: %v", err, err)
	}
	if unreachable.URL == "" || unreachable.Cause == "" {
		t.Errorf("UnreachableError fields empty: %+v", unreachable)
	}
}

func TestNewFromEnv_RespectsEnvVar(t *testing.T) {
	t.Setenv(llamacpp.EnvLocalURL, "http://custom:9999")
	c := llamacpp.NewFromEnv()
	if c.BaseURL() != "http://custom:9999" {
		t.Errorf("BaseURL: want http://custom:9999, got %q", c.BaseURL())
	}
}

func TestNewFromEnv_DefaultsWhenEnvAbsent(t *testing.T) {
	t.Setenv(llamacpp.EnvLocalURL, "")
	c := llamacpp.NewFromEnv()
	if c.BaseURL() != "http://localhost:8081" {
		t.Errorf("BaseURL: want http://localhost:8081, got %q", c.BaseURL())
	}
}

func TestComplete_RequestBodyIsJSON(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(goodResponse())
	}))
	t.Cleanup(srv.Close)

	c := llamacpp.New(srv.URL)
	_, _ = c.Complete(context.Background(), makeReq())

	var parsed map[string]any
	if err := json.Unmarshal(captured, &parsed); err != nil {
		t.Fatalf("captured body is not JSON: %v — body: %s", err, captured)
	}
	if parsed["model"] != "qwen2.5-32b" {
		t.Errorf("model: want qwen2.5-32b, got %v", parsed["model"])
	}
}

func TestBaseURLFromEnv(t *testing.T) {
	t.Run("canonical wins over legacy", func(t *testing.T) {
		t.Setenv("TOOLKIT_LOCAL_URL", "http://canon:8081")
		t.Setenv("LLAMA_CPP_BASE_URL", "http://legacy:8081")
		if got := llamacpp.BaseURLFromEnv(); got != "http://canon:8081" {
			t.Fatalf("got %q, want canonical", got)
		}
	})
	t.Run("legacy fallback when canonical unset", func(t *testing.T) {
		t.Setenv("TOOLKIT_LOCAL_URL", "")
		t.Setenv("LLAMA_CPP_BASE_URL", "http://legacy:8081")
		if got := llamacpp.BaseURLFromEnv(); got != "http://legacy:8081" {
			t.Fatalf("got %q, want legacy fallback", got)
		}
	})
	t.Run("default when neither set", func(t *testing.T) {
		t.Setenv("TOOLKIT_LOCAL_URL", "")
		t.Setenv("LLAMA_CPP_BASE_URL", "")
		if got := llamacpp.BaseURLFromEnv(); got != "http://localhost:8081" {
			t.Fatalf("got %q, want default http://localhost:8081", got)
		}
	})
}
