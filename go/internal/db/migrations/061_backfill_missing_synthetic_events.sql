-- agent-substrate-crud-retirement T6 follow-up — fill the Option-A
-- backfill gap.
--
-- Migration 056 emitted synthetic TaskCreated events for ~1015 of 2864
-- pre-substrate tasks but never emitted synthetic ChainCreated /
-- BugReported / RoadmapUpdated for the pre-substrate entities those
-- tasks reference. Plus the live system had pre-substrate state in
-- proj_* tables (from snapshot-seed at migration time) that has no
-- event source at all. Result: `toolkit-server rebuild-projections`
-- completed (post-bug-1493) but produced row counts diverging wildly
-- from live state — 38/277 chains, 142/845 bugs, 229/2864 tasks —
-- because the rebuild fold skips events whose chain_slug can't be
-- resolved (fold's SELECT id FROM proj_chain_status returns ErrNoRows
-- and the handler returns nil to silently skip).
--
-- This migration walks the live projection tables and synthesizes one
-- *Created / *Reported event per entity that lacks one. Migration 056's
-- "pre-substrate boundary" gate (created_at < earliest events.ts) is
-- relaxed here: any live projection row without a corresponding event
-- gets one, regardless of timestamp. The synthesised events carry the
-- entity's current created_at as event.ts so the rebuild-from-empty
-- order matches the live system's incremental-fold order to the extent
-- the original ordering survived.
--
-- The fold modules already tolerate both shapes (post-bug-1493 the Go
-- UnmarshalJSON wraps string-typed acceptance_criteria as a single-
-- element list); this migration produces canonical-shape payloads
-- where possible. acceptance_criteria / constraints carry the joined
-- string from the projection column (the snapshot-seed already
-- joined-on-newline-dash from the original CRUD), wrapped in a single
-- JSON-array element to keep the canonical event shape.
--
-- Bug `option-a-backfill-missing-chain-bug-suggestion-events-for-pre-
-- substrate-entities` (1495) drives this migration; it closes the
-- chain `agent-substrate-crud-retirement`'s completion_condition (b)
-- (byte-identical rebuild from empty) for chains/tasks/bugs/roadmap.
-- benchmark_results retains Option-B per design §13.3.
--
-- Event-ID shape: UUIDv7 with the row's created_at as the 48-bit
-- timestamp prefix (so events sort naturally by entity creation time).
-- Same pattern as migration 056 §UUIDv7 comment block; see there for
-- the rationale on randomblob suffix vs deterministic.

-- ───────────────────────────────────────────────────────────────────
-- 1. Synthetic ChainCreated for proj_chain_status rows without one.
-- ───────────────────────────────────────────────────────────────────
INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch(cs.created_at) * 1000) / 65536,
        (unixepoch(cs.created_at) * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', cs.created_at) AS ts,
    'system' AS actor_kind,
    'option-a-backfill-061' AS actor_id,
    'ChainCreated' AS type,
    'chain' AS entity_kind,
    cs.slug AS entity_slug,
    cs.project_id AS entity_project_id,
    json_object(
        'output', cs.output,
        'design_decisions', cs.design_decisions,
        'completion_condition', cs.completion_condition
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
FROM proj_chain_status cs
WHERE NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.type = 'ChainCreated'
      AND e.entity_kind = 'chain'
      AND e.entity_slug = cs.slug
      AND e.entity_project_id = cs.project_id
);

-- ───────────────────────────────────────────────────────────────────
-- 2. Synthetic BugReported for proj_current_bugs rows without one.
-- ───────────────────────────────────────────────────────────────────
INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch(b.filed_at) * 1000) / 65536,
        (unixepoch(b.filed_at) * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', b.filed_at) AS ts,
    'system' AS actor_kind,
    'option-a-backfill-061' AS actor_id,
    'BugReported' AS type,
    'bug' AS entity_kind,
    b.slug AS entity_slug,
    b.project_id AS entity_project_id,
    json_object(
        'title', b.title,
        'problem_statement', b.problem_statement,
        'surface', b.surface,
        'severity', b.severity,
        'source', b.source,
        'tags', b.tags,
        'acceptance_criteria', json_array(b.acceptance_criteria),
        'constraints', b.constraints,
        'qwen_task_id', b.qwen_task_id,
        'routed_suggestion_slug', b.routed_suggestion_slug
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
FROM proj_current_bugs b
WHERE NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.type = 'BugReported'
      AND e.entity_kind = 'bug'
      AND e.entity_slug = b.slug
      AND e.entity_project_id = b.project_id
);

-- ───────────────────────────────────────────────────────────────────
-- 3. Synthetic TaskCreated for proj_current_tasks rows without one.
--    Runs AFTER ChainCreated synthesis so the chain_slug lookup
--    (via proj_chain_status JOIN) resolves cleanly for every task.
-- ───────────────────────────────────────────────────────────────────
INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch(t.created_at) * 1000) / 65536,
        (unixepoch(t.created_at) * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', t.created_at) AS ts,
    'system' AS actor_kind,
    'option-a-backfill-061' AS actor_id,
    'TaskCreated' AS type,
    'task' AS entity_kind,
    t.slug AS entity_slug,
    cs.project_id AS entity_project_id,
    json_object(
        'chain_slug', cs.slug,
        'position', t.position,
        'problem_statement', t.problem_statement,
        'acceptance_criteria', json_array(t.acceptance_criteria),
        'context_required', t.context_required,
        'constraints', t.constraints,
        'handoff_output', t.handoff_output
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
FROM proj_current_tasks t
JOIN proj_chain_status cs ON cs.id = t.chain_id
WHERE NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.type = 'TaskCreated'
      AND e.entity_kind = 'task'
      AND e.entity_slug = t.slug
      AND e.entity_project_id = cs.project_id
);

-- ───────────────────────────────────────────────────────────────────
-- 4. Synthetic RoadmapUpdated.set per project carrying the live
--    layout. The fold's "set" action_kind wipes proj_roadmap_view for
--    the project and re-inserts from payload.items[]. Emitting one
--    per project with the live state means rebuild-from-empty's
--    final state for proj_roadmap_view matches live (subsequent
--    production RoadmapUpdated events, if any, mutate from there).
--
--    Note on event-id ordering: this set event is timestamped just
--    after the latest existing event for the project (or now if none),
--    so it applies LAST in the rebuild fold for that project — the
--    chain-of-events arrives at the same end state regardless of
--    interleaving production events that came before. Idempotent:
--    re-running detects the existing 061-synth set event and skips.
-- ───────────────────────────────────────────────────────────────────
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
    'option-a-backfill-061' AS actor_id,
    'RoadmapUpdated' AS type,
    'roadmap' AS entity_kind,
    'main' AS entity_slug,
    project_id AS entity_project_id,
    json_object(
        'action_kind', 'set',
        'positions', (
            SELECT json_group_array(position)
            FROM proj_roadmap_view r2
            WHERE r2.project_id = r.project_id
            ORDER BY position
        ),
        'item_count', (
            SELECT COUNT(*) FROM proj_roadmap_view r2 WHERE r2.project_id = r.project_id
        ),
        'items', (
            SELECT json_group_array(
                json_object(
                    'position', position,
                    'ref_kind', ref_kind,
                    'ref_slug', ref_slug,
                    'chain_slug', chain_slug,
                    'note', note
                )
            )
            FROM proj_roadmap_view r2
            WHERE r2.project_id = r.project_id
            ORDER BY position
        )
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
FROM (SELECT DISTINCT project_id FROM proj_roadmap_view) r
WHERE NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.type = 'RoadmapUpdated'
      AND e.entity_project_id = r.project_id
      AND e.actor_id = 'option-a-backfill-061'
);
