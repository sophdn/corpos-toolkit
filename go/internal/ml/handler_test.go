package ml_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/dispatch"
	"toolkit/internal/events"
	"toolkit/internal/ml"
)

// TestInference_EndToEndThroughTable exercises the full ml surface:
// table-build → dispatch → handler → registry → fake session →
// prediction-row write. Verifies the substrate's acceptance criteria
// for T5: latency <50ms on stub model, span_id flows through, telemetry
// row lands.
func TestInference_EndToEndThroughTable(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModelRow(t, pool, "router-v1", "source-router", "promoted", "source-router/v1/model.onnx")

	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(func(string, string, string) (ml.Session, error) {
			return &fakeSession{
				OnRun: func(in []float32, shape []int64) ([]float32, []int64, error) {
					// Tiny "model": echo input as-is.
					out := make([]float32, len(in))
					copy(out, in)
					return out, shape, nil
				},
			}, nil
		}),
		ml.WithModelsRoot("/unused"))
	defer reg.Close()

	table := ml.BuildTable(ml.TableDeps{Pool: pool, Registry: reg})

	handler, ok := table["inference"]
	if !ok {
		t.Fatal("table missing 'inference' action")
	}

	// Stamp span_id into ctx like the dispatcher would.
	ctx := events.WithSpanID(context.Background(), "test-span-abc123")

	params := mustJSONRaw(t, map[string]any{
		"task":           "source-router",
		"features_data":  []float32{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0},
		"features_shape": []int64{1, 10},
	})

	start := time.Now()
	result, err := handler(ctx, "mcp-servers", params)
	latency := time.Since(start)
	if err != nil {
		t.Fatalf("handler.Run: %v", err)
	}

	// Latency through the dispatcher should be well under 50ms with a
	// fake session.
	if latency > 50*time.Millisecond {
		t.Errorf("end-to-end latency %v exceeds 50ms acceptance budget", latency)
	}

	// Decode the result envelope (dispatch.Handler returns any; the
	// adapter has already marshaled the InferenceResult).
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var resp ml.InferenceResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if resp.Error != "" {
		t.Fatalf("unexpected error envelope: %s (hint=%s)", resp.Error, resp.Hint)
	}
	if !resp.OK {
		t.Errorf("expected OK=true, got %+v", resp)
	}
	if resp.Prediction == nil {
		t.Fatal("missing prediction")
	}
	if resp.SpanID != "test-span-abc123" {
		t.Errorf("expected span_id propagation; got %q", resp.SpanID)
	}
	if resp.PredictionRowID == 0 {
		t.Errorf("expected non-zero prediction_row_id (telemetry write)")
	}
	if resp.Prediction.LatencyMs < 0 || resp.Prediction.LatencyMs > 50 {
		t.Errorf("inference latency out of bounds: %fms (>= 0 and <= 50 expected)", resp.Prediction.LatencyMs)
	}

	// Verify the model_predictions row actually landed.
	var rowModelID int64
	var rowSpan, rowHash string
	var rowLat float64
	if err := pool.DB().QueryRow(
		`SELECT model_id, span_id, features_hash, latency_ms FROM model_predictions WHERE id = ?`,
		resp.PredictionRowID).Scan(&rowModelID, &rowSpan, &rowHash, &rowLat); err != nil {
		t.Fatalf("read back prediction row: %v", err)
	}
	if rowSpan != "test-span-abc123" {
		t.Errorf("row span_id = %q, want %q", rowSpan, "test-span-abc123")
	}
	if rowModelID == 0 {
		t.Errorf("row model_id is zero")
	}
	if rowHash == "" || rowHash == "marshal-error" {
		t.Errorf("row features_hash unset or marshal-error: %q", rowHash)
	}
}

// TestInference_AutoSpanID_NoExplicitStamp verifies that an inference
// call without an explicit span_id stamp still works — events.SpanIDFromContext
// mints a fresh UUIDv4 so the model_predictions row stays writable.
// The MCP dispatcher always stamps a real span_id in production; this
// test pins the direct-handler path's behavior.
func TestInference_AutoSpanID_NoExplicitStamp(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModelRow(t, pool, "router-v1", "source-router", "promoted", "x/v1/m.onnx")

	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(func(string, string, string) (ml.Session, error) {
			return &fakeSession{}, nil
		}))
	defer reg.Close()

	table := ml.BuildTable(ml.TableDeps{Pool: pool, Registry: reg})
	handler := table["inference"]

	// No explicit span_id stamp — auto-mint kicks in.
	result, err := handler(context.Background(), "mcp-servers", mustJSONRaw(t, map[string]any{
		"task":           "source-router",
		"features_data":  []float32{1, 2, 3},
		"features_shape": []int64{1, 3},
	}))
	if err != nil {
		t.Fatalf("handler call: %v", err)
	}

	raw, _ := json.Marshal(result)
	var resp ml.InferenceResult
	_ = json.Unmarshal(raw, &resp)

	if !resp.OK {
		t.Errorf("expected OK envelope with auto-span, got %+v", resp)
	}
	if resp.SpanID == "" {
		t.Error("expected auto-minted span_id in response")
	}
	if resp.PredictionRowID == 0 {
		t.Error("expected telemetry row to land even with auto-minted span")
	}
}

// TestInference_RequiresModelIDOrTask surfaces a clear envelope when
// neither model_id nor task is supplied.
func TestInference_RequiresModelIDOrTask(t *testing.T) {
	pool := openTestPool(t)
	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(func(string, string, string) (ml.Session, error) {
			t.Fatal("factory should not be invoked")
			return nil, nil
		}))
	defer reg.Close()
	table := ml.BuildTable(ml.TableDeps{Pool: pool, Registry: reg})
	handler := table["inference"]

	ctx := events.WithSpanID(context.Background(), "test-span")
	result, err := handler(ctx, "mcp-servers", mustJSONRaw(t, map[string]any{
		"features_data":  []float32{1, 2},
		"features_shape": []int64{1, 2},
	}))
	if err != nil {
		t.Fatalf("handler.Run: %v", err)
	}

	raw, _ := json.Marshal(result)
	var resp ml.InferenceResult
	_ = json.Unmarshal(raw, &resp)

	if resp.Error == "" {
		t.Errorf("expected error envelope, got %+v", resp)
	}
}

// TestBuildTable_DegradedModeWhenDepsNil confirms the surface still
// registers without panicking even when pool/registry are nil.
func TestBuildTable_DegradedModeWhenDepsNil(t *testing.T) {
	table := ml.BuildTable(ml.TableDeps{})
	handler, ok := table["inference"]
	if !ok {
		t.Fatal("inference should register even in degraded mode")
	}

	result, err := handler(context.Background(), "mcp-servers", mustJSONRaw(t, map[string]any{"task": "x"}))
	if err != nil {
		t.Fatalf("handler.Run: %v", err)
	}
	raw, _ := json.Marshal(result)
	var resp ml.InferenceResult
	_ = json.Unmarshal(raw, &resp)
	if resp.Error == "" {
		t.Errorf("expected degraded-mode error envelope, got %+v", resp)
	}
}

// TestBuildTable_ConvenienceActionRegistered verifies the
// per-task-convenience-action registry pattern. Used by downstream ML
// chains (T5 follow-on) to wire route_query / curation_score / etc.
func TestBuildTable_ConvenienceActionRegistered(t *testing.T) {
	pool := openTestPool(t)
	reg := ml.NewRegistry(pool)
	defer reg.Close()

	called := false
	conv := ml.ConvenienceAction{
		Name: "route_query",
		Handler: dispatch.Adapt(func(_ context.Context, _ string, _ json.RawMessage) (map[string]string, error) {
			called = true
			return map[string]string{"top_source": "vault"}, nil
		}),
	}

	table := ml.BuildTable(ml.TableDeps{Pool: pool, Registry: reg}, conv)
	handler, ok := table["route_query"]
	if !ok {
		t.Fatal("convenience action route_query missing from table")
	}
	if _, err := handler(context.Background(), "", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("handler call: %v", err)
	}
	if !called {
		t.Error("convenience handler was registered but never invoked")
	}
}

func mustJSONRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// poolForBenchmark is a fixture for the latency assertion. db.Pool is
// already imported via openTestPool's signature.
var _ = (*db.Pool)(nil)
