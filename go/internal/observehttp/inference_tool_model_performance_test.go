package observehttp

import (
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// seedInferenceRow inserts an inference_invocations row with an explicit
// success flag — seedQwenWithTime leaves success at its default, but the
// projection's success_count needs the failure case exercised too.
func seedInferenceRow(t *testing.T, pool *db.Pool, taskID, model string, latency int64, in, out *int64, success int64, at time.Time) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO inference_invocations
			(task_id, model_name, latency_ms, input_tokens, output_tokens, success, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		taskID, model, latency, in, out, success, at.UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed inference_invocations: %v", err)
	}
}

// The endpoint reads proj_inference_tool_model_performance, which
// newTestServer rebuilds from inference_invocations. Pins the computed
// read-side fields (success_rate, avg/max latency, avg_tokens) and the
// per-tool ordering (most-used model first).
func TestInferenceToolModelPerformance_Endpoint(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	in, out := int64(10), int64(40)

	// classify_x: qwen 4 calls (3 success), claude 1 call (success). qwen
	// is the most-used → sorts first within the tool.
	for i := 0; i < 4; i++ {
		success := int64(1)
		var ip, op *int64 = &in, &out
		if i == 3 {
			success = 0 // one failure → success_rate 3/4
		}
		seedInferenceRow(t, pool, "classify_x", "qwen2.5-32b", int64(100+i*100), ip, op, success, now)
	}
	seedInferenceRow(t, pool, "classify_x", "claude-sonnet-4-6", 500, nil, nil, 1, now)

	srv := newTestServer(t, pool)
	var got []ToolModelStat
	getJSON(t, srv, "/inference/tool-model-performance", &got)

	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
	}
	// Ordering: classify_x|qwen (4 calls) before classify_x|claude (1 call).
	qwen := got[0]
	if qwen.TaskID != "classify_x" || qwen.ModelName != "qwen2.5-32b" {
		t.Fatalf("first row = %+v, want classify_x/qwen2.5-32b", qwen)
	}
	if qwen.CallCount != 4 {
		t.Errorf("qwen call_count = %d, want 4", qwen.CallCount)
	}
	if qwen.SuccessRate != 0.75 {
		t.Errorf("qwen success_rate = %v, want 0.75", qwen.SuccessRate)
	}
	// latencies 100,200,300,400 → total 1000, avg 250, max 400.
	if qwen.AvgLatencyMS != 250 || qwen.MaxLatencyMS != 400 {
		t.Errorf("qwen latency avg=%d max=%d, want 250/400", qwen.AvgLatencyMS, qwen.MaxLatencyMS)
	}
	// All 4 qwen calls carry tokens → avg (10+40) = 50.
	if qwen.AvgTokens == nil || *qwen.AvgTokens != 50 {
		t.Errorf("qwen avg_tokens = %v, want 50", qwen.AvgTokens)
	}
	// last_invoked_at carries MAX(created_at) through the projection.
	if qwen.LastInvokedAt == "" {
		t.Error("qwen last_invoked_at should be populated from the projection")
	}

	claude := got[1]
	if claude.ModelName != "claude-sonnet-4-6" || claude.CallCount != 1 {
		t.Errorf("second row = %+v, want claude-sonnet-4-6 with 1 call", claude)
	}
	// claude call had no tokens → avg_tokens nil.
	if claude.AvgTokens != nil {
		t.Errorf("claude avg_tokens = %v, want nil (no usage recorded)", claude.AvgTokens)
	}
}

// The both-layers feature-delta (chain telemetry-success-model-unification):
// outcome_success_rate is the materialized Layer-2 rollup, distinct from the
// call-level success_rate. Pin that they DIVERGE: a passing classify benchmark
// makes every row outcome-success=1 even when a call-level failure drags
// success_rate below 1.
func TestInferenceToolModelPerformance_OutcomeSuccessRate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	seedProject(t, pool, "p")
	seedBenchmarkAccuracy(t, pool, "p", "classify_pass", 0.9, now)
	// 3 qwen calls, one call-level failure → call-level success_rate 2/3.
	seedInferenceRow(t, pool, "classify_pass", "qwen2.5-32b", 100, nil, nil, 1, now)
	seedInferenceRow(t, pool, "classify_pass", "qwen2.5-32b", 100, nil, nil, 1, now)
	seedInferenceRow(t, pool, "classify_pass", "qwen2.5-32b", 100, nil, nil, 0, now)

	srv := newTestServer(t, pool)
	var got []ToolModelStat
	getJSON(t, srv, "/inference/tool-model-performance", &got)
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(got), got)
	}
	r := got[0]
	if r.SuccessRate < 0.66 || r.SuccessRate > 0.67 {
		t.Errorf("success_rate (call-level) = %v, want ~0.667 (2/3)", r.SuccessRate)
	}
	if r.OutcomeSuccessRate != 1.0 {
		t.Errorf("outcome_success_rate = %v, want 1.0 (classify benchmark passes for every row) — the outcome layer must differ from the call layer", r.OutcomeSuccessRate)
	}
}

func TestInferenceToolModelPerformance_EmptyReturnsEmptyArray(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	var got []ToolModelStat
	getJSON(t, srv, "/inference/tool-model-performance", &got)
	if len(got) != 0 {
		t.Errorf("want empty array, got %d rows", len(got))
	}
}
