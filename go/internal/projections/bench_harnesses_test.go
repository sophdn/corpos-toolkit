package projections_test

import (
	"testing"

	"toolkit/internal/testutil"
)

// TestBenchHarnesses_FoldRebuildAndGrandfather covers the bench_harnesses
// projection added in chain 311 T7 Stage 6 P2-A:
//   - a full-payload BenchmarkForged event (flag_set_json present) folds into a
//     bench_harnesses row with the canonical columns;
//   - a PRE-BUMP BenchmarkForged event (no flag_set_json) is grandfathered —
//     skipped by the fold, so no row appears for it;
//   - a from-empty rebuild reproduces the full-payload row and still skips the
//     pre-bump event (the rebuild is the DR-replay coherence gate for bench).
func TestBenchHarnesses_FoldRebuildAndGrandfather(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")

	// Full-payload event (post-P2-A shape): reconstructable → folds to a row.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7e00-0001-7000-8000-000000000001', '2026-05-25 00:00:01', 'system', 'test',
		 'BenchmarkForged', 'bench', 'bench-full', 'p1',
		 '{"slug":"bench-full","binary_path":"go/bin/x","baseline_json_path":"x/baseline.json","parse_output_as":"json","timeout_ms":1234,"flag_set_json":"[{\"flag\":\"--http-url\",\"value\":\"http://x\"}]","gate_metrics":"n,none.envelope_bytes_p50"}',
		 '019e7e00-0001-7000-8000-000000000001', 1)`)

	// Pre-bump event (lacks flag_set_json): grandfathered → fold skips it.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7e00-0002-7000-8000-000000000002', '2026-05-25 00:00:02', 'system', 'test',
		 'BenchmarkForged', 'bench', 'bench-pre', 'p1',
		 '{"slug":"bench-pre","binary_path":"go/bin/y","baseline_json_path":"y/baseline.json","parse_output_as":"json","timeout_ms":60000}',
		 '019e7e00-0002-7000-8000-000000000002', 1)`)

	// A fresh test DB has no bench_harnesses rows; rebuild from events.
	mustRebuild(t, pool, []string{"bench_harnesses"})

	if got := tableCount(t, pool, "bench_harnesses"); got != 1 {
		t.Fatalf("bench_harnesses rows after rebuild = %d, want 1 (full folds, pre-bump grandfathered-skip)", got)
	}

	var (
		binaryPath, flagSetJSON, baseline, parseAs, gateMetrics, lastEventID string
		timeoutMs                                                            int
	)
	if err := pool.DB().QueryRow(`SELECT binary_path, flag_set_json, baseline_json_path,
		parse_output_as, timeout_ms, gate_metrics, last_event_id
		FROM bench_harnesses WHERE project_id='p1' AND slug='bench-full'`).Scan(
		&binaryPath, &flagSetJSON, &baseline, &parseAs, &timeoutMs, &gateMetrics, &lastEventID); err != nil {
		t.Fatalf("read bench-full row: %v", err)
	}
	if binaryPath != "go/bin/x" || baseline != "x/baseline.json" || parseAs != "json" || timeoutMs != 1234 {
		t.Errorf("bench-full canonical cols wrong: binary=%q baseline=%q parse=%q timeout=%d",
			binaryPath, baseline, parseAs, timeoutMs)
	}
	if flagSetJSON != `[{"flag":"--http-url","value":"http://x"}]` {
		t.Errorf("bench-full flag_set_json = %q", flagSetJSON)
	}
	if gateMetrics != "n,none.envelope_bytes_p50" {
		t.Errorf("bench-full gate_metrics = %q", gateMetrics)
	}
	if lastEventID != "019e7e00-0001-7000-8000-000000000001" {
		t.Errorf("bench-full last_event_id = %q (fold should stamp the materializing event)", lastEventID)
	}

	// The grandfathered pre-bump event must NOT have produced a row.
	var n int
	if err := pool.DB().QueryRow(
		`SELECT count(*) FROM bench_harnesses WHERE slug='bench-pre'`).Scan(&n); err != nil {
		t.Fatalf("count bench-pre: %v", err)
	}
	if n != 0 {
		t.Errorf("pre-bump bench-pre produced %d rows, want 0 (must be grandfathered-skipped)", n)
	}

	// Rebuild again — idempotent, still exactly the one reconstructable row.
	mustRebuild(t, pool, []string{"bench_harnesses"})
	if got := tableCount(t, pool, "bench_harnesses"); got != 1 {
		t.Fatalf("bench_harnesses rows after second rebuild = %d, want 1 (idempotent)", got)
	}
}
