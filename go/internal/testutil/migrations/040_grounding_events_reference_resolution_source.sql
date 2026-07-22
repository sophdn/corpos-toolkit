-- reference-resolution-substrate chain T5: widen grounding_events.query_source
-- CHECK to admit 'reference_resolution' as a first-class enum value.
--
-- Path A from the T1 design (docs/REFERENCE_RESOLUTION.md §6.1):
-- ship a CHECK-widening migration so downstream training_data_for_reranker
-- pivots and dashboards key by name (no parallel query_subsource column).
-- Path B's bare 'other' + secondary discriminator is reserved for
-- genuinely speculative sources; reference_resolution is stable, named,
-- and well-bounded.
--
-- SQLite has no ALTER TABLE DROP CONSTRAINT or MODIFY COLUMN; the
-- standard idiom is a table rebuild. We recreate grounding_events with
-- the wider CHECK, copy every row over, drop the old table, rename in
-- place, and reinstall the indexes from migrations 019, 034, and 037.
--
-- Foreign keys from query_interactions and the telemetry projections
-- (migration 038) reference grounding_events(id) by name; after the
-- RENAME the FK target resolves to the new table. PRAGMA foreign_keys
-- is left at whatever the connection has set — SQLite only checks FKs
-- at INSERT/UPDATE/DELETE time, and the INSERT below preserves id
-- values so existing references stay valid.

CREATE TABLE grounding_events_new (
    id                   INTEGER PRIMARY KEY,
    project_id           TEXT NOT NULL,
    session_id           TEXT NOT NULL,
    call_id              TEXT NOT NULL,
    action               TEXT NOT NULL,
    results_count        INTEGER NOT NULL DEFAULT 0,
    source_refs          TEXT NOT NULL DEFAULT '[]',
    next_turn_has_output INTEGER NOT NULL DEFAULT 0,
    used                 INTEGER,
    created_at           TEXT NOT NULL DEFAULT (datetime('now')),
    -- From migration 034:
    span_id              TEXT,
    -- From migration 037:
    prompt_id            TEXT,
    parent_span_id       TEXT,
    query_source         TEXT NOT NULL DEFAULT 'agent_initiated'
        CHECK (query_source IN (
            'agent_initiated',
            'proactive_hook',
            'dashboard_user',
            'reference_resolution',
            'other'
        )),
    user_message_id      TEXT,
    query_text           TEXT
);

INSERT INTO grounding_events_new
    (id, project_id, session_id, call_id, action,
     results_count, source_refs, next_turn_has_output, used, created_at,
     span_id, prompt_id, parent_span_id, query_source, user_message_id, query_text)
SELECT
    id, project_id, session_id, call_id, action,
    results_count, source_refs, next_turn_has_output, used, created_at,
    span_id, prompt_id, parent_span_id, query_source, user_message_id, query_text
FROM grounding_events;

DROP TABLE grounding_events;

ALTER TABLE grounding_events_new RENAME TO grounding_events;

-- Reinstall indexes from migrations 019, 034, 037 — the rebuild
-- removed them with the table.
CREATE INDEX idx_grounding_project ON grounding_events (project_id);
CREATE INDEX idx_grounding_gaps ON grounding_events (results_count, next_turn_has_output);
CREATE UNIQUE INDEX idx_grounding_call ON grounding_events (session_id, call_id);
CREATE INDEX idx_grounding_span ON grounding_events (span_id);
CREATE INDEX idx_grounding_prompt ON grounding_events (prompt_id);
CREATE INDEX idx_grounding_source ON grounding_events (query_source);
CREATE INDEX idx_grounding_parent ON grounding_events (parent_span_id);
