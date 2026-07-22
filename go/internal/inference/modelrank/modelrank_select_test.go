package modelrank

import (
	"context"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/qwenctx"
	"toolkit/internal/testutil"
)

// T5 densification for the cached, DB-backed selector. Hermetic: the projection
// is seeded directly (no live state — per the characterization-nets-must-be-
// hermetic reference). The cache clock (rk.now) + ttl are driven from these
// package-internal tests.

func seedPerf(t *testing.T, pool *db.Pool, task, model string, calls, outcome, totalLatencyMS int64) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_inference_tool_model_performance
			(task_id, model_name, call_count, success_count, outcome_success_count, total_latency_ms)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		task, model, calls, outcome, outcome, totalLatencyMS); err != nil {
		t.Fatalf("seed perf %q/%q: %v", task, model, err)
	}
}

func ctxFor(task string) context.Context {
	return qwenctx.WithTaskID(context.Background(), task)
}

// Select ranks from the seeded projection: a warmed default + a materially
// better warmed candidate → the candidate, switched.
func TestSelect_RanksFromSeededProjection(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedPerf(t, pool, "classify_x", "qwen2.5-32b", 100, 50, 100_000)       // 0.50
	seedPerf(t, pool, "classify_x", "claude-sonnet-4-6", 100, 95, 300_000) // 0.95

	rk := NewRanker(pool, "qwen2.5-32b")
	model, ok := rk.Select(ctxFor("classify_x"))
	if model != "claude-sonnet-4-6" || !ok {
		t.Errorf("Select = (%q,%v), want (claude-sonnet-4-6,true)", model, ok)
	}
}

// A task with no projection rows → the static default, not switched (the
// router then uses its default regardless of the name).
func TestSelect_NoRowsReturnsDefaultNotSwitched(t *testing.T) {
	pool := testutil.NewTestDB(t)
	rk := NewRanker(pool, "qwen2.5-32b")
	model, ok := rk.Select(ctxFor("never-seen-task"))
	if ok {
		t.Errorf("Select = (%q,true), want switched=false for an unseen task", model)
	}
}

// An unattributed / empty task short-circuits to ("",false) without a query.
func TestSelect_UnattributedReturnsEmptyFalse(t *testing.T) {
	pool := testutil.NewTestDB(t)
	rk := NewRanker(pool, "qwen2.5-32b")
	if model, ok := rk.Select(context.Background()); model != "" || ok {
		t.Errorf("Select(no task) = (%q,%v), want (\"\",false)", model, ok)
	}
	if model, ok := rk.Select(ctxFor(qwenctx.Unattributed)); model != "" || ok {
		t.Errorf("Select(unattributed) = (%q,%v), want (\"\",false)", model, ok)
	}
}

// Cache: a hit within the TTL serves the prior result even after the
// underlying projection changes; once the TTL elapses the next Select
// re-queries and reflects the change.
func TestSelect_CacheHitThenExpiry(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedPerf(t, pool, "classify_x", "qwen2.5-32b", 100, 50, 100_000)       // 0.50
	seedPerf(t, pool, "classify_x", "claude-sonnet-4-6", 100, 95, 300_000) // 0.95

	rk := NewRanker(pool, "qwen2.5-32b")
	fake := time.Now()
	rk.now = func() time.Time { return fake }

	if model, ok := rk.Select(ctxFor("classify_x")); model != "claude-sonnet-4-6" || !ok {
		t.Fatalf("first Select = (%q,%v), want (claude-sonnet-4-6,true)", model, ok)
	}
	// Remove the winning candidate — a fresh query would now stay on the default.
	if _, err := pool.DB().Exec(
		`DELETE FROM proj_inference_tool_model_performance WHERE task_id='classify_x' AND model_name='claude-sonnet-4-6'`); err != nil {
		t.Fatal(err)
	}
	// Within the TTL: the cached (stale) winner is still served.
	if model, ok := rk.Select(ctxFor("classify_x")); model != "claude-sonnet-4-6" || !ok {
		t.Errorf("cached Select = (%q,%v), want the stale (claude-sonnet-4-6,true) within TTL", model, ok)
	}
	// Past the TTL: re-query reflects the deletion → default, not switched.
	fake = fake.Add(cacheTTL + time.Second)
	if model, ok := rk.Select(ctxFor("classify_x")); ok {
		t.Errorf("post-TTL Select = (%q,true), want switched=false after the candidate was removed", model)
	}
}
