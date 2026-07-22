package projections

import (
	"context"
	"database/sql"
)

// Chain 2 (telemetry-success-model-unification, T4): the OUTCOME-level
// (Layer 2) success materialization. proj_inference_call_success holds one row
// per inference_invocations row — uniform with the search cluster's per-row
// proj_retrieval_success_per_query — carrying BOTH the call-level liveness
// layer (call_success) and the materialized outcome layer (outcome_success).
//
// This replaces the read-time predicate-SQL interpolation that used to live in
// observehttp/inference_success_predicates.go + inference_v2.go. The success
// DEFINITION now lives with the fold (here); the read handler keeps only the
// windowed/warmup/per-day aggregation and the basis label. See
// docs/CHAIN2_SUCCESS_MODEL_TARGET.md.

// inferenceOutcomeSuccessExpr is the Layer-2 outcome predicate, dispatched by
// task_id and evaluated per inference_invocations row aliased `qi`. It is the
// materialized form of the former read-time predicate registry:
//
//   - default (any unregistered task): the call produced output AND took
//     nonzero time (output_tokens IS NOT NULL AND latency_ms > 0) — the
//     liveness floor.
//   - classify_* : EXISTS a proj_benchmark_results row for the task scoring
//     > 0.5. This is ANY matching row, not "latest": the former code's
//     `ORDER BY run_at DESC LIMIT 1` inside EXISTS was inert (EXISTS tests for
//     >=1 matching row regardless of order), so it is dropped here — a
//     behavior-preserving correction of bug 948 (the net pins any-row via
//     TestCharacterization_ClassifyAnyBenchmarkRowNotLatest).
//   - vault-rerank-retrieve : EXISTS a proximate action='vault_search'
//     grounding_events row with results_count > 0, within a latency-scaled
//     window |ge.created_at - qi.created_at| <= latency_ms/1000 + 2 s. The
//     scaling accommodates the two-pass search shape: one grounding row per
//     search is written at pass-2 exit, ~latency seconds after the pass-1
//     invocation row, so a flat window would mis-classify the pass-1 row.
//
// Dispatch uses substr(task_id,1,9) = 'classify_' (NOT LIKE 'classify_%' — '_'
// is a LIKE wildcard) to mirror the Go lookupSuccessPredicate's
// taskID[:9] == "classify_" exactly; the 8-vs-9-char and dash-not-underscore
// boundaries are pinned by TestLookupSuccessPredicate_Registry. The SQL and Go
// dispatch are kept in agreement by TestInferenceCallSuccess_DispatchMatchesRegistry.
const inferenceOutcomeSuccessExpr = `CASE
		WHEN qi.task_id = 'vault-rerank-retrieve' THEN (CASE WHEN EXISTS (
			SELECT 1 FROM grounding_events ge
			WHERE ge.action = 'vault_search'
			  AND ABS(strftime('%s', ge.created_at) - strftime('%s', qi.created_at))
			      <= (qi.latency_ms / 1000) + 2
			  AND ge.results_count > 0
		) THEN 1 ELSE 0 END)
		WHEN substr(qi.task_id, 1, 9) = 'classify_' THEN (CASE WHEN EXISTS (
			SELECT 1 FROM proj_benchmark_results br
			WHERE br.task_id = qi.task_id
			  AND br.accuracy_score IS NOT NULL
			  AND br.accuracy_score > 0.5
		) THEN 1 ELSE 0 END)
		ELSE (CASE WHEN qi.output_tokens IS NOT NULL AND qi.latency_ms > 0 THEN 1 ELSE 0 END)
	END`

// inferenceOutcomeKindExpr names the predicate that produced outcome_success,
// in lockstep with the dispatch in inferenceOutcomeSuccessExpr. Short stable
// tokens (not the prose Description, which stays the single source in
// observehttp) so there is no prose duplication across the SQL/Go boundary.
const inferenceOutcomeKindExpr = `CASE
		WHEN qi.task_id = 'vault-rerank-retrieve' THEN 'vault-rerank-retrieve'
		WHEN substr(qi.task_id, 1, 9) = 'classify_' THEN 'classify'
		ELSE 'default'
	END`

// inferenceCallSuccessSQL re-snapshots proj_inference_call_success from
// inference_invocations, one row per invocation, materializing both success
// layers. The DELETE+INSERT converges on the same end state regardless of
// trigger frequency (read-side projection: Fold == RebuildFromEmpty).
const inferenceCallSuccessSQL = `
	INSERT INTO proj_inference_call_success
		(id, task_id, model_name, created_at,
		 call_success, outcome_success, outcome_kind,
		 last_event_id, last_event_ts)
	SELECT
		qi.id,
		qi.task_id,
		qi.model_name,
		qi.created_at,
		qi.success,
		` + inferenceOutcomeSuccessExpr + `,
		` + inferenceOutcomeKindExpr + `,
		-- per-row projection: the row id IS the watermark; created_at is NOT NULL.
		CAST(qi.id AS TEXT), qi.created_at
	FROM inference_invocations qi
`

// inferenceCallSuccess folds inference_invocations into
// proj_inference_call_success — the per-call both-layers success projection
// (chain telemetry-success-model-unification). Read-side projection: Fold
// ignores the RawEvent and re-snapshots from the source table + ground-truth
// joins, so the byte-identical rebuild-from-empty invariant holds vacuously.
type inferenceCallSuccess struct{}

func init() { Register(inferenceCallSuccess{}) }

func (inferenceCallSuccess) Name() string      { return "inference_call_success" }
func (inferenceCallSuccess) TableName() string { return "proj_inference_call_success" }

// DependsOn: the classify outcome arm reads proj_benchmark_results inside the
// fold transaction, so benchmark_results must rebuild first on a full pass
// (otherwise the EXISTS captures pre-event benchmark state). grounding_events
// is a source table (not a projection), so it needs no dependency edge.
func (inferenceCallSuccess) DependsOn() []string { return []string{"benchmark_results"} }

func (p inferenceCallSuccess) Fold(ctx context.Context, tx *sql.Tx, _ RawEvent) error {
	return p.RebuildFromEmpty(ctx, tx)
}

func (p inferenceCallSuccess) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	return rebuildProjection(ctx, tx, p.TableName(), inferenceCallSuccessSQL)
}
