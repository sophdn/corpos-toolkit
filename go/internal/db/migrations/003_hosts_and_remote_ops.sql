-- Transport surface — hosts registry + remote_ops audit.
--
-- See seed-packet `process-docs/adhoc/network-and-setup-recipes/
-- schema-design_2026-05-05.md` for the full design rationale. Two
-- decisions worth surfacing inline:
--
--   1. `hosts` deliberately does NOT carry `project_id`. Hosts are
--      shared infrastructure (one mini-PC serves recipes from
--      multiple projects); a per-project host registry would
--      fragment audit history when two projects target the same
--      machine. This is the only table in the unified DB that
--      breaks the project-id-on-everything convention.
--
--   2. `remote_ops` carries `project_id` (the audit row belongs to
--      whichever project's recipe / ad-hoc call produced it) and
--      `host_slug` (the target). `recipe_slug` and `step_slug` are
--      nullable — populated for recipe-driven calls, NULL for
--      ad-hoc remote_exec calls.

CREATE TABLE IF NOT EXISTS hosts (
    slug          TEXT PRIMARY KEY,                    -- 'mini-pc', 'workshop-rpi'
    addr          TEXT NOT NULL,                       -- IP or hostname
    ssh_user      TEXT NOT NULL,
    ssh_port      INTEGER NOT NULL DEFAULT 22,
    ssh_key_path  TEXT,                                -- e.g. '~/.ssh/id_ed25519'; NULL = agent-only
    notes         TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    retired_at    TEXT                                 -- soft-delete; preserves remote_ops FK history
);

CREATE INDEX IF NOT EXISTS idx_hosts_retired ON hosts (retired_at);

CREATE TABLE IF NOT EXISTS remote_ops (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id    TEXT NOT NULL REFERENCES projects(id),
    host_slug     TEXT NOT NULL REFERENCES hosts(slug),
    recipe_slug   TEXT,                                -- NULL = ad-hoc remote_exec call
    step_slug     TEXT,                                -- NULL when recipe_slug is NULL
    kind          TEXT NOT NULL DEFAULT 'command',     -- 'command' | 'idempotency_check' | 'completion_check'
    command       TEXT NOT NULL,
    stdout        TEXT NOT NULL DEFAULT '',
    stderr        TEXT NOT NULL DEFAULT '',
    exit_code     INTEGER,                             -- NULL = command never returned (timeout, transport error)
    duration_ms   INTEGER,
    started_at    TEXT NOT NULL DEFAULT (datetime('now')),
    finished_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_remote_ops_project ON remote_ops (project_id);
CREATE INDEX IF NOT EXISTS idx_remote_ops_host    ON remote_ops (host_slug);
CREATE INDEX IF NOT EXISTS idx_remote_ops_recipe  ON remote_ops (recipe_slug, step_slug);
CREATE INDEX IF NOT EXISTS idx_remote_ops_started ON remote_ops (started_at);
