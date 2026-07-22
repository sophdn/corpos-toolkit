-- agent-substrate-crud-retirement chain T6 — drop the seven retired
-- artifact-lifecycle CRUD tables. Reads have all repointed to projections
-- (T4 + cleanups in this commit's predecessor); writes flipped to event-
-- only in T5's six sub-commits (c7c5d6e..7128e48); the fold constructs
-- projection rows from event payload alone for every retired entity kind.
-- This migration removes the dead-weight tables.
--
-- Drop order is FK-safe (see PRAGMA foreign_key_list audit in §13 of
-- docs/SUBSTRATE_CRUD_RETIREMENT.md):
--   task_dependencies  →  FK INTO tasks (dead table; zero prod refs)
--   task_blockers      →  FK INTO tasks
--   tasks              →  FK INTO chains
--   chains             →  no incoming FKs from retired tables
--   bugs, benchmark_results, roadmap_items, suggestions — no FKs FROM
--     other retired tables (all FKs point at projects / benchmark_provenance,
--     both out of scope).
--
-- FTS5 virtual tables (bugs_fts, suggestions_fts) are PRESERVED per design
-- doc §7: they are parent-driven from the projection now and the fold
-- modules (bugs.go, suggestions.go) maintain them on every event. Dropping
-- them would force a full rebuild from the projection; keeping them is the
-- accepted design.
--
-- benchmark_results_quarantine, benchmark_provenance, and all proj_* tables
-- are PRESERVED — they're either out-of-scope or the post-T6 source of truth.

DROP TABLE IF EXISTS task_dependencies;
DROP TABLE IF EXISTS task_blockers;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS chains;
DROP TABLE IF EXISTS bugs;
DROP TABLE IF EXISTS benchmark_results;
DROP TABLE IF EXISTS roadmap_items;
DROP TABLE IF EXISTS suggestions;
