package ml

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/db"
)

// PredictionRow mirrors a model_predictions row. Used by future
// eval projections + drift-detection helpers; T5 only writes via
// WritePrediction.
type PredictionRow struct {
	ID               int64   `json:"id"`
	ModelID          int64   `json:"model_id"`
	FeaturesHash     string  `json:"features_hash"`
	OutputSummary    string  `json:"output_summary"`
	LatencyMs        float64 `json:"latency_ms"`
	SpanID           string  `json:"span_id"`
	GroundingEventID *int64  `json:"grounding_event_id,omitempty"`
	CreatedAt        string  `json:"created_at"`
}

// WritePrediction persists one row to model_predictions. Called by
// HandleInference after every Model.Infer call. Returns the inserted
// row id.
//
// span_id is required (caller derives via events.SpanIDFromContext);
// grounding_event_id is optional (non-zero → INSERT; zero → NULL).
func WritePrediction(
	ctx context.Context,
	pool *db.Pool,
	modelID int64,
	pred Prediction,
	spanID string,
	groundingEventID int64,
) (int64, error) {
	if pool == nil {
		return 0, fmt.Errorf("WritePrediction: pool is nil")
	}
	if spanID == "" {
		return 0, fmt.Errorf("WritePrediction: span_id is required")
	}

	summary, err := outputSummary(pred)
	if err != nil {
		return 0, fmt.Errorf("WritePrediction: summarize output: %w", err)
	}

	var id int64
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// sql.NullInt64 carries Valid=false when grounding_event_id is
		// unset, which SQLite stores as NULL — keeps the FK clean for
		// the classifier-shaped predictions that don't tie back to a
		// search row.
		groundingArg := sql.NullInt64{Int64: groundingEventID, Valid: groundingEventID != 0}
		const q = `INSERT INTO model_predictions
			(model_id, features_hash, output_summary, latency_ms, span_id, grounding_event_id)
			VALUES (?, ?, ?, ?, ?, ?)`
		res, err := tx.ExecContext(ctx, q,
			modelID, pred.FeatHash, summary, pred.LatencyMs, spanID, groundingArg)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	return id, err
}

// outputSummary produces a bounded-size JSON of the model output for
// the predictions table. Caps long output vectors at the first 16
// values to keep the row small; full output is reproducible from
// features_hash + model_id by re-running offline.
func outputSummary(pred Prediction) (string, error) {
	const maxValues = 16
	type summaryShape struct {
		Shape    []int64   `json:"shape"`
		HeadVals []float32 `json:"head_values"`
		Total    int       `json:"total_values"`
	}
	out := summaryShape{
		Shape: pred.OutputShape,
		Total: len(pred.Output),
	}
	if len(pred.Output) <= maxValues {
		out.HeadVals = pred.Output
	} else {
		out.HeadVals = pred.Output[:maxValues]
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
