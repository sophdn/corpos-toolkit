package ml

import (
	"context"
	"encoding/json"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// InferenceParams is the JSON shape `ml.inference` accepts. Either
// model_id OR task must be supplied; model_id wins when both are set.
//
// features is the input shape Model.Infer accepts: Data + Shape.
// grounding_event_id is optional — populated when the caller is
// resolving a search-triggered inference (cross-encoder reranker,
// source router) and wants the prediction joinable to the originating
// grounding_events row.
type InferenceParams struct {
	ModelID          int64     `json:"model_id"`
	Task             string    `json:"task"`
	FeaturesData     []float32 `json:"features_data"`
	FeaturesShape    []int64   `json:"features_shape"`
	GroundingEventID int64     `json:"grounding_event_id"`
}

// InferenceResult is the ml.inference response envelope. Carries the
// Prediction body + the span_id the telemetry row was written under.
// Error envelope (Error + Hint) mirrors work-surface convention.
type InferenceResult struct {
	OK              bool        `json:"ok,omitempty"`
	Prediction      *Prediction `json:"prediction,omitempty"`
	SpanID          string      `json:"span_id,omitempty"`
	PredictionRowID int64       `json:"prediction_row_id,omitempty"`
	Error           string      `json:"error,omitempty"`
	Hint            string      `json:"hint,omitempty"`
}

// HandlerDeps bundles the dispatch dependencies the inference handler
// needs. Pool is used for the telemetry write; Registry is used for the
// model lookup + session load. Both required at table-build time.
type HandlerDeps struct {
	Pool     *db.Pool
	Registry *Registry
}

// HandleInference implements ml.inference.
//
// Resolution order for the model:
//  1. If params.model_id > 0: Registry.LoadByID(model_id).
//  2. Else if params.task != "": Registry.LoadByPromoted(project, task).
//  3. Else: error envelope.
//
// On success, writes a model_predictions row (T5 telemetry side-table)
// keyed by span_id (carried in ctx via events.SpanIDFromContext) and
// returns the Prediction + row id.
func HandleInference(ctx context.Context, deps HandlerDeps, project string, params json.RawMessage) (InferenceResult, error) {
	var p InferenceParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return InferenceResult{}, fmt.Errorf("parse params: %w", err)
		}
	}

	model, err := resolveModel(ctx, deps.Registry, project, p)
	if err != nil {
		return InferenceResult{Error: err.Error()}, nil
	}

	feat := Features{Data: p.FeaturesData, Shape: p.FeaturesShape}
	pred, err := model.Infer(ctx, feat)
	if err != nil {
		return InferenceResult{Error: err.Error()}, nil
	}

	// span_id from ctx (agent-first-substrate envelope). The MCP
	// dispatcher stamps an explicit span_id at dispatch time; tests
	// that exercise HandleInference directly can stamp via
	// events.WithSpanID. When ctx has no explicit stamp,
	// events.SpanIDFromContext mints a fresh UUIDv4 — keeping the
	// model_predictions row writable and joinable even from direct
	// handler calls.
	spanID, err := events.SpanIDFromContext(ctx)
	if err != nil {
		return InferenceResult{Error: fmt.Sprintf("derive span_id: %v", err)}, nil
	}

	rowID, err := WritePrediction(ctx, deps.Pool, model.ID(), pred, spanID, p.GroundingEventID)
	if err != nil {
		return InferenceResult{Error: fmt.Sprintf("inference succeeded but telemetry write failed: %v", err)}, nil
	}

	return InferenceResult{
		OK:              true,
		Prediction:      &pred,
		SpanID:          spanID,
		PredictionRowID: rowID,
	}, nil
}

func resolveModel(ctx context.Context, reg *Registry, project string, p InferenceParams) (*Model, error) {
	if p.ModelID > 0 {
		return reg.LoadByID(ctx, p.ModelID)
	}
	if p.Task != "" {
		if project == "" {
			return nil, fmt.Errorf("ml.inference requires project when resolving by task; pass project at the dispatch envelope")
		}
		return reg.LoadByPromoted(ctx, project, p.Task)
	}
	return nil, fmt.Errorf("ml.inference requires either model_id or task")
}
