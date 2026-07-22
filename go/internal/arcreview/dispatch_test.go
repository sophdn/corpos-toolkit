package arcreview

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/inference/router"
)

func TestParseReviewResponse_NakedJSON(t *testing.T) {
	body := `{
		"filing_decisions": [
			{
				"action": "nothing_to_file",
				"payload": null,
				"confidence": 0.4,
				"reasoning": "session ran on rails"
			}
		],
		"summary": "uneventful arc"
	}`
	out, err := ParseReviewResponse(body)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if len(out.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(out.Decisions))
	}
	if out.Summary != "uneventful arc" {
		t.Fatalf("expected summary 'uneventful arc', got %q", out.Summary)
	}
}

func TestParseReviewResponse_FencedJSON(t *testing.T) {
	body := "```json\n" + `{
  "filing_decisions": [],
  "summary": "nothing"
}` + "\n```"
	out, err := ParseReviewResponse(body)
	if err != nil {
		t.Fatalf("fenced JSON should parse, got %v", err)
	}
	if out.Decisions == nil {
		t.Fatalf("nil decisions should normalize to empty slice")
	}
}

func TestParseReviewResponse_PrefaceTextStripped(t *testing.T) {
	body := `Sure, here is the JSON output:

{
  "filing_decisions": [],
  "summary": "ok"
}

Let me know if you need adjustments.`
	out, err := ParseReviewResponse(body)
	if err != nil {
		t.Fatalf("preface-text response should parse, got %v", err)
	}
	if out.Summary != "ok" {
		t.Fatalf("expected summary 'ok', got %q", out.Summary)
	}
}

func TestParseReviewResponse_BracesInsideStrings(t *testing.T) {
	body := `{
		"filing_decisions": [],
		"summary": "use the {placeholder} pattern}"
	}`
	out, err := ParseReviewResponse(body)
	if err != nil {
		t.Fatalf("braces inside strings should not confuse the parser, got %v", err)
	}
	if !strings.Contains(out.Summary, "placeholder") {
		t.Fatalf("expected summary preserved, got %q", out.Summary)
	}
}

func TestParseReviewResponse_NoObjectFound(t *testing.T) {
	body := `Sorry, I cannot answer that.`
	_, err := ParseReviewResponse(body)
	if err == nil {
		t.Fatalf("expected error when no JSON object present")
	}
}

func TestParseReviewResponse_UnknownTopLevelFieldsTolerated(t *testing.T) {
	body := `{
		"filing_decisions": [],
		"summary": "ok",
		"notes": "extra commentary Qwen wedged in"
	}`
	out, err := ParseReviewResponse(body)
	if err != nil {
		t.Fatalf("unknown top-level fields should not fail parse, got %v", err)
	}
	if out.Summary != "ok" {
		t.Fatalf("expected summary 'ok', got %q", out.Summary)
	}
}

func TestExtractJSONObject_BalancedDepth(t *testing.T) {
	in := `prefix {"a": {"b": 1}} suffix`
	got := extractJSONObject(in)
	want := `{"a": {"b": 1}}`
	if got != want {
		t.Fatalf("extractJSONObject got %q want %q", got, want)
	}
}

func TestDispatchReview_NilRouterRejected(t *testing.T) {
	snap := Snapshot{Messages: []Message{{Role: "user", Content: "x"}}}
	_, err := DispatchReview(context.Background(), nil, snap, nil, nil)
	if err == nil {
		t.Fatalf("expected error when router is nil")
	}
}

func TestDispatchReview_EmptySnapshotRejected(t *testing.T) {
	// Router constructed with nil clients — never used because the
	// empty-snapshot check exits before any Generate call.
	r := router.NewWithClients(nil, nil, "stub")
	_, err := DispatchReview(context.Background(), r, Snapshot{}, nil, nil)
	if err == nil {
		t.Fatalf("expected error on empty snapshot")
	}
}
