-- query-telemetry-substrate chain TT2 — interactions and resolutions tables.
--
-- Lands the read-side telemetry substrate designed in TT1
-- (docs/TELEMETRY_SUBSTRATE.md) with the click_kind / label_kind enums
-- closed by TT1.5 (docs/TELEMETRY_LABEL_SPIKE.md). Together with the
-- existing 019_grounding_events.sql + 034_grounding_events_span_id.sql,
-- this migration completes the read-side schema: every search call now
-- has its full lifecycle captured — query → results → tiered click
-- signals → terminal resolution — and reads out as training pairs for
-- the cross-encoder reranker (local-ml-roadmap.md §1.1), source router
-- (§1.2), and chunk-quality scorer (§2.5).
--
-- Cross-substrate seam: query_resolutions.write_event_ids holds a JSON
-- array of UUIDv7 strings referencing events.event_id (migration 032).
-- A BEFORE INSERT trigger validates every event_id in the array exists
-- in events at write time — SQLite doesn't enforce FKs on JSON arrays,
-- so this is the structural integrity check. See
-- docs/TELEMETRY_SUBSTRATE.md §12 for the FK contract.
--
-- Span identity (TT1 §2): three-layer hierarchy (session_id ⊇ prompt_id
-- ⊇ span_id). span_id is per-MCP-request (matches events.span_id and
-- grounding_events.span_id from migration 034). prompt_id is per-user-
-- input arc (sourced from transcript JSONL's promptId field; Stop-hook
-- stamped post-session). session_id is per-CLI-launch (already on
-- grounding_events.session_id since migration 019).
--
-- click_kind enum (TT1.5 CONFIRM): followed / cited / mentioned /
-- resolved-from. CHECK constraint enforces.
--
-- =====================================================================
-- PART 1: forward-compat columns on grounding_events
-- =====================================================================
--
-- span_id and idx_grounding_span are already present (migration 034);
-- this section does NOT re-add them.
--
-- query_source carries the discriminator for proactive-injection
-- prerequisites (TT1 §8). 'other' fallback lets future query sources
-- flow without a schema migration during exploration; closing them
-- into the enum is a follow-on once the source surfaces.
--
-- prompt_id and parent_span_id are NULLABLE because they're stamped
-- post-session by the Stop hook (TT2 wires that in a follow-on commit
-- under user confirmation, since the hook lives outside this repo at
-- ~/.claude/hooks/). Live emits during a tools/call leave them NULL;
-- queries that need them tolerate the lag.

ALTER TABLE grounding_events ADD COLUMN prompt_id      TEXT;
ALTER TABLE grounding_events ADD COLUMN parent_span_id TEXT;
ALTER TABLE grounding_events ADD COLUMN query_source   TEXT NOT NULL DEFAULT 'agent_initiated'
    CHECK (query_source IN ('agent_initiated', 'proactive_hook', 'dashboard_user', 'other'));
ALTER TABLE grounding_events ADD COLUMN user_message_id TEXT;
ALTER TABLE grounding_events ADD COLUMN query_text      TEXT;

CREATE INDEX idx_grounding_prompt ON grounding_events (prompt_id);
CREATE INDEX idx_grounding_source ON grounding_events (query_source);
CREATE INDEX idx_grounding_parent ON grounding_events (parent_span_id);

-- =====================================================================
-- PART 2: query_interactions
-- =====================================================================
--
-- One row per detected click signal. Multiple signal kinds may fire
-- per (span_id, source_ref) — each is its own row, identified by the
-- (span_id, source_ref, click_kind) triple. See TT1 §3 and TT1.5 §4.
--
-- Mutation posture (TT1 §3.4):
--   INSERT: from the Stop hook (TT2 follow-on) or the in-process emit
--     helper (this commit).
--   UPDATE: allowed for click_kind refinement (e.g. a row first
--     detected as `mentioned` is upgraded to `cited` once the full turn
--     is in the transcript).
--   DELETE: blocked structurally; misclassifications get a compensating
--     row.

CREATE TABLE query_interactions (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    grounding_event_id          INTEGER NOT NULL REFERENCES grounding_events(id),
    source_ref                  TEXT NOT NULL,
    position                    INTEGER,                                  -- 1-indexed rank in original result list
    click_kind                  TEXT NOT NULL CHECK (click_kind IN
                                    ('followed', 'cited', 'mentioned', 'resolved-from')),
    click_weight                REAL NOT NULL,
    citation_kind               TEXT,                                     -- sub-classifier when click_kind='cited':
                                                                          --   'markdown-link' | 'file-line' | 'quoted-block'
                                                                          -- NULL for non-cited kinds.
    citation_quote_chars        INTEGER,                                  -- for cited: length of the quoted span
    dwell_ms_estimate           INTEGER,                                  -- for followed: ms between grounding event and follow-up
    was_injected                INTEGER NOT NULL DEFAULT 0,               -- proactive-injection forward-compat (TT1 §8)
    injection_position          INTEGER,                                  -- nullable; only set when was_injected
    injection_was_user_visible  INTEGER,                                  -- nullable; only set when was_injected
    span_id                     TEXT NOT NULL,                            -- per-tools/call (matches grounding_events.span_id)
    prompt_id                   TEXT,                                     -- per-user-input arc (Stop-hook stamped)
    session_id                  TEXT NOT NULL,                            -- denormalised for cheap session-rollup
    parent_span_id              TEXT,                                     -- for sidechain subagent parent linkage
    detected_at                 TEXT NOT NULL,                            -- when the hook (or emit helper) detected this
    created_at                  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Triple is the natural key (TT1 §3.2). Multiple click_kinds for the
-- same (span_id, source_ref) are different rows; same kind twice is
-- collapsed via INSERT-OR-REPLACE.
CREATE UNIQUE INDEX idx_qi_triple   ON query_interactions (span_id, source_ref, click_kind);
CREATE INDEX        idx_qi_ground   ON query_interactions (grounding_event_id);
CREATE INDEX        idx_qi_prompt   ON query_interactions (prompt_id);
CREATE INDEX        idx_qi_session  ON query_interactions (session_id);
CREATE INDEX        idx_qi_source   ON query_interactions (source_ref);

-- DELETE blocked; corrections via compensating rows.
CREATE TRIGGER query_interactions_no_delete BEFORE DELETE ON query_interactions
BEGIN
    SELECT RAISE(ABORT, 'query_interactions deletion is not supported; use a compensating row');
END;

-- =====================================================================
-- PART 3: query_resolutions
-- =====================================================================
--
-- One row per terminal resolution event (BugResolved / TaskCompleted /
-- TaskCancelled / ChainClosed) with JSON-array FKs to:
--   - write-side events (write_event_ids → events.event_id)
--   - grounding_events that fed the resolution
--   - query_interactions that fired in the trajectory
--
-- See TT1 §4 and §12 for the FK contract.
--
-- Mutation posture: append-only. Reopen+resolve cycles get a new row.

CREATE TABLE query_resolutions (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    resolution_id           TEXT NOT NULL UNIQUE,                         -- UUIDv7 minted by emit helper
    prompt_id               TEXT NOT NULL,                                -- trajectory join key
    session_id              TEXT NOT NULL,
    span_id                 TEXT NOT NULL,                                -- per-tools/call of the resolving event (matches events.span_id)
    entity_kind             TEXT NOT NULL CHECK (entity_kind IN ('bug', 'task', 'chain')),
    entity_slug             TEXT NOT NULL,
    entity_project_id       TEXT NOT NULL,
    outcome_kind            TEXT NOT NULL CHECK (outcome_kind IN
                                ('resolved', 'completed', 'cancelled', 'closed', 'superseded', 'discarded')),
    write_event_ids         TEXT NOT NULL DEFAULT '[]',                   -- JSON array of UUIDv7 → events.event_id
    grounding_event_ids     TEXT NOT NULL DEFAULT '[]',                   -- JSON array of INTEGER → grounding_events.id
    query_interaction_ids   TEXT NOT NULL DEFAULT '[]',                   -- JSON array of INTEGER → query_interactions.id
    detected_at             TEXT NOT NULL,
    created_at              TEXT NOT NULL DEFAULT (datetime('now'))
);

-- One resolution per (entity, prompt). Reopen+resolve cycles produce a
-- new prompt_id, hence a new row.
CREATE UNIQUE INDEX idx_qr_entity_prompt
    ON query_resolutions (entity_kind, entity_slug, entity_project_id, prompt_id);
CREATE INDEX idx_qr_prompt   ON query_resolutions (prompt_id);
CREATE INDEX idx_qr_entity   ON query_resolutions (entity_kind, entity_slug, entity_project_id);
CREATE INDEX idx_qr_outcome  ON query_resolutions (outcome_kind);

-- Append-only triggers. Reopen+resolve cycles get a new row.
CREATE TRIGGER query_resolutions_no_update BEFORE UPDATE ON query_resolutions
BEGIN
    SELECT RAISE(ABORT, 'query_resolutions is append-only; reopen+resolve cycles get new rows');
END;
CREATE TRIGGER query_resolutions_no_delete BEFORE DELETE ON query_resolutions
BEGIN
    SELECT RAISE(ABORT, 'query_resolutions deletion is not supported');
END;

-- Cross-substrate FK integrity check. SQLite doesn't enforce FKs on
-- JSON arrays, so this trigger validates every event_id in
-- write_event_ids exists in events. Reject the INSERT if any element
-- is missing. write_event_ids='[]' (no events claimed) is permitted —
-- json_array_length('[]')=0 equals the empty join, so the check passes
-- vacuously.
CREATE TRIGGER query_resolutions_event_id_fk
BEFORE INSERT ON query_resolutions
WHEN json_array_length(NEW.write_event_ids) != (
    SELECT COUNT(*) FROM json_each(NEW.write_event_ids) AS je
    WHERE EXISTS (SELECT 1 FROM events WHERE event_id = je.value)
)
BEGIN
    SELECT RAISE(ABORT,
        'query_resolutions.write_event_ids contains an event_id not present in events; integrity check failed');
END;
