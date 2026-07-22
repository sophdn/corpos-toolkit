-- Add project_id scoping to roadmap_items so identical slugs across
-- projects don't collide on UNIQUE(ref_kind, ref_slug). Every other
-- work-surface table in the canonical schema (chains, tasks, bugs,
-- library_entries, kiwix_references, setup_recipes, remote_ops) carries
-- project_id and namespaces uniqueness per project; roadmap was the
-- odd one out.
--
-- Strategy: rebuild the table with the new shape and copy rows back
-- with project_id resolved via JOIN. Roadmap entries whose target
-- chain/task no longer exists (orphans from earlier-era cleanup gaps)
-- are dropped on the way through — they couldn't be backfilled and
-- already produced a NULL status in list().

CREATE TABLE IF NOT EXISTS roadmap_items_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  TEXT    NOT NULL REFERENCES projects(id),
    position    INTEGER NOT NULL,
    ref_kind    TEXT    NOT NULL CHECK (ref_kind IN ('chain', 'task')),
    ref_slug    TEXT    NOT NULL,
    chain_slug  TEXT,
    note        TEXT,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, ref_kind, ref_slug)
);

INSERT OR IGNORE INTO roadmap_items_new
    (id, project_id, position, ref_kind, ref_slug, chain_slug, note, created_at, updated_at)
SELECT r.id, c.project_id, r.position, r.ref_kind, r.ref_slug, r.chain_slug, r.note, r.created_at, r.updated_at
FROM roadmap_items r
JOIN chains c ON r.ref_kind = 'chain' AND r.ref_slug = c.slug;

INSERT OR IGNORE INTO roadmap_items_new
    (id, project_id, position, ref_kind, ref_slug, chain_slug, note, created_at, updated_at)
SELECT r.id, c.project_id, r.position, r.ref_kind, r.ref_slug, r.chain_slug, r.note, r.created_at, r.updated_at
FROM roadmap_items r
JOIN tasks t ON r.ref_kind = 'task' AND r.ref_slug = t.slug
JOIN chains c ON t.chain_id = c.id;

DROP TABLE roadmap_items;

ALTER TABLE roadmap_items_new RENAME TO roadmap_items;

CREATE INDEX IF NOT EXISTS idx_roadmap_items_position ON roadmap_items (position);
