package projections

import (
	"context"
	"database/sql"
)

// retrievalSuccessPerQuerySQL rebuilds the row-per-grounding-event
// projection by aggregating click_kinds via SQL GROUP BY. The
// kinds_fired JSON array uses SQLite's json_group_array(DISTINCT
// click_kind) so an event with followed+cited gets ["followed","cited"]
// in some order; consumers don't depend on ordering. NULL → '[]'
// coercion keeps the column type-stable.
const retrievalSuccessPerQuerySQL = `
	INSERT INTO proj_retrieval_success_per_query
		(grounding_event_id, project_id, action, query_text, prompt_id,
		 results_count,
		 had_followed, had_cited, had_mentioned, had_resolved_from,
		 max_click_weight, kinds_fired, success, was_proactive,
		 last_event_id, last_event_ts)
	SELECT
		ge.id,
		ge.project_id,
		ge.action,
		ge.query_text,
		ge.prompt_id,
		ge.results_count,
		COALESCE(MAX(CASE WHEN qi.click_kind = 'followed'      THEN 1 ELSE 0 END), 0) AS had_followed,
		COALESCE(MAX(CASE WHEN qi.click_kind = 'cited'         THEN 1 ELSE 0 END), 0) AS had_cited,
		COALESCE(MAX(CASE WHEN qi.click_kind = 'mentioned'     THEN 1 ELSE 0 END), 0) AS had_mentioned,
		COALESCE(MAX(CASE WHEN qi.click_kind = 'resolved-from' THEN 1 ELSE 0 END), 0) AS had_resolved_from,
		COALESCE(MAX(qi.click_weight), 0.0) AS max_click_weight,
		COALESCE(
			(SELECT json_group_array(DISTINCT click_kind)
			 FROM query_interactions
			 WHERE grounding_event_id = ge.id),
			'[]'
		) AS kinds_fired,
		CASE
			WHEN COALESCE(MAX(qi.click_weight), 0.0) >= 0.8 THEN 1
			WHEN COALESCE(MAX(CASE WHEN qi.click_kind = 'resolved-from' THEN 1 ELSE 0 END), 0) = 1 THEN 1
			ELSE 0
		END AS success,
		CASE WHEN ge.query_source = 'proactive_hook' THEN 1 ELSE 0 END AS was_proactive,
		-- Bug query-telemetry-projections-hardcode-empty-last-event-id-ts:
		-- hardcoded '' masked the population gap (sibling of the reranker fix
		-- 5c8fd43b). This projection is one row per grounding_event (GROUP BY
		-- ge.id), so carry the source event's id + real created_at directly.
		CAST(ge.id AS TEXT), ge.created_at
	FROM grounding_events ge
	LEFT JOIN query_interactions qi ON qi.grounding_event_id = ge.id
	GROUP BY ge.id
`

// retrievalSuccessPerQuery folds grounding_events + query_interactions
// + query_resolutions into proj_retrieval_success_per_query — one row
// per grounding_events.id with per-tier boolean flags, max_click_weight
// rollup, and a `success` convenience boolean. See TT1 §6.2 for shape.
//
// `success` = max_click_weight ≥ 0.8 OR had_resolved_from = 1; this is
// the consumer-facing definition used by the dashboard's retrieval
// health panel and is intentionally inclusive of resolved-from even
// when no other tier fired (the resolved-from rationale is the
// canonical positive signal for a closed entity).
type retrievalSuccessPerQuery struct{}

func init() { Register(retrievalSuccessPerQuery{}) }

func (retrievalSuccessPerQuery) Name() string      { return "retrieval_success_per_query" }
func (retrievalSuccessPerQuery) TableName() string { return "proj_retrieval_success_per_query" }

func (r retrievalSuccessPerQuery) Fold(ctx context.Context, tx *sql.Tx, _ RawEvent) error {
	return r.RebuildFromEmpty(ctx, tx)
}

func (r retrievalSuccessPerQuery) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	return rebuildProjection(ctx, tx, r.TableName(), retrievalSuccessPerQuerySQL)
}
