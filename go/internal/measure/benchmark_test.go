package measure_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/measure"
	"toolkit/internal/projections"
	"toolkit/internal/testutil"
)

func newBenchmarkDeps(t *testing.T) measure.BenchmarkDeps {
	t.Helper()
	pool := testutil.NewTestDB(t)
	// Register the events→projections fold hook so post-T5 handlers
	// (no longer dual-writing CRUD) see proj_* updates.
	events.SetFoldHook(func(ctx context.Context, tx *sql.Tx, evt events.RawEvent) error {
		return projections.FoldAll(ctx, tx, projections.RawEvent{
			EventID:         evt.EventID,
			Ts:              evt.Ts,
			ActorKind:       evt.ActorKind,
			ActorID:         evt.ActorID,
			Type:            evt.Type,
			EntityKind:      evt.EntityKind,
			EntitySlug:      evt.EntitySlug,
			EntityProjectID: evt.EntityProjectID,
			Payload:         evt.Payload,
			Rationale:       evt.Rationale,
			CausedByEventID: evt.CausedByEventID,
			RelatedEntities: evt.RelatedEntities,
			SpanID:          evt.SpanID,
			SchemaVersion:   evt.SchemaVersion,
		})
	})
	return measure.BenchmarkDeps{Pool: pool}
}

// seedProvenance writes one benchmark_provenance row and returns its id.
// T6's cutover trigger requires every benchmark_results INSERT to carry a
// valid provenance_id; tests use this helper to satisfy the trigger
// without re-implementing the harness's start_run flow.
//
// Generates a UUIDv7-shaped started_event_id so downstream events that
// reference this id (e.g. BenchmarkRunStarted re-emit during replay)
// pass the envelope schema's UUIDv7 pattern check.
func seedProvenance(t *testing.T, pool *db.Pool, runID string) string {
	t.Helper()
	id := "test-prov-" + runID
	ctx := context.Background()
	const insert = `INSERT INTO benchmark_provenance
		(id, run_id, model_id, model_version, prompt_template_hash,
		 corpus_hash, retriever_version, retriever_config_hash,
		 seed, env_hash, started_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := pool.DB().ExecContext(ctx, insert,
		id, runID, "test-model", "test-model-v1",
		"prompt-hash", "corpus-hash", "test-retriever", "config-hash",
		int64(0), "env-hash", fakeUUIDv7(runID),
	); err != nil {
		t.Fatalf("seedProvenance insert: %v", err)
	}
	return id
}

// fakeUUIDv7 produces a deterministic UUIDv7-shaped string for tests.
// Real UUIDv7 has a time-prefix + random; this helper hashes the
// salt and threads it into the right bit pattern so the envelope-schema
// regex (^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$)
// accepts it. Used only by test seeds.
func fakeUUIDv7(salt string) string {
	h := sha256.Sum256([]byte(salt))
	hex := hex.EncodeToString(h[:])
	// Force version 7 (4 bits) + variant 10 (2 bits).
	return hex[0:8] + "-" + hex[8:12] + "-7" + hex[13:16] + "-8" + hex[17:20] + "-" + hex[20:32]
}

// Minimum valid params for benchmark_record — every required field present,
// optionals omitted. Helpers extend this in tests that exercise filters.
func validRecordParams(t *testing.T, pool *db.Pool) json.RawMessage {
	t.Helper()
	provenanceID := seedProvenance(t, pool, "run-"+t.Name())
	raw, err := json.Marshal(map[string]any{
		"scenario_id":   "scn-benchmark-record-001",
		"tool_name":     "knowledge.vault_search",
		"model_name":    "qwen2.5-32b",
		"run_at":        1715600000,
		"wall_clock_ms": 1234,
		"invocation_ok": true,
		"provenance_id": provenanceID,
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return raw
}

func TestHandleBenchmarkRecord_RoundtripRecordThenQuery(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()

	out, err := measure.HandleBenchmarkRecord(ctx, deps, "mcp-servers", validRecordParams(t, deps.Pool))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !out.OK {
		t.Fatalf("record response missing ok=true: %+v", out)
	}
	id := out.ID
	if id == "" {
		t.Fatalf("record response missing id: %+v", out)
	}

	results, err := measure.HandleBenchmarkQuery(ctx, deps, "mcp-servers", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 row, got %d", len(results))
	}
	r := results[0]
	if r.ID != id {
		t.Errorf("id mismatch: insert=%s query=%s", id, r.ID)
	}
	if r.ScenarioID != "scn-benchmark-record-001" {
		t.Errorf("scenario_id: %q", r.ScenarioID)
	}
	if r.ToolName != "knowledge.vault_search" {
		t.Errorf("tool_name: %q", r.ToolName)
	}
	if r.ModelName != "qwen2.5-32b" {
		t.Errorf("model_name: %q", r.ModelName)
	}
	if r.RunAt != 1715600000 {
		t.Errorf("run_at: %d", r.RunAt)
	}
	if r.WallClockMS != 1234 {
		t.Errorf("wall_clock_ms: %d", r.WallClockMS)
	}
	// invocation_ok provided as bool=true → INTEGER 1.
	if r.InvocationOK != 1 {
		t.Errorf("invocation_ok: %d", r.InvocationOK)
	}
	// invoked_contextually defaults to 1 when omitted.
	if r.InvokedContextually != 1 {
		t.Errorf("invoked_contextually default: %d", r.InvokedContextually)
	}
}

func TestHandleBenchmarkRecord_MissingRequiredFields(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()

	// Omit scenario_id, tool_name, run_at, wall_clock_ms, invocation_ok,
	// provenance_id (all required after T6 cutover).
	params, _ := json.Marshal(map[string]any{
		"model_name": "qwen2.5-32b",
	})
	out, err := measure.HandleBenchmarkRecord(ctx, deps, "mcp-servers", params)
	if err != nil {
		t.Fatalf("expected error envelope, got go error: %v", err)
	}
	msg := out.Error
	if !strings.Contains(msg, "missing required params") {
		t.Fatalf("unexpected error: %q", msg)
	}
	for _, field := range []string{"scenario_id", "tool_name", "run_at", "wall_clock_ms", "invocation_ok", "provenance_id"} {
		if !strings.Contains(msg, "params."+field) {
			t.Errorf("missing-fields error did not mention %q: %s", field, msg)
		}
	}
}

func TestHandleBenchmarkRecord_RequiresProject(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()
	out, err := measure.HandleBenchmarkRecord(ctx, deps, "", validRecordParams(t, deps.Pool))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("expected error envelope for empty project, got %+v", out)
	}
}

func TestHandleBenchmarkQuery_FiltersByToolAndModel(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()

	insert := func(scenario, tool, model string, runAt int64) {
		provenanceID := seedProvenance(t, deps.Pool, "run-"+scenario)
		params, _ := json.Marshal(map[string]any{
			"scenario_id":   scenario,
			"tool_name":     tool,
			"model_name":    model,
			"run_at":        runAt,
			"wall_clock_ms": 100,
			"invocation_ok": 1,
			"provenance_id": provenanceID,
		})
		if _, err := measure.HandleBenchmarkRecord(ctx, deps, "mcp-servers", params); err != nil {
			t.Fatalf("record %s: %v", scenario, err)
		}
	}
	insert("s-a", "tool-a", "qwen2.5-32b", 1715600000)
	insert("s-b", "tool-b", "qwen2.5-32b", 1715600100)
	insert("s-c", "tool-a", "claude-opus", 1715600200)

	q := func(filters map[string]any) []measure.BenchmarkResult {
		raw, _ := json.Marshal(filters)
		rows, err := measure.HandleBenchmarkQuery(ctx, deps, "mcp-servers", raw)
		if err != nil {
			t.Fatalf("query %v: %v", filters, err)
		}
		return rows
	}

	all := q(map[string]any{})
	if len(all) != 3 {
		t.Fatalf("expected 3 rows total, got %d", len(all))
	}
	// ORDER BY run_at DESC.
	if all[0].ScenarioID != "s-c" || all[2].ScenarioID != "s-a" {
		t.Errorf("ordering wrong: %s, %s, %s", all[0].ScenarioID, all[1].ScenarioID, all[2].ScenarioID)
	}

	byTool := q(map[string]any{"tool_name": "tool-a"})
	if len(byTool) != 2 {
		t.Fatalf("tool filter: expected 2, got %d", len(byTool))
	}

	byModel := q(map[string]any{"model_name": "claude-opus"})
	if len(byModel) != 1 || byModel[0].ScenarioID != "s-c" {
		t.Fatalf("model filter: %+v", byModel)
	}

	since := q(map[string]any{"since": 1715600150})
	if len(since) != 1 || since[0].ScenarioID != "s-c" {
		t.Fatalf("since filter: %+v", since)
	}

	limited := q(map[string]any{"limit": 2})
	if len(limited) != 2 {
		t.Fatalf("limit filter: expected 2, got %d", len(limited))
	}
}

func TestHandleBenchmarkRecord_PreservesOptionalFields(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()

	provenanceID := seedProvenance(t, deps.Pool, "run-optionals")
	params, _ := json.Marshal(map[string]any{
		"scenario_id":           "scn-optionals",
		"tool_name":             "measure.benchmark_record",
		"model_name":            "qwen2.5-32b",
		"run_at":                1715600000,
		"wall_clock_ms":         42,
		"invocation_ok":         true,
		"run_id":                "run-001",
		"input_tokens":          512,
		"output_tokens":         64,
		"invoked_contextually":  false,
		"args_match":            1,
		"extracted_args":        `{"k":"v"}`,
		"interpretation_ok":     true,
		"detected_tool":         "vault_search",
		"notes":                 "production",
		"task_shape":            "Classify",
		"accuracy_score":        0.95,
		"honesty_score":         1.0,
		"ranking_quality_score": 0.5,
		"within_budget_score":   0.8,
		"provenance_id":         provenanceID,
	})
	if _, err := measure.HandleBenchmarkRecord(ctx, deps, "mcp-servers", params); err != nil {
		t.Fatalf("record: %v", err)
	}

	rows, err := measure.HandleBenchmarkQuery(ctx, deps, "mcp-servers", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	// Bool-coerced INTEGER columns.
	if r.InvokedContextually != 0 {
		t.Errorf("invoked_contextually: bool false should map to 0, got %d", r.InvokedContextually)
	}
	if r.InterpretationOK == nil || *r.InterpretationOK != 1 {
		t.Errorf("interpretation_ok: bool true should map to 1, got %v", r.InterpretationOK)
	}
	if r.ArgsMatch == nil || *r.ArgsMatch != 1 {
		t.Errorf("args_match: %v", r.ArgsMatch)
	}
	if r.RunID == nil || *r.RunID != "run-001" {
		t.Errorf("run_id: %v", r.RunID)
	}
	if r.InputTokens == nil || *r.InputTokens != 512 {
		t.Errorf("input_tokens: %v", r.InputTokens)
	}
	if r.ExtractedArgs == nil || *r.ExtractedArgs != `{"k":"v"}` {
		t.Errorf("extracted_args: %v", r.ExtractedArgs)
	}
	if r.TaskShape == nil || *r.TaskShape != "Classify" {
		t.Errorf("task_shape: %v", r.TaskShape)
	}
	if r.AccuracyScore == nil || *r.AccuracyScore != 0.95 {
		t.Errorf("accuracy_score: %v", r.AccuracyScore)
	}
	if r.ProvenanceID != provenanceID {
		t.Errorf("provenance_id: got %q, want %q", r.ProvenanceID, provenanceID)
	}
}

// TestHandleBenchmarkRecord_T6CutoverRejectsMissingProvenance — the
// parameter parser rejects missing provenance_id with the same shape as
// every other required field. This is the "happy" failure path: the
// caller sees a structured error envelope, not the trigger's RAISE.
func TestHandleBenchmarkRecord_T6CutoverRejectsMissingProvenance(t *testing.T) {
	deps := newBenchmarkDeps(t)
	ctx := context.Background()

	params, _ := json.Marshal(map[string]any{
		"scenario_id":   "scn-no-provenance",
		"tool_name":     "x",
		"model_name":    "y",
		"run_at":        1715600000,
		"wall_clock_ms": 1,
		"invocation_ok": true,
	})
	out, err := measure.HandleBenchmarkRecord(ctx, deps, "mcp-servers", params)
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if !strings.Contains(out.Error, "params.provenance_id") {
		t.Fatalf("expected error to flag missing provenance_id, got %q", out.Error)
	}
}

// TestHandleBenchmarkRecord_T6CutoverTriggerFiresOnRawNullInsert was
// deleted as part of agent-substrate-crud-retirement T6: the CRUD
// benchmark_results table — along with its require-provenance trigger —
// no longer exists. The handler-level missing-provenance guard is
// covered by TestHandleBenchmarkRecord_T6CutoverRejectsMissingProvenance
// above; there is no longer a database-level guard to exercise.
