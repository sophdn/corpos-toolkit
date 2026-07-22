-- Chain substrate-health-audit-projections T4 follow-on — lock the
-- last_event_id / last_event_ts population for the two grounding-events-
-- derived read-side projections (bug query-telemetry-projections-hardcode-
-- empty-last-event-id-ts), mirroring migration 068 (drop masking default)
-- + 071 (NOT NULL CHECK invariant) for the reranker projection.
--
-- Both writers hardcoded ('', '') for the watermark columns — exactly the
-- shape query_training.go had before bug 881 (5c8fd43b). The NOT NULL
-- DEFAULT '' masked the gap as populated-with-blank: proj_query_volume_by_
-- source (52/52 rows) and proj_retrieval_success_per_query (555/555 rows)
-- carried empty watermarks on 100% of rows. The writer fix carries the real
-- source values through (query_volume.go: MAX(ge.id)/MAX(ge.created_at) over
-- the aggregated bucket; query_success.go: ge.id/ge.created_at per row).
-- This migration drops the masking default and enforces non-empty at the DB
-- so a future writer regression surfaces as a REJECTED insert, not a silent
-- blank — which would break any time-windowed / incremental-watermark
-- consumer of these projections.
--
-- Both are full-rebuild read-side projections (DELETE + INSERT...SELECT on
-- every telemetry emit via FoldAllReadSide; rebuildProjection), fully
-- derived from grounding_events (+ query_interactions). Safe to DROP +
-- CREATE empty: the next emit / `rebuild-projections` repopulates through
-- the now-fixed writer, so every rebuilt row satisfies the new invariant.
-- Verified 2026-05-24: all 560 live grounding_events carry non-empty
-- created_at, so the CHECK rejects zero legitimate rebuilt rows. Indexes
-- recreated to match migration 038.
--
-- NOT a backfill (chain no-backfill invariant): no synthetic values written;
-- the tables are empty after this migration until the next real event folds
-- them through the corrected writer.

DROP TABLE proj_query_volume_by_source;

CREATE TABLE proj_query_volume_by_source (
    project_id        TEXT NOT NULL,
    action            TEXT NOT NULL,                       -- vault_search / kiwix_search / knowledge_search
    query_source      TEXT NOT NULL,                       -- matches grounding_events.query_source CHECK set
    day               TEXT NOT NULL,                       -- UTC YYYY-MM-DD bucket
    query_count       INTEGER NOT NULL DEFAULT 0,
    zero_result_count INTEGER NOT NULL DEFAULT 0,
    success_count     INTEGER NOT NULL DEFAULT 0,
    avg_results_count REAL NOT NULL DEFAULT 0.0,
    -- Bucket's most-recent grounding_event: MAX(id) / MAX(created_at).
    last_event_id     TEXT NOT NULL CHECK (last_event_id <> ''),
    last_event_ts     TEXT NOT NULL CHECK (last_event_ts <> ''),
    PRIMARY KEY (project_id, action, query_source, day)
);

CREATE INDEX proj_qvbs_day_idx     ON proj_query_volume_by_source (day DESC);
CREATE INDEX proj_qvbs_project_idx ON proj_query_volume_by_source (project_id, day DESC);

DROP TABLE proj_retrieval_success_per_query;

CREATE TABLE proj_retrieval_success_per_query (
    grounding_event_id INTEGER PRIMARY KEY REFERENCES grounding_events(id),
    project_id         TEXT NOT NULL,
    action             TEXT NOT NULL,
    query_text         TEXT,                                -- nullable; pre-TT2 rows don't have it
    prompt_id          TEXT,
    results_count      INTEGER NOT NULL DEFAULT 0,
    had_followed       INTEGER NOT NULL DEFAULT 0,
    had_cited          INTEGER NOT NULL DEFAULT 0,
    had_mentioned      INTEGER NOT NULL DEFAULT 0,
    had_resolved_from  INTEGER NOT NULL DEFAULT 0,
    max_click_weight   REAL NOT NULL DEFAULT 0.0,
    kinds_fired        TEXT NOT NULL DEFAULT '[]',          -- JSON array of distinct click_kind values
    success            INTEGER NOT NULL DEFAULT 0,
    was_proactive      INTEGER NOT NULL DEFAULT 0,
    -- Source grounding_event's id / created_at (one row per event).
    last_event_id      TEXT NOT NULL CHECK (last_event_id <> ''),
    last_event_ts      TEXT NOT NULL CHECK (last_event_ts <> '')
);

CREATE INDEX proj_rspq_project_action_idx ON proj_retrieval_success_per_query (project_id, action);
CREATE INDEX proj_rspq_success_idx        ON proj_retrieval_success_per_query (success);
CREATE INDEX proj_rspq_prompt_idx         ON proj_retrieval_success_per_query (prompt_id);
