-- reference-resolution-substrate-frontend chain RF2: side-table indexing
-- the per-reference detail that doesn't fit cleanly on grounding_events.
--
-- One row per resolve_references emit; FK to grounding_events. Lets the
-- Context Pull Inspector page filter on shape / confidence_tier /
-- resolver_name without re-parsing source_refs prefixes, and gives a
-- forward-compat home for the T7 ML confidence score.
--
-- See docs/REFERENCE_RESOLUTION_FRONTEND.md §3.6 for the
-- "side-table-not-extra-columns" rationale: grounding_events is shared
-- across all query_source values; adding reference-resolution-specific
-- columns there would either leave 99% of rows with NULLs or require
-- defaults that don't make sense for non-reference-resolution sources.
-- Same posture as query_interactions and query_resolutions (migration
-- 037 — both are themselves side-tables of grounding_events).
--
-- ml_confidence_score is NULLABLE (not DEFAULT 0.0) so the inspector can
-- distinguish "not yet classified" from "classified low" once T7 lands.

CREATE TABLE reference_resolution_emits (
    grounding_event_id            INTEGER PRIMARY KEY REFERENCES grounding_events(id),
    shape                         TEXT NOT NULL,
    confidence_score              REAL NOT NULL,
    detection_method              TEXT NOT NULL,
    start_pos                     INTEGER NOT NULL,
    end_pos                       INTEGER NOT NULL,
    confidence_tier               TEXT NOT NULL,
    presentation_recommendation   TEXT NOT NULL,
    presented_as                  TEXT NOT NULL,
    resolver_name                 TEXT NOT NULL,
    retrieval_cost_ms             INTEGER NOT NULL,
    ml_confidence_score           REAL,
    created_at                    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_rre_shape    ON reference_resolution_emits (shape);
CREATE INDEX idx_rre_tier     ON reference_resolution_emits (confidence_tier);
CREATE INDEX idx_rre_resolver ON reference_resolution_emits (resolver_name);
