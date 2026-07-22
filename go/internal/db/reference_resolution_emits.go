package db

import (
	"context"
	"database/sql"
	"fmt"
)

// ReferenceResolutionEmitInsert is the typed payload for one row in the
// reference_resolution_emits side-table (migration 042). One row per
// resolve_references emit; FK to grounding_events by ID.
//
// See docs/REFERENCE_RESOLUTION_FRONTEND.md §3.6 for the side-table
// rationale (kept off grounding_events because reference-resolution-
// specific columns would leave 99% of rows with NULLs).
//
// MLConfidenceScore is NULLABLE: a *float64 nil distinguishes
// "not yet classified" from "classified low" once T7 ML scoring lands.
type ReferenceResolutionEmitInsert struct {
	GroundingEventID           int64
	Shape                      string
	ConfidenceScore            float64
	DetectionMethod            string
	StartPos                   int
	EndPos                     int
	ConfidenceTier             string
	PresentationRecommendation string
	PresentedAs                string
	ResolverName               string
	RetrievalCostMs            int64
	MLConfidenceScore          *float64
}

// InsertReferenceResolutionEmitTx inserts one side-table row inside an
// existing transaction. Called from refresolve.emitGroundingEvents
// alongside InsertGroundingEventTx so the side-table row lands in the
// same atomic write — either both land or both roll back.
//
// No ON CONFLICT clause: the grounding_event_id is the primary key, and
// the upstream emitGroundingEvents synthesizes a fresh (session_id,
// call_id) per reference per call. A duplicate here would indicate a
// bug worth surfacing, not a state to silently absorb.
func InsertReferenceResolutionEmitTx(ctx context.Context, tx *sql.Tx, e ReferenceResolutionEmitInsert) error {
	var ml sql.NullFloat64
	if e.MLConfidenceScore != nil {
		ml.Valid = true
		ml.Float64 = *e.MLConfidenceScore
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO reference_resolution_emits
			(grounding_event_id, shape, confidence_score, detection_method,
			 start_pos, end_pos, confidence_tier, presentation_recommendation,
			 presented_as, resolver_name, retrieval_cost_ms, ml_confidence_score)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.GroundingEventID, e.Shape, e.ConfidenceScore, e.DetectionMethod,
		e.StartPos, e.EndPos, e.ConfidenceTier, e.PresentationRecommendation,
		e.PresentedAs, e.ResolverName, e.RetrievalCostMs, ml,
	)
	if err != nil {
		return fmt.Errorf("insert reference_resolution_emits: %w", err)
	}
	return nil
}
