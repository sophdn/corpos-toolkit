-- agent-first-substrate chain T2 — append-only events table.
--
-- This is the source-of-truth ledger for every state mutation in the work
-- meta-tool. Existing CRUD tables (bugs, tasks, chains, benchmark_results)
-- remain in place; every mutation dual-writes — row UPDATE/INSERT first,
-- event INSERT second (post-T4 order; T2 originally chose the reverse) —
-- inside a single SQLite transaction. The fold hook inside events.Emit
-- needs post-update CRUD state to populate projection rows; the schema-
-- reject failure mode is preserved by tx rollback. The events table is
-- the durable record of "what happened and why"; the CRUD tables become
-- a denormalized projection cache. See docs/EVENT_SUBSTRATE.md §1 history
-- note and docs/PROJECTIONS.md §2.3 for the full reasoning.
--
-- Schema-version-1 envelope (mirrors docs/EVENT_SUBSTRATE.md §2):
--   event_id           UUIDv7, primary key. STABLE FK TARGET FOREVER.
--   ts                 RFC 3339 with TZ, ms precision.
--   actor_{kind,id}    Inferred at dispatch from transport.
--   type               PascalCase past-tense; closed catalog (see
--                      blueprints/events/<type>.json).
--   entity_{kind,slug,project_id}  Primary entity reference.
--   payload            Type-specific JSON, validated at write time.
--   rationale          Required for actor_kind='agent' on mutating actions;
--                      enforced at the dispatch boundary (T3), not here.
--   caused_by_event_id Causal-graph parent for cascade/compensating events.
--   related_entities   JSON array of cross-entity references.
--   span_id            Per-MCP-request UUIDv4; join key to structured logs (T5).
--   schema_version     Envelope version; currently 1.
--
-- CRITICAL CROSS-CHAIN INVARIANT: event_id is the FK target for the
-- sibling chain query-telemetry-substrate's query_resolutions.write_event_ids
-- JSON-array column (TT2). Future schema_version bumps MUST NOT rename or
-- retype this column. Adding a column requires a new migration; removing
-- event_id requires a chain-level decision involving both substrates.
--
-- Append-only enforcement is at the trigger layer, not by convention.
-- UPDATE and DELETE on the events table fail with RAISE(ABORT, ...).
-- The only "correction" path is a compensating event (e.g.
-- BugTriageReversed pointing at the BugTriaged it undoes via
-- caused_by_event_id). This is structural, not advisory; admin tools and
-- ad-hoc SQL alike hit the trigger.

CREATE TABLE events (
    event_id           TEXT PRIMARY KEY,
    ts                 TEXT NOT NULL,
    actor_kind         TEXT NOT NULL CHECK (actor_kind IN ('agent', 'human', 'system')),
    actor_id           TEXT NOT NULL,
    type               TEXT NOT NULL,
    entity_kind        TEXT NOT NULL,
    entity_slug        TEXT NOT NULL,
    entity_project_id  TEXT,
    payload            TEXT NOT NULL,
    rationale          TEXT,
    caused_by_event_id TEXT REFERENCES events(event_id),
    related_entities   TEXT NOT NULL DEFAULT '[]',
    span_id            TEXT NOT NULL,
    schema_version     INTEGER NOT NULL DEFAULT 1
);

-- Index choices match the expected query shapes:
--   - entity timeline:        per-entity scrollback (bug history, task history).
--   - type+ts:                "all BugResolved events since X" — projection folds.
--   - span_id:                join across events sharing a request (the parent
--                             cascade + every child emit).
--   - (project_id, ts):       project-scoped feeds for per-project dashboards.
CREATE INDEX events_entity_idx     ON events (entity_kind, entity_slug, ts);
CREATE INDEX events_type_ts_idx    ON events (type, ts);
CREATE INDEX events_span_idx       ON events (span_id);
CREATE INDEX events_project_ts_idx ON events (entity_project_id, ts);

-- Append-only triggers. RAISE(ABORT, ...) aborts the statement AND the
-- enclosing transaction, so a handler attempting an UPDATE inside its
-- WithWrite closure loses the entire txn — both the bogus UPDATE and any
-- legitimate sibling INSERTs are rolled back. That is the intended
-- failure shape: the only way to record a correction is a compensating
-- event with caused_by_event_id pointing at the event being reversed.
CREATE TRIGGER events_no_update
BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events table is append-only; use a compensating event with caused_by_event_id');
END;

CREATE TRIGGER events_no_delete
BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events table is append-only; deletion is not a supported operation');
END;
