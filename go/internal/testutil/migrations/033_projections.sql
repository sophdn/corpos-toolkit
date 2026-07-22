-- agent-first-substrate chain T4 — projection tables + watermark.
--
-- Projections are denormalized materialized views derived from the CRUD
-- tables (bugs / chains / tasks / roadmap_items) — see docs/PROJECTIONS.md
-- for the full design. Three projections ship in this migration:
--
--   proj_current_bugs   — bug list/detail surface for the dashboard +
--                         observe-http /bugs endpoints. Folds Bug* events.
--   proj_chain_status   — chain summary with denormalised task-status
--                         counts. Folds Chain* and Task* events.
--   proj_roadmap_view   — roadmap entries with current chain/task status
--                         pre-joined. Folds Chain*, Task*, and (future)
--                         roadmap-mutating events.
--
-- Plus one substrate table:
--
--   projections_watermark — one row per registered projection, tracking
--                           the highest event_id folded into that
--                           projection. Resume point for incremental
--                           folds; sentinel '' on snapshot-seeded rows.
--
-- TRANSITIONAL-PHASE BEHAVIOUR (this chain only):
--
-- The events table holds ~44 rows; CRUD has 3k+ entities pre-dating the
-- event log (T2 close was 2026-05-17). A pure events-only fold would
-- leave projections empty.
--
-- Per chain `agent-first-substrate` design_decisions item 1
-- ("dual-write keeps mutable tables in place; events ledger is source of
-- truth at write boundary"), this migration BACKFILLS projection tables
-- from current CRUD state. Projection FOLD operations in handlers
-- subsequently refresh the touched row from CRUD inside the same tx
-- after the events row INSERTs.
--
-- The events log remains the canonical audit trail; the projection
-- tables are a denormalised read surface refreshed by the same write
-- path that updates CRUD. When Phase 4 retires CRUD tables (out of scope
-- here), the fold contract switches to "construct from payload alone";
-- see docs/PROJECTIONS.md "Future" section.
--
-- CROSS-CHAIN INVARIANT (query-telemetry-substrate TT3): the
-- projections_watermark schema accommodates rows for projections added
-- by the sibling chain (no hardcoded enum of names; TEXT PRIMARY KEY).
-- Three more projections will land there against the same interface; no
-- migration change is needed in this chain to receive them.

CREATE TABLE proj_current_bugs (
    -- Domain columns mirror the bugs CRUD table read-path projection;
    -- nullable resolution_kind / resolved_commit_sha / resolved_at /
    -- qwen_task_id stay nullable here too so the dashboard's null
    -- semantics are preserved verbatim.
    slug                    TEXT    NOT NULL,
    project_id              TEXT    NOT NULL,
    id                      INTEGER NOT NULL,
    title                   TEXT    NOT NULL,
    problem_statement       TEXT    NOT NULL DEFAULT '',
    surface                 TEXT    NOT NULL DEFAULT '',
    severity                TEXT    NOT NULL DEFAULT 'medium',
    source                  TEXT    NOT NULL DEFAULT '',
    acceptance_criteria     TEXT    NOT NULL DEFAULT '',
    constraints             TEXT    NOT NULL DEFAULT '',
    status                  TEXT    NOT NULL DEFAULT 'open',
    resolution_note         TEXT    NOT NULL DEFAULT '',
    resolution_kind         TEXT,
    routed_chain_slug       TEXT    NOT NULL DEFAULT '',
    routed_task_slug        TEXT    NOT NULL DEFAULT '',
    resolved_commit_sha     TEXT,
    qwen_task_id            TEXT,
    tags                    TEXT    NOT NULL DEFAULT '',
    filed_at                TEXT    NOT NULL,
    resolved_at             TEXT,
    updated_at              TEXT    NOT NULL,
    -- Per-row event watermark — empty string '' on rows seeded by the
    -- snapshot (no event has touched them yet). On a Fold the column
    -- gets stamped with the event_id that triggered the refresh.
    last_event_id           TEXT    NOT NULL DEFAULT '',
    last_event_ts           TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, slug)
);

CREATE INDEX proj_current_bugs_status_idx   ON proj_current_bugs (status);
CREATE INDEX proj_current_bugs_severity_idx ON proj_current_bugs (severity);
CREATE INDEX proj_current_bugs_filed_at_idx ON proj_current_bugs (filed_at DESC);

CREATE TABLE proj_chain_status (
    -- Mirrors the observehttp chainRow shape: chain identity + the five
    -- task-status counts + closure-state fields. Counts are folded by
    -- re-reading the tasks table on every Chain*/Task* event.
    slug             TEXT    NOT NULL,
    project_id       TEXT    NOT NULL,
    id               INTEGER NOT NULL,
    status           TEXT    NOT NULL DEFAULT 'open',
    output           TEXT    NOT NULL DEFAULT '',
    design_decisions TEXT    NOT NULL DEFAULT '',
    completion_condition TEXT NOT NULL DEFAULT '',
    closure_summary  TEXT    NOT NULL DEFAULT '',
    total_tasks      INTEGER NOT NULL DEFAULT 0,
    pending          INTEGER NOT NULL DEFAULT 0,
    active           INTEGER NOT NULL DEFAULT 0,
    blocked          INTEGER NOT NULL DEFAULT 0,
    closed           INTEGER NOT NULL DEFAULT 0,
    cancelled        INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL,
    last_event_id    TEXT    NOT NULL DEFAULT '',
    last_event_ts    TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, slug)
);

CREATE INDEX proj_chain_status_status_idx     ON proj_chain_status (status);
CREATE INDEX proj_chain_status_updated_at_idx ON proj_chain_status (updated_at DESC);

CREATE TABLE proj_roadmap_view (
    -- One row per roadmap_items row, with the current ref-target status
    -- + updated_at denormalised from chains/tasks. ref_kind in
    -- ('chain', 'task') matches the CRUD constraint.
    project_id       TEXT    NOT NULL,
    position         INTEGER NOT NULL,
    ref_kind         TEXT    NOT NULL,
    ref_slug         TEXT    NOT NULL,
    chain_slug       TEXT,
    note             TEXT,
    target_status    TEXT,
    target_updated_at TEXT,
    last_event_id    TEXT    NOT NULL DEFAULT '',
    last_event_ts    TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, ref_kind, ref_slug)
);

CREATE INDEX proj_roadmap_view_project_position_idx
    ON proj_roadmap_view (project_id, position);

CREATE TABLE projections_watermark (
    -- TEXT PRIMARY KEY (not an enum) so sibling-chain projections add
    -- rows without a migration here. See cross-chain invariant in the
    -- file header.
    projection_name  TEXT PRIMARY KEY,
    last_event_id    TEXT,
    last_folded_ts   TEXT,
    schema_version   INTEGER NOT NULL DEFAULT 1
);

-- ── Initial snapshot: backfill projections from current CRUD state ────
--
-- The three INSERTs below mirror the SnapshotSQL constants in
-- go/internal/projections/{bugs,chains,roadmap}.go — the same SELECTs
-- run there at rebuild time. Keeping them duplicated (here at migration
-- vs there at runtime) is intentional: the migration runs ONCE at
-- schema upgrade; the rebuild CLI runs ON DEMAND after that. Drift
-- between the two is caught by the byte-identical projection test.

INSERT INTO proj_current_bugs (
    slug, project_id, id, title, problem_statement, surface, severity,
    source, acceptance_criteria, constraints, status, resolution_note,
    resolution_kind, routed_chain_slug, routed_task_slug,
    resolved_commit_sha, qwen_task_id, tags, filed_at, resolved_at,
    updated_at, last_event_id, last_event_ts
)
SELECT slug, project_id, id, title, problem_statement, surface, severity,
       source, acceptance_criteria, constraints, status, resolution_note,
       resolution_kind, routed_chain_slug, routed_task_slug,
       resolved_commit_sha, qwen_task_id, tags, filed_at, resolved_at,
       updated_at, '', ''
FROM bugs;

INSERT INTO proj_chain_status (
    slug, project_id, id, status, output, design_decisions,
    completion_condition, closure_summary,
    total_tasks, pending, active, blocked, closed, cancelled,
    created_at, updated_at, last_event_id, last_event_ts
)
SELECT c.slug, c.project_id, c.id, c.status, c.output, c.design_decisions,
       c.completion_condition, c.closure_summary,
       (SELECT COUNT(*) FROM tasks WHERE chain_id = c.id),
       (SELECT COUNT(*) FROM tasks WHERE chain_id = c.id AND status = 'pending'),
       (SELECT COUNT(*) FROM tasks WHERE chain_id = c.id AND status = 'active'),
       (SELECT COUNT(*) FROM tasks WHERE chain_id = c.id AND status = 'blocked'),
       (SELECT COUNT(*) FROM tasks WHERE chain_id = c.id AND status = 'closed'),
       (SELECT COUNT(*) FROM tasks WHERE chain_id = c.id AND status = 'cancelled'),
       c.created_at, c.updated_at, '', ''
FROM chains c;

INSERT INTO proj_roadmap_view (
    project_id, position, ref_kind, ref_slug, chain_slug, note,
    target_status, target_updated_at, last_event_id, last_event_ts
)
SELECT r.project_id, r.position, r.ref_kind, r.ref_slug, r.chain_slug, r.note,
       CASE WHEN r.ref_kind = 'chain' THEN c.status
            WHEN r.ref_kind = 'task'  THEN t.status END,
       CASE WHEN r.ref_kind = 'chain' THEN c.updated_at
            WHEN r.ref_kind = 'task'  THEN t.updated_at END,
       '', ''
FROM roadmap_items r
LEFT JOIN chains c ON r.ref_kind = 'chain' AND c.slug = r.ref_slug
LEFT JOIN tasks  t ON r.ref_kind = 'task'  AND t.slug = r.ref_slug;

-- Seed the watermark rows for the three registered projections. The
-- last_event_id stamps to the highest existing event_id so handlers
-- that emit on top of the snapshot won't replay pre-snapshot events.
-- If the events table is empty (fresh DB), last_event_id stays NULL —
-- the first fold writes the watermark.
INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'current_bugs', max_id, max_ts FROM (
    SELECT (SELECT event_id FROM events ORDER BY event_id DESC LIMIT 1) AS max_id,
           (SELECT ts       FROM events ORDER BY event_id DESC LIMIT 1) AS max_ts
);

INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'chain_status', max_id, max_ts FROM (
    SELECT (SELECT event_id FROM events ORDER BY event_id DESC LIMIT 1) AS max_id,
           (SELECT ts       FROM events ORDER BY event_id DESC LIMIT 1) AS max_ts
);

INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'roadmap_view', max_id, max_ts FROM (
    SELECT (SELECT event_id FROM events ORDER BY event_id DESC LIMIT 1) AS max_id,
           (SELECT ts       FROM events ORDER BY event_id DESC LIMIT 1) AS max_ts
);
