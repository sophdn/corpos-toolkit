// Package projections owns the materialized read-side views folded
// from the events log — proj_current_bugs, proj_chain_status,
// proj_roadmap_view, plus any future projections registered at init.
//
// ## Intended use
//
// **Workflow served:** the dashboard frontend reads from projection
// tables instead of the CRUD tables so the read path is rebuildable
// from the events log alone; every projection registers a Fold function
// the dispatch emit-hook calls inside the writing transaction.
//
// **Invocation pattern:** projections register at init via
// `projections.Register(currentBugs{})`; the dispatch emit hook calls
// `Fold(ctx, tx, event)` after `events.Emit` lands in the same write
// transaction; the CLI subcommand `toolkit-server rebuild-projections`
// drives incremental catch-up (using last_event_id watermark) or full
// rebuild from empty.
//
// **Success shape:** every projection table carries a `last_event_id`
// column; full rebuild from empty produces byte-identical state to
// incremental fold; during the dual-write phase projection rows match
// the latest CRUD-table state at the close of each write tx.
//
// **Non-goals:** does not own event emission (internal/events does that),
// does not gate writes (folds are best-effort within the write tx and
// roll back with it), does not replace the CRUD tables in this chain
// — frontend cutover and legacy retirement is the Phase 4 follow-on
// chain.
package projections
