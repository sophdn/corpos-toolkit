-- query-telemetry-substrate chain TT3 — read-side projections.
--
-- Three proj_* tables in the `query_*` namespace (TT1 §7.3) that turn
-- the raw telemetry substrate (grounding_events + query_interactions +
-- query_resolutions) into analytics-shaped and training-data-shaped
-- views. Each is folded by a Projection implementation under
-- go/internal/projections/ that re-snapshots the table from CRUD on
-- every fold; the trigger is telemetry.EmitInteraction /
-- telemetry.EmitResolution via the SetFoldHook → projections.FoldAllReadSide
-- bridge. Same-tx invariant per TT3 AC: fold failure aborts the emit.
--
-- label_kind enum on proj_training_data_for_reranker uses the TT1.5-REVISED
-- 5-value set (docs/TELEMETRY_LABEL_SPIKE.md §5). The four-value original
-- proposal is OBSOLETE and was never committed to a schema.
--
-- Reserved namespaces (NOT implemented here; documented in
-- docs/TELEMETRY_SUBSTRATE.md §7.3):
--   - injection_quality_by_source  (proactive-injection follow-on chain)
--   - offload_* projections        (Qwen/ML offload future chains)

-- ====================================================================
-- PART 1: proj_query_volume_by_source
-- ====================================================================
--
-- Per-day rollup of search call volume sliced by (project, action,
-- query_source). The dashboard's per-project search-volume panel reads
-- from this. Distinguishes agent-initiated vs proactive-hook traffic
-- once the proactive-injection chain ships writes.

CREATE TABLE proj_query_volume_by_source (
    project_id        TEXT NOT NULL,
    action            TEXT NOT NULL,                       -- vault_search / kiwix_search / knowledge_search
    query_source      TEXT NOT NULL,                       -- matches grounding_events.query_source CHECK set
    day               TEXT NOT NULL,                       -- UTC YYYY-MM-DD bucket
    query_count       INTEGER NOT NULL DEFAULT 0,
    zero_result_count INTEGER NOT NULL DEFAULT 0,
    success_count     INTEGER NOT NULL DEFAULT 0,          -- bucket subset with a query_interactions row whose
                                                            --   click_kind IN ('followed','resolved-from') fired
    avg_results_count REAL NOT NULL DEFAULT 0.0,
    last_event_id     TEXT NOT NULL DEFAULT '',            -- watermark per package convention; empty for read-side
    last_event_ts     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, action, query_source, day)
);

CREATE INDEX proj_qvbs_day_idx     ON proj_query_volume_by_source (day DESC);
CREATE INDEX proj_qvbs_project_idx ON proj_query_volume_by_source (project_id, day DESC);

-- ====================================================================
-- PART 2: proj_retrieval_success_per_query
-- ====================================================================
--
-- One row per grounding_events.id with the click-tier decomposition
-- collapsed into per-flag booleans + a max_click_weight rollup. The
-- 'success' boolean is the convenience field consumers reach for; the
-- per-flag columns let dashboards filter on specific signal kinds.

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
    success            INTEGER NOT NULL DEFAULT 0,          -- max_click_weight >= 0.8 OR had_resolved_from = 1
    was_proactive      INTEGER NOT NULL DEFAULT 0,
    last_event_id      TEXT NOT NULL DEFAULT '',
    last_event_ts      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX proj_rspq_project_action_idx ON proj_retrieval_success_per_query (project_id, action);
CREATE INDEX proj_rspq_success_idx        ON proj_retrieval_success_per_query (success);
CREATE INDEX proj_rspq_prompt_idx         ON proj_retrieval_success_per_query (prompt_id);

-- ====================================================================
-- PART 3: proj_training_data_for_reranker
-- ====================================================================
--
-- One row per (grounding_event_id, source_ref) pair — i.e. one row per
-- (query, candidate) pair. The substrate-to-ML bridge: cross-encoder
-- reranker fine-tuning (roadmap §1.1) and chunk-quality scoring (§2.5)
-- consume this table directly without joins.
--
-- label_kind 5-value enum (TT1.5 §5):
--   positive          max_click_weight >= 0.8
--   weakly_positive   max_click_weight > 0 AND < 0.8 (mentioned-only)
--   negative          shown, no tier fired, position <= 10
--   hard_negative     shown, no tier fired, position <= 3 AND results_count >= 5
--   unlabeled         in-flight, no resolution yet

CREATE TABLE proj_training_data_for_reranker (
    training_id          INTEGER PRIMARY KEY AUTOINCREMENT,
    grounding_event_id   INTEGER NOT NULL REFERENCES grounding_events(id),
    query_text           TEXT,
    candidate_pointer_id INTEGER,                            -- FK target (knowledge_pointers); nullable when ref didn't resolve
    source_ref           TEXT NOT NULL,
    candidate_position   INTEGER NOT NULL,
    label_kind           TEXT NOT NULL CHECK (label_kind IN
                            ('positive', 'weakly_positive', 'negative', 'hard_negative', 'unlabeled')),
    weight               REAL NOT NULL DEFAULT 0.0,
    label_sources        TEXT NOT NULL DEFAULT '[]',         -- JSON array of contributing click_kind strings
    query_source         TEXT NOT NULL DEFAULT 'agent_initiated',
    was_injected         INTEGER NOT NULL DEFAULT 0,
    prompt_id            TEXT,
    span_id              TEXT,
    last_event_id        TEXT NOT NULL DEFAULT '',
    last_event_ts        TEXT NOT NULL DEFAULT '',
    UNIQUE (grounding_event_id, source_ref)
);

CREATE INDEX proj_tdfr_label_kind_idx    ON proj_training_data_for_reranker (label_kind);
CREATE INDEX proj_tdfr_query_source_idx  ON proj_training_data_for_reranker (query_source);
CREATE INDEX proj_tdfr_pointer_idx       ON proj_training_data_for_reranker (candidate_pointer_id);
CREATE INDEX proj_tdfr_prompt_idx        ON proj_training_data_for_reranker (prompt_id);

-- ====================================================================
-- Seed watermark rows
-- ====================================================================
--
-- One row per projection so ReadWatermark returns rows on first read
-- without depending on the fold sequence. last_event_id stays NULL
-- (read-side projections don't anchor against the events ledger;
-- the fold is a from-scratch re-snapshot at homelab scale).

INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
VALUES ('query_volume_by_source', NULL, NULL);

INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
VALUES ('retrieval_success_per_query', NULL, NULL);

INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
VALUES ('training_data_for_reranker', NULL, NULL);
