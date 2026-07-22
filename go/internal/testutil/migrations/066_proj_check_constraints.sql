-- rust-retirement-and-db-hardening chain T2 — CHECK constraints on
-- proj_current_bugs / proj_current_tasks / proj_chain_status terminal-
-- status invariants. Closes the structural class of regression where
-- rebuild-projections (or any UPDATE path) can flip a row into a
-- semantically invalid state.
--
-- 2026-05-22 incident shape: rebuild-projections from an incomplete
-- events ledger flipped 707 bugs from terminal status (fixed/wontfix/
-- routed) back to 'open' while leaving resolved_at populated. The
-- biconditional invariant `status='open' XNOR resolved_at IS NULL`
-- catches that class of write — the projection row CANNOT be
-- semantically inconsistent regardless of how it was produced.
--
-- ## Constraint set (three tables, two checks each)
--
-- proj_current_bugs:
--   1. status IN ('open','fixed','wontfix','upstream','dup','routed').
--      Six-value vocabulary; 'upstream' added vs the original
--      five-value spec to match the live work-handler vocab
--      (go/internal/work/bug.go canonical list).
--   2. status='open' iff resolved_at IS NULL.
--      Biconditional — catches BOTH the regression direction (open + a
--      stale resolved_at) AND the inverse (terminal + missing resolved_at).
--
-- proj_current_tasks:
--   1. status IN ('pending','active','blocked','closed','cancelled').
--   2. status='closed' implies commit_sha IS NOT NULL.
--      Single-direction implication — `cancelled` is terminal but
--      doesn't carry a commit (923 rows; legit), so the symmetric
--      biconditional doesn't hold. Pre-discipline-adoption closures
--      with empty-string commit_sha pass (the check is NOT NULL, not
--      != ''); the soft "supply 'unversioned' for non-repo fixes"
--      convention stays informal.
--
-- proj_chain_status:
--   1. status IN ('open','closed').
--   2. status='closed' implies closure_summary IS NOT NULL.
--      Same shape as tasks: 6 historical chains have closure_summary='',
--      not NULL, so the NOT NULL check passes for them; the soft
--      "write a summary on close" norm stays informal.
--
-- ## SQLite recreate-pattern
--
-- SQLite has no `ALTER TABLE ... ADD CHECK` — adding CHECK constraints
-- to an existing table requires the create-copy-drop-rename ritual.
-- Each table-recreate preserves the full live column shape (current as
-- of migration 065's prose-column drops + migration 055's
-- routed_suggestion_slug addition on bugs), copies all rows verbatim,
-- and re-creates the original indexes.
--
-- The migration is idempotent under the runner's "skip already-applied"
-- check (each migration runs once at the schema-version boundary).
--
-- ## Pre-migration sanity (informational only — runs at sql parse time)
--
-- The audit run prior to authoring this migration (2026-05-22) showed:
--   bugs_open_with_resolved_at = 0
--   bugs_terminal_no_resolved_at = 0
--   bugs_unknown_status = 4 (resolved to 'upstream' vocab addition)
--   tasks_closed_no_commit_sha (NULL) = 0  (23 with '' empty string, pass)
--   tasks_unknown_status = 0
--   chains_closed_no_closure_summary (NULL) = 0  (6 with '' empty, pass)
--   chains_unknown_status = 0
-- → every existing row passes the constraint set as written.

-- ────────────────────────────────────────────────────────────────────
-- proj_current_bugs
-- ────────────────────────────────────────────────────────────────────

CREATE TABLE proj_current_bugs_new (
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
    resolution_kind         TEXT,
    routed_chain_slug       TEXT    NOT NULL DEFAULT '',
    routed_task_slug        TEXT    NOT NULL DEFAULT '',
    resolved_commit_sha     TEXT,
    qwen_task_id            TEXT,
    tags                    TEXT    NOT NULL DEFAULT '',
    filed_at                TEXT    NOT NULL,
    resolved_at             TEXT,
    updated_at              TEXT    NOT NULL,
    last_event_id           TEXT    NOT NULL DEFAULT '',
    last_event_ts           TEXT    NOT NULL DEFAULT '',
    routed_suggestion_slug  TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, slug),
    CHECK (status IN ('open','fixed','wontfix','upstream','dup','routed')),
    CHECK (
        (status = 'open' AND resolved_at IS NULL)
        OR (status != 'open' AND resolved_at IS NOT NULL)
    )
);

INSERT INTO proj_current_bugs_new SELECT * FROM proj_current_bugs;
DROP TABLE proj_current_bugs;
ALTER TABLE proj_current_bugs_new RENAME TO proj_current_bugs;
CREATE INDEX proj_current_bugs_status_idx   ON proj_current_bugs (status);
CREATE INDEX proj_current_bugs_severity_idx ON proj_current_bugs (severity);
CREATE INDEX proj_current_bugs_filed_at_idx ON proj_current_bugs (filed_at DESC);

-- ────────────────────────────────────────────────────────────────────
-- proj_current_tasks
-- ────────────────────────────────────────────────────────────────────

CREATE TABLE proj_current_tasks_new (
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
    last_event_id           TEXT    NOT NULL DEFAULT '',
    last_event_ts           TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (chain_id, slug),
    CHECK (status IN ('pending','active','blocked','closed','cancelled')),
    CHECK (status != 'closed' OR commit_sha IS NOT NULL)
);

INSERT INTO proj_current_tasks_new SELECT * FROM proj_current_tasks;
DROP TABLE proj_current_tasks;
ALTER TABLE proj_current_tasks_new RENAME TO proj_current_tasks;
CREATE INDEX proj_current_tasks_status_idx     ON proj_current_tasks (status);
CREATE INDEX proj_current_tasks_chain_id_idx   ON proj_current_tasks (chain_id);
CREATE INDEX proj_current_tasks_updated_at_idx ON proj_current_tasks (updated_at DESC);

-- ────────────────────────────────────────────────────────────────────
-- proj_chain_status
-- ────────────────────────────────────────────────────────────────────

CREATE TABLE proj_chain_status_new (
    slug             TEXT    NOT NULL,
    project_id       TEXT    NOT NULL,
    id               INTEGER NOT NULL,
    status           TEXT    NOT NULL DEFAULT 'open',
    output           TEXT    NOT NULL DEFAULT '',
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
    PRIMARY KEY (project_id, slug),
    CHECK (status IN ('open','closed')),
    CHECK (status != 'closed' OR closure_summary IS NOT NULL)
);

INSERT INTO proj_chain_status_new SELECT * FROM proj_chain_status;
DROP TABLE proj_chain_status;
ALTER TABLE proj_chain_status_new RENAME TO proj_chain_status;
CREATE INDEX proj_chain_status_status_idx     ON proj_chain_status (status);
CREATE INDEX proj_chain_status_updated_at_idx ON proj_chain_status (updated_at DESC);
