-- knowledge_pointers: the unified retrieval index over all knowledge sources.
-- FTS5 is a standalone (content-less) virtual table; Rust layer syncs it on
-- write so split_sql_statements does not have to parse BEGIN...END trigger bodies.
--
-- The DROP statements guard against partial application from a prior failed
-- migration attempt — they are no-ops when the tables do not exist.

DROP TABLE IF EXISTS knowledge_pointers_fts;
DROP INDEX IF EXISTS idx_kp_project_status;
DROP INDEX IF EXISTS idx_kp_source_type;
DROP TABLE IF EXISTS knowledge_pointers;

CREATE TABLE knowledge_pointers (
    id                      INTEGER PRIMARY KEY,
    project_id              TEXT NOT NULL,
    source_type             TEXT NOT NULL,
    -- source_ref ENCODING (JOIN TRAP — see suggestion #18): `<scope>::<slug>`
    -- for DB entities (chain/bug/suggestion → `<project>::<slug>`; task →
    -- `<project>::<chain>::<slug>`) and a repo-relative DOC PATH for vault
    -- notes / retrospectives (e.g. `decisions/2026-..._slug.md`). Built by
    -- forge/indexsync.go. This does NOT share an encoding with
    -- grounding_events.source_refs (per-resolver `<type>:<rest>`) — a direct
    -- JOIN is a ~100% miss; normalize first. See
    -- vault/reference/2026-05-23_source-ref-encoding-divergence-grounding-events-vs-knowledge-pointers.md
    -- + bug `context-pulls-first-candidate-source-type-empty-due-to-source-ref-format-mismatch`.
    source_ref              TEXT NOT NULL,
    question                TEXT NOT NULL,
    invoke_when             TEXT NOT NULL,
    description             TEXT,
    tags                    TEXT NOT NULL DEFAULT '[]',
    quality_score           REAL,
    staleness_hint          TEXT,
    negative_feedback_count INTEGER NOT NULL DEFAULT 0,
    usage_count             INTEGER NOT NULL DEFAULT 0,
    last_used_at            TEXT,
    status                  TEXT NOT NULL DEFAULT 'active',
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, source_type, source_ref)
);

CREATE INDEX idx_kp_project_status ON knowledge_pointers (project_id, status);
CREATE INDEX idx_kp_source_type ON knowledge_pointers (source_type);

-- Standalone FTS5: rowid matches knowledge_pointers.id.
-- Synced by Rust on add/retire operations (no SQL triggers needed).
CREATE VIRTUAL TABLE knowledge_pointers_fts USING fts5(question, invoke_when);
