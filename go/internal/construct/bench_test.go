package construct_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/construct"
	"toolkit/internal/db"
)

// benchRow is the authored-content column set both the forge path and the
// construct path must produce identically. created_at/updated_at and the
// last_event_* watermark pointers are intentionally excluded — they're
// projection bookkeeping that legitimately differs between a direct-written
// forge row (last_event_id=") and a fold-written construct row, exactly as
// chain-replay-verify excludes them.
type benchRow struct {
	binaryPath, flagSetJSON, baselineJSONPath, parseOutputAs, gateMetrics string
	timeoutMs                                                             int
}

func readBenchRow(t *testing.T, pool *db.Pool, slug string) benchRow {
	t.Helper()
	var r benchRow
	if err := pool.DB().QueryRow(`SELECT binary_path, flag_set_json, baseline_json_path,
		parse_output_as, timeout_ms, gate_metrics
		FROM bench_harnesses WHERE project_id='mcp-servers' AND slug = ?`, slug).Scan(
		&r.binaryPath, &r.flagSetJSON, &r.baselineJSONPath, &r.parseOutputAs, &r.timeoutMs, &r.gateMetrics); err != nil {
		t.Fatalf("read bench row %q: %v", slug, err)
	}
	return r
}

// TestCreateForgeBenchParity: the bench arm of construct.Create lands a
// bench_harnesses row byte-identical (on authored columns) to forge(bench).
// Both paths route field validation + flag_set normalization through the
// shared construct.ResolveBenchFields seam, so the canonical columns must match —
// including flag_set_json (the P2-A payload-bump field) and gate_metrics.
func TestCreateForgeBenchParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	const (
		binaryPath  = "go/bin/parity-bench"
		flagSet     = "--http-url http://localhost:3000 --project mcp-servers"
		baseline    = "x/testdata/baseline.json"
		gateMetrics = "n,none.envelope_bytes_p50"
	)

	if _, err := forgeCreateRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"bench","slug":"parity-bench-forge","binary_path":"`+binaryPath+
			`","flag_set":"`+flagSet+`","baseline_json_path":"`+baseline+
			`","gate_metrics":"`+gateMetrics+`"}`,
	)); err != nil {
		t.Fatalf("create(bench): %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	res, err := construct.Create(ctx, deps, "bench", "mcp-servers", construct.Input{
		Bench: &construct.BenchInput{
			Slug:             "parity-bench-record",
			BinaryPath:       binaryPath,
			FlagSet:          flagSet,
			BaselineJSONPath: baseline,
			GateMetrics:      gateMetrics,
		},
	})
	if err != nil {
		t.Fatalf("Create(bench): %v", err)
	}
	if res.EntitySlug != "parity-bench-record" {
		t.Fatalf("CreateResult.EntitySlug=%q, want parity-bench-record", res.EntitySlug)
	}
	if len(res.EventsEmitted) != 1 || res.EventsEmitted[0].Type != "BenchmarkForged" {
		t.Fatalf("CreateResult.EventsEmitted=%+v, want one BenchmarkForged", res.EventsEmitted)
	}
	if res.RoutingNote == "" {
		t.Errorf("CreateResult.RoutingNote empty; forge produces a registration note")
	}

	f := readBenchRow(t, pool, "parity-bench-forge")
	r := readBenchRow(t, pool, "parity-bench-record")
	if f != r {
		t.Fatalf("forge vs construct bench parity mismatch:\n  forge:     %+v\n  construct: %+v", f, r)
	}
	// Spot-check the canonical values landed (not just that they're equal).
	if r.parseOutputAs != "json" || r.timeoutMs != 60000 {
		t.Errorf("construct bench defaults wrong: parse_output_as=%q timeout_ms=%d (want json/60000)", r.parseOutputAs, r.timeoutMs)
	}
	if r.flagSetJSON == "" || r.gateMetrics != gateMetrics {
		t.Errorf("construct bench flag_set_json/gate_metrics wrong: flag_set_json=%q gate_metrics=%q", r.flagSetJSON, r.gateMetrics)
	}
}

// TestCreateBenchRejectsSchemaInputMismatch: schema "bench" with the wrong
// Input arm (or no arm) is rejected by the union discipline.
func TestCreateBenchRejectsSchemaInputMismatch(t *testing.T) {
	pool := openTestPool(t)
	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	ctx := context.Background()

	if _, err := construct.Create(ctx, deps, "bench", "mcp-servers", construct.Input{}); err == nil {
		t.Error("Create(bench) with no Bench input: want error, got nil")
	}
	if _, err := construct.Create(ctx, deps, "bench", "mcp-servers", construct.Input{
		Bench: &construct.BenchInput{Slug: "x", BinaryPath: "b", FlagSet: "--f v", BaselineJSONPath: "bl"},
		Bug:   &construct.BugInput{Title: "stray"},
	}); err == nil {
		t.Error("Create(bench) with a stray Bug arm: want union-discipline error, got nil")
	}
}
