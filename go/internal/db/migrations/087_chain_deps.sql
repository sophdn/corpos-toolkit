-- dependency-driven-roadmap chain T1 — chain-level dependency edges.
--
-- Each row is a directed edge: the chain identified by `dependent_chain_id`
-- depends on (must come after) the chain identified by
-- `prerequisite_chain_id`. The roadmap's order is COMPUTED from these
-- edges (a topological sort of open chains), so the sequence and its
-- rationale live in the data rather than being hand-set per prompt — see
-- roadmap_plan.
--
-- `reason` is freeform prose explaining WHY the dependency exists ("needs
-- the typed core before the HTTP layer"); it is the human-readable half of
-- the "why this order" answer, alongside the edge itself.
--
-- Direct-write table, NOT an event-sourced projection. This mirrors
-- roadmap_meta (the other roadmap-supporting metadata table, written
-- directly by roadmap_mark_reassessed) rather than the event-sourced
-- entity tables. Chain dependency edges are roadmap configuration, not
-- lifecycle history; they don't need a ledger replay, so chain_dep_add /
-- chain_dep_remove INSERT/DELETE here inside their WithWrite tx. The table
-- is excluded from the events-rebuild byte-identical test for the same
-- reason roadmap_meta is.
--
-- Cascade-on-delete on both sides: chains are not physically deleted today
-- (close is a status transition), but the cascade is cheap belt-and-
-- suspenders against future deletion semantics, matching task_blockers.
--
-- Project consistency (both endpoints in the same project) is enforced in
-- the chain_dep_add handler, not here — SQLite CHECK can't cross-reference
-- the chains rows, and the handler already resolves both slugs.

CREATE TABLE IF NOT EXISTS chain_deps (
    dependent_chain_id    INTEGER NOT NULL REFERENCES chains(id) ON DELETE CASCADE,
    prerequisite_chain_id INTEGER NOT NULL REFERENCES chains(id) ON DELETE CASCADE,
    reason                TEXT    NOT NULL DEFAULT '',
    created_at            TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (dependent_chain_id, prerequisite_chain_id),
    CHECK (dependent_chain_id != prerequisite_chain_id)
);

CREATE INDEX IF NOT EXISTS idx_chain_deps_dependent
    ON chain_deps(dependent_chain_id);

CREATE INDEX IF NOT EXISTS idx_chain_deps_prerequisite
    ON chain_deps(prerequisite_chain_id);
