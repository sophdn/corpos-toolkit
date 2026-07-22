-- Chain substrate-health-audit-projections T7 — add the proj_memories
-- projection. Memories were the ONLY entity kind with no projection table.
-- The proj_* sweep mandated by this chain's completion condition (c)
-- surfaced the asymmetry: the arc-close dedup loader (F2,
-- go/internal/arcreview/dedupe.go) JSON-extracts MemoryWritten events from
-- the events ledger directly, while every other kind reads a projection
-- (proj_current_bugs / proj_current_suggestions / knowledge_pointers).
-- Adopts suggestion #20 (proj-memories-projection-table-paralleling-
-- proj-current-bugs).
--
-- ## Keyed by `name` (last-write-wins)
--
-- The auto-memory dir is a single GLOBAL namespace keyed by filename
-- (~/.claude/vault/memory/<kind>/<name>.md). The same name can be re-filed
-- from a different project context — 16 such names in live data (e.g.
-- linguistic-tics under both seed-packet and mcp-servers). That is ONE
-- memory, not two, so the projection's PK is `name` and the fold is
-- last-write-wins by event ts. project_id records the most-recent write's
-- project; filed_at preserves the first write's ts (the fold's ON CONFLICT
-- deliberately does NOT update filed_at).
--
-- ## Population invariants (substrate-health lock, completion condition b)
--
-- Every column a downstream consumer relies on is non-empty by
-- construction. Verified 2026-05-24: all 93 live MemoryWritten events
-- satisfy these (bad_kind=0, empty_desc=0, empty_path=0, null_blen=0,
-- empty_ts=0), so a from-events rebuild lands zero rejected rows.
--   - kind IN (user/feedback/project/reference): the MemoryWritten payload
--     enum (blueprints/events/MemoryWritten.json).
--   - description / vault_path non-empty: required minLength-1 schema fields.
--   - body_length_bytes >= 0: schema minimum (0 is legal — empty body).
--   - last_event_ts non-empty: folded from the event's real ts, so a future
--     writer regression surfaces as a REJECTED insert, not a blank column
--     (same shape as migration 071's reranker last_event_ts lock).
--
-- ## No backfill / synth (chain no-backfill invariant)
--
-- This migration only DEFINES the shape + invariants; it ships the table
-- EMPTY. The writer is the fold in go/internal/projections/memories.go;
-- `toolkit-server rebuild-projections` folds the real MemoryWritten events
-- ledger to populate it. Replaying the real events ledger is the
-- legitimate rebuild path (identical to proj_current_bugs /
-- proj_current_suggestions, which also fold from events post-CRUD-
-- retirement) — NOT a synthetic backfill.

CREATE TABLE proj_memories (
    name              TEXT    NOT NULL PRIMARY KEY,
    kind              TEXT    NOT NULL CHECK (kind IN ('user','feedback','project','reference')),
    description       TEXT    NOT NULL CHECK (description <> ''),
    body_length_bytes INTEGER NOT NULL DEFAULT 0 CHECK (body_length_bytes >= 0),
    vault_path        TEXT    NOT NULL CHECK (vault_path <> ''),
    project_id        TEXT    NOT NULL DEFAULT '',
    filed_at          TEXT    NOT NULL,
    last_event_id     TEXT    NOT NULL DEFAULT '',
    last_event_ts     TEXT    NOT NULL CHECK (last_event_ts <> '')
);

CREATE INDEX proj_memories_kind_idx     ON proj_memories (kind);
CREATE INDEX proj_memories_project_idx  ON proj_memories (project_id);
CREATE INDEX proj_memories_filed_at_idx ON proj_memories (filed_at DESC);

-- Watermark row seeded to the current MAX(event_id), mirroring migration
-- 033 / 059. On a fresh DB (empty events table) last_event_id stays NULL
-- and the first fold writes the watermark. The rebuild path resets/writes
-- this watermark itself, so the seed is only load-bearing for incremental
-- folds emitted on top of an un-rebuilt table.
INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'memories', max_id, max_ts FROM (
    SELECT (SELECT event_id FROM events ORDER BY event_id DESC LIMIT 1) AS max_id,
           (SELECT ts       FROM events ORDER BY event_id DESC LIMIT 1) AS max_ts
);
