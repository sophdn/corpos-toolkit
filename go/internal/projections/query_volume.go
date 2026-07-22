package projections

import (
	"context"
	"database/sql"
)

// queryVolumeBySourceSQL re-snapshots the bucketed volume from
// grounding_events with a sub-select for success_count.
//
// success_count uses the SAME definition as
// proj_retrieval_success_per_query.success — `MAX(click_weight) >= 0.8 OR
// any resolved-from` — so the two /telemetry "success rate" surfaces
// (volume-by-source and retrieval-health) agree (bug 954). The previous
// `click_kind IN ('followed','resolved-from')` definition silently EXCLUDED
// `cited` (weight 0.8), which the canonical definition counts as a success;
// it included resolved-from regardless of weight, which the weight arm alone
// would miss when a per-installation config weights resolved-from below 0.8,
// so the explicit resolved-from arm is retained. The COUNT(DISTINCT) over
// grounding_event_id prevents double-counting per query.
const queryVolumeBySourceSQL = `
	INSERT INTO proj_query_volume_by_source
		(project_id, action, query_source, day,
		 query_count, zero_result_count, success_count, avg_results_count,
		 last_event_id, last_event_ts)
	SELECT
		ge.project_id,
		ge.action,
		ge.query_source,
		substr(ge.created_at, 1, 10) AS day,
		COUNT(*) AS query_count,
		SUM(CASE WHEN ge.results_count = 0 THEN 1 ELSE 0 END) AS zero_result_count,
		COUNT(DISTINCT s.grounding_event_id) AS success_count,
		COALESCE(AVG(CAST(ge.results_count AS REAL)), 0.0) AS avg_results_count,
		-- Bug query-telemetry-projections-hardcode-empty-last-event-id-ts:
		-- these were hardcoded '' on every row, masking the population gap
		-- as populated-with-blank (the same shape the reranker projection had
		-- before 5c8fd43b). This is an aggregated bucket, so carry the
		-- MOST-RECENT grounding_event in the group: MAX(id) as the watermark
		-- id (cast to the TEXT column), MAX(created_at) as the real timestamp.
		-- grounding_events.created_at is NOT NULL so MAX is always real.
		CAST(MAX(ge.id) AS TEXT), MAX(ge.created_at)
	FROM grounding_events ge
	LEFT JOIN (
		SELECT grounding_event_id
		FROM query_interactions
		GROUP BY grounding_event_id
		HAVING MAX(click_weight) >= 0.8
			OR MAX(CASE WHEN click_kind = 'resolved-from' THEN 1 ELSE 0 END) = 1
	) s ON s.grounding_event_id = ge.id
	GROUP BY ge.project_id, ge.action, ge.query_source, substr(ge.created_at, 1, 10)
`

// queryVolumeBySource folds grounding_events + query_interactions into
// proj_query_volume_by_source — the per-day rollup of search volume
// broken down by project, action, and query_source. See
// docs/TELEMETRY_SUBSTRATE.md §6.1 for the column-by-column shape and
// docs/PROJECTIONS.md for the registry contract. Read-side projection
// per TT3: Fold ignores the RawEvent and re-snapshots from the source
// tables on every invocation. At homelab scale (~hundreds of search
// calls per day) the rebuild cost is trivial and the byte-identical
// rebuild invariant holds vacuously (fold == rebuild).
type queryVolumeBySource struct{}

func init() { Register(queryVolumeBySource{}) }

func (queryVolumeBySource) Name() string      { return "query_volume_by_source" }
func (queryVolumeBySource) TableName() string { return "proj_query_volume_by_source" }

// Fold ignores the RawEvent (read-side projections fold from telemetry
// emits, not the events ledger) and re-snapshots the table from CRUD.
// Idempotent by design — DELETE + INSERT…SELECT…GROUP BY converges on
// the same end state regardless of trigger frequency.
func (q queryVolumeBySource) Fold(ctx context.Context, tx *sql.Tx, _ RawEvent) error {
	return q.RebuildFromEmpty(ctx, tx)
}

func (q queryVolumeBySource) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	return rebuildProjection(ctx, tx, q.TableName(), queryVolumeBySourceSQL)
}
