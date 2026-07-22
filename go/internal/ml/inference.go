package ml

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInputShape surfaces from Infer when the supplied features don't
// match the model's expected input shape. Validated at the handler
// seam (not deeper in the ORT binding) so the error envelope can carry
// shape diagnostics.
var ErrInputShape = errors.New("ml: input features don't match expected shape")

// Prediction is the inference handler's return shape. Carries the
// output (type-dispatched at the convenience-action layer in T5),
// latency, the model_id this row was served by, and a content hash of
// the input features (drift detection + cache key per design doc §7).
type Prediction struct {
	Output      []float32 `json:"output"`
	OutputShape []int64   `json:"output_shape"`
	LatencyMs   float64   `json:"latency_ms"`
	ModelID     int64     `json:"model_id"`
	FeatHash    string    `json:"feat_hash"`
}

// Features is the input shape Infer accepts. T3 single-input single-
// output models read Data via the model's declared input tensor.
// Multi-input models (cross-encoder reranker) will land a wider type
// in T5/T6 — likely a discriminated union of Features types.
type Features struct {
	// Data is the flattened tensor input. For an input of shape
	// (batch=2, vec=10) Data is len=20 with row-major ordering.
	Data []float32
	// Shape declares the input tensor shape (e.g. []int64{2, 10}).
	// Required; the handler validates that len(Data) equals the
	// product of the shape dims.
	Shape []int64
}

// Infer runs the model on the supplied features and returns a
// Prediction. Validates shape; runs the session; tracks latency; hashes
// features for telemetry.
func (m *Model) Infer(ctx context.Context, f Features) (Prediction, error) {
	_ = ctx // reserved for cancellation / deadline propagation in T5
	if err := validateFeatures(f); err != nil {
		return Prediction{}, err
	}

	start := time.Now()
	out, outShape, err := m.session.Run(f.Data, f.Shape)
	latency := time.Since(start)
	if err != nil {
		return Prediction{}, fmt.Errorf("model %s (id=%d) inference failed: %w", m.row.Slug, m.row.ID, err)
	}

	return Prediction{
		Output:      out,
		OutputShape: outShape,
		LatencyMs:   float64(latency.Microseconds()) / 1000.0,
		ModelID:     m.row.ID,
		FeatHash:    hashFeatures(f),
	}, nil
}

// Close releases the underlying session. Tests close models manually;
// production wiring closes via Registry.Close().
func (m *Model) Close() error {
	if m.session == nil {
		return nil
	}
	return m.session.Close()
}

func validateFeatures(f Features) error {
	if len(f.Data) == 0 {
		return fmt.Errorf("%w: input data is empty", ErrInputShape)
	}
	if len(f.Shape) == 0 {
		return fmt.Errorf("%w: input shape is empty", ErrInputShape)
	}
	expected := int64(1)
	for _, d := range f.Shape {
		if d <= 0 {
			return fmt.Errorf("%w: input shape has non-positive dim %d", ErrInputShape, d)
		}
		expected *= d
	}
	if int64(len(f.Data)) != expected {
		return fmt.Errorf("%w: len(data)=%d but shape=%v implies %d elements",
			ErrInputShape, len(f.Data), f.Shape, expected)
	}
	return nil
}

// hashFeatures produces a SHA-256 hex digest of the canonical-serialized
// features. Used as the content key for the prediction telemetry row
// (T7 / model_predictions table) and as a drift-detection input.
func hashFeatures(f Features) string {
	// JSON serialization is the canonical form. For [-Inf, Inf, NaN]
	// edge cases the standard encoder errors — those are upstream
	// validation problems and would surface as ErrInputShape here.
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
