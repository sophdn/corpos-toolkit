-- Roadmap reassessment flow: a flat ordered backlog of pending/active
-- chains and tasks the user works through sequentially. Edited via the
-- chain-close-reassessment skill, never through the dashboard. See
-- the roadmap-reassessment-flow chain in seed-packet for design.

CREATE TABLE IF NOT EXISTS roadmap_items (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    position    INTEGER NOT NULL,
    ref_kind    TEXT    NOT NULL CHECK (ref_kind IN ('chain', 'task')),
    ref_slug    TEXT    NOT NULL,
    chain_slug  TEXT,
    note        TEXT,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (ref_kind, ref_slug)
);

CREATE INDEX IF NOT EXISTS idx_roadmap_items_position ON roadmap_items (position);

-- Single-row signal table. No kv_config existed when this migration
-- landed, so a tiny dedicated meta table is the right shape per the
-- chain's design_decisions block.
CREATE TABLE IF NOT EXISTS roadmap_meta (
    key         TEXT    PRIMARY KEY,
    value       TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- Seed last_reassessed_at to the epoch so the first roadmap_diff after
-- the migration surfaces every pending/active row created since.
INSERT OR IGNORE INTO roadmap_meta (key, value, updated_at)
    VALUES ('last_reassessed_at', '1970-01-01 00:00:00', datetime('now'))
