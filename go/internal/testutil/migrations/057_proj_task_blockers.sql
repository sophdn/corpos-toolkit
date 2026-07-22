-- agent-substrate-crud-retirement chain T2 — proj_task_blockers projection
-- table + Option-A synthetic-event backfill for pre-substrate blocker edges.
--
-- task_blockers is a live join table — each row a directed edge (blocked
-- task ← blocker task) carrying a freeform `reason`. The projection
-- mirrors the join shape so T4 can repoint the task_blockers selector
-- without changing the read shape.
--
-- See docs/SUBSTRATE_CRUD_RETIREMENT.md §4.4 for the column matrix and
-- §6 for the Option-A pre-substrate strategy. Note also §9.1: T3 must
-- extend TaskTransitioned's payload to carry removed_blocker_slug
-- before T5 can flip this projection's fold to payload-only. In the
-- meantime (T2 — this migration; T4 — handler repointing; T5 awaiting
-- T3's bump) the fold reads task_blockers CRUD post-write, matching the
-- agent-first-substrate T4 dual-write contract.

CREATE TABLE proj_task_blockers (
    blocked_task_id   INTEGER NOT NULL,
    blocker_task_id   INTEGER NOT NULL,
    reason            TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL,
    last_event_id     TEXT    NOT NULL DEFAULT '',
    last_event_ts     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (blocked_task_id, blocker_task_id)
);

CREATE INDEX proj_task_blockers_blocked_idx ON proj_task_blockers (blocked_task_id);
CREATE INDEX proj_task_blockers_blocker_idx ON proj_task_blockers (blocker_task_id);

-- Snapshot-seed from task_blockers CRUD.
INSERT INTO proj_task_blockers (
    blocked_task_id, blocker_task_id, reason, created_at,
    last_event_id, last_event_ts
)
SELECT blocked_task_id, blocker_task_id, reason, created_at, '', ''
FROM task_blockers;

-- Watermark row.
INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'task_blockers', max_id, max_ts FROM (
    SELECT (SELECT event_id FROM events ORDER BY event_id DESC LIMIT 1) AS max_id,
           (SELECT ts       FROM events ORDER BY event_id DESC LIMIT 1) AS max_ts
);

-- ────────────────────────────────────────────────────────────────────
-- Option-A synthetic-event backfill (per docs/SUBSTRATE_CRUD_RETIREMENT.md §6).
-- ────────────────────────────────────────────────────────────────────
--
-- One synthetic TaskTransitioned per pre-substrate task_blockers row,
-- carrying { from_status: 'pending', to_status: 'blocked', blocker_slug:
-- <blocker task slug> } so a payload-only fold (post-T3 / T5) can
-- reconstruct the edge. The entity is the *blocked* task; the blocker
-- shows up as a related-entity reference.
--
-- The synthesised actor matches §6: system / pre-substrate-backfill,
-- ts pinned to the row's created_at. The pre-substrate boundary check
-- (created_at < MIN(events.ts)) gates the backfill against re-running
-- on a DB that already accrued real events.

INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch(tb.created_at) * 1000) / 65536,
        (unixepoch(tb.created_at) * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', tb.created_at) AS ts,
    'system' AS actor_kind,
    'pre-substrate-backfill' AS actor_id,
    'TaskTransitioned' AS type,
    'task' AS entity_kind,
    blocked.slug AS entity_slug,
    c_blocked.project_id AS entity_project_id,
    json_object(
        'from_status', 'pending',
        'to_status',   'blocked',
        'blocker_slug', blocker.slug
    ) AS payload,
    NULL AS rationale,
    NULL AS caused_by_event_id,
    json_array(json_object(
        'kind', 'task',
        'slug', blocker.slug,
        'project_id', c_blocker.project_id
    )) AS related_entities,
    lower(printf(
        '%s-%s-4%s-%s%s-%s',
        lower(hex(randomblob(4))),
        lower(hex(randomblob(2))),
        substr(lower(hex(randomblob(2))), 2),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        substr(lower(hex(randomblob(2))), 2),
        lower(hex(randomblob(6)))
    )) AS span_id,
    1 AS schema_version
FROM task_blockers tb
JOIN tasks blocked   ON blocked.id = tb.blocked_task_id
JOIN chains c_blocked ON c_blocked.id = blocked.chain_id
JOIN tasks blocker   ON blocker.id = tb.blocker_task_id
JOIN chains c_blocker ON c_blocker.id = blocker.chain_id
WHERE tb.created_at < COALESCE(
    (SELECT MIN(ts) FROM events),
    '9999-12-31'
);
