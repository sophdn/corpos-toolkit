// Package telemetry is the read-side query-lifecycle substrate for the
// toolkit-server unified knowledge surface. Sibling to internal/events
// (write-side audit ledger); the two substrates share span_id and
// JSON-array FK semantics. See docs/TELEMETRY_SUBSTRATE.md for the full
// design and docs/TELEMETRY_LABEL_SPIKE.md for the click_kind enum
// closure.
//
// ## Intended use
//
// Workflow served: an agent or hook detects one of the four click
// signals (followed / cited / mentioned / resolved-from) against a
// previously-emitted grounding_events row, OR a work-handler resolves
// a bug/task/chain whose trajectory needs to be linked back to its
// feeding searches. Each call emits one row — query_interactions for
// click signals, query_resolutions for terminal resolutions.
//
// Invocation pattern: inside a write transaction, the in-process emit
// path is:
//
//	id, err := telemetry.EmitInteraction(ctx, tx, telemetry.InteractionArgs{
//	    GroundingEventID: 42,
//	    SourceRef:        "vault/learnings/general/2026-05-17_x.md",
//	    ClickKind:        telemetry.ClickFollowed,
//	    SpanID:           span,
//	    SessionID:        session,
//	    DetectedAt:       time.Now().UTC().Format(time.RFC3339Nano),
//	})
//
//	resID, err := telemetry.EmitResolution(ctx, tx, telemetry.ResolutionArgs{
//	    EntityKind:     "bug",
//	    EntitySlug:     slug,
//	    OutcomeKind:    telemetry.OutcomeResolved,
//	    WriteEventIDs:  []string{bugResolvedEventID},
//	    ...
//	})
//
// Success shape: EmitInteraction returns the autoincrement id and nil;
// the row is committed when the enclosing WithWrite closure commits.
// EmitResolution returns a freshly-minted UUIDv7 resolution_id and nil;
// the cross-substrate FK check (write_event_ids → events.event_id) runs
// at INSERT time via a BEFORE trigger and bubbles up as an error if any
// referenced event_id is missing.
//
// Non-goals: this package does not detect click_kind signals — that's
// the Stop hook's job (~/.claude/hooks/grounding-events-processor.sh)
// extended in a separate follow-on commit, OR the in-process callers
// passing a pre-classified ClickKind to EmitInteraction. It does not
// run projections (internal/projections does that for TT3). It does
// not own JSON-array FK enforcement at the schema level — migration
// 037's BEFORE INSERT trigger does — but it pre-validates in Go to
// return a typed error instead of a raw SQLite ABORT.
package telemetry
