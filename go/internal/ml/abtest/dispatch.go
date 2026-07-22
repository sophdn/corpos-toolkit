package abtest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/ml"
)

// Policy controls which output Dispatch returns to the caller when
// both baseline and trained have fired.
type Policy string

const (
	// PreferBaseline returns the baseline output. Use during early
	// ab_testing when the baseline is still trusted as ground truth.
	PreferBaseline Policy = "prefer_baseline"
	// PreferTrained returns the trained output. Use late in ab_testing
	// once eval rates suggest the trained model is reliable.
	PreferTrained Policy = "prefer_trained"
	// Alternate flips between baseline and trained on alternating calls.
	// Useful for collecting unbiased click-through data on both paths.
	// Alternation is per-model and persisted only across the lifetime of
	// the in-memory state (not across binary restarts) — the comparison
	// rows are the durable record.
	Alternate Policy = "alternate"
)

// BaselineFunc is the existing implementation Dispatch wraps. Same shape
// as Model.Infer's return so callers see consistent output regardless
// of which path served.
type BaselineFunc func(ctx context.Context, feat ml.Features) (output []float32, outputShape []int64, err error)

// Config is the input to Dispatch.
type Config struct {
	Baseline         BaselineFunc
	Model            *ml.Model
	Features         ml.Features
	Policy           Policy
	GroundingEventID int64 // optional FK into grounding_events
}

// Result is the Dispatch return shape.
type Result struct {
	Output             []float32 `json:"output"`
	OutputShape        []int64   `json:"output_shape"`
	UsedPath           string    `json:"used_path"`
	BaselineLatencyMs  float64   `json:"baseline_latency_ms"`
	TrainedLatencyMs   float64   `json:"trained_latency_ms"`
	ComparisonRowID    int64     `json:"comparison_row_id"`
	BaselineSkipped    bool      `json:"baseline_skipped,omitempty"`
	TrainedSkipped     bool      `json:"trained_skipped,omitempty"`
	ShortCircuitReason string    `json:"short_circuit_reason,omitempty"`
}

// Deps bundles the persistence handles Dispatch needs.
type Deps struct {
	Pool *db.Pool
}

// alternateCounter tracks per-model alternation across calls within
// a single binary process. Keyed by trained_model.id.
type alternateCounter struct {
	mu       sync.Mutex
	counters map[int64]int64
}

func (a *alternateCounter) pick(modelID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.counters == nil {
		a.counters = map[int64]int64{}
	}
	c := a.counters[modelID]
	a.counters[modelID] = c + 1
	if c%2 == 0 {
		return "baseline"
	}
	return "trained"
}

var globalAlternate = &alternateCounter{}

// ErrConfigInvalid surfaces from Dispatch when the Config is missing
// required fields.
var ErrConfigInvalid = errors.New("abtest: invalid Config")

// Dispatch runs the A/B harness.
//
// Short-circuit modes (per design doc §8.1):
//   - trained_model.status='promoted': only trained fires; baseline
//     is skipped (Result.BaselineSkipped=true). No comparison row.
//   - trained_model.status in {'evaluating','retired'} OR Model is
//     nil: only baseline fires; trained is skipped. No comparison row.
//   - trained_model.status='ab_testing': both fire (in parallel via
//     goroutines), one row lands in ab_comparisons, the Policy-selected
//     output is returned.
//
// The status filter intentionally checks Model.Status() at dispatch
// time — Registry.LoadByPromoted/LoadByID has already returned the
// row, and Reload() invalidates the cache after a status flip.
func Dispatch(ctx context.Context, deps Deps, cfg Config) (Result, error) {
	if cfg.Baseline == nil {
		return Result{}, fmt.Errorf("%w: Baseline func is nil", ErrConfigInvalid)
	}
	if cfg.Policy == "" {
		cfg.Policy = PreferBaseline
	}
	switch cfg.Policy {
	case PreferBaseline, PreferTrained, Alternate:
		// OK.
	default:
		return Result{}, fmt.Errorf("%w: unknown Policy %q", ErrConfigInvalid, cfg.Policy)
	}

	// Short-circuit: no trained model — baseline-only path.
	if cfg.Model == nil {
		return runBaselineOnly(ctx, cfg, "no_model_supplied")
	}

	switch cfg.Model.Status() {
	case "promoted":
		return runTrainedOnly(ctx, cfg, "model_promoted")
	case "evaluating", "retired":
		return runBaselineOnly(ctx, cfg, "model_in_"+cfg.Model.Status())
	case "ab_testing":
		// fall through to dual-fire
	default:
		// Unknown status — refuse rather than guess.
		return Result{}, fmt.Errorf("abtest: model status %q is not loadable for serving", cfg.Model.Status())
	}

	if deps.Pool == nil {
		return Result{}, fmt.Errorf("abtest: ab_testing path requires Pool for ab_comparisons write")
	}

	// Dual-fire path.
	type baselineRes struct {
		out       []float32
		outShape  []int64
		latencyMs float64
		err       error
	}
	type trainedRes struct {
		pred      ml.Prediction
		latencyMs float64
		err       error
	}

	bCh := make(chan baselineRes, 1)
	tCh := make(chan trainedRes, 1)

	go func() {
		start := time.Now()
		out, shape, err := cfg.Baseline(ctx, cfg.Features)
		bCh <- baselineRes{
			out: out, outShape: shape,
			latencyMs: float64(time.Since(start).Microseconds()) / 1000.0,
			err:       err,
		}
	}()
	go func() {
		start := time.Now()
		pred, err := cfg.Model.Infer(ctx, cfg.Features)
		tCh <- trainedRes{
			pred:      pred,
			latencyMs: float64(time.Since(start).Microseconds()) / 1000.0,
			err:       err,
		}
	}()

	bRes := <-bCh
	tRes := <-tCh

	if bRes.err != nil {
		return Result{}, fmt.Errorf("baseline path failed: %w", bRes.err)
	}
	if tRes.err != nil {
		return Result{}, fmt.Errorf("trained path failed: %w", tRes.err)
	}

	used := selectPath(cfg.Policy, cfg.Model.ID())

	rowID, err := writeComparisonRow(ctx, deps.Pool, cfg, bRes.out, tRes.pred.Output, used, bRes.latencyMs, tRes.latencyMs)
	if err != nil {
		return Result{}, fmt.Errorf("ab_comparisons write: %w", err)
	}

	out, outShape := bRes.out, bRes.outShape
	if used == "trained" {
		out, outShape = tRes.pred.Output, tRes.pred.OutputShape
	}
	return Result{
		Output:            out,
		OutputShape:       outShape,
		UsedPath:          used,
		BaselineLatencyMs: bRes.latencyMs,
		TrainedLatencyMs:  tRes.latencyMs,
		ComparisonRowID:   rowID,
	}, nil
}

func selectPath(policy Policy, modelID int64) string {
	switch policy {
	case PreferBaseline:
		return "baseline"
	case PreferTrained:
		return "trained"
	case Alternate:
		return globalAlternate.pick(modelID)
	}
	return "baseline"
}

func runBaselineOnly(ctx context.Context, cfg Config, reason string) (Result, error) {
	start := time.Now()
	out, shape, err := cfg.Baseline(ctx, cfg.Features)
	if err != nil {
		return Result{}, fmt.Errorf("baseline (only path) failed: %w", err)
	}
	return Result{
		Output:             out,
		OutputShape:        shape,
		UsedPath:           "baseline",
		BaselineLatencyMs:  float64(time.Since(start).Microseconds()) / 1000.0,
		TrainedSkipped:     true,
		ShortCircuitReason: reason,
	}, nil
}

func runTrainedOnly(ctx context.Context, cfg Config, reason string) (Result, error) {
	start := time.Now()
	pred, err := cfg.Model.Infer(ctx, cfg.Features)
	if err != nil {
		return Result{}, fmt.Errorf("trained (only path) failed: %w", err)
	}
	return Result{
		Output:             pred.Output,
		OutputShape:        pred.OutputShape,
		UsedPath:           "trained",
		TrainedLatencyMs:   float64(time.Since(start).Microseconds()) / 1000.0,
		BaselineSkipped:    true,
		ShortCircuitReason: reason,
	}, nil
}

func writeComparisonRow(
	ctx context.Context,
	pool *db.Pool,
	cfg Config,
	baselineOut, trainedOut []float32,
	usedPath string,
	baselineLatency, trainedLatency float64,
) (int64, error) {
	spanID, err := events.SpanIDFromContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("derive span_id: %w", err)
	}

	bOut, err := summarizeOutput(baselineOut)
	if err != nil {
		return 0, fmt.Errorf("summarize baseline output: %w", err)
	}
	tOut, err := summarizeOutput(trainedOut)
	if err != nil {
		return 0, fmt.Errorf("summarize trained output: %w", err)
	}

	// features_hash matches the model_predictions.features_hash field
	// (T5/migration 044). Same canonical-serialization → SHA-256.
	featHash := featuresHash(cfg.Features)

	groundingArg := sql.NullInt64{Int64: cfg.GroundingEventID, Valid: cfg.GroundingEventID != 0}

	var id int64
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		const q = `INSERT INTO ab_comparisons
			(model_id, features_hash, baseline_output, trained_output,
			 used_path, policy, baseline_latency_ms, trained_latency_ms,
			 span_id, grounding_event_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		res, err := tx.ExecContext(ctx, q,
			cfg.Model.ID(), featHash, bOut, tOut,
			usedPath, string(cfg.Policy), baselineLatency, trainedLatency,
			spanID, groundingArg)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	return id, err
}

// summarizeOutput mirrors ml.outputSummary's bounded shape — caps long
// vectors at 16 head values + total length.
func summarizeOutput(out []float32) (string, error) {
	const maxValues = 16
	type shape struct {
		HeadVals []float32 `json:"head_values"`
		Total    int       `json:"total_values"`
	}
	body := shape{Total: len(out)}
	if len(out) <= maxValues {
		body.HeadVals = out
	} else {
		body.HeadVals = out[:maxValues]
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// featuresHash mirrors ml.hashFeatures — same canonical-serialization
// so a comparison row's hash joins to a prediction row's hash when
// both refer to the same input. Inline rather than calling into ml to
// keep the abtest subpackage self-contained for the unlikely case of
// a future ml-internal restructure.
func featuresHash(f ml.Features) string {
	raw, err := json.Marshal(struct {
		Data  []float32 `json:"data"`
		Shape []int64   `json:"shape"`
	}{Data: f.Data, Shape: f.Shape})
	if err != nil {
		return "marshal-error"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
