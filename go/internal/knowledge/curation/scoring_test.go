package curation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/testutil"
)

func TestParseExtraction(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want curation.ExtractedMeta
		ok   bool
	}{
		{
			name: "well-formed three lines",
			in: "QUESTION: What does X do?\n" +
				"INVOKE_WHEN: When the agent encounters X.\n" +
				"DESCRIPTION: X is a thing that does Y.",
			want: curation.ExtractedMeta{
				Question:    "What does X do?",
				InvokeWhen:  "When the agent encounters X.",
				Description: "X is a thing that does Y.",
			},
			ok: true,
		},
		{
			name: "extra whitespace trimmed",
			in: "QUESTION:    What does X do?   \n" +
				"INVOKE_WHEN: When X.   \n" +
				"DESCRIPTION: Y.",
			want: curation.ExtractedMeta{
				Question:    "What does X do?",
				InvokeWhen:  "When X.",
				Description: "Y.",
			},
			ok: true,
		},
		{
			name: "out-of-order labels are fine",
			in: "DESCRIPTION: Y.\n" +
				"QUESTION: What does X do?\n" +
				"INVOKE_WHEN: When X.",
			want: curation.ExtractedMeta{
				Question:    "What does X do?",
				InvokeWhen:  "When X.",
				Description: "Y.",
			},
			ok: true,
		},
		{
			name: "description allowed empty",
			in: "QUESTION: What does X do?\n" +
				"INVOKE_WHEN: When X.\n" +
				"DESCRIPTION: ",
			want: curation.ExtractedMeta{
				Question:    "What does X do?",
				InvokeWhen:  "When X.",
				Description: "",
			},
			ok: true,
		},
		{
			name: "missing QUESTION fails",
			in: "INVOKE_WHEN: When X.\n" +
				"DESCRIPTION: Y.",
			ok: false,
		},
		{
			name: "missing INVOKE_WHEN fails",
			in: "QUESTION: What does X do?\n" +
				"DESCRIPTION: Y.",
			ok: false,
		},
		{
			name: "empty QUESTION fails",
			in: "QUESTION:  \n" +
				"INVOKE_WHEN: When X.\n" +
				"DESCRIPTION: Y.",
			ok: false,
		},
		{
			name: "all empty fails",
			in:   "",
			ok:   false,
		},
		{
			name: "unrelated text only fails",
			in:   "Sorry, I cannot help with that.",
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := curation.ParseExtraction(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok: want %v, got %v (out=%+v)", tc.ok, ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("metadata mismatch:\n  want: %+v\n  got:  %+v", tc.want, got)
			}
		})
	}
}

func TestParseScore(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{name: "decimal with explanation", in: "0.85 The source directly answers.", want: 0.85, ok: true},
		{name: "decimal alone", in: "0.42", want: 0.42, ok: true},
		{name: "leading whitespace", in: "  0.5 yes", want: 0.5, ok: true},
		{name: "exactly 1.0", in: "1.0 perfect match", want: 1.0, ok: true},
		{name: "exactly 0.0", in: "0.0 irrelevant", want: 0.0, ok: true},
		{name: "integer is fine", in: "1 perfect", want: 1.0, ok: true},
		{name: "above 1.0 rejected", in: "1.5 over", ok: false},
		{name: "below 0.0 rejected", in: "-0.1 under", ok: false},
		{name: "non-numeric first token", in: "high quality", ok: false},
		{name: "empty input", in: "", ok: false},
		{name: "whitespace only", in: "   ", ok: false},
		{name: "no first token before space", in: " 0.5", want: 0.5, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := curation.ParseScore(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok: want %v, got %v (out=%v)", tc.ok, ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("score: want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestQwenExtract_RoundTripViaMockServer(t *testing.T) {
	mockResp := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"content": "QUESTION: What does the dispatcher do?\n" +
						"INVOKE_WHEN: When investigating action routing.\n" +
						"DESCRIPTION: The dispatcher routes meta-tool calls.",
				},
			},
		},
	}
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{
		"/v1/chat/completions": testutil.JSON(t, mockResp),
	})
	client := llamacpp.New(srv.URL)

	meta, err := curation.QwenExtract(context.Background(), client,
		"task", "mcp-servers::dispatcher-doc",
		"The dispatcher routes meta-tool calls based on action manifest.")
	if err != nil {
		t.Fatalf("QwenExtract: %v", err)
	}
	if meta.Question == "" || !strings.Contains(meta.Question, "dispatcher") {
		t.Errorf("unexpected question: %q", meta.Question)
	}
	if meta.InvokeWhen == "" {
		t.Errorf("invoke_when empty")
	}
}

func TestQwenExtract_FailsOnUnparseableResponse(t *testing.T) {
	mockResp := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "I'm sorry, I don't know.",
				},
			},
		},
	}
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{
		"/v1/chat/completions": testutil.JSON(t, mockResp),
	})
	client := llamacpp.New(srv.URL)

	_, err := curation.QwenExtract(context.Background(), client, "task", "ref", "material")
	if err == nil {
		t.Fatal("QwenExtract: want error on unparseable response, got nil")
	}
}

func TestQwenScore_RoundTripViaMockServer(t *testing.T) {
	mockResp := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "0.91 The source addresses the question directly.",
				},
			},
		},
	}
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{
		"/v1/chat/completions": testutil.JSON(t, mockResp),
	})
	client := llamacpp.New(srv.URL)

	score, err := curation.QwenScore(context.Background(), client,
		"What does X do?", "X does Y by Z.")
	if err != nil {
		t.Fatalf("QwenScore: %v", err)
	}
	if score != 0.91 {
		t.Errorf("score: want 0.91, got %v", score)
	}
}

func TestQwenScore_FailsOnNonDecimalResponse(t *testing.T) {
	mockResp := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "high quality match",
				},
			},
		},
	}
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{
		"/v1/chat/completions": testutil.JSON(t, mockResp),
	})
	client := llamacpp.New(srv.URL)

	_, err := curation.QwenScore(context.Background(), client, "Q", "M")
	if err == nil {
		t.Fatal("QwenScore: want error on non-decimal response, got nil")
	}
}

func TestQwenExtract_PropagatesHTTPError(t *testing.T) {
	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{})
	// MockLlamaCPP with empty map returns 404 for any unmatched path.
	client := llamacpp.New(srv.URL)

	_, err := curation.QwenExtract(context.Background(), client, "task", "ref", "material")
	if err == nil {
		t.Fatal("want error on 404, got nil")
	}
}

func TestPromptConstants_ByteIdenticalToRustReference(t *testing.T) {
	// These constants are the cross-language contract. If anyone edits
	// them, scores produced by Go and Rust runs diverge — the curation
	// migration depends on parity. This test isn't checking a specific
	// value, it's pinning the constants so an editor sees a test failure
	// and remembers to update the Rust originals at
	// benchmarks/src/bin/knowledge_curate.rs (EXTRACTION_SYSTEM /
	// SCORING_SYSTEM constants) in lockstep.
	if !strings.HasPrefix(curation.ExtractionSystem,
		"You extract retrieval metadata from a technical knowledge source.") {
		t.Errorf("ExtractionSystem drifted from Rust reference")
	}
	if !strings.HasPrefix(curation.ScoringSystem,
		"You assess whether a knowledge source can answer a specific question.") {
		t.Errorf("ScoringSystem drifted from Rust reference")
	}
	if curation.ExcerptChars != 2000 {
		t.Errorf("ExcerptChars: want 2000 (Rust EXCERPT_CHARS), got %d", curation.ExcerptChars)
	}
}

func TestQwenExtract_FailsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"oom"}`))
	}))
	t.Cleanup(srv.Close)

	client := llamacpp.New(srv.URL)
	_, err := curation.QwenExtract(context.Background(), client, "task", "ref", "material")
	if err == nil {
		t.Fatal("want error on HTTP 500, got nil")
	}
}
