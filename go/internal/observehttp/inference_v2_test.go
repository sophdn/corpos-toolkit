package observehttp

import (
	"fmt"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// seedQwenWithTime stamps an inference_invocations row with a specific
// created_at — the v2 endpoints care about per-day buckets and warmup
// thresholds, which the default `datetime('now')` doesn't let us control.
// (Post-T12 cutover the endpoints read inference_invocations; the fixture
// writes the same columns it always did — success/error_class take their
// defaults, which the predicate-based success_rate doesn't read — so the
// characterization goldens stay byte-identical. The name is kept for the
// large existing call-site set.)
func seedQwenWithTime(t *testing.T, pool *db.Pool, taskID, model string, latencyMs int64, inputTokens, outputTokens *int64, at time.Time) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO inference_invocations
			(task_id, model_name, latency_ms, input_tokens, output_tokens, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		taskID, model, latencyMs, inputTokens, outputTokens, at.UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}
}

func TestInferenceHealthCards_EmptyReturnsEmptyArray(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if len(got) != 0 {
		t.Errorf("expected empty array, got %d cards", len(got))
	}
}

func TestInferenceHealthCards_OneTaskUnderWarmup(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// 10 calls — under both warmupMinCallsForP99 (100) and
	// warmupMinCallsForSuccessRate (20). p50/p95 should compute; p99 +
	// success_rate are warming-up.
	for i := 0; i < 10; i++ {
		seedQwenWithTime(t, pool, "task-warmup", "qwen2.5-32b", int64(100+i*10), nil, nil, now.Add(-time.Duration(i)*time.Minute))
	}
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if len(got) != 1 {
		t.Fatalf("expected 1 card, got %d", len(got))
	}
	c := got[0]
	if c.TaskID != "task-warmup" {
		t.Errorf("task_id: %s", c.TaskID)
	}
	if c.CallCount != 10 {
		t.Errorf("call_count: %d", c.CallCount)
	}
	if c.P50LatencyMS == nil {
		t.Error("p50 should be populated for 10 calls")
	}
	if c.P95LatencyMS == nil {
		t.Error("p95 should be populated for 10 calls")
	}
	if c.P99LatencyMS != nil {
		t.Errorf("p99 should be NULL under warmup (got %v)", *c.P99LatencyMS)
	}
	if !c.WarmingUp.P99 {
		t.Error("warming_up.p99 should be true for 10 calls < threshold 100")
	}
	if c.SuccessRate != nil {
		t.Errorf("success_rate should be NULL under warmup (got %v)", *c.SuccessRate)
	}
	if !c.WarmingUp.SuccessRate {
		t.Error("warming_up.success_rate should be true for 10 calls < threshold 20")
	}
}

func TestInferenceHealthCards_PercentilesAndSuccessRate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// 100 calls — clears both warmup thresholds. All return output
	// tokens so the default success predicate marks every call success.
	tokens := int64(50)
	for i := 0; i < 100; i++ {
		latency := int64(100 + i) // 100..199
		seedQwenWithTime(t, pool, "task-healthy", "qwen2.5-32b", latency, &tokens, &tokens, now.Add(-time.Duration(i)*time.Minute))
	}
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if len(got) != 1 {
		t.Fatalf("expected 1 card, got %d", len(got))
	}
	c := got[0]
	if c.CallCount != 100 {
		t.Errorf("call_count: %d", c.CallCount)
	}
	if c.P99LatencyMS == nil {
		t.Fatal("p99 should populate at 100 calls (warmup threshold inclusive)")
	}
	// p99 of 100..199 by nearest-rank R-1 is the 99th element (index 98) → 198.
	if *c.P99LatencyMS != 198 {
		t.Errorf("p99 = %d, want 198", *c.P99LatencyMS)
	}
	if c.SuccessRate == nil || *c.SuccessRate != 1.0 {
		t.Errorf("success_rate = %v, want 1.0 (every call has output_tokens)", c.SuccessRate)
	}
	if c.WarmingUp.P99 || c.WarmingUp.SuccessRate {
		t.Errorf("warming_up flags should be false at 100 calls: %+v", c.WarmingUp)
	}
}

func TestInferenceHealthCards_ModelBreakdown(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	seedQwenWithTime(t, pool, "task-multi", "qwen2.5-32b", 100, nil, nil, now)
	seedQwenWithTime(t, pool, "task-multi", "qwen2.5-32b", 200, nil, nil, now.Add(-time.Hour))
	seedQwenWithTime(t, pool, "task-multi", "qwen-14b", 50, nil, nil, now.Add(-2*time.Hour))
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if len(got) != 1 {
		t.Fatalf("expected 1 card, got %d", len(got))
	}
	if len(got[0].ModelBreakdown) != 2 {
		t.Fatalf("expected 2 models, got %d", len(got[0].ModelBreakdown))
	}
	// Models are alphabetically ordered by SQL ORDER BY.
	var qwen32, qwen14 *ModelStat
	for i := range got[0].ModelBreakdown {
		m := &got[0].ModelBreakdown[i]
		switch m.ModelName {
		case "qwen2.5-32b":
			qwen32 = m
		case "qwen-14b":
			qwen14 = m
		}
	}
	if qwen32 == nil || qwen32.CallCount != 2 {
		t.Errorf("qwen2.5-32b model_stat wrong: %+v", qwen32)
	}
	if qwen14 == nil || qwen14.CallCount != 1 {
		t.Errorf("qwen-14b model_stat wrong: %+v", qwen14)
	}
}

func TestInferenceHealthCards_LastCallAtPopulated(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	mostRecent := now.Add(-30 * time.Minute)
	seedQwenWithTime(t, pool, "task-recency", "qwen2.5-32b", 100, nil, nil, now.Add(-2*time.Hour))
	seedQwenWithTime(t, pool, "task-recency", "qwen2.5-32b", 100, nil, nil, mostRecent)
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if got[0].LastCallAt == nil {
		t.Fatal("last_call_at should be populated")
	}
}

func TestInferenceHealthCards_WindowFilters(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// One row inside the default 7d window; one row well outside (30d).
	seedQwenWithTime(t, pool, "task-w", "qwen2.5-32b", 100, nil, nil, now.Add(-1*time.Hour))
	seedQwenWithTime(t, pool, "task-w", "qwen2.5-32b", 999, nil, nil, now.Add(-30*24*time.Hour))
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards?window_days=7", &got)
	if len(got) != 1 || got[0].CallCount != 1 {
		t.Fatalf("default 7d window should see 1 call, got cards=%d count=%d", len(got), func() int64 {
			if len(got) > 0 {
				return got[0].CallCount
			}
			return 0
		}())
	}
	// Wider window picks up both.
	getJSON(t, srv, "/inference/health-cards?window_days=60", &got)
	if got[0].CallCount != 2 {
		t.Errorf("60d window should see 2 calls, got %d", got[0].CallCount)
	}
}

func TestInferenceHealthCards_SuccessPredicateDefault(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	tokens := int64(5)
	for i := 0; i < 25; i++ {
		// Even i: has output_tokens, latency > 0 → success.
		// Odd i: latency_ms = 0 → fails default predicate.
		var lat int64
		var outPtr *int64
		if i%2 == 0 {
			lat = 100
			outPtr = &tokens
		} else {
			lat = 0
			outPtr = nil
		}
		seedQwenWithTime(t, pool, "task-x", "qwen2.5-32b", lat, nil, outPtr, now.Add(-time.Duration(i)*time.Minute))
	}
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if got[0].SuccessRate == nil {
		t.Fatal("success_rate should populate at 25 calls (≥ threshold 20)")
	}
	// 13 even out of 25 → 13/25 = 0.52.
	if got[0].SuccessRate == nil || *got[0].SuccessRate < 0.50 || *got[0].SuccessRate > 0.54 {
		t.Errorf("success_rate %v, want ~0.52", got[0].SuccessRate)
	}
	if got[0].SuccessRateBasis != defaultSuccessPredicate.Description {
		t.Errorf("success_rate_basis: %q", got[0].SuccessRateBasis)
	}
}

func TestInferenceHealthCards_SuccessPredicateClassify(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// Seed a benchmark_results row indicating high accuracy for the
	// classify_ task. The classify predicate fires when accuracy > 0.5.
	mustSeedProject(t, pool, "p")
	if _, err := pool.DB().Exec(`INSERT INTO benchmark_provenance
		(id, run_id, model_id, model_version, prompt_template_hash, corpus_hash,
		 retriever_version, retriever_config_hash, seed, env_hash, started_event_id)
		VALUES ('prov-1', 'r-1', 'qwen', 'v1', 'p', 'c', 'r', 'rc', 0, 'e', 'ev-1')`); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
	if _, err := pool.DB().Exec(`INSERT INTO proj_benchmark_results
		(id, project_id, scenario_id, tool_name, model_name, run_at, wall_clock_ms,
		 invocation_ok, task_id, accuracy_score, provenance_id)
		VALUES ('br-1', 'p', 's', 'classify_x', 'qwen', ?, 100, 1, 'classify_x', 0.9, 'prov-1')`,
		now.Unix()); err != nil {
		t.Fatalf("seed proj_benchmark_results: %v", err)
	}
	for i := 0; i < 25; i++ {
		seedQwenWithTime(t, pool, "classify_x", "qwen2.5-32b", 100, nil, nil, now.Add(-time.Duration(i)*time.Minute))
	}
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if got[0].SuccessRate == nil || *got[0].SuccessRate != 1.0 {
		t.Errorf("classify_ predicate with accuracy=0.9 should give success_rate=1.0, got %v", got[0].SuccessRate)
	}
	if got[0].SuccessRateBasis != classifyPredicate.Description {
		t.Errorf("expected classify basis, got: %q", got[0].SuccessRateBasis)
	}
}

func TestInferenceSparklines_PerDayBuckets(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// Three days of activity: today (10 calls), yesterday (6 calls),
	// 5 days ago (3 calls — below the per-day p95 threshold of 5).
	//
	// Anchor each day's seeds to NOON of the target day, not `now - i minutes`:
	// when now is near a midnight boundary the minute-offsets spilled the
	// "today" batch into yesterday's date() bucket → an extra bucket → flake
	// ("expected 3, got 4"). Noon ± 9 minutes stays within the day. The
	// sparkline query has no upper time bound (created_at >= window-start only),
	// so a noon-today timestamp is safe even when the test runs before noon.
	dayNoon := func(daysAgo int) time.Time {
		d := now.AddDate(0, 0, -daysAgo)
		return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, d.Location())
	}
	for i := 0; i < 10; i++ {
		seedQwenWithTime(t, pool, "task-s", "qwen2.5-32b", int64(100+i), nil, nil, dayNoon(0).Add(-time.Duration(i)*time.Minute))
	}
	for i := 0; i < 6; i++ {
		seedQwenWithTime(t, pool, "task-s", "qwen2.5-32b", int64(200+i), nil, nil, dayNoon(1).Add(-time.Duration(i)*time.Minute))
	}
	for i := 0; i < 3; i++ {
		seedQwenWithTime(t, pool, "task-s", "qwen2.5-32b", int64(300+i), nil, nil, dayNoon(5).Add(-time.Duration(i)*time.Minute))
	}
	srv := newTestServer(t, pool)
	var got []Sparkline
	getJSON(t, srv, "/inference/sparklines?window_days=7", &got)
	if len(got) != 1 {
		t.Fatalf("expected 1 sparkline series, got %d", len(got))
	}
	s := got[0]
	if len(s.Buckets) != 3 {
		t.Fatalf("expected 3 day buckets, got %d", len(s.Buckets))
	}
	// Day with 3 calls has p95 = nil (below 5-call floor).
	for _, b := range s.Buckets {
		if b.CallCount == 3 && b.P95LatencyMS != nil {
			t.Errorf("day with 3 calls should have NULL p95 (below 5-call floor): %+v", b)
		}
		if b.CallCount >= 5 && b.P95LatencyMS == nil {
			t.Errorf("day with %d calls should have populated p95: %+v", b.CallCount, b)
		}
	}
}

func TestInferenceSparklines_TaskFilter(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	seedQwenWithTime(t, pool, "task-a", "qwen2.5-32b", 100, nil, nil, now)
	seedQwenWithTime(t, pool, "task-b", "qwen2.5-32b", 200, nil, nil, now)
	srv := newTestServer(t, pool)
	var got []Sparkline
	getJSON(t, srv, "/inference/sparklines?task_id=task-a", &got)
	if len(got) != 1 || got[0].TaskID != "task-a" {
		t.Errorf("task_id filter returned wrong shape: %+v", got)
	}
}

func TestNearestRank_ExactBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   []int64
		pct  int
		want int64
	}{
		{"empty", nil, 50, 0},
		{"one element", []int64{42}, 50, 42},
		{"p50 of 1..10", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 50, 5},
		{"p95 of 1..10", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 95, 10},
		{"p99 of 1..100", makeRange(1, 100), 99, 99},
		// Clamp guards (defensive for pcts the production callers never
		// pass — they only ask 50/95/99 — but pinned so a nearestRank
		// refactor preserves them). rank floors at 1 and ceilings at len.
		{"pct 0 clamps to first", []int64{7, 8, 9}, 0, 7},
		{"pct 100 is last", []int64{7, 8, 9}, 100, 9},
		{"pct over 100 clamps to last", []int64{7, 8, 9}, 150, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nearestRank(c.in, c.pct)
			if got != c.want {
				t.Errorf("nearestRank pct=%d: got %d, want %d", c.pct, got, c.want)
			}
		})
	}
}

func makeRange(lo, hi int64) []int64 {
	out := make([]int64, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

// mustSeedProject inserts a project row so FK constraints on
// benchmark_results.project_id are satisfied for the classify-predicate
// test fixture.
func mustSeedProject(t *testing.T, pool *db.Pool, id string) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT OR IGNORE INTO projects (id, name, path) VALUES (?, ?, ?)`,
		id, id, fmt.Sprintf("/tmp/%s", id),
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}
