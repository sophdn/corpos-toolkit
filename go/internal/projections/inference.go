package projections

import (
	"context"
	"database/sql"
)

// inferenceToolModelPerformanceSQL re-snapshots per-(task_id, model_name)
// totals from inference_invocations. Mirrors queryVolumeBySourceSQL: a
// DELETE + INSERT…SELECT…GROUP BY that converges on the same end state
// regardless of trigger frequency. success_count sums the call-level
// success column; outcome_success_count sums the materialized Layer-2 outcome
// predicate (chain telemetry-success-model-unification — the both-layers
// rollup, the Chain-3 router signal); latency/token columns are stored as
// totals so rates and averages compute on read (percentiles are intentionally
// absent — not foldable from totals; they stay on the per-call table).
//
// inference_invocations is aliased `qi` so the outcome predicate's correlated
// subqueries (classify->proj_benchmark_results, vault->grounding_events)
// resolve. The per-row outcome expression is shared with
// proj_inference_call_success (inferenceOutcomeSuccessExpr) so the rollup and
// the per-call projection cannot drift.
const inferenceToolModelPerformanceSQL = `
	INSERT INTO proj_inference_tool_model_performance
		(task_id, model_name, call_count, success_count, outcome_success_count,
		 total_latency_ms, max_latency_ms,
		 total_input_tokens, total_output_tokens, calls_with_tokens, last_invoked_at,
		 last_event_id, last_event_ts)
	SELECT
		qi.task_id,
		qi.model_name,
		COUNT(*)                                  AS call_count,
		COALESCE(SUM(qi.success), 0)              AS success_count,
		COALESCE(SUM(` + inferenceOutcomeSuccessExpr + `), 0) AS outcome_success_count,
		COALESCE(SUM(qi.latency_ms), 0)           AS total_latency_ms,
		COALESCE(MAX(qi.latency_ms), 0)           AS max_latency_ms,
		COALESCE(SUM(qi.input_tokens), 0)         AS total_input_tokens,
		COALESCE(SUM(qi.output_tokens), 0)        AS total_output_tokens,
		SUM(CASE WHEN qi.input_tokens IS NOT NULL OR qi.output_tokens IS NOT NULL THEN 1 ELSE 0 END) AS calls_with_tokens,
		MAX(qi.created_at)                        AS last_invoked_at,
		-- watermark: carry the most-recent row's id (TEXT column) + created_at.
		-- inference_invocations.created_at is NOT NULL so MAX is always real.
		CAST(MAX(qi.id) AS TEXT), MAX(qi.created_at)
	FROM inference_invocations qi
	GROUP BY qi.task_id, qi.model_name
`

// inferenceToolModelPerformance folds inference_invocations into
// proj_inference_tool_model_performance — the per-(tool × model) ranking
// projection (chain per-tool-per-model-observability). Read-side projection
// (its Name carries the "inference_" prefix): Fold ignores the RawEvent and
// re-snapshots from the source table, so the byte-identical rebuild-from-
// empty invariant holds vacuously (fold == rebuild). At homelab inference
// volume the re-snapshot cost is trivial; an incremental fold is a future
// optimization if call volume ever makes the GROUP BY scan matter.
type inferenceToolModelPerformance struct{}

func init() { Register(inferenceToolModelPerformance{}) }

func (inferenceToolModelPerformance) Name() string { return "inference_tool_model_performance" }
func (inferenceToolModelPerformance) TableName() string {
	return "proj_inference_tool_model_performance"
}

// DependsOn: the Layer-2 outcome rollup (outcome_success_count) reads
// proj_benchmark_results inside the fold via the classify arm of the shared
// outcome predicate, so benchmark_results must rebuild first on a full pass.
func (inferenceToolModelPerformance) DependsOn() []string { return []string{"benchmark_results"} }

func (p inferenceToolModelPerformance) Fold(ctx context.Context, tx *sql.Tx, _ RawEvent) error {
	return p.RebuildFromEmpty(ctx, tx)
}

func (p inferenceToolModelPerformance) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	return rebuildProjection(ctx, tx, p.TableName(), inferenceToolModelPerformanceSQL)
}
