package projections

import (
	"context"
	"database/sql"
)

// trainingDataForRerankerSQL produces one row per (grounding_event_id,
// source_ref) pair by walking grounding_events.source_refs JSON arrays
// via json_each. For each pair, an outer LEFT JOIN on query_interactions
// collapses to (max click_weight, label_sources JSON, fired-tier flag).
// The label classifier is a CASE that pastes the TT1.5 5-value enum
// verbatim; changes here must move in lockstep with the TT1.5 enum
// (the spike's §8 paste-target is the single source of truth).
//
// candidate_pointer_id LEFT JOINs a dedup'd view of knowledge_pointers
// keyed by source_ref (lowest id wins on ties). The dedup is defense-
// in-depth against parallel pointer rows that share a source_ref but
// differ on project_id (the projection's UNIQUE (grounding_event_id,
// source_ref) constraint fires if the raw JOIN multiplies rows; chain
// 617 T1 surfaced this when forge_edit + the legacy seeder both wrote
// pointers for the same `learnings/general/<file>.md` with different
// project_ids — `general` vs the `vault` sentinel). The root-cause fix
// for the parallel-rows shape lives in `resolveVaultNoteProjectID`;
// this MIN(id) dedup makes the projection rebuild correct even if
// future drift produces dups.
const trainingDataForRerankerSQL = `
	INSERT INTO proj_training_data_for_reranker
		(grounding_event_id, query_text, candidate_pointer_id, source_ref,
		 candidate_position, label_kind, weight, label_sources,
		 query_source, was_injected, prompt_id, span_id,
		 last_event_id, last_event_ts)
	SELECT
		pair.grounding_event_id,
		pair.query_text,
		kp.id AS candidate_pointer_id,
		pair.source_ref,
		pair.candidate_position,
		CASE
			WHEN pair.max_weight >= 0.8                                   THEN 'positive'
			WHEN pair.max_weight  > 0   AND pair.max_weight < 0.8         THEN 'weakly_positive'
			WHEN pair.max_weight  = 0   AND pair.candidate_position <= 3
			                            AND pair.results_count >= 5      THEN 'hard_negative'
			WHEN pair.max_weight  = 0   AND pair.candidate_position <= 10 THEN 'negative'
			ELSE 'unlabeled'
		END AS label_kind,
		pair.max_weight AS weight,
		COALESCE(pair.label_sources, '[]') AS label_sources,
		pair.query_source,
		pair.was_injected,
		pair.prompt_id,
		pair.span_id,
		-- Bug reranker-projection-last-event-ts-never-populated:
		-- last_event_ts was hardcoded '' on every row, masking the gap as
		-- populated-with-blank and blocking the most-recent-~15% time-
		-- based held-out split (chain 272 T1). Populate from the source
		-- grounding_event's real created_at (NOT a synthetic stamp); since
		-- this projection fully rebuilds on every event, all rows get the
		-- real timestamp. last_event_id carries the source grounding_event
		-- id for traceability.
		CAST(pair.grounding_event_id AS TEXT),
		pair.created_at
	FROM (
		SELECT
			ge.id AS grounding_event_id,
			ge.query_text,
			ge.query_source,
			ge.prompt_id,
			ge.span_id,
			ge.results_count,
			ge.created_at,
			-- Collapse a source_ref that appears more than once in one event's
			-- source_refs array (e.g. parse_context surfaced the same memory
			-- file as several candidates from a duplicated MEMORY.md index
			-- line) to a single row at the earliest position. Grouping by
			-- je.key instead splits the dups into rows that collide on
			-- UNIQUE(grounding_event_id, source_ref), aborting the full
			-- rebuild and emptying the projection. Sibling defense-in-depth
			-- to the kp MIN(id) dedup above.
			(MIN(CAST(je.key AS INTEGER)) + 1) AS candidate_position,
			je.value AS source_ref,
			COALESCE(MAX(qi.click_weight), 0.0) AS max_weight,
			COALESCE(MAX(qi.was_injected), 0) AS was_injected,
			(SELECT json_group_array(DISTINCT click_kind)
			   FROM query_interactions
			   WHERE grounding_event_id = ge.id
			     AND source_ref = je.value) AS label_sources
		FROM grounding_events ge,
		     json_each(ge.source_refs) je
		LEFT JOIN query_interactions qi
			ON qi.grounding_event_id = ge.id
			AND qi.source_ref = je.value
		GROUP BY ge.id, je.value
	) pair
	LEFT JOIN (
		SELECT MIN(id) AS id, source_ref
		FROM knowledge_pointers
		GROUP BY source_ref
	) kp ON kp.source_ref = pair.source_ref
	-- Chain substrate-health-audit-projections T3: this projection is the
	-- (query, candidate, label) corpus for a cross-encoder reranker, which
	-- scores (query, candidate) — a row with no query is untrainable. Drop
	-- rows whose source grounding_event carries no query_text (the 458
	-- legacy rows from un-backfilled processor events, across ALL label
	-- classes — the gap was never positives-only). Keeping them would also
	-- trip migration 071's query_text NOT NULL CHECK on every rebuild. Not
	-- a backfill: the rows are excluded, never synthesised. New post-fix
	-- traffic carries query_text and lands normally.
	WHERE pair.query_text IS NOT NULL AND pair.query_text <> ''
`

// trainingDataForReranker is the substrate-to-ML bridge: materializes
// (query, candidate, label) triples that a cross-encoder reranker
// fine-tune (roadmap §1.1), source router (§1.2), and chunk-quality
// scorer (§2.5) consume directly. One row per (grounding_event_id,
// source_ref) pair.
//
// label_kind enum (TT1.5 §5, FROZEN):
//
//	positive          max_click_weight >= 0.8 (followed / cited / resolved-from fired)
//	weakly_positive   max_click_weight ∈ (0, 0.8) (mentioned-only)
//	negative          shown, no tier fired, position <= 10
//	hard_negative     shown, no tier fired, position <= 3 AND results_count >= 5
//	unlabeled         in-flight, no resolution yet (placeholder)
//
// label_sources is a JSON array of click_kind strings preserving every
// firing kind for that pair; consumers can re-aggregate by tier without
// re-joining query_interactions.
type trainingDataForReranker struct{}

func init() { Register(trainingDataForReranker{}) }

func (trainingDataForReranker) Name() string      { return "training_data_for_reranker" }
func (trainingDataForReranker) TableName() string { return "proj_training_data_for_reranker" }

func (t trainingDataForReranker) Fold(ctx context.Context, tx *sql.Tx, _ RawEvent) error {
	return t.RebuildFromEmpty(ctx, tx)
}

func (t trainingDataForReranker) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	return rebuildProjection(ctx, tx, t.TableName(), trainingDataForRerankerSQL)
}
