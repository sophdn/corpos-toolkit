package projections_test

import (
	"testing"

	"toolkit/internal/testutil"
)

// TestBenchmarkResults_RebuildFromEmpty seeds synthetic
// BenchmarkRunCompleted events, captures the rebuilt checksum of
// proj_benchmark_results, TRUNCATEs the projection, runs RebuildFromEmpty
// again, asserts the post-rebuild checksum matches byte-for-byte.
//
// benchmark_provenance is still required (it's the FK target for the
// projection's provenance_id column, kept post-T6) so the test seeds it
// up front.
func TestBenchmarkResults_RebuildFromEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")

	mustExec(t, pool, `INSERT INTO benchmark_provenance
		(id, run_id, model_id, model_version, prompt_template_hash, corpus_hash, retriever_version, retriever_config_hash, seed, env_hash, started_event_id)
		VALUES ('prov-1', 'run-1', 'opus', '4-7', 'phash', 'chash', 'rv', 'rch', 42, 'envhash', 'evt-start')`)

	// Post-T6: rebuild replays events; the retired benchmark_results CRUD
	// table is gone. Seed synthetic BenchmarkRunCompleted events whose
	// result_columns payload mirrors the per-row metadata.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7d00-0001-7000-8000-000000000001', '2026-05-21 00:00:01', 'system', 'test',
		 'BenchmarkRunCompleted', 'benchmark_run', 'run-1', 'p1',
		 '{"run_id":"run-1","wall_clock_ms":1234,"benchmark_result_id":"br-1","project_id":"p1","scenario_id":"scn-1","provenance_id":"prov-1","run_at":1700000000,"result_columns":{"tool_name":"tool-a","model_name":"opus","layer":"l6","task_shape":"Extract","task_id":"task-x","run_shape":"happy","invocation_ok":true,"invoked_contextually":true}}',
		 '019e7d00-0001-7000-8000-000000000001', 1),
		('019e7d00-0002-7000-8000-000000000002', '2026-05-21 00:00:02', 'system', 'test',
		 'BenchmarkRunCompleted', 'benchmark_run', 'run-1', 'p1',
		 '{"run_id":"run-1","wall_clock_ms":2345,"benchmark_result_id":"br-2","project_id":"p1","scenario_id":"scn-2","provenance_id":"prov-1","run_at":1700000100,"result_columns":{"tool_name":"tool-b","model_name":"opus","layer":"l5","task_shape":"Classify","task_id":"task-y","run_shape":"happy","invocation_ok":false,"invoked_contextually":true}}',
		 '019e7d00-0002-7000-8000-000000000002', 1)`)

	mustExec(t, pool, `DELETE FROM proj_benchmark_results`)
	mustRebuild(t, pool, []string{"benchmark_results"})
	reference := tableChecksum(t, pool, "proj_benchmark_results")

	mustExec(t, pool, `DELETE FROM proj_benchmark_results`)
	mustRebuild(t, pool, []string{"benchmark_results"})
	after := tableChecksum(t, pool, "proj_benchmark_results")
	if reference != after {
		t.Fatalf("proj_benchmark_results checksum drift: reference=%s after=%s", reference, after)
	}

	if got := tableCount(t, pool, "proj_benchmark_results"); got != 2 {
		t.Errorf("proj_benchmark_results rows = %d, want 2", got)
	}
}
