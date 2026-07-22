-- Setup-recipe surface — parent + ordered children, mirroring chain → task.
--
-- See seed-packet `process-docs/adhoc/network-and-setup-recipes/
-- schema-design_2026-05-05.md` § 4 for design rationale. The two-schema
-- split was chosen over a pipe-delimited `steps` field on a single
-- recipe row because each step carries multi-line shell + four
-- properties (name, command, idempotency_check, on_failure) that
-- pipe-delimitation cannot accommodate.

CREATE TABLE IF NOT EXISTS setup_recipes (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id         TEXT NOT NULL REFERENCES projects(id),
    slug               TEXT NOT NULL,
    host_slug          TEXT NOT NULL REFERENCES hosts(slug),
    description        TEXT NOT NULL DEFAULT '',
    completion_check   TEXT NOT NULL DEFAULT '',          -- shell cmd; exit 0 = recipe achieves its end
    status             TEXT NOT NULL DEFAULT 'unapplied', -- unapplied | partial | applied | failed
    last_applied_at    TEXT,
    last_failure_step  TEXT,                              -- step slug on which the most recent run halted
    last_failure_note  TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_setup_recipes_project ON setup_recipes (project_id);
CREATE INDEX IF NOT EXISTS idx_setup_recipes_host    ON setup_recipes (host_slug);

CREATE TABLE IF NOT EXISTS recipe_steps (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    recipe_id          INTEGER NOT NULL REFERENCES setup_recipes(id) ON DELETE CASCADE,
    slug               TEXT NOT NULL,
    position           INTEGER NOT NULL,
    name               TEXT NOT NULL DEFAULT '',
    command            TEXT NOT NULL,
    idempotency_check  TEXT NOT NULL DEFAULT '',          -- empty = always run
    on_failure         TEXT NOT NULL DEFAULT 'halt',      -- halt | continue
    status             TEXT NOT NULL DEFAULT 'pending',   -- pending | applied | skipped | failed
    last_run_at        TEXT,
    UNIQUE (recipe_id, slug),
    UNIQUE (recipe_id, position)
);

CREATE INDEX IF NOT EXISTS idx_recipe_steps_recipe ON recipe_steps (recipe_id);
