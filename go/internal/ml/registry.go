package ml

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"toolkit/internal/db"
)

// ErrModelNotFound surfaces when no trained_model row matches the
// supplied lookup. The MCP wrapper (T5) maps this to a typed envelope.
var ErrModelNotFound = errors.New("ml: trained model not found")

// ErrModelGated surfaces when a lookup resolves to a row whose status
// is one of {training, retired}. The serving layer refuses to load
// those — they're gated states per the lifecycle (see
// docs/ML_CAPABILITY_SUBSTRATE.md §4.2). Loadable states are
// `evaluating` (spot-check), `ab_testing` (dual-fire), `promoted`
// (live traffic).
var ErrModelGated = errors.New("ml: trained model is in a gated lifecycle state (training/retired)")

// loadableStatuses lists the lifecycle states the registry will load
// for serving. Mirrors the design doc's §4.4.
var loadableStatuses = map[string]struct{}{
	"evaluating": {},
	"ab_testing": {},
	"promoted":   {},
}

// modelRow holds the trained_models columns the registry needs at load
// time. Not exported — callers see Model + Prediction; this row is the
// internal lookup result.
type modelRow struct {
	ID           int64
	ProjectID    string
	Slug         string
	Task         string
	Version      string
	Status       string
	ArtifactPath string
}

// Model is a loaded trained_model + its live ORT session, ready for
// Infer calls. Registry returns Model handles; callers should not
// retain them across Reload() — the registry may evict the underlying
// session.
type Model struct {
	row     modelRow
	session Session
	// inputName + outputName name the model's input/output tensors. T3
	// ships single-input / single-output models. The dynamic_axes
	// fixture's "input_vectors" / "output_scalars" pair is the default;
	// T4-T5 will move these to per-model manifest.toml driven config.
	inputName  string
	outputName string
}

// ID returns the trained_model.id this Model was loaded from.
func (m *Model) ID() int64 { return m.row.ID }

// Slug returns the trained_model.slug.
func (m *Model) Slug() string { return m.row.Slug }

// Status returns the row's lifecycle status at load time. This is the
// status when the registry resolved the lookup; subsequent
// trained_model_promote / _retire calls may have changed the live row,
// in which case Registry.Reload() rebuilds the cache.
func (m *Model) Status() string { return m.row.Status }

// Registry resolves and caches loaded Models. Backed by the
// trained_models table (T4); each Model wraps an ORT session built
// from the row's artifact_path under ML_MODELS_ROOT.
//
// Cache eviction is LRU with maxLoaded; reload drops all cached
// sessions so the next lookup picks up post-promote/retire state.
type Registry struct {
	pool       *db.Pool
	factory    SessionFactory
	modelsRoot string
	maxLoaded  int
	defaultIO  IONames
	perTaskIO  map[string]IONames // optional override per task

	mu     sync.Mutex
	loaded map[int64]*Model // keyed by trained_model.id
	// lruOrder tracks LRU ordering (front = most recently used). Slice
	// of ids matching keys in `loaded`.
	lruOrder []int64
}

// IONames pairs the input + output tensor names a Session needs at
// construction. T3 single-IO model default is "input_vectors" /
// "output_scalars" matching the dynamic-axes fixture; T4-T5 will move
// to per-model manifest.toml resolution.
type IONames struct {
	Input  string
	Output string
}

// RegistryOption configures a Registry at construction time.
type RegistryOption func(*Registry)

// WithSessionFactory overrides the SessionFactory. Production code
// uses the default (real ORT); tests inject a fake.
func WithSessionFactory(f SessionFactory) RegistryOption {
	return func(r *Registry) { r.factory = f }
}

// WithModelsRoot overrides the ML_MODELS_ROOT directory. Defaults to
// the env var, falling back to "$HOME/dev/ml-training/models".
func WithModelsRoot(root string) RegistryOption {
	return func(r *Registry) { r.modelsRoot = root }
}

// WithMaxLoaded caps the in-memory session cache. Defaults to 8 —
// enough for the five ml-temp candidates plus the shared embedder
// plus headroom.
func WithMaxLoaded(n int) RegistryOption {
	return func(r *Registry) { r.maxLoaded = n }
}

// WithDefaultIONames sets the default input/output tensor names used
// when no per-task override is registered. Defaults to
// "input_vectors" / "output_scalars".
func WithDefaultIONames(io IONames) RegistryOption {
	return func(r *Registry) { r.defaultIO = io }
}

// WithTaskIONames registers a per-task override. Wired by T5 when the
// MCP convenience-action layer knows each model's manifest.
func WithTaskIONames(task string, io IONames) RegistryOption {
	return func(r *Registry) { r.perTaskIO[task] = io }
}

// NewRegistry returns a Registry. Most options have reasonable defaults;
// production wiring passes WithModelsRoot from config.
func NewRegistry(pool *db.Pool, opts ...RegistryOption) *Registry {
	r := &Registry{
		pool:      pool,
		factory:   realSessionFactory,
		maxLoaded: 8,
		defaultIO: IONames{Input: "input_vectors", Output: "output_scalars"},
		perTaskIO: map[string]IONames{},
		loaded:    map[int64]*Model{},
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.modelsRoot == "" {
		r.modelsRoot = os.Getenv("ML_MODELS_ROOT")
		if r.modelsRoot == "" {
			home, _ := os.UserHomeDir()
			r.modelsRoot = filepath.Join(home, "dev", "ml-training", "models")
		}
	}
	return r
}

// LoadByPromoted resolves the model currently promoted for the given
// (project, task) pair and returns a loaded Model handle. Returns
// ErrModelNotFound when no promoted row exists.
func (r *Registry) LoadByPromoted(ctx context.Context, project, task string) (*Model, error) {
	row, err := r.readPromotedRow(ctx, project, task)
	if err != nil {
		return nil, err
	}
	return r.loadByRow(row)
}

// LoadByID resolves a specific trained_model.id and returns a Model.
// Useful for the A/B harness (T6) which compares a specific trained
// version against a baseline.
func (r *Registry) LoadByID(ctx context.Context, id int64) (*Model, error) {
	row, err := r.readRowByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.loadByRow(row)
}

// Reload drops all cached sessions so subsequent lookups re-resolve
// from the DB. Called by trained_model_promote / _retire to apply
// status flips without a process restart.
func (r *Registry) Reload() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.loaded {
		_ = m.session.Close()
	}
	r.loaded = map[int64]*Model{}
	r.lruOrder = nil
}

// Close releases all cached sessions. Called at process shutdown; the
// caller should ensure no inflight Infer calls remain.
func (r *Registry) Close() error {
	r.Reload()
	return nil
}

func (r *Registry) readPromotedRow(ctx context.Context, project, task string) (modelRow, error) {
	const q = `SELECT id, project_id, slug, task, version, status, artifact_path
	           FROM trained_models
	           WHERE project_id = ? AND task = ? AND status = 'promoted'
	           LIMIT 1`
	row := r.pool.DB().QueryRowContext(ctx, q, project, task)
	var mr modelRow
	err := row.Scan(&mr.ID, &mr.ProjectID, &mr.Slug, &mr.Task, &mr.Version, &mr.Status, &mr.ArtifactPath)
	if errors.Is(err, sql.ErrNoRows) {
		return modelRow{}, fmt.Errorf("%w: project=%q task=%q status=promoted", ErrModelNotFound, project, task)
	}
	if err != nil {
		return modelRow{}, err
	}
	return mr, nil
}

func (r *Registry) readRowByID(ctx context.Context, id int64) (modelRow, error) {
	const q = `SELECT id, project_id, slug, task, version, status, artifact_path
	           FROM trained_models WHERE id = ?`
	row := r.pool.DB().QueryRowContext(ctx, q, id)
	var mr modelRow
	err := row.Scan(&mr.ID, &mr.ProjectID, &mr.Slug, &mr.Task, &mr.Version, &mr.Status, &mr.ArtifactPath)
	if errors.Is(err, sql.ErrNoRows) {
		return modelRow{}, fmt.Errorf("%w: id=%d", ErrModelNotFound, id)
	}
	if err != nil {
		return modelRow{}, err
	}
	return mr, nil
}

func (r *Registry) loadByRow(row modelRow) (*Model, error) {
	if _, ok := loadableStatuses[row.Status]; !ok {
		return nil, fmt.Errorf("%w: status=%q (loadable: evaluating, ab_testing, promoted)", ErrModelGated, row.Status)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if cached, ok := r.loaded[row.ID]; ok {
		r.touchLRU(row.ID)
		return cached, nil
	}

	// Build the session. resolveModelPath joins models_root + artifact_path.
	modelPath := r.resolveModelPath(row.ArtifactPath)
	io := r.ioForTask(row.Task)
	sess, err := r.factory(modelPath, io.Input, io.Output)
	if err != nil {
		return nil, fmt.Errorf("session build for id=%d slug=%q path=%q: %w", row.ID, row.Slug, modelPath, err)
	}

	m := &Model{
		row:        row,
		session:    sess,
		inputName:  io.Input,
		outputName: io.Output,
	}
	r.loaded[row.ID] = m
	r.lruOrder = append([]int64{row.ID}, r.lruOrder...)
	r.evictOverflowLocked()
	return m, nil
}

func (r *Registry) resolveModelPath(artifact string) string {
	if filepath.IsAbs(artifact) {
		return artifact
	}
	return filepath.Join(r.modelsRoot, artifact)
}

func (r *Registry) ioForTask(task string) IONames {
	if io, ok := r.perTaskIO[task]; ok {
		return io
	}
	return r.defaultIO
}

// touchLRU moves id to the front of lruOrder. Caller must hold r.mu.
func (r *Registry) touchLRU(id int64) {
	for i, x := range r.lruOrder {
		if x == id {
			if i == 0 {
				return
			}
			// Move to front.
			r.lruOrder = append([]int64{id}, append(r.lruOrder[:i], r.lruOrder[i+1:]...)...)
			return
		}
	}
}

// evictOverflowLocked drops least-recently-used sessions until len(loaded)
// <= maxLoaded. Caller must hold r.mu.
func (r *Registry) evictOverflowLocked() {
	for len(r.loaded) > r.maxLoaded {
		evict := r.lruOrder[len(r.lruOrder)-1]
		r.lruOrder = r.lruOrder[:len(r.lruOrder)-1]
		if m, ok := r.loaded[evict]; ok {
			_ = m.session.Close()
			delete(r.loaded, evict)
		}
	}
}
