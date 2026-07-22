-- agent-substrate-crud-retirement chain T2 — proj_current_suggestions
-- projection table. Mirrors proj_current_bugs in shape (suggestion is
-- the bug-sibling entity with native vocabulary: priority not severity,
-- adopted/deferred/rejected resolution kinds).
--
-- See docs/SUBSTRATE_CRUD_RETIREMENT.md §4.7 for the column matrix.
--
-- Backfill: per §6 of the design doc all three live suggestion rows are
-- post-substrate (filed after 2026-05-19); the synthetic-event INSERT
-- below is structurally identical to the other T2 migrations but only
-- fires if pre-substrate rows actually exist. The MIN(events.ts) gate
-- evaluates empty on fresh DBs (in which case every suggestion row is
-- "pre-substrate" by the COALESCE fallback) so test databases get the
-- same synthesise-everything treatment as production.

CREATE TABLE proj_current_suggestions (
    slug                    TEXT    NOT NULL,
    project_id              TEXT    NOT NULL,
    id                      INTEGER NOT NULL,
    title                   TEXT    NOT NULL,
    problem_statement       TEXT    NOT NULL DEFAULT '',
    surface                 TEXT    NOT NULL DEFAULT '',
    priority                TEXT    NOT NULL DEFAULT 'medium',
    source                  TEXT    NOT NULL DEFAULT '',
    acceptance_criteria     TEXT    NOT NULL DEFAULT '',
    constraints             TEXT    NOT NULL DEFAULT '',
    status                  TEXT    NOT NULL DEFAULT 'open',
    resolution_note         TEXT    NOT NULL DEFAULT '',
    resolution_kind         TEXT,
    routed_chain_slug       TEXT    NOT NULL DEFAULT '',
    routed_task_slug        TEXT    NOT NULL DEFAULT '',
    routed_bug_slug         TEXT    NOT NULL DEFAULT '',
    resolved_commit_sha     TEXT,
    tags                    TEXT    NOT NULL DEFAULT '',
    filed_at                TEXT    NOT NULL,
    resolved_at             TEXT,
    updated_at              TEXT    NOT NULL,
    last_event_id           TEXT    NOT NULL DEFAULT '',
    last_event_ts           TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, slug)
);

CREATE INDEX proj_current_suggestions_status_idx   ON proj_current_suggestions (status);
CREATE INDEX proj_current_suggestions_priority_idx ON proj_current_suggestions (priority);
CREATE INDEX proj_current_suggestions_filed_at_idx ON proj_current_suggestions (filed_at DESC);

-- Snapshot-seed from suggestions CRUD.
INSERT INTO proj_current_suggestions (
    slug, project_id, id, title, problem_statement, surface, priority,
    source, acceptance_criteria, constraints, status, resolution_note,
    resolution_kind, routed_chain_slug, routed_task_slug, routed_bug_slug,
    resolved_commit_sha, tags, filed_at, resolved_at, updated_at,
    last_event_id, last_event_ts
)
SELECT slug, project_id, id, title, problem_statement, surface, priority,
       source, acceptance_criteria, constraints, status, resolution_note,
       resolution_kind, routed_chain_slug, routed_task_slug, routed_bug_slug,
       resolved_commit_sha, tags, filed_at, resolved_at, updated_at,
       '', ''
FROM suggestions;

INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'current_suggestions', max_id, max_ts FROM (
    SELECT (SELECT event_id FROM events ORDER BY event_id DESC LIMIT 1) AS max_id,
           (SELECT ts       FROM events ORDER BY event_id DESC LIMIT 1) AS max_ts
);

-- ────────────────────────────────────────────────────────────────────
-- Option-A synthetic-event backfill (per docs/SUBSTRATE_CRUD_RETIREMENT.md §6).
-- ────────────────────────────────────────────────────────────────────
--
-- One SuggestionReported per pre-substrate suggestion row. Production
-- carries zero such rows today (all 3 suggestions are post-substrate),
-- but the SQL is structurally identical to the tasks / bugs sibling so
-- a fresh test DB with seeded suggestion rows gets the same coverage.

INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (unixepoch(s.filed_at) * 1000) / 65536,
        (unixepoch(s.filed_at) * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', s.filed_at) AS ts,
    'system' AS actor_kind,
    'pre-substrate-backfill' AS actor_id,
    'SuggestionReported' AS type,
    'suggestion' AS entity_kind,
    s.slug AS entity_slug,
    s.project_id AS entity_project_id,
    json_object(
        'title', s.title,
        'problem_statement', s.problem_statement,
        'surface', s.surface,
        'priority', s.priority,
        'source', s.source,
        'tags', s.tags,
        'acceptance_criteria', s.acceptance_criteria,
        'constraints', s.constraints
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
FROM suggestions s
WHERE s.filed_at < COALESCE(
    (SELECT MIN(ts) FROM events),
    '9999-12-31'
);
