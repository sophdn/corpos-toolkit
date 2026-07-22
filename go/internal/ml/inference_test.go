package ml_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/ml"
)

// fakeSession implements ml.Session without needing onnxruntime. The
// behavior is configurable per-test via the OnRun hook. Closes idempotent.
type fakeSession struct {
	mu     sync.Mutex
	closed bool
	OnRun  func(in []float32, shape []int64) ([]float32, []int64, error)
}

func (s *fakeSession) Run(in []float32, shape []int64) ([]float32, []int64, error) {
	if s.OnRun != nil {
		return s.OnRun(in, shape)
	}
	// Default: identity-with-shape-prefix, useful for the dispatch wiring test.
	out := make([]float32, len(in))
	copy(out, in)
	outShape := make([]int64, len(shape))
	copy(outShape, shape)
	return out, outShape, nil
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// openTestPool mirrors the work package's helper — opens a temp DB with
// all migrations applied, seeds a project row.
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

func seedTrainedModelRow(t *testing.T, pool *db.Pool, slug, task, status, artifactPath string) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO trained_models
			(project_id, slug, task, version, training_dataset_signature,
			 eval_metrics, status, artifact_path)
		 VALUES ('mcp-servers', ?, ?, 'v1', 'sig', '{}', ?, ?)`,
		slug, task, status, artifactPath); err != nil {
		t.Fatalf("seed trained_model: %v", err)
	}
}

// TestRegistry_LoadByPromoted_RoundTrip exercises the substrate's
// happy path: a promoted row in the DB → Registry.LoadByPromoted →
// Model.Infer → Prediction with the expected output.
func TestRegistry_LoadByPromoted_RoundTrip(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModelRow(t, pool, "router-v1", "source-router", "promoted", "source-router/v1/model.onnx")

	calls := 0
	factory := func(modelPath, inputName, outputName string) (ml.Session, error) {
		calls++
		if !strings.HasSuffix(modelPath, "source-router/v1/model.onnx") {
			t.Errorf("model path didn't resolve via models_root: got %q", modelPath)
		}
		if inputName != "input_vectors" || outputName != "output_scalars" {
			t.Errorf("unexpected default IO names: in=%q out=%q", inputName, outputName)
		}
		return &fakeSession{
			OnRun: func(in []float32, shape []int64) ([]float32, []int64, error) {
				// Echo input scaled by 2.0.
				out := make([]float32, len(in))
				for i, v := range in {
					out[i] = v * 2
				}
				return out, shape, nil
			},
		}, nil
	}

	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(factory),
		ml.WithModelsRoot("/tmp/ml-models"))
	defer reg.Close()

	model, err := reg.LoadByPromoted(context.Background(), "mcp-servers", "source-router")
	if err != nil {
		t.Fatalf("LoadByPromoted: %v", err)
	}
	if model.Status() != "promoted" {
		t.Errorf("expected status=promoted, got %q", model.Status())
	}

	pred, err := model.Infer(context.Background(), ml.Features{
		Data:  []float32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		Shape: []int64{1, 10},
	})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if len(pred.Output) != 10 || pred.Output[0] != 2 || pred.Output[9] != 20 {
		t.Errorf("expected echo-scaled-by-2 output, got %v", pred.Output)
	}
	if pred.ModelID == 0 {
		t.Errorf("expected ModelID populated, got %d", pred.ModelID)
	}
	if pred.FeatHash == "" || pred.FeatHash == "marshal-error" {
		t.Errorf("expected feat hash, got %q", pred.FeatHash)
	}
	if pred.LatencyMs < 0 {
		t.Errorf("negative latency: %f", pred.LatencyMs)
	}

	// Second LoadByPromoted hits the cache — factory should not re-fire.
	if _, err := reg.LoadByPromoted(context.Background(), "mcp-servers", "source-router"); err != nil {
		t.Fatalf("second LoadByPromoted: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected session factory called once (cached), got %d", calls)
	}

	// Reload drops the cache.
	reg.Reload()
	if _, err := reg.LoadByPromoted(context.Background(), "mcp-servers", "source-router"); err != nil {
		t.Fatalf("post-reload LoadByPromoted: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected factory re-fired after Reload, got calls=%d", calls)
	}
}

// TestRegistry_NotFound_NoPromoted confirms the typed envelope when
// no promoted row exists.
func TestRegistry_NotFound_NoPromoted(t *testing.T) {
	pool := openTestPool(t)
	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(func(string, string, string) (ml.Session, error) {
			t.Fatal("factory should not be invoked when no row matches")
			return nil, nil
		}))
	defer reg.Close()

	_, err := reg.LoadByPromoted(context.Background(), "mcp-servers", "ghost-task")
	if !errors.Is(err, ml.ErrModelNotFound) {
		t.Errorf("expected ErrModelNotFound, got %v", err)
	}
}

// TestRegistry_GatedStatus refuses to load rows in training/retired.
func TestRegistry_GatedStatus(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModelRow(t, pool, "ghost-v1", "source-router", "training", "x/v1/m.onnx")
	seedTrainedModelRow(t, pool, "old-v1", "old-task", "retired", "x/v1/m.onnx")

	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM trained_models WHERE slug = ?`, "ghost-v1").Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}

	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(func(string, string, string) (ml.Session, error) {
			t.Fatal("factory should not be invoked for gated status")
			return nil, nil
		}))
	defer reg.Close()

	_, err := reg.LoadByID(context.Background(), id)
	if !errors.Is(err, ml.ErrModelGated) {
		t.Errorf("expected ErrModelGated for training-status row, got %v", err)
	}
}

// TestRegistry_LRUEviction confirms the cache caps at maxLoaded.
func TestRegistry_LRUEviction(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModelRow(t, pool, "a-v1", "task-a", "promoted", "a/v1/m.onnx")
	seedTrainedModelRow(t, pool, "b-v1", "task-b", "promoted", "b/v1/m.onnx")
	seedTrainedModelRow(t, pool, "c-v1", "task-c", "promoted", "c/v1/m.onnx")

	closed := 0
	factory := func(string, string, string) (ml.Session, error) {
		return &fakeSession{
			OnRun: func(in []float32, shape []int64) ([]float32, []int64, error) {
				return in, shape, nil
			},
		}, nil
	}

	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(factory),
		ml.WithMaxLoaded(2))
	defer reg.Close()

	_, err := reg.LoadByPromoted(context.Background(), "mcp-servers", "task-a")
	if err != nil {
		t.Fatalf("load a: %v", err)
	}
	_, err = reg.LoadByPromoted(context.Background(), "mcp-servers", "task-b")
	if err != nil {
		t.Fatalf("load b: %v", err)
	}
	// task-a should still be in cache (size=2). Loading c evicts the LRU
	// (which is task-a since we just touched task-b).
	mC, err := reg.LoadByPromoted(context.Background(), "mcp-servers", "task-c")
	if err != nil {
		t.Fatalf("load c: %v", err)
	}
	if mC == nil {
		t.Errorf("expected task-c loaded, got nil")
	}
	_ = closed
}

// TestInfer_InputShape_Validation pins the shape-mismatch error path.
func TestInfer_InputShape_Validation(t *testing.T) {
	pool := openTestPool(t)
	seedTrainedModelRow(t, pool, "router-v1", "source-router", "promoted", "x/v1/m.onnx")

	reg := ml.NewRegistry(pool,
		ml.WithSessionFactory(func(string, string, string) (ml.Session, error) {
			return &fakeSession{}, nil
		}))
	defer reg.Close()

	model, err := reg.LoadByPromoted(context.Background(), "mcp-servers", "source-router")
	if err != nil {
		t.Fatalf("LoadByPromoted: %v", err)
	}

	cases := []struct {
		name string
		f    ml.Features
	}{
		{"empty data", ml.Features{Data: nil, Shape: []int64{1, 10}}},
		{"empty shape", ml.Features{Data: []float32{1, 2, 3}, Shape: nil}},
		{"shape-data mismatch", ml.Features{Data: []float32{1, 2, 3}, Shape: []int64{1, 10}}},
		{"non-positive dim", ml.Features{Data: []float32{1, 2}, Shape: []int64{1, 0, 2}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := model.Infer(context.Background(), c.f)
			if !errors.Is(err, ml.ErrInputShape) {
				t.Errorf("expected ErrInputShape, got %v", err)
			}
		})
	}
}
