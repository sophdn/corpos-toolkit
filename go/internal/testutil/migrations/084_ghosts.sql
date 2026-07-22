-- Chain emit-surface-forge-v2 (T4 ghosts-and-rejection-projection): the
-- ghosts table — the persistent fumble record for the forge-v2 `record`
-- surface.
--
-- A `record(events[])` event that fails the thin-fast-local validation tier
-- is neither dropped nor folded into entity projections. It becomes a GHOST:
-- a durable "you tried X, rejected because Y, here is enough to rewrite it"
-- record, anchored to the session that produced it (EMIT_SURFACE_PHASE2 §5).
--
-- Why a DIRECT-WRITE table, not an event-folded projection: a rejected event
-- is, by definition, NOT a valid event — it cannot be appended to the events
-- ledger. The ghost is a separate fact ABOUT the rejection. Keeping ghosts in
-- their own table (never written by the projection fold path) is exactly what
-- makes the §7 invariant hold structurally: ghosts CANNOT fold into entity
-- projections, and a from-empty projection rebuild (rebuild-projections /
-- RebuildAll, which truncates + refolds the proj_* tables from events) never
-- touches this table — so the entity projections rebuild byte-identically
-- whether or not ghosts exist. Squashing the hot draft (rewriting unpublished
-- events) likewise cannot lose the fumble record: ghosts are not derived from
-- the squashable event stream.
--
-- The rejection / fumble PROJECTION (EMIT_SURFACE_PHASE2 §6, closing the
-- forge-shape-liveness "success AND rejection counts per shape" gap) is a
-- count query over this table grouped by attempted_type — see
-- work.GhostFumbleCounts.

CREATE TABLE ghosts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    span_id         TEXT NOT NULL,
    session_id      TEXT,
    project_id      TEXT,
    -- The event the caller attempted to record (its declared type + entity).
    attempted_type  TEXT NOT NULL,
    entity_kind     TEXT,
    entity_slug     TEXT,
    -- Why it was rejected (the validator's descriptive reason) + enough
    -- context to rewrite it (the original payload bytes).
    reason          TEXT NOT NULL,
    rewrite_payload TEXT NOT NULL DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- The fumble projection's grouping key (rejections per attempted shape) and
-- the session-anchored surfacing query both read these.
CREATE INDEX ghosts_attempted_type_idx ON ghosts (attempted_type);
CREATE INDEX ghosts_session_idx        ON ghosts (session_id, created_at);
