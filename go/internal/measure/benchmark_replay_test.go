package measure_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/measure"
)

// Pre-Phase-7 (T5) tests used a benchmarks-stub.sh subprocess to
// simulate the Rust `target/release/benchmarks --replay` binary. That
// shape is gone: HandleBenchmarkReplay now calls benchmarks.RunReplay
// in-process. The round-trip tests below exercise the in-process path
// against a real testutil pool + fold hook.

// seedOriginalRunForReplay inserts a benchmark_provenance row + a fully
// populated proj_benchmark_results row that the in-process replay will
// re-emit. The provenance carries a UUIDv7-shaped started_event_id so
// the BenchmarkRunStarted re-emit's caused_by_event_id pattern check
// passes.
func seedOriginalRunForReplay(t *testing.T, pool *db.Pool) (rowID, runID string) {
	t.Helper()
	ctx := context.Background()
	rowID = "original-row"
	runID = "original-run"
	provID := seedProvenance(t, pool, runID)
	const insert = `INSERT INTO proj_benchmark_results
		(id, project_id, scenario_id, tool_name, model_name, run_id, run_at,
		 wall_clock_ms, invoked_contextually, invocation_ok, task_id,
		 task_shape, run_shape, accuracy_score, honesty_score,
		 ranking_quality_score, within_budget_score, args_match,
		 extracted_args, interpretation_ok, detected_tool, notes,
		 layer, provenance_id)
		VALUES (?, 'mcp-servers', 'scn-replay', 'tool', 'qwen2.5-32b', ?, 1715600000,
		        500, 1, 1, 'task-x',
		        'Classify', 'production', 0.95, 1.0,
		        NULL, NULL, NULL,
		        NULL, NULL, NULL, NULL,
		        'e4', ?)`
	if _, err := pool.DB().ExecContext(ctx, insert, rowID, runID, provID); err != nil {
		t.Fatalf("seedOriginalRunForReplay: %v", err)
	}
	return
}

func TestHandleBenchmarkReplay_MissingRowID(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()
	out, err := measure.HandleBenchmarkReplay(ctx, deps, "mcp-servers", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if !strings.Contains(out.Error, "params.row_id") {
		t.Fatalf("expected error to flag missing row_id, got %q", out.Error)
	}
}

func TestHandleBenchmarkReplay_RowNotFound(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()
	out, err := measure.HandleBenchmarkReplay(ctx, deps, "mcp-servers",
		json.RawMessage(`{"row_id":"does-not-exist"}`))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if !strings.Contains(out.Error, "not found") {
		t.Fatalf("expected error to say not found, got %q", out.Error)
	}
}

func TestHandleBenchmarkReplay_LegacyRowWithoutProvenanceRejected(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()
	// Post-T6: the CRUD benchmark_results table + its require-provenance
	// trigger are gone, so seeding a legacy row is now a plain INSERT into
	// proj_benchmark_results with NULL provenance_id (the projection has
	// no equivalent trigger; the handler-level guard is what we exercise).
	if _, err := deps.Pool.DB().ExecContext(ctx,
		`INSERT INTO proj_benchmark_results (id, project_id, scenario_id, tool_name, model_name,
		    run_at, wall_clock_ms, invoked_contextually, invocation_ok)
		 VALUES ('legacy', 'mcp-servers', 'scn-legacy', 'tool', 'm', 0, 0, 1, 1)`); err != nil {
		t.Fatal(err)
	}

	out, err := measure.HandleBenchmarkReplay(ctx, deps, "mcp-servers",
		json.RawMessage(`{"row_id":"legacy"}`))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if !strings.Contains(out.Error, "no provenance") {
		t.Fatalf("expected legacy-row error to mention 'no provenance', got %q", out.Error)
	}
}

func TestHandleBenchmarkReplay_InProcessRoundTripIdentical(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()
	rowID, _ := seedOriginalRunForReplay(t, deps.Pool)

	out, err := measure.HandleBenchmarkReplay(ctx, deps, "mcp-servers",
		json.RawMessage(`{"row_id":"`+rowID+`"}`))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected error envelope: %q", out.Error)
	}
	if !out.OK {
		t.Fatalf("want ok=true, got %+v", out)
	}
	if out.ReplayRowID == "" || out.ReplayRunID == "" {
		t.Errorf("replay ids missing: row=%q run=%q", out.ReplayRowID, out.ReplayRunID)
	}
	if !out.Identical {
		t.Fatalf("expected identical (copy-of-original), got diff: %s", out.Diff)
	}
	// The replay should have a new ID, distinct from the original.
	if out.ReplayRowID == rowID {
		t.Errorf("replay_row_id should not equal original row id")
	}

	// Verify the row landed in proj_benchmark_results with run_shape='replay'.
	var runShape string
	if err := deps.Pool.DB().QueryRowContext(ctx,
		`SELECT run_shape FROM proj_benchmark_results WHERE id = ?`, out.ReplayRowID,
	).Scan(&runShape); err != nil {
		t.Fatalf("query replay row run_shape: %v", err)
	}
	if runShape != "replay" {
		t.Errorf("replay row run_shape: got %q, want 'replay'", runShape)
	}
}
