package projections_test

import (
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// Chain 2 (telemetry-success-model-unification) step-7 densification for the
// MATERIALIZED both-layers model. The step-2 net pinned outcome success through
// the read-time predicate path; these pin it against the new structure that
// path was replaced by — proj_inference_call_success (per-call Layer-2) and the
// proj_inference_tool_model_performance.outcome_success_count rollup — across
// the join boundaries the materialization introduced. All describe CURRENT
// (post-refactor) behavior.

// seedBenchmarkRow inserts one provenance + proj_benchmark_results pair keyed by
// task_id with a caller-supplied id suffix, so a test can seed several benchmark
// rows for one task (the classify outcome reads proj_benchmark_results directly;
// RebuildAll over an explicit list does not rebuild benchmark_results, so these
// directly-seeded rows survive).
func seedBenchmarkRow(t *testing.T, pool *db.Pool, project, taskID, suffix string, accuracy float64, runAt time.Time) {
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

type callSuccessRow struct {
	OutcomeSuccess int
	CallSuccess    int
	OutcomeKind    string
}

// firstCallSuccessRow returns the proj_inference_call_success row for a task
// (tests seed one row per task so the choice is unambiguous).
func firstCallSuccessRow(t *testing.T, pool *db.Pool, taskID string) callSuccessRow {
	t.Helper()
	var r callSuccessRow
	if err := pool.DB().QueryRow(
		`SELECT outcome_success, call_success, outcome_kind
		   FROM proj_inference_call_success WHERE task_id = ? LIMIT 1`, taskID,
	).Scan(&r.OutcomeSuccess, &r.CallSuccess, &r.OutcomeKind); err != nil {
		t.Fatalf("scan proj_inference_call_success for %q: %v", taskID, err)
	}
	return r
}

// The two layers are independent: call_success is the emit-time liveness flag;
// outcome_success is the materialized predicate. A row can be call-success=1
// yet outcome-success=0 (default predicate: output_tokens NULL) and vice versa.
func TestInferenceCallSuccess_OutcomeByPredicateClass(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p")
	now := time.Now().UTC()
	five := int64(5)

	// default predicate arms (kind 'default'), call_success deliberately 1 so
	// the layers are seen to diverge from outcome.
	seedInference(t, pool, "default-both-ok", "qwen2.5-32b", 100, &five, &five, 1, "", now) // tokens + latency>0 → outcome 1
	seedInference(t, pool, "default-out-null", "qwen2.5-32b", 100, &five, nil, 1, "", now)  // output NULL → outcome 0
	seedInference(t, pool, "default-lat-zero", "qwen2.5-32b", 0, &five, &five, 1, "", now)  // latency 0 → outcome 0

	// classify (kind 'classify'): a passing benchmark → 1; none → 0.
	seedBenchmarkRow(t, pool, "p", "classify_pass", "only", 0.9, now)
	seedInference(t, pool, "classify_pass", "qwen2.5-32b", 100, nil, nil, 1, "", now)
	seedInference(t, pool, "classify_none", "qwen2.5-32b", 100, nil, nil, 1, "", now) // no benchmark → 0

	// vault (kind 'vault-rerank-retrieve'): proximate vault_search results>0 → 1.
	seedInference(t, pool, "vault-rerank-retrieve", "qwen2.5-32b", 1000, nil, nil, 1, "", now)
	testutil.SeedGroundingEvent(t, pool,
		testutil.WithGroundingAction("vault_search"),
		testutil.WithGroundingResultsCount(3),
		testutil.WithGroundingCreatedAt(now.Format(time.RFC3339)),
	)

	mustRebuildAll(t, pool, []string{"inference_call_success"})

	cases := []struct {
		task        string
		wantOutcome int
		wantKind    string
	}{
		{"default-both-ok", 1, "default"},
		{"default-out-null", 0, "default"},
		{"default-lat-zero", 0, "default"},
		{"classify_pass", 1, "classify"},
		{"classify_none", 0, "classify"},
		{"vault-rerank-retrieve", 1, "vault-rerank-retrieve"},
	}
	for _, c := range cases {
		t.Run(c.task, func(t *testing.T) {
			r := firstCallSuccessRow(t, pool, c.task)
			if r.OutcomeKind != c.wantKind {
				t.Errorf("outcome_kind = %q, want %q", r.OutcomeKind, c.wantKind)
			}
			if r.OutcomeSuccess != c.wantOutcome {
				t.Errorf("outcome_success = %d, want %d", r.OutcomeSuccess, c.wantOutcome)
			}
			if r.CallSuccess != 1 {
				t.Errorf("call_success = %d, want 1 (carried from inference_invocations.success)", r.CallSuccess)
			}
		})
	}
}

// The materialized classify arm is ANY-row, not "latest" (bug 948): the
// materialized form drops the inert ORDER BY/LIMIT. Pins it at the projection
// level — a newer failing benchmark plus an older passing one → outcome 1.
func TestInferenceCallSuccess_ClassifyAnyRowNotLatest(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p")
	now := time.Now().UTC()
	seedBenchmarkRow(t, pool, "p", "classify_multi", "newer", 0.3, now)
	seedBenchmarkRow(t, pool, "p", "classify_multi", "older", 0.9, now.Add(-24*time.Hour))
	seedInference(t, pool, "classify_multi", "qwen2.5-32b", 100, nil, nil, 1, "", now)

	mustRebuildAll(t, pool, []string{"inference_call_success"})

	if r := firstCallSuccessRow(t, pool, "classify_multi"); r.OutcomeSuccess != 1 {
		t.Errorf("outcome_success = %d, want 1 — the older 0.9 row satisfies EXISTS(accuracy>0.5); "+
			"a 0 would mean the materialized arm honors 'latest' (newer 0.3)", r.OutcomeSuccess)
	}
}

// The vault arm's latency-scaled proximity window survives materialization:
// with latency 1000ms the window is 3s — a grounding row 2s away hits, 5s misses.
func TestInferenceCallSuccess_VaultProximityWindow(t *testing.T) {
	run := func(t *testing.T, offset time.Duration) int {
		pool := testutil.NewTestDB(t)
		base := time.Now().UTC()
		seedInference(t, pool, "vault-rerank-retrieve", "qwen2.5-32b", 1000, nil, nil, 1, "", base)
		testutil.SeedGroundingEvent(t, pool,
			testutil.WithGroundingAction("vault_search"),
			testutil.WithGroundingResultsCount(3),
			testutil.WithGroundingCreatedAt(base.Add(offset).Format(time.RFC3339)),
		)
		mustRebuildAll(t, pool, []string{"inference_call_success"})
		return firstCallSuccessRow(t, pool, "vault-rerank-retrieve").OutcomeSuccess
	}
	t.Run("inside_window_hits", func(t *testing.T) {
		if got := run(t, 2*time.Second); got != 1 {
			t.Errorf("outcome_success = %d, want 1 (2s ≤ 3s window)", got)
		}
	})
	t.Run("outside_window_misses", func(t *testing.T) {
		if got := run(t, 5*time.Second); got != 0 {
			t.Errorf("outcome_success = %d, want 0 (5s > 3s window)", got)
		}
	})
}

// The SQL dispatch (substr(task_id,1,9)='classify_') must agree with the Go
// registry's lookupSuccessPredicate dispatch (taskID[:9]=="classify_"), which
// TestLookupSuccessPredicate_Registry pins on the Go side. Pin the same boundary
// task_ids here on the SQL side so any drift between the two trips one test.
func TestInferenceCallSuccess_DispatchMatchesRegistry(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now().UTC()
	cases := map[string]string{
		"vault-rerank-retrieve": "vault-rerank-retrieve", // exact registry hit
		"classify_x":            "classify",              // prefix hit
		"classify_":             "classify",              // exactly 9 chars — boundary IN
		"classify":              "default",               // 8 chars — boundary OUT
		"classify-dash":         "default",               // 9th char is '-', not '_'
		"knowledge-search":      "default",               // unregistered
	}
	for task := range cases {
		seedInference(t, pool, task, "qwen2.5-32b", 100, nil, nil, 1, "", now)
	}
	mustRebuildAll(t, pool, []string{"inference_call_success"})

	for task, wantKind := range cases {
		if r := firstCallSuccessRow(t, pool, task); r.OutcomeKind != wantKind {
			t.Errorf("task %q outcome_kind = %q, want %q (SQL dispatch must match the Go registry)", task, r.OutcomeKind, wantKind)
		}
	}
}

// The rollup proj_inference_tool_model_performance.outcome_success_count is the
// per-(task,model) SUM of the materialized per-row outcome, alongside the
// call-level success_count. Pin both layers' rollup independently.
func TestInferenceCallSuccess_RollupOutcomeCount(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p")
	now := time.Now().UTC()
	five := int64(5)

	// classify_pass on qwen: 3 calls, a passing benchmark → every row outcome 1
	// (classify is per-task). call_success mix (2 ok, 1 error) is independent.
	seedBenchmarkRow(t, pool, "p", "classify_pass", "only", 0.9, now)
	seedInference(t, pool, "classify_pass", "qwen2.5-32b", 100, nil, nil, 1, "", now)
	seedInference(t, pool, "classify_pass", "qwen2.5-32b", 100, nil, nil, 1, "", now)
	seedInference(t, pool, "classify_pass", "qwen2.5-32b", 100, nil, nil, 0, "upstream_error", now)
	// classify_none: no benchmark → every row outcome 0, but call_success 1.
	seedInference(t, pool, "classify_none", "qwen2.5-32b", 100, &five, &five, 1, "", now)

	mustRebuildAll(t, pool, []string{"inference_call_success", "inference_tool_model_performance"})

	type rollup struct{ call, success, outcome int64 }
	read := func(task string) rollup {
		var r rollup
		if err := pool.DB().QueryRow(
			`SELECT call_count, success_count, outcome_success_count
			   FROM proj_inference_tool_model_performance WHERE task_id = ? AND model_name = 'qwen2.5-32b'`, task,
		).Scan(&r.call, &r.success, &r.outcome); err != nil {
			t.Fatalf("read rollup %q: %v", task, err)
		}
		return r
	}
	if got := read("classify_pass"); got != (rollup{call: 3, success: 2, outcome: 3}) {
		t.Errorf("classify_pass rollup = %+v, want {call:3 success:2 outcome:3}", got)
	}
	if got := read("classify_none"); got != (rollup{call: 1, success: 1, outcome: 0}) {
		t.Errorf("classify_none rollup = %+v, want {call:1 success:1 outcome:0} (call-level 1, outcome 0)", got)
	}
}

// Read-side invariant for the new projection: rebuilding twice over a populated
// table converges on the same rows (Fold == RebuildFromEmpty, no double-count).
func TestInferenceCallSuccess_RebuildIsByteIdentical(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p")
	now := time.Now().UTC()
	five := int64(5)
	seedBenchmarkRow(t, pool, "p", "classify_x", "only", 0.9, now)
	seedInference(t, pool, "classify_x", "qwen2.5-32b", 120, &five, &five, 1, "", now)
	seedInference(t, pool, "default-task", "claude-sonnet-4-6", 90, nil, nil, 0, "timeout", now)

	mustRebuildAll(t, pool, []string{"inference_call_success"})
	first := tableChecksum(t, pool, "proj_inference_call_success")
	mustRebuildAll(t, pool, []string{"inference_call_success"})
	second := tableChecksum(t, pool, "proj_inference_call_success")
	if first != second {
		t.Fatalf("proj_inference_call_success rebuild not byte-identical: %s → %s", first, second)
	}
}
