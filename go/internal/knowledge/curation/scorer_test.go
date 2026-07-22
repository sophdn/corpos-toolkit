package curation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/testutil"
)

func TestQwenScorer_HealthSucceedsOnReachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	scorer := curation.NewQwenScorer(llamacpp.New(srv.URL))
	if err := scorer.Health(context.Background()); err != nil {
		t.Fatalf("Health: want nil, got %v", err)
	}
}

func TestQwenScorer_HealthPropagatesTypedUnreachable(t *testing.T) {
	scorer := curation.NewQwenScorer(llamacpp.New("http://127.0.0.1:1"))
	err := scorer.Health(context.Background())
	if err == nil {
		t.Fatal("Health: want error on dead port, got nil")
	}
	var unreachable *llamacpp.UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("Health: want errors.As(*UnreachableError), got %T: %v", err, err)
	}
}

func TestQwenScorer_ExtractAndScoreDelegateToHelpers(t *testing.T) {
	mockResp := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"content": "QUESTION: Q\n" +
						"INVOKE_WHEN: when\n" +
						"DESCRIPTION: desc",
				},
			},
		},
	}
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{
		"/v1/chat/completions": testutil.JSON(t, mockResp),
	})
	scorer := curation.NewQwenScorer(llamacpp.New(srv.URL))

	meta, err := scorer.Extract(context.Background(), "task", "ref", "material")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if meta.Question != "Q" {
		t.Errorf("Question: want %q, got %q", "Q", meta.Question)
	}
}

func TestQwenScorer_BaseURLSurfacesClientURL(t *testing.T) {
	scorer := curation.NewQwenScorer(llamacpp.New("http://example:9000"))
	if got := scorer.BaseURL(); got != "http://example:9000" {
		t.Errorf("BaseURL: want %q, got %q", "http://example:9000", got)
	}
}
