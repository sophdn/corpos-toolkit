-- agent-suggestion-box chain T6 — suggestions table + bidirectional bug↔suggestion
-- routing + FTS5 backing for both bugs and suggestions.
--
-- See chain `agent-suggestion-box` design_decisions §1 (bidirectional routing)
-- and §2 (FTS5 standalone shadow tables modelled on knowledge_pointers_fts;
-- application code maintains in the same transaction as the parent write).
--
-- Three coordinated changes land in this migration:
--
--   1. CREATE TABLE suggestions — mirrors bugs SHAPE with native vocabulary
--      (`priority` not `severity`; resolution_kind values enforced at the
--      MCP action layer, not via CHECK). Carries routed_bug_slug for the
--      suggestion → bug direction of cross-table routing.
--
--   2. ALTER TABLE bugs ADD COLUMN routed_suggestion_slug — symmetric
--      bug → suggestion routing. Additive only; existing rows default to ''.
--
--   3. CREATE VIRTUAL TABLE bugs_fts / suggestions_fts USING fts5 —
--      standalone (no contentless coupling) so the migration runner doesn't
--      need to parse BEGIN…END trigger bodies. rowid maps to parent id.
--      bugs_fts is backfilled from existing rows here in the migration;
--      suggestions_fts starts empty (no source rows yet). Subsequent
--      writes are maintained by Go handler code in the parent tx.

CREATE TABLE IF NOT EXISTS suggestions (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id              TEXT    NOT NULL REFERENCES projects(id),
    slug                    TEXT    NOT NULL,
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
    filed_at                TEXT    NOT NULL DEFAULT (datetime('now')),
    resolved_at             TEXT,
    updated_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    tags                    TEXT    NOT NULL DEFAULT '',
    UNIQUE (project_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_suggestions_project  ON suggestions (project_id);
CREATE INDEX IF NOT EXISTS idx_suggestions_status   ON suggestions (status);
CREATE INDEX IF NOT EXISTS idx_suggestions_priority ON suggestions (priority);

-- Symmetric bug → suggestion routing column on the bugs table. Additive
-- ADD COLUMN with a default '' so existing rows pick up the same shape
-- as the existing routed_chain_slug / routed_task_slug columns.
ALTER TABLE bugs ADD COLUMN routed_suggestion_slug TEXT NOT NULL DEFAULT '';

-- Standalone FTS5 virtual tables for bugs and suggestions. Modelled on
-- knowledge_pointers_fts (migration 020): rowid = parent.id; no SQL
-- triggers; Go handler code keeps the index in sync inside the same
-- write transaction as the parent INSERT/UPDATE.
CREATE VIRTUAL TABLE bugs_fts        USING fts5(title, problem_statement);
CREATE VIRTUAL TABLE suggestions_fts USING fts5(title, problem_statement);

-- Backfill bugs_fts from the existing bugs rows. suggestions_fts starts
-- empty (no source rows yet — every future row will be inserted by the
-- createSuggestion path which writes both tables in the same tx).
INSERT INTO bugs_fts (rowid, title, problem_statement)
SELECT id, title, problem_statement FROM bugs;
