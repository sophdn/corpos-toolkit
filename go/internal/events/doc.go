// Package events is the append-only typed event ledger that records every
// state mutation in the toolkit-server.
//
// ## Intended use
//
// **Workflow served:** every state mutation emits a typed event with
// actor, span_id, and rationale; the events table is the source of truth
// from which projections rebuild and from which the agent reconstructs
// project history. Event types are closed — emitting an unknown type or
// a payload that fails its JSON Schema is a hard error at write time.
//
// **Invocation pattern:** inside a write transaction:
//
//	id, err := events.Emit(ctx, tx, events.EmitArgs{
//	    Entity:  events.NewEntityRef("bug", slug, project),
//	    Payload: events.BugResolvedPayload{Kind: kind, CommitSHA: &sha},
//	})
//
// Dual-write order: CRUD row update first, `Emit` second; both succeed
// or the surrounding `pool.WithWrite` rolls back together.
//
// **Success shape:** an INSERT into `events` returns a monotonic UUIDv7
// `event_id`; JSON Schema validation against `_envelope.json` plus the
// per-type schema runs at write time. Append-only enforcement: BEFORE
// UPDATE and BEFORE DELETE triggers raise ABORT.
//
// **Non-goals:** not a message bus (see internal/eventbus for live SSE
// broadcast), not async — `Emit` is synchronous and transactional,
// payload shapes are closed (new types require a PR adding the JSON
// schema + Go struct + catalog entry), does not generate spans
// (internal/obs does that — events carry the existing span_id from ctx).
//
// ## Invariant: ts is the chronological authority; event_id is an identifier
//
// Every query that needs chronological order MUST order on `(ts, event_id)`
// — `ts` is the primary sort, `event_id` only the tiebreaker for same-tx
// emits that share a wall-clock. `ORDER BY event_id` ALONE is a bug, even
// though it looks like it ought to work for ULID/UUIDv7 event_ids.
//
// Two reasons:
//
//  1. Not every event_id is ULID-shaped. Backfill / migration / external-
//     source programs author event_ids in non-chronological shapes —
//     `started-<uuid>` and `completed-<uuid>` from the BenchmarkRun
//     backfill are the standing example (1,432 rows on the live DB as
//     of 2026-05-23). These lex-sort AFTER every ULID (`0xxx…` < `s…` /
//     `c…`), so an `ORDER BY event_id DESC` listing surfaces them at
//     the top instead of the actually-newest events. The audit-ledger
//     freeze of 2026-05-23 was this exact bug; see fix `audit-ledger-
//     orders-by-event-id-buries-real-events-behind-synthetic-backfill`.
//
//  2. Even pure ULIDs aren't reliably chronological inside a single
//     millisecond — the low bits are random. So `ORDER BY event_id`
//     produces wobble for same-ms emits even in a synthetic-id-free
//     codebase. `ts` is the only stable chronological key.
//
// **The contract**: event authors can mint event_ids in any shape that
// satisfies the DB constraints (currently a 36-char UUID-shape for
// canonical Emit; synthetic prefixes for backfill programs). Consumers
// MUST NOT depend on event_id ordering for chronology. The cost is a
// `(ts, event_id)` composite index instead of a single-column event_id
// index for chronological queries — cheap.
//
// **The lint**: `scripts/precommit.sh` greps production Go for
// `ORDER BY event_id` and rejects every hit. Use `(ts ASC|DESC,
// event_id ASC|DESC)` instead; the canonical pattern is documented at
// the lint-stage's error message.
//
// **Point lookups are fine**: `WHERE event_id = ?` is the correct shape
// for fetching a specific event by id — that's identifier semantics,
// not chronology, and the lint doesn't fire on it.
package events
