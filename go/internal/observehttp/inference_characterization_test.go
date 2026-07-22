package observehttp

import (
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// Characterization net (chain per-tool-per-model-observability T9, the
// refactor GATE) — the rejection and boundary input classes that the
// endpoint goldens don't cleanly surface. The goldens pin the populated /
// multi / hit cases; these pin the cases where a number must come out 0 or
// a warmup flag must flip, which is exactly where a relocation regression
// would hide. All assertions describe CURRENT behavior; none may be edited
// to make a later refactor pass (refactoring-discipline reflex 3).

// success_rate is gated at warmupMinCallsForSuccessRate (20) with a `>=`
// comparison. Pin both sides of the boundary: 20 computes, 19 warms.
func TestCharacterization_SuccessRateWarmupBoundary(t *testing.T) {
	now := time.Now()
	tokens := int64(5)
	t.Run("exactly_20_computes", func(t *testing.T) {
		pool := testutil.NewTestDB(t)
		for i := 0; i < 20; i++ {
			seedQwenWithTime(t, pool, "task-20", "qwen2.5-32b", 100, &tokens, &tokens, now.Add(-time.Duration(i)*time.Minute))
		}
		srv := newTestServer(t, pool)
		var got []HealthCard
		getJSON(t, srv, "/inference/health-cards", &got)
		if got[0].SuccessRate == nil {
			t.Fatal("success_rate must populate at exactly 20 calls (threshold is >=)")
		}
		if *got[0].SuccessRate != 1.0 {
			t.Errorf("success_rate = %v, want 1.0", *got[0].SuccessRate)
		}
		if got[0].WarmingUp.SuccessRate {
			t.Error("warming_up.success_rate must be false at exactly 20 calls")
		}
	})
	t.Run("19_still_warming", func(t *testing.T) {
		pool := testutil.NewTestDB(t)
		for i := 0; i < 19; i++ {
			seedQwenWithTime(t, pool, "task-19", "qwen2.5-32b", 100, &tokens, &tokens, now.Add(-time.Duration(i)*time.Minute))
		}
		srv := newTestServer(t, pool)
		var got []HealthCard
		getJSON(t, srv, "/inference/health-cards", &got)
		if got[0].SuccessRate != nil {
			t.Errorf("success_rate must be NULL at 19 calls (< threshold 20), got %v", *got[0].SuccessRate)
		}
		if !got[0].WarmingUp.SuccessRate {
			t.Error("warming_up.success_rate must be true at 19 calls")
		}
	})
}

// classify_* success counts a call only when the latest benchmark accuracy
// for the task is > 0.5. Pin the two rejection cases: a low score, and no
// benchmark row at all (the EXISTS subquery returns false → every call
// fails → success_rate 0.0).
func TestCharacterization_ClassifyPredicateRejections(t *testing.T) {
	now := time.Now()
	t.Run("accuracy_at_or_below_floor", func(t *testing.T) {
		pool := testutil.NewTestDB(t)
		seedProject(t, pool, "p")
		seedBenchmarkAccuracy(t, pool, "p", "classify_low", 0.5, now) // 0.5 is NOT > 0.5
		for i := 0; i < 25; i++ {
			seedQwenWithTime(t, pool, "classify_low", "qwen2.5-32b", 100, nil, nil, now.Add(-time.Duration(i)*time.Minute))
		}
		srv := newTestServer(t, pool)
		var got []HealthCard
		getJSON(t, srv, "/inference/health-cards", &got)
		if got[0].SuccessRate == nil {
			t.Fatal("success_rate should populate at 25 calls")
		}
		if *got[0].SuccessRate != 0.0 {
			t.Errorf("success_rate = %v, want 0.0 (accuracy 0.5 is not > 0.5)", *got[0].SuccessRate)
		}
		if got[0].SuccessRateBasis != classifyPredicate.Description {
			t.Errorf("basis = %q, want classify", got[0].SuccessRateBasis)
		}
	})
	t.Run("no_benchmark_row", func(t *testing.T) {
		pool := testutil.NewTestDB(t)
		for i := 0; i < 25; i++ {
			seedQwenWithTime(t, pool, "classify_orphan", "qwen2.5-32b", 100, nil, nil, now.Add(-time.Duration(i)*time.Minute))
		}
		srv := newTestServer(t, pool)
		var got []HealthCard
		getJSON(t, srv, "/inference/health-cards", &got)
		if got[0].SuccessRate == nil {
			t.Fatal("success_rate should populate at 25 calls")
		}
		if *got[0].SuccessRate != 0.0 {
			t.Errorf("success_rate = %v, want 0.0 (no benchmark row → EXISTS false)", *got[0].SuccessRate)
		}
	})
}

// vault-rerank-retrieve fails a call when no proximate vault_search
// grounding row exists. The kiwix-grounding case lives in
// inference_success_predicates_test; this pins the empty-substrate case
// (no grounding rows at all).
func TestCharacterization_VaultRerankNoGroundingMiss(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	for i := 0; i < 25; i++ {
		seedQwenWithTime(t, pool, "vault-rerank-retrieve", "qwen2.5-32b", 1000, nil, nil, now)
	}
	// No grounding rows seeded.
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if got[0].SuccessRate == nil {
		t.Fatal("success_rate should populate at 25 calls")
	}
	if *got[0].SuccessRate != 0.0 {
		t.Errorf("success_rate = %v, want 0.0 (no grounding rows to match)", *got[0].SuccessRate)
	}
}

// bug_count joins proj_current_bugs.qwen_task_id. Pin that a task with two
// attributed bugs reports 2, and a task with none reports 0 — the join is
// otherwise only incidentally exercised inside the health-cards golden.
func TestCharacterization_BugCountJoin(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p")
	now := time.Now()
	seedQwenWithTime(t, pool, "task-with-bugs", "qwen2.5-32b", 100, nil, nil, now)
	seedQwenWithTime(t, pool, "task-no-bugs", "qwen2.5-32b", 100, nil, nil, now)
	seedBugWithQwenTask(t, pool, "p", "b1", "task-with-bugs")
	seedBugWithQwenTask(t, pool, "p", "b2", "task-with-bugs")

	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	counts := map[string]int64{}
	for _, c := range got {
		counts[c.TaskID] = c.BugCount
	}
	if counts["task-with-bugs"] != 2 {
		t.Errorf("task-with-bugs bug_count = %d, want 2", counts["task-with-bugs"])
	}
	if counts["task-no-bugs"] != 0 {
		t.Errorf("task-no-bugs bug_count = %d, want 0", counts["task-no-bugs"])
	}
}

// The ?project= filter on health-cards scopes ONLY the bug_count join (the
// task list + latency/token aggregates are never project-scoped). Pin that
// partial-scoping quirk: a bug attributed under one project is counted
// without the filter but drops to 0 when filtering to a different project,
// while the card itself still appears.
func TestCharacterization_BugCountProjectScope(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "alpha")
	seedProject(t, pool, "beta")
	now := time.Now()
	seedQwenWithTime(t, pool, "scoped-task", "qwen2.5-32b", 100, nil, nil, now)
	seedBugWithQwenTask(t, pool, "alpha", "bug-a", "scoped-task")

	srv := newTestServer(t, pool)

	var unfiltered []HealthCard
	getJSON(t, srv, "/inference/health-cards", &unfiltered)
	if len(unfiltered) != 1 || unfiltered[0].BugCount != 1 {
		t.Fatalf("unfiltered: want 1 card with bug_count 1, got %+v", unfiltered)
	}

	var filtered []HealthCard
	getJSON(t, srv, "/inference/health-cards?project=beta", &filtered)
	if len(filtered) != 1 {
		t.Fatalf("card must still appear under a foreign project filter, got %d cards", len(filtered))
	}
	if filtered[0].BugCount != 0 {
		t.Errorf("bug_count under project=beta = %d, want 0 (bug is alpha's)", filtered[0].BugCount)
	}
}

// ── Chain 2 (success-model-unification) net densification ─────────────────
// The tests below pin the success-COMPUTATION input classes that Chain 1's
// endpoint goldens leave conflated or unpinned. Chain 2 materializes these
// read-time predicates into the projection; each class here is one the
// materialized join must reproduce (or change with a documented delta). All
// describe CURRENT behavior; none may be edited to make the unification pass.

// The default predicate is `output_tokens IS NOT NULL AND latency_ms > 0` —
// a two-condition AND. The health-cards golden's aaa-default-mixed fails BOTH
// conditions at once (its fail rows have latency 0 AND null output_tokens),
// so a unification that reproduced only ONE arm would still pass the golden.
// Pin each arm in isolation, plus the both-satisfied acceptance arm, so the
// AND is fully characterized.
func TestCharacterization_DefaultPredicateArms(t *testing.T) {
	now := time.Now()
	five := int64(5)
	seed := func(t *testing.T, taskID string, latency int64, in, out *int64) []HealthCard {
		pool := testutil.NewTestDB(t)
		for i := 0; i < 20; i++ { // ≥ warmup threshold so success_rate computes
			seedQwenWithTime(t, pool, taskID, "qwen2.5-32b", latency, in, out, now.Add(-time.Duration(i)*time.Minute))
		}
		srv := newTestServer(t, pool)
		var got []HealthCard
		getJSON(t, srv, "/inference/health-cards", &got)
		if len(got) != 1 || got[0].SuccessRate == nil {
			t.Fatalf("want 1 card with computed success_rate, got %+v", got)
		}
		if got[0].SuccessRateBasis != defaultSuccessPredicate.Description {
			t.Errorf("basis = %q, want default", got[0].SuccessRateBasis)
		}
		return got
	}
	t.Run("output_null_fails_even_with_latency", func(t *testing.T) {
		// input_tokens present, output_tokens NULL, latency > 0 → the
		// output_tokens arm fails alone → 0.
		got := seed(t, "default-output-null", 100, &five, nil)
		if *got[0].SuccessRate != 0.0 {
			t.Errorf("success_rate = %v, want 0.0 (output_tokens NULL fails the predicate even with latency>0)", *got[0].SuccessRate)
		}
	})
	t.Run("zero_latency_fails_even_with_output", func(t *testing.T) {
		// output_tokens present, latency_ms = 0 → the latency arm fails
		// alone → 0. Pins the latency_ms > 0 boundary at exactly 0.
		got := seed(t, "default-latency-zero", 0, &five, &five)
		if *got[0].SuccessRate != 0.0 {
			t.Errorf("success_rate = %v, want 0.0 (latency_ms=0 fails the predicate even with output_tokens set)", *got[0].SuccessRate)
		}
	})
	t.Run("both_satisfied_succeeds", func(t *testing.T) {
		// output_tokens set AND latency_ms = 1 (the boundary's success side) → 1.
		got := seed(t, "default-both-ok", 1, &five, &five)
		if *got[0].SuccessRate != 1.0 {
			t.Errorf("success_rate = %v, want 1.0 (output_tokens set AND latency_ms=1 satisfies both arms)", *got[0].SuccessRate)
		}
	})
}

// classify_* selection across MULTIPLE benchmark rows. The predicate's
// description says "latest benchmark_results.accuracy_score ... > 0.5", but
// the SQL is `EXISTS(... accuracy_score > 0.5 ORDER BY run_at DESC LIMIT 1)`
// — the ORDER BY/LIMIT inside EXISTS is inert (EXISTS only checks for ≥1
// matching row), so the ACTUAL behavior is "ANY benchmark row > 0.5", not
// "the latest". This pins that actual behavior: a NEWER failing benchmark
// (0.3) plus an OLDER passing one (0.9) yields success_rate 1.0. A faithful
// "latest" implementation would yield 0.0 — so this test is the tripwire
// that flags the description/code divergence for Chain 2's audit step.
func TestCharacterization_ClassifyAnyBenchmarkRowNotLatest(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p")
	now := time.Now()
	const taskID = "classify_multi"

	// Two benchmark rows for the same task with distinct ids + run_at:
	// newer = 0.3 (would fail "latest"), older = 0.9 (passes "any-row").
	seedClassifyBenchmarkRow(t, pool, "p", taskID, "newer", 0.3, now)
	seedClassifyBenchmarkRow(t, pool, "p", taskID, "older", 0.9, now.Add(-24*time.Hour))

	for i := 0; i < 25; i++ {
		seedQwenWithTime(t, pool, taskID, "qwen2.5-32b", 100, nil, nil, now.Add(-time.Duration(i)*time.Minute))
	}
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if len(got) != 1 || got[0].SuccessRate == nil {
		t.Fatalf("want 1 card with computed success_rate, got %+v", got)
	}
	if *got[0].SuccessRate != 1.0 {
		t.Errorf("success_rate = %v, want 1.0 — the older 0.9 row satisfies EXISTS(accuracy>0.5); "+
			"a 0.0 here would mean the predicate actually honors 'latest' (newer 0.3), contradicting current SQL", *got[0].SuccessRate)
	}
}

// seedClassifyBenchmarkRow inserts one provenance + proj_benchmark_results
// pair with a caller-supplied id suffix, so a test can seed SEVERAL benchmark
// rows for one task_id (seedBenchmarkAccuracy keys both rows by task_id and
// would collide on a second call).
func seedClassifyBenchmarkRow(t *testing.T, pool *db.Pool, project, taskID, suffix string, accuracy float64, runAt time.Time) {
	t.Helper()
	provID := "prov-" + taskID + "-" + suffix
	if _, err := pool.DB().Exec(`INSERT INTO benchmark_provenance
		(id, run_id, model_id, model_version, prompt_template_hash, corpus_hash,
		 retriever_version, retriever_config_hash, seed, env_hash, started_event_id)
		VALUES (?, ?, 'qwen', 'v1', 'p', 'c', 'r', 'rc', 0, 'e', ?)`,
		provID, "run-"+taskID+"-"+suffix, "ev-"+taskID+"-"+suffix); err != nil {
		t.Fatalf("seed provenance %q/%q: %v", taskID, suffix, err)
	}
	if _, err := pool.DB().Exec(`INSERT INTO proj_benchmark_results
		(id, project_id, scenario_id, tool_name, model_name, run_at, wall_clock_ms,
		 invocation_ok, task_id, accuracy_score, provenance_id)
		VALUES (?, ?, 's', ?, 'qwen', ?, 100, 1, ?, ?, ?)`,
		"br-"+taskID+"-"+suffix, project, taskID, runAt.Unix(), taskID, accuracy, provID); err != nil {
		t.Fatalf("seed proj_benchmark_results %q/%q: %v", taskID, suffix, err)
	}
}

// vault-rerank: a proximate vault_search grounding row whose results_count is
// 0 must NOT satisfy the predicate (the `ge.results_count > 0` clause). The
// existing hit test uses results_count 3; this pins the boundary at 0.
func TestCharacterization_VaultRerankZeroResultsMiss(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	for i := 0; i < 25; i++ {
		seedQwenWithTime(t, pool, "vault-rerank-retrieve", "qwen2.5-32b", 1000, nil, nil, now)
	}
	testutil.SeedGroundingEvent(t, pool,
		testutil.WithGroundingAction("vault_search"),
		testutil.WithGroundingResultsCount(0), // proximate + right action, but zero results
	)
	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)
	if len(got) != 1 || got[0].SuccessRate == nil {
		t.Fatalf("want 1 card with computed success_rate, got %+v", got)
	}
	if *got[0].SuccessRate != 0.0 {
		t.Errorf("success_rate = %v, want 0.0 (results_count=0 fails the predicate's `> 0` clause)", *got[0].SuccessRate)
	}
}

// vault-rerank proximity window. The predicate matches a grounding row only
// when ABS(ge.created_at - qi.created_at) <= latency_ms/1000 + 2 seconds —
// the latency-scaled tolerance that accommodates the two-pass search shape.
// With latency_ms=1000 the window is 3s. Pin both sides of that boundary:
// a grounding row 2s away hits; one 5s away misses. (No existing test pins
// this; the comment in inference_success_predicates_test claiming the time
// math lives in inference_retrieval_test is mistaken — that file tests the
// retrieval-health aggregate, not this predicate.)
func TestCharacterization_VaultRerankProximityWindowBoundary(t *testing.T) {
	const latencyMS = 1000 // → window = 1000/1000 + 2 = 3s
	run := func(t *testing.T, groundingOffset time.Duration) *float64 {
		pool := testutil.NewTestDB(t)
		base := time.Now().UTC()
		for i := 0; i < 25; i++ {
			seedQwenWithTime(t, pool, "vault-rerank-retrieve", "qwen2.5-32b", latencyMS, nil, nil, base)
		}
		testutil.SeedGroundingEvent(t, pool,
			testutil.WithGroundingAction("vault_search"),
			testutil.WithGroundingResultsCount(3),
			testutil.WithGroundingCreatedAt(base.Add(groundingOffset).Format(time.RFC3339)),
		)
		srv := newTestServer(t, pool)
		var got []HealthCard
		getJSON(t, srv, "/inference/health-cards", &got)
		if len(got) != 1 || got[0].SuccessRate == nil {
			t.Fatalf("want 1 card with computed success_rate, got %+v", got)
		}
		return got[0].SuccessRate
	}
	t.Run("inside_window_hits", func(t *testing.T) {
		if rate := run(t, 2*time.Second); *rate != 1.0 { // 2s ≤ 3s
			t.Errorf("success_rate = %v, want 1.0 (grounding 2s away is inside the 3s window)", *rate)
		}
	})
	t.Run("outside_window_misses", func(t *testing.T) {
		if rate := run(t, 5*time.Second); *rate != 0.0 { // 5s > 3s
			t.Errorf("success_rate = %v, want 0.0 (grounding 5s away is outside the 3s window)", *rate)
		}
	})
}
