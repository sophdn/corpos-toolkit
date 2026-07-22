-- Unified MCP Toolkit Schema
-- All tables carry project_id for cross-project operation.

-- Projects registry
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,          -- slug: "seed-packet", "dm-toolkit"
    name        TEXT NOT NULL,
    path        TEXT NOT NULL DEFAULT '',  -- filesystem path, informational
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ============================================================
-- WORK SURFACE
-- ============================================================

CREATE TABLE IF NOT EXISTS chains (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id              TEXT    NOT NULL REFERENCES projects(id),
    slug                    TEXT    NOT NULL,
    status                  TEXT    NOT NULL DEFAULT 'open',
    output                  TEXT    NOT NULL DEFAULT '',
    design_decisions        TEXT    NOT NULL DEFAULT '',
    completion_condition    TEXT    NOT NULL DEFAULT '',
    created_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, slug)
);

CREATE TABLE IF NOT EXISTS tasks (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    chain_id                INTEGER NOT NULL REFERENCES chains(id),
    slug                    TEXT    NOT NULL,
    position                INTEGER NOT NULL DEFAULT 0,
    status                  TEXT    NOT NULL DEFAULT 'pending',
    problem_statement       TEXT    NOT NULL DEFAULT '',
    acceptance_criteria     TEXT    NOT NULL DEFAULT '',
    context_required        TEXT    NOT NULL DEFAULT '',
    constraints             TEXT    NOT NULL DEFAULT '',
    handoff_output          TEXT    NOT NULL DEFAULT '',
    originated_chain_id     INTEGER REFERENCES chains(id),
    moved_on                TEXT,
    created_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (chain_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_tasks_chain_id ON tasks (chain_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status);
CREATE INDEX IF NOT EXISTS idx_tasks_chain_position ON tasks (chain_id, position);

CREATE TABLE IF NOT EXISTS task_dependencies (
    blocker_id  INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    blocked_id  INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    PRIMARY KEY (blocker_id, blocked_id)
);

CREATE INDEX IF NOT EXISTS idx_task_deps_blocked ON task_dependencies (blocked_id);

CREATE TABLE IF NOT EXISTS bugs (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id              TEXT    NOT NULL REFERENCES projects(id),
    slug                    TEXT    NOT NULL,
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
    filed_at                TEXT    NOT NULL DEFAULT (datetime('now')),
    resolved_at             TEXT,
    updated_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_bugs_project ON bugs (project_id);
CREATE INDEX IF NOT EXISTS idx_bugs_status ON bugs (status);
CREATE INDEX IF NOT EXISTS idx_bugs_severity ON bugs (severity);

-- ============================================================
-- MEASURE SURFACE
-- ============================================================

CREATE TABLE IF NOT EXISTS benchmark_results (
    id                      TEXT    PRIMARY KEY,
    project_id              TEXT    NOT NULL REFERENCES projects(id),
    scenario_id             TEXT    NOT NULL,
    tool_name               TEXT    NOT NULL,
    model_name              TEXT    NOT NULL,
    run_id                  TEXT,
    run_at                  INTEGER NOT NULL,
    wall_clock_ms           INTEGER NOT NULL,
    input_tokens            INTEGER,
    output_tokens           INTEGER,
    invoked_contextually    INTEGER NOT NULL DEFAULT 1,
    invocation_ok           INTEGER NOT NULL,
    args_match              INTEGER,
    extracted_args          TEXT,
    interpretation_ok       INTEGER,
    detected_tool           TEXT,
    notes                   TEXT
);

CREATE INDEX IF NOT EXISTS idx_bench_project ON benchmark_results (project_id);
CREATE INDEX IF NOT EXISTS idx_bench_tool ON benchmark_results (tool_name);
CREATE INDEX IF NOT EXISTS idx_bench_model ON benchmark_results (model_name);

CREATE TABLE IF NOT EXISTS session_journal (
    id                      TEXT    PRIMARY KEY,
    project_id              TEXT    NOT NULL REFERENCES projects(id),
    task_id                 TEXT,
    opened_at               TEXT    NOT NULL DEFAULT (datetime('now')),
    closed_at               TEXT,
    status                  TEXT    NOT NULL DEFAULT 'open',
    summary                 TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_session_project ON session_journal (project_id);

CREATE TABLE IF NOT EXISTS emotive_results (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id              TEXT    NOT NULL REFERENCES projects(id),
    battery_name            TEXT    NOT NULL,
    scenario_name           TEXT    NOT NULL,
    role                    TEXT    NOT NULL DEFAULT '',
    model_name              TEXT    NOT NULL,
    run_at                  TEXT    NOT NULL DEFAULT (datetime('now')),
    friction_count          INTEGER NOT NULL DEFAULT 0,
    flow_count              INTEGER NOT NULL DEFAULT 0,
    rederivation_count      INTEGER NOT NULL DEFAULT 0,
    raw_observations        TEXT    NOT NULL DEFAULT '[]',
    notes                   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_emotive_project ON emotive_results (project_id);
CREATE INDEX IF NOT EXISTS idx_emotive_battery ON emotive_results (battery_name);

-- ============================================================
-- KNOWLEDGE SURFACE
-- ============================================================

CREATE TABLE IF NOT EXISTS library_entries (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id              TEXT    NOT NULL REFERENCES projects(id),
    dewey                   TEXT    NOT NULL,
    primary_author          TEXT    NOT NULL DEFAULT '',
    year                    INTEGER,
    citation                TEXT    NOT NULL DEFAULT '',
    establishes             TEXT    NOT NULL DEFAULT '',
    what_it_answers         TEXT    NOT NULL DEFAULT '',
    invoke_when             TEXT    NOT NULL DEFAULT '',
    tags                    TEXT    NOT NULL DEFAULT '',
    status                  TEXT    NOT NULL DEFAULT 'active',
    index_pointers          TEXT    NOT NULL DEFAULT '[]',
    created_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, dewey)
);

CREATE INDEX IF NOT EXISTS idx_library_project ON library_entries (project_id);
CREATE INDEX IF NOT EXISTS idx_library_dewey ON library_entries (dewey);
CREATE INDEX IF NOT EXISTS idx_library_status ON library_entries (status);

CREATE TABLE IF NOT EXISTS kiwix_references (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id              TEXT    NOT NULL REFERENCES projects(id),
    zim_id                  TEXT    NOT NULL,
    article_slug            TEXT    NOT NULL,
    snapshot_id             TEXT    NOT NULL DEFAULT '',
    why                     TEXT    NOT NULL DEFAULT '',
    tags                    TEXT    NOT NULL DEFAULT '',
    added_at                TEXT    NOT NULL DEFAULT (datetime('now')),
    retired_at              TEXT,
    UNIQUE (project_id, zim_id, article_slug)
);

CREATE INDEX IF NOT EXISTS idx_kiwix_project ON kiwix_references (project_id);
