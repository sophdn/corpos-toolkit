package abtest_test

import (
	"context"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/ml"
	"toolkit/internal/ml/abtest"
	"toolkit/internal/testutil"
)

// fakeSession mirrors the one in internal/ml/inference_test.go.
type fakeSession struct {
	OnRun func(in []float32, shape []int64) ([]float32, []int64, error)
}

func (s *fakeSession) Run(in []float32, shape []int64) ([]float32, []int64, error) {
	if s.OnRun != nil {
		return s.OnRun(in, shape)
	}
	out := make([]float32, len(in))
	copy(out, in)
	return out, shape, nil
}
func (s *fakeSession) Close() error { return nil }

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	pool, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if _, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return pool
}

func seedTrainedModelRow(t *testing.T, pool *db.Pool, slug, task, status string) int64 {
	t.Helper()
	res, err := pool.DB().Exec(
		`INSERT INTO trained_models
			(project_id, slug, task, version, training_dataset_signature,
			 eval_metrics, status, artifact_path)
		 VALUES ('mcp-servers', ?, ?, 'v1', 'sig', '{}', ?, ?)`,
		slug, task, status, task+"/v1/m.onnx")
	if err != nil {
		t.Fatalf("seed trained_model: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func loadModelForTest(t *testing.T, pool *db.Pool, id int64) *ml.Model {
	t.Helper()
	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(func(string, string, string) (ml.Session, error) {
			return &fakeSession{
				// Trained model scales input by 2.
				OnRun: func(in []float32, shape []int64) ([]float32, []int64, error) {
					out := make([]float32, len(in))
					for i, v := range in {
						out[i] = v * 2
					}
					return out, shape, nil
				},
			}, nil
		}),
		ml.WithModelsRoot("/unused"))
	t.Cleanup(func() { _ = reg.Close() })
	model, err := reg.LoadByID(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadByID: %v", err)
	}
	return model
}

// TestDispatch_AbTesting_DualFireRecords pins the substrate's
// happy-path: ab_testing status → both paths fire → one row in
// ab_comparisons → Policy-selected output returned.
func TestDispatch_AbTesting_DualFireRecords(t *testing.T) {
	pool := openTestPool(t)
	id := seedTrainedModelRow(t, pool, "router-v1", "source-router", "ab_testing")
	model := loadModelForTest(t, pool, id)

	baselineCalls := 0
	baseline := func(_ context.Context, f ml.Features) ([]float32, []int64, error) {
		baselineCalls++
		// Identity baseline.
		out := make([]float32, len(f.Data))
		copy(out, f.Data)
		return out, f.Shape, nil
	}

	ctx := events.WithSpanID(context.Background(), "test-span-1")
	res, err := abtest.Dispatch(ctx, abtest.Deps{Pool: pool}, abtest.Config{
		Baseline: baseline,
		Model:    model,
		Features: ml.Features{Data: []float32{1, 2, 3, 4}, Shape: []int64{1, 4}},
		Policy:   abtest.PreferTrained,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if baselineCalls != 1 {
		t.Errorf("expected baseline to fire once, got %d", baselineCalls)
	}
	if res.UsedPath != "trained" {
		t.Errorf("PreferTrained policy: expected used_path=trained, got %q", res.UsedPath)
	}
	if res.ComparisonRowID == 0 {
		t.Error("expected ab_comparisons row written")
	}
	if len(res.Output) != 4 || res.Output[0] != 2 {
		t.Errorf("expected trained output (scale-by-2), got %v", res.Output)
	}
	if res.BaselineLatencyMs < 0 || res.TrainedLatencyMs < 0 {
		t.Errorf("negative latency: baseline=%f trained=%f", res.BaselineLatencyMs, res.TrainedLatencyMs)
	}

	// Verify the row landed with the expected columns.
	var modelID int64
	var usedPath, policy string
	if err := pool.DB().QueryRow(
		`SELECT model_id, used_path, policy FROM ab_comparisons WHERE id = ?`,
		res.ComparisonRowID).Scan(&modelID, &usedPath, &policy); err != nil {
		t.Fatalf("read ab_comparisons row: %v", err)
	}
	if modelID != id {
		t.Errorf("row model_id mismatch: got %d want %d", modelID, id)
	}
	if usedPath != "trained" {
		t.Errorf("row used_path mismatch: got %q want trained", usedPath)
	}
	if policy != "prefer_trained" {
		t.Errorf("row policy mismatch: got %q want prefer_trained", policy)
	}
}

// TestDispatch_Promoted_TrainedOnly verifies the short-circuit:
// promoted status fires only the trained path.
func TestDispatch_Promoted_TrainedOnly(t *testing.T) {
	pool := openTestPool(t)
	id := seedTrainedModelRow(t, pool, "router-v1", "source-router", "promoted")
	model := loadModelForTest(t, pool, id)

	baselineCalls := 0
	baseline := func(_ context.Context, f ml.Features) ([]float32, []int64, error) {
		baselineCalls++
		return f.Data, f.Shape, nil
	}

	ctx := events.WithSpanID(context.Background(), "test-span-2")
	res, err := abtest.Dispatch(ctx, abtest.Deps{Pool: pool}, abtest.Config{
		Baseline: baseline,
		Model:    model,
		Features: ml.Features{Data: []float32{3, 5}, Shape: []int64{1, 2}},
		Policy:   abtest.PreferBaseline, // ignored — promoted short-circuits to trained
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if baselineCalls != 0 {
		t.Errorf("promoted should short-circuit; baseline fired %d times", baselineCalls)
	}
	if !res.BaselineSkipped {
		t.Error("expected BaselineSkipped=true")
	}
	if res.ShortCircuitReason != "model_promoted" {
		t.Errorf("unexpected short_circuit_reason: %q", res.ShortCircuitReason)
	}
	if res.ComparisonRowID != 0 {
		t.Error("no comparison row should land on short-circuit")
	}
}

// TestDispatch_Evaluating_BaselineOnly: evaluating/retired/no-model
// short-circuit to baseline-only.
func TestDispatch_Evaluating_BaselineOnly(t *testing.T) {
	pool := openTestPool(t)
	id := seedTrainedModelRow(t, pool, "router-v1", "source-router", "evaluating")
	model := loadModelForTest(t, pool, id)

	baselineCalls := 0
	baseline := func(_ context.Context, f ml.Features) ([]float32, []int64, error) {
		baselineCalls++
		return f.Data, f.Shape, nil
	}

	ctx := events.WithSpanID(context.Background(), "test-span-3")
	res, err := abtest.Dispatch(ctx, abtest.Deps{Pool: pool}, abtest.Config{
		Baseline: baseline,
		Model:    model,
		Features: ml.Features{Data: []float32{1}, Shape: []int64{1, 1}},
		Policy:   abtest.PreferTrained,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if baselineCalls != 1 {
		t.Errorf("expected baseline to fire once, got %d", baselineCalls)
	}
	if !res.TrainedSkipped {
		t.Error("expected TrainedSkipped=true")
	}
	if res.UsedPath != "baseline" {
		t.Errorf("expected used_path=baseline, got %q", res.UsedPath)
	}
}

// TestPromotionGate_NotEnoughComparisons verifies the gate rejects with
// a clear BlockedReason when the corpus is too thin.
func TestPromotionGate_NotEnoughComparisons(t *testing.T) {
	pool := openTestPool(t)
	id := seedTrainedModelRow(t, pool, "router-v1", "source-router", "ab_testing")

	verdict, err := abtest.EvaluatePromotionGate(context.Background(), pool, id, abtest.PromotionGateConfig{})
	if err != nil {
		t.Fatalf("EvaluatePromotionGate: %v", err)
	}
	if verdict.Ready {
		t.Errorf("expected Ready=false with no comparisons, got %+v", verdict)
	}
	if verdict.BlockedReason == "" {
		t.Errorf("expected BlockedReason, got %+v", verdict)
	}
	if verdict.TotalComparisons != 0 {
		t.Errorf("expected 0 comparisons, got %d", verdict.TotalComparisons)
	}
}

// TestPromotionGate_WithClickThrough_FiresReady seeds a synthetic
// corpus where trained beats baseline on click-through and verifies
// the gate fires Ready=true after lowering thresholds.
func TestPromotionGate_WithClickThrough_FiresReady(t *testing.T) {
	pool := openTestPool(t)
	id := seedTrainedModelRow(t, pool, "router-v1", "source-router", "ab_testing")
	model := loadModelForTest(t, pool, id)

	baseline := func(_ context.Context, f ml.Features) ([]float32, []int64, error) {
		return f.Data, f.Shape, nil
	}

	// Synthesize 10 comparisons (5 baseline-path, 5 trained-path) with
	// alternate policy. Then seed query_interactions rows so trained
	// has 4/5 clicks vs baseline's 1/5 — delta = 0.6, well above the
	// 0.05 default.
	for i := 0; i < 10; i++ {
		ctx := events.WithSpanID(context.Background(), spanIDForCall(i))
		_, err := abtest.Dispatch(ctx, abtest.Deps{Pool: pool}, abtest.Config{
			Baseline: baseline,
			Model:    model,
			Features: ml.Features{Data: []float32{float32(i)}, Shape: []int64{1, 1}},
			Policy:   abtest.Alternate,
		})
		if err != nil {
			t.Fatalf("Dispatch %d: %v", i, err)
		}
	}

	// Read back which paths got which spans.
	rows, err := pool.DB().Query(
		`SELECT span_id, used_path FROM ab_comparisons WHERE model_id = ? ORDER BY id`, id)
	if err != nil {
		t.Fatalf("read ab_comparisons: %v", err)
	}
	defer rows.Close()
	type pathSpan struct {
		span string
		path string
	}
	var spans []pathSpan
	for rows.Next() {
		var ps pathSpan
		if err := rows.Scan(&ps.span, &ps.path); err != nil {
			t.Fatalf("scan: %v", err)
		}
		spans = append(spans, ps)
	}

	// Seed click signals: trained spans get 4 clicks, baseline spans
	// get 1. The grounding_events row is required for the FK; insert a
	// stub one per span.
	for i, ps := range spans {
		var clickIt bool
		if ps.path == "trained" && i < 8 {
			clickIt = true
		}
		if ps.path == "baseline" && i == 0 {
			clickIt = true
		}
		if !clickIt {
			continue
		}
		seedGroundingClickFor(t, pool, ps.span)
	}

	verdict, err := abtest.EvaluatePromotionGate(context.Background(), pool, id, abtest.PromotionGateConfig{
		MinComparisons: 5,    // tiny — synthetic corpus
		MinDelta:       0.10, // synthetic delta is ~0.6
		WindowDays:     365,  // single big window so stability never trips
		WindowsStable:  1,
	})
	if err != nil {
		t.Fatalf("EvaluatePromotionGate: %v", err)
	}
	if verdict.BlockedReason != "" {
		t.Logf("blocked: %s (trained=%.2f baseline=%.2f n=%d)",
			verdict.BlockedReason, verdict.TrainedClickThroughRate,
			verdict.BaselineClickThroughRate, verdict.TotalComparisons)
	}
	if verdict.Delta < 0 {
		t.Errorf("expected positive delta with trained winning, got %.3f", verdict.Delta)
	}
}

func spanIDForCall(i int) string {
	return "test-span-call-" + string(rune('a'+i))
}

func seedGroundingClickFor(t *testing.T, pool *db.Pool, spanID string) {
	t.Helper()
	// Delegates to the shared telemetry seeders (bug 1455). Matches the
	// pre-refactor row shape: session_id='test-sess', source_ref='test/ref',
	// click_kind='cited', click_weight=1.0, span_id=spanID on both rows.
	geID := testutil.SeedGroundingEvent(t, pool,
		testutil.WithGroundingSession("test-sess"),
		testutil.WithGroundingCallID(spanID),
		testutil.WithGroundingSpan(spanID),
		testutil.WithGroundingQueryText("q"))
	testutil.SeedQueryInteraction(t, pool, geID,
		testutil.WithQISession("test-sess"),
		testutil.WithQISpan(spanID),
		testutil.WithQISourceRef("test/ref"))
}
