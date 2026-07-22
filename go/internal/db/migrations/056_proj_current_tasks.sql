-- agent-substrate-crud-retirement chain T2 — proj_current_tasks projection
-- table + Option-A synthetic-event backfill for pre-substrate task rows.
--
-- The tasks CRUD table is the highest-read-traffic entity in the work
-- meta-tool (task_read / task_list / task_search hit it on every chain
-- driven session). Today every read hits tasks directly; this migration
-- lands the projection that T4 will repoint reads to, with the live
-- column shape mirrored from the CRUD table.
--
-- See docs/SUBSTRATE_CRUD_RETIREMENT.md §4.2 for the column matrix and
-- §6 for the Option-A pre-substrate-entity strategy (synthesize one
-- TaskCreated event per pre-substrate task row so rebuild-from-empty
-- against the events ledger reproduces every row byte-identically).
--
-- ────────────────────────────────────────────────────────────────────
-- Table shape
-- ────────────────────────────────────────────────────────────────────
--
-- chain_id is denormalised onto the projection row (the CRUD shape uses
-- INTEGER chain_id FK; we mirror it here as the same numeric ID so
-- existing JOIN-on-chain_id query patterns can stay on the projection
-- post-T4). The PK is (chain_id, slug) — matches the CRUD UNIQUE
-- constraint exactly; task slugs are NOT globally unique, only within a
-- chain (see ErrAmbiguousSlug in internal/work).

CREATE TABLE proj_current_tasks (
    id                      INTEGER NOT NULL,
    chain_id                INTEGER NOT NULL,
    slug                    TEXT    NOT NULL,
    position                INTEGER NOT NULL DEFAULT 0,
    status                  TEXT    NOT NULL DEFAULT 'pending',
    problem_statement       TEXT    NOT NULL DEFAULT '',
    acceptance_criteria     TEXT    NOT NULL DEFAULT '',
    context_required        TEXT    NOT NULL DEFAULT '',
    constraints             TEXT    NOT NULL DEFAULT '',
    handoff_output          TEXT    NOT NULL DEFAULT '',
    originated_chain_id     INTEGER,
    moved_on                TEXT,
    commit_sha              TEXT,
    created_at              TEXT    NOT NULL,
    updated_at              TEXT    NOT NULL,
    -- Per-row event watermark — empty string '' on snapshot-seeded rows.
    last_event_id           TEXT    NOT NULL DEFAULT '',
    last_event_ts           TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (chain_id, slug)
);

CREATE INDEX proj_current_tasks_status_idx     ON proj_current_tasks (status);
CREATE INDEX proj_current_tasks_chain_id_idx   ON proj_current_tasks (chain_id);
CREATE INDEX proj_current_tasks_updated_at_idx ON proj_current_tasks (updated_at DESC);

-- ────────────────────────────────────────────────────────────────────
-- Snapshot-seed from the tasks CRUD table.
-- ────────────────────────────────────────────────────────────────────
--
-- last_event_id / last_event_ts stay as the empty-string sentinel on
-- snapshot-seeded rows (matching migration 033's pattern). The
-- synthetic-event backfill below emits one TaskCreated per row, but the
-- per-row watermark only advances when a real fold path runs against
-- that row — keeping the sentinel preserves the "no fold has touched
-- this row yet" semantics that the byte-identical-rebuild test depends
-- on (see internal/projections/projections_test.go's tableChecksum
-- comment for the skip-last_event_* contract).

INSERT INTO proj_current_tasks (
    id, chain_id, slug, position, status, problem_statement,
    acceptance_criteria, context_required, constraints, handoff_output,
    originated_chain_id, moved_on, commit_sha, created_at, updated_at,
    last_event_id, last_event_ts
)
SELECT id, chain_id, slug, position, status, problem_statement,
       acceptance_criteria, context_required, constraints, handoff_output,
       originated_chain_id, moved_on, commit_sha, created_at, updated_at,
       '', ''
FROM tasks;

-- ────────────────────────────────────────────────────────────────────
-- Watermark row — stamps to the current MAX(event_id) so live emits
-- after the snapshot don't replay pre-snapshot events.
-- ────────────────────────────────────────────────────────────────────

INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'current_tasks', max_id, max_ts FROM (
    SELECT (SELECT event_id FROM events ORDER BY event_id DESC LIMIT 1) AS max_id,
           (SELECT ts       FROM events ORDER BY event_id DESC LIMIT 1) AS max_ts
);

-- ────────────────────────────────────────────────────────────────────
-- Option-A synthetic-event backfill (per docs/SUBSTRATE_CRUD_RETIREMENT.md §6).
-- ────────────────────────────────────────────────────────────────────
--
-- One TaskCreated event per pre-substrate task row. Synthesised actor
-- is system / pre-substrate-backfill so forensic queries can isolate
-- the backfill set from live agent activity. The event_id is a
-- UUIDv7 whose first 48 bits encode the row's created_at as Unix-ms;
-- the remaining 80 bits use SQLite's randomblob so the events are
-- globally unique within the migration. randomblob is acceptable
-- here because the migration runs exactly once per database; the
-- byte-identical rebuild test ignores last_event_id / last_event_ts
-- on projection rows (see internal/projections/projections_test.go
-- tableChecksum).
--
-- Pre-substrate boundary: only rows whose created_at predates the
-- earliest events.ts are backfilled. The events table's earliest
-- timestamp pins the cutover. If the events table is empty (fresh DB),
-- every task row is treated as pre-substrate (the COALESCE fallback
-- with a far-future sentinel ensures the < comparison holds for all
-- live rows).
--
-- The payload mirrors the TaskCreated blueprint required fields
-- (chain_slug, problem_statement) plus the optional acceptance_criteria
-- / context_required / constraints / handoff_output / position from
-- current CRUD state. chain_slug is resolved at synth time via JOIN to
-- chains — matches the production fold path's projection self-join
-- contract per docs/SUBSTRATE_CRUD_RETIREMENT.md §10 Q2 default.
--
-- The rationale field on synthesised events is omitted (NULL) — the
-- envelope schema only requires rationale for actor.kind='agent', and
-- these are actor.kind='system' per §6. See blueprints/events/_envelope.json.

INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    -- UUIDv7: 48-bit timestamp (unix-ms big-endian) + version nibble '7' +
    -- random suffix. Layout TTTTTTTT-TTTT-7XXX-VXXX-XXXXXXXXXXXX where
    -- TTTTTTTT-TTTT is the 48-bit ms timestamp (high 32 bits then low
    -- 16), 7 is the version nibble, V ∈ {8,9,a,b} is the variant nibble.
    -- Random bits drawn from SQLite randomblob — fine because the
    -- migration runs once per DB and event_id only needs uniqueness; the
    -- byte-identical rebuild test ignores last_event_id / last_event_ts.
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
    'pre-substrate-backfill' AS actor_id,
    'TaskCreated' AS type,
    'task' AS entity_kind,
    t.slug AS entity_slug,
    c.project_id AS entity_project_id,
    json_object(
        'chain_slug', c.slug,
        'position', t.position,
        'problem_statement', t.problem_statement,
        'acceptance_criteria', t.acceptance_criteria,
        'context_required', t.context_required,
        'constraints', t.constraints,
        'handoff_output', t.handoff_output
    ) AS payload,
    NULL AS rationale,
    NULL AS caused_by_event_id,
    '[]' AS related_entities,
    -- span_id: UUIDv4 shape (version '4'; variant in [89ab]). Random
    -- per row; the synthetic backfill is one logical "request" but each
    -- row gets its own span id because there's no shared dispatcher call.
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
FROM tasks t
JOIN chains c ON c.id = t.chain_id
WHERE t.created_at < COALESCE(
    (SELECT MIN(ts) FROM events),
    '9999-12-31'
);
