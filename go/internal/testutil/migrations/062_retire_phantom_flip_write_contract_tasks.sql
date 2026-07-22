-- agent-substrate-crud-retirement T6a unblock — retire the 6 phantom
-- task rows that survive `rebuild-projections` but don't exist in
-- live state. Closes bug
-- `rebuild-phantom-tasks-from-pre-event-forge-delete-path` (filed
-- post-061 sanity-check).
--
-- The 6 slugs are the T5 sub-tasks the prior agent forge(task)-
-- created when splitting T5 (each got a TaskCreated event), task_
-- complete'd, and then forge_deleted via the pre-8f2cb87 buggy path
-- that didn't emit a retirement event. Bug
-- `forge-task-delete-lacks-event-emit` was fixed at 8f2cb87 by
-- removing the entire delete code path, so no future occurrences —
-- but the orphan TaskCreated events for these 6 slugs persist in
-- the events log with no matching retirement. Rebuild-from-empty
-- resurrects them as phantoms.
--
-- This migration emits one TaskRetired event per phantom slug. The
-- TaskRetired event type (new this commit) is folded into a DELETE
-- on proj_current_tasks + cleanup of any proj_task_blockers edges +
-- a counter refresh on the parent proj_chain_status. Idempotent:
-- the NOT EXISTS guard means re-running this migration on a DB that
-- already has the retirement events is a no-op.
--
-- Future drift: this migration captures the SPECIFIC 6 slugs that
-- were already known phantoms at 2026-05-21. If future pre-event-
-- emit deletions (which shouldn't happen post-8f2cb87) produce more
-- phantoms, a similar one-shot would handle them. The bigger
-- prevention is the no-task-delete invariant 8f2cb87 established.

INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch('now') * 1000) / 65536,
        (unixepoch('now') * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', 'now') AS ts,
    'system' AS actor_kind,
    'phantom-task-retire-062' AS actor_id,
    'TaskRetired' AS type,
    'task' AS entity_kind,
    e.entity_slug AS entity_slug,
    e.entity_project_id AS entity_project_id,
    json_object('reason', 'pre-8f2cb87 forge-task-delete buggy path; retroactively retired in migration 062') AS payload,
    NULL AS rationale,
    e.event_id AS caused_by_event_id, -- the original TaskCreated, for audit linkage
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
FROM events e
WHERE e.type = 'TaskCreated'
  AND e.entity_slug IN (
      'flip-write-contract-suggestions',
      'flip-write-contract-roadmap',
      'flip-write-contract-bugs',
      'flip-write-contract-chains',
      'flip-write-contract-benchmarks',
      'flip-write-contract-tasks'
  )
  AND e.entity_project_id = 'mcp-servers'
  AND NOT EXISTS (
      SELECT 1 FROM events e2
      WHERE e2.type = 'TaskRetired'
        AND e2.entity_kind = 'task'
        AND e2.entity_slug = e.entity_slug
        AND e2.entity_project_id = e.entity_project_id
  );
