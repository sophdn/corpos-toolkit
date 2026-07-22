//go:build cgo

package ml_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/ml"
)

// TestIntegration_LoadAndInferRealONNX exercises the full ORT path:
// initialize the runtime, load a fixture ONNX, run inference, assert
// the output shape. Skipped when libonnxruntime.so isn't installed or
// the fixture isn't present.
//
// Fixture: testdata/example_dynamic_axes.onnx (320 bytes; from
// yalue/onnxruntime_go MIT-licensed test_data). The model takes
// dynamic batch size of (N, 10) float32 vectors and returns (N,)
// float32 scalars.
//
// To run this test locally:
//
//	scripts/setup-ml-deps.sh   # downloads libonnxruntime.so to vendor/
//	make -C go test ./internal/ml/...
//
// CI runs with the lib absent (skips clean); manual exercises confirm
// the binding wiring works end-to-end.
func TestIntegration_LoadAndInferRealONNX(t *testing.T) {
	fixturePath := fixturePath(t)
	if _, err := os.Stat(fixturePath); err != nil {
		t.Skipf("fixture not present at %q (run scripts/setup-ml-deps.sh to vendor it): %v", fixturePath, err)
	}
	if err := initializeORTForTest(t); err != nil {
		t.Skipf("onnxruntime not available (%v); install via scripts/setup-ml-deps.sh", err)
	}

	pool := openTestPool(t)

	// Seed a trained_model row pointing at the fixture's absolute path.
	// Using artifact_path=<abs> bypasses ML_MODELS_ROOT resolution which
	// is the right shape for a test that vendors its own ONNX file.
	if _, err := pool.DB().Exec(
		`INSERT INTO trained_models
			(project_id, slug, task, version, training_dataset_signature,
			 eval_metrics, status, artifact_path)
		 VALUES ('mcp-servers', 'integration-v1', 'integration-test', 'v1',
		         'fixture@2026-05-19;rows=0', '{}', 'promoted', ?)`,
		fixturePath); err != nil {
		t.Fatalf("seed trained_model row: %v", err)
	}

	reg := ml.NewRegistry(pool,
		ml.WithModelsRoot("/unused"),
		ml.WithDefaultIONames(ml.IONames{Input: "input_vectors", Output: "output_scalars"}))
	defer reg.Close()

	model, err := reg.LoadByPromoted(context.Background(), "mcp-servers", "integration-test")
	if err != nil {
		t.Fatalf("LoadByPromoted: %v", err)
	}

	// The fixture's contract: input (batch, 10) → output (batch,). Batch=2.
	input := make([]float32, 20)
	for i := range input {
		input[i] = float32(i) * 0.1
	}
	pred, err := model.Infer(context.Background(), ml.Features{
		Data:  input,
		Shape: []int64{2, 10},
	})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	if len(pred.OutputShape) == 0 {
		t.Errorf("expected non-empty output shape, got %v", pred.OutputShape)
	}
	if pred.OutputShape[0] != 2 {
		t.Errorf("expected batch=2 in output shape, got %v", pred.OutputShape)
	}
	if pred.LatencyMs > 100 {
		t.Errorf("inference too slow: %fms (>100ms budget per acceptance criteria)", pred.LatencyMs)
	}
	if pred.LatencyMs <= 0 {
		t.Errorf("non-positive latency: %fms", pred.LatencyMs)
	}
}

// fixturePath returns the absolute path to the ONNX fixture. Relies on
// the working directory being the package dir (Go test convention).
func fixturePath(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(cwd, "testdata", "example_dynamic_axes.onnx")
}

// initializeORTForTest tries the conventional resolution paths. Returns
// non-nil err when no library can be located — caller t.Skip()s.
func initializeORTForTest(t *testing.T) error {
	t.Helper()
	if ml.IsONNXRuntimeInitialized() {
		return nil
	}
	// Resolution order: ML_ONNXRUNTIME_LIB_PATH env, then repo's
	// vendor/onnxruntime/lib/libonnxruntime.so. From the package's
	// test working dir (.../go/internal/ml), walk up to repo root.
	if env := os.Getenv("ML_ONNXRUNTIME_LIB_PATH"); env != "" {
		return ml.InitializeONNXRuntime(env)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// .../mcp-servers/go/internal/ml → walk up to mcp-servers
	repoRoot := cwd
	for i := 0; i < 5; i++ {
		repoRoot = filepath.Dir(repoRoot)
		candidate := filepath.Join(repoRoot, "vendor", "onnxruntime", "lib", "libonnxruntime.so")
		if _, err := os.Stat(candidate); err == nil {
			return ml.InitializeONNXRuntime(candidate)
		}
	}
	return ml.ErrONNXRuntimeNotInitialized
}
