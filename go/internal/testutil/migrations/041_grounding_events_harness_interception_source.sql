-- reference-resolution-substrate chain T9: widen grounding_events.query_source
-- CHECK to admit 'harness_reminder_interception' as a first-class enum value.
--
-- Path A from the T1 design (docs/REFERENCE_RESOLUTION.md §6.1 + §8.4):
-- the UserPromptSubmit interception hook emits one grounding_events row per
-- fire (strip / preserve / fail_open) so the analytics in §8.5 can chart
-- "how often is the upstream reminder firing vs being stripped" and trip
-- on text drift via the fail-open row count. Both T5 and T9 widen the same
-- CHECK; chain task order is the sequencing handle (T5 = migration 040,
-- T9 = migration 041).
--
-- Same table-rebuild idiom as migration 040 — SQLite has no ALTER TABLE
-- DROP CONSTRAINT. Indexes from migrations 019, 034, 037 reinstalled.

CREATE TABLE grounding_events_new (
    id                   INTEGER PRIMARY KEY,
    project_id           TEXT NOT NULL,
    session_id           TEXT NOT NULL,
    call_id              TEXT NOT NULL,
    action               TEXT NOT NULL,
    results_count        INTEGER NOT NULL DEFAULT 0,
    -- source_refs ENCODING (JOIN TRAP — see suggestion #18): JSON array of
    -- per-resolver Candidate.SourceRef strings. refresolve resolvers use
    -- `<type>:<rest>` (chain:/bug:/skill:/memory:/vault:/kiwix:<book>::<entry>);
    -- the knowledge-search handlers write BARE corpus ids (vault paths like
    -- `decisions/x.md`, kiwix `<book>::<entry>`). This does NOT share an
    -- encoding with knowledge_pointers.source_ref (`<scope>::<slug>`) — a
    -- direct JOIN is a ~100% miss; normalize first. See
    -- vault/reference/2026-05-23_source-ref-encoding-divergence-grounding-events-vs-knowledge-pointers.md
    -- + bug `context-pulls-first-candidate-source-type-empty-due-to-source-ref-format-mismatch`.
    source_refs          TEXT NOT NULL DEFAULT '[]',
    next_turn_has_output INTEGER NOT NULL DEFAULT 0,
    used                 INTEGER,
    created_at           TEXT NOT NULL DEFAULT (datetime('now')),
    span_id              TEXT,
    prompt_id            TEXT,
    parent_span_id       TEXT,
    query_source         TEXT NOT NULL DEFAULT 'agent_initiated'
        CHECK (query_source IN (
            'agent_initiated',
            'proactive_hook',
            'dashboard_user',
            'reference_resolution',
            'harness_reminder_interception',
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

CREATE INDEX idx_grounding_project ON grounding_events (project_id);
CREATE INDEX idx_grounding_gaps ON grounding_events (results_count, next_turn_has_output);
CREATE UNIQUE INDEX idx_grounding_call ON grounding_events (session_id, call_id);
CREATE INDEX idx_grounding_span ON grounding_events (span_id);
CREATE INDEX idx_grounding_prompt ON grounding_events (prompt_id);
CREATE INDEX idx_grounding_source ON grounding_events (query_source);
CREATE INDEX idx_grounding_parent ON grounding_events (parent_span_id);
