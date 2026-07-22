// Package grounding parses Claude Code session JSONL transcripts and records the
// search-grounding telemetry they imply: one grounding_events row per
// vault_search / kiwix_search / knowledge_search call, the click_kind interactions
// it earned (followed / cited / mentioned), and the terminal query_resolutions a
// later bug/task/chain write resolves back to its feeding searches. Extracted from
// the cmd/grounding-events-processor binary (2026-06) so the SAME parse+emit
// implementation backs both the host binary and the container's ingest_grounding
// action — no cross-process drift. Sibling to internal/telemetry (the emit
// primitives) and internal/db (the grounding_events row writer).
//
// ## Intended use
//
// Workflow served: the user-level Stop hook
// (~/.claude/hooks/grounding-events-processor.sh) fires the binary on session-end.
// Post-cutover the binary PARSES the transcript host-side (Parse) and POSTs the
// parsed structures to the container's ingest_grounding action, which runs the emit
// inside the single-writer container — so no host process opens the canonical DB
// (the cross-mount-namespace WAL invariant). A --db fallback runs parse+emit in one
// process, valid only as a single writer (container-down one-shot / historical
// backfill).
//
// Invocation pattern:
//
//	// host side (binary): parse only, no DB
//	events, entries, err := grounding.Parse(transcriptPath)
//	// container side (ingest_grounding action): emit inside the writer tx
//	r, err := grounding.HandleIngest(ctx, pool, project, paramsJSON)
//	// single-writer fallback: parse + emit in one process
//	r, err := grounding.ProcessFile(ctx, pool, path, projectID, parentSpanID, false)
//
// Success shape: Parse returns the parsed ProcessedEvents + raw jsonlEntries.
// ProcessParsed / ProcessFile / HandleIngest return a Result with per-file counts
// (events / gaps / interactions / resolutions); the rows commit when the enclosing
// WithWrite closure commits. The pass is idempotent — re-running over the same
// transcript hits ON CONFLICT DO NOTHING on grounding_events, the UPSERT triple on
// query_interactions, and the already-resolved pre-check on query_resolutions.
//
// **Non-goals:** this package does not own the emit primitives (internal/telemetry
// does — EmitInteraction / EmitResolution) nor the projection fold (internal/
// projections, wired via telemetry.SetFoldHook by the server/binary at startup). It
// does not open or own the canonical DB (the caller supplies a *db.Pool / *sql.Tx),
// and it does not perform HTTP — the binary's CLI wrapper owns the --http-base POST.
package grounding
