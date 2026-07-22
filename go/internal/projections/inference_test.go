package projections_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/projections"
	"toolkit/internal/testutil"
)

// seedInference inserts one inference_invocations row.
func seedInference(t *testing.T, pool *db.Pool, taskID, model string, latency int64, in, out *int64, success int, errorClass string, at time.Time) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO inference_invocations
			(task_id, model_name, latency_ms, input_tokens, output_tokens, success, error_class, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID, model, latency, in, out, success, errorClass, at.UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed inference_invocations: %v", err)
	}
}

type itmpRow struct {
	CallCount, SuccessCount, TotalLatency, MaxLatency, TotalIn, TotalOut, CallsWithTokens int64
}

func dumpITMP(t *testing.T, pool *db.Pool) map[string]itmpRow {
	t.Helper()
	rows, err := pool.DB().Query(
		`SELECT task_id, model_name, call_count, success_count, total_latency_ms, max_latency_ms,
		        total_input_tokens, total_output_tokens, calls_with_tokens
		 FROM proj_inference_tool_model_performance`)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	defer rows.Close()
	out := map[string]itmpRow{}
	for rows.Next() {
		var task, model string
		var r itmpRow
		if err := rows.Scan(&task, &model, &r.CallCount, &r.SuccessCount, &r.TotalLatency, &r.MaxLatency,
			&r.TotalIn, &r.TotalOut, &r.CallsWithTokens); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[task+"|"+model] = r
	}
	return out
}

// TestInferenceToolModelPerformance_Rebuild pins the totals aggregation
// across two tools and a multi-model tool, with a success mix and
// partial token coverage.
func TestInferenceToolModelPerformance_Rebuild(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	in, out := int64(10), int64(40)

	// classify_x on two models: qwen (2 calls, 1 success, tokens on both)
	// and claude (1 call, success, no tokens).
	seedInference(t, pool, "classify_x", "qwen2.5-32b", 100, &in, &out, 1, "", now)
	seedInference(t, pool, "classify_x", "qwen2.5-32b", 300, &in, &out, 0, "upstream_error", now)
	seedInference(t, pool, "classify_x", "claude-sonnet-4-6", 500, nil, nil, 1, "", now)
	// vault-rerank on qwen: 1 call, success, tokens.
	seedInference(t, pool, "vault-rerank-retrieve", "qwen2.5-32b", 200, &in, &out, 1, "", now)

	mustRebuildAll(t, pool, []string{"inference_tool_model_performance"})

	got := dumpITMP(t, pool)
	want := map[string]itmpRow{
		"classify_x|qwen2.5-32b":            {CallCount: 2, SuccessCount: 1, TotalLatency: 400, MaxLatency: 300, TotalIn: 20, TotalOut: 80, CallsWithTokens: 2},
		"classify_x|claude-sonnet-4-6":      {CallCount: 1, SuccessCount: 1, TotalLatency: 500, MaxLatency: 500, TotalIn: 0, TotalOut: 0, CallsWithTokens: 0},
		"vault-rerank-retrieve|qwen2.5-32b": {CallCount: 1, SuccessCount: 1, TotalLatency: 200, MaxLatency: 200, TotalIn: 10, TotalOut: 40, CallsWithTokens: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("row %s = %+v, want %+v", k, got[k], v)
		}
	}
}

// TestInferenceToolModelPerformance_RebuildIsByteIdentical exercises the
// read-side invariant: because Fold re-snapshots from source, rebuilding
// twice (and rebuilding over an already-populated table) converges on the
// exact same rows — the byte-identical-rebuild-from-empty guarantee.
func TestInferenceToolModelPerformance_RebuildIsByteIdentical(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	in, out := int64(7), int64(11)
	seedInference(t, pool, "classify_x", "qwen2.5-32b", 120, &in, &out, 1, "", now)
	seedInference(t, pool, "session-routing", "claude-sonnet-4-6", 90, nil, nil, 0, "timeout", now)

	mustRebuildAll(t, pool, []string{"inference_tool_model_performance"})
	first := dumpITMP(t, pool)
	// Rebuild again over the populated table — must not double-count or drift.
	mustRebuildAll(t, pool, []string{"inference_tool_model_performance"})
	second := dumpITMP(t, pool)

	if len(first) != len(second) {
		t.Fatalf("rebuild changed row count: %d → %d", len(first), len(second))
	}
	for k, v := range first {
		if second[k] != v {
			t.Errorf("rebuild not idempotent for %s: %+v → %+v", k, v, second[k])
		}
	}
}

// Folding via the read-side Fold entrypoint (RawEvent ignored) equals the
// rebuild — proving the projection is correctly wired into FoldAllReadSide.
func TestInferenceToolModelPerformance_FoldEqualsRebuild(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	seedInference(t, pool, "classify_x", "qwen2.5-32b", 100, nil, nil, 1, "", now)

	if err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		return projections.FoldAllReadSide(context.Background(), tx)
	}); err != nil {
		t.Fatalf("FoldAllReadSide: %v", err)
	}
	got := dumpITMP(t, pool)
	if r, ok := got["classify_x|qwen2.5-32b"]; !ok || r.CallCount != 1 || r.SuccessCount != 1 {
		t.Errorf("FoldAllReadSide did not populate inference projection: %+v", got)
	}
}
