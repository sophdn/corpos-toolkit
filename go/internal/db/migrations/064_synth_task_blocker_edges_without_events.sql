-- agent-substrate-crud-retirement T6a unblock — synth missing
-- TaskTransitioned events for proj_task_blockers edges that have no
-- event source. Closes bug
-- `proj-task-blockers-rebuild-produces-many-extra-edges-fold-semantic-
-- divergence` for the "live edges without events" half (the orphan-
-- close cleanup half is fixed by the foldTaskBlockersCleanupOnClose
-- handler in this commit's projections/tasks.go change).
--
-- Root cause: pre-T3 HandleTaskBlock fired direct INSERT INTO
-- task_blockers (CRUD) without emitting TaskTransitioned with a
-- blocker_slug payload (the L1181 guard rejected the 2nd+ edge per
-- bug `task-blockers-payload-gap-for-substrate-rebuild`, resolved
-- in T3 + T5-tasks). The pre-fix edges live in proj_task_blockers
-- via migration 057's snapshot-seed from CRUD but have no events.
-- Rebuild-from-empty therefore omits them.
--
-- Five such edges existed as of 2026-05-21; this migration walks
-- proj_task_blockers and emits one synthetic TaskTransitioned per
-- edge that lacks a matching add-blocker event. Idempotent: NOT
-- EXISTS guard on the synthetic actor_id.

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
    'task-blocker-edge-backfill-064' AS actor_id,
    'TaskTransitioned' AS type,
    'task' AS entity_kind,
    blocked.slug AS entity_slug,
    blocked_chain.project_id AS entity_project_id,
    json_object(
        'from_status', 'pending',
        'to_status', 'blocked',
        'blocker_slug', blocker.slug
    ) AS payload,
    NULL AS rationale,
    NULL AS caused_by_event_id,
    '[]' AS related_entities,
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
FROM proj_task_blockers tb
JOIN proj_current_tasks blocked ON blocked.id = tb.blocked_task_id
JOIN proj_current_tasks blocker ON blocker.id = tb.blocker_task_id
JOIN proj_chain_status blocked_chain ON blocked_chain.id = blocked.chain_id
WHERE NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.type = 'TaskTransitioned'
      AND e.entity_kind = 'task'
      AND e.entity_slug = blocked.slug
      AND json_extract(e.payload, '$.blocker_slug') = blocker.slug
);
