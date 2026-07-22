-- bug `reranker-projection-last-event-ts-never-populated`: the
-- last_event_id / last_event_ts columns on proj_training_data_for_reranker
-- carried `NOT NULL DEFAULT ''`. The projection writer never set them, so
-- all 943 rows read as populated-with-blank rather than never-written —
-- masking the gap and blocking chain 272 T1's most-recent-~15% time-based
-- held-out split.
--
-- The writer is fixed in the same change (go/internal/projections/
-- query_training.go now populates last_event_ts from the source
-- grounding_event's real created_at and last_event_id from its id). This
-- migration completes the fix per the bug's constraint ("Replace the
-- empty-string default rather than tolerating it"): drop the masking
-- defaults so any future regression surfaces as NULL (visibly
-- never-written) instead of '' (looks-populated).
--
-- Safe to DROP + CREATE without copying rows: proj_training_data_for_reranker
-- is a full-rebuild projection (rebuildProjection TRUNCATEs + re-INSERTs on
-- every event via FoldAll), so the next event repopulates it from
-- grounding_events with the corrected SQL. Its sole consumer (chain 272,
-- the cross-encoder reranker training pipeline) has not started yet, so
-- there is no read in the repopulation window. The derived rows carry no
-- source-of-truth state (grounding_events + query_interactions are the
-- source). Indexes are recreated to match migration 038.

DROP TABLE proj_training_data_for_reranker;

CREATE TABLE proj_training_data_for_reranker (
    training_id          INTEGER PRIMARY KEY AUTOINCREMENT,
    grounding_event_id   INTEGER NOT NULL REFERENCES grounding_events(id),
    query_text           TEXT,
    candidate_pointer_id INTEGER,
    source_ref           TEXT NOT NULL,
    candidate_position   INTEGER NOT NULL,
    label_kind           TEXT NOT NULL CHECK (label_kind IN
                            ('positive', 'weakly_positive', 'negative', 'hard_negative', 'unlabeled')),
    weight               REAL NOT NULL DEFAULT 0.0,
    label_sources        TEXT NOT NULL DEFAULT '[]',
    query_source         TEXT NOT NULL DEFAULT 'agent_initiated',
    was_injected         INTEGER NOT NULL DEFAULT 0,
    prompt_id            TEXT,
    span_id              TEXT,
    -- Masking defaults dropped: nullable, no default. The writer always
    -- populates these from the source grounding_event; a NULL here now
    -- means "never written" (a real, visible gap), not "written blank".
    last_event_id        TEXT,
    last_event_ts        TEXT,
    UNIQUE (grounding_event_id, source_ref)
);

CREATE INDEX proj_tdfr_label_kind_idx    ON proj_training_data_for_reranker (label_kind);
CREATE INDEX proj_tdfr_query_source_idx  ON proj_training_data_for_reranker (query_source);
CREATE INDEX proj_tdfr_pointer_idx       ON proj_training_data_for_reranker (candidate_pointer_id);
CREATE INDEX proj_tdfr_prompt_idx        ON proj_training_data_for_reranker (prompt_id);
