# Arc-Close Filing Review — Substrate Listener Wiring (design)

**Chain:** `arc-close-filing-review-substrate-listener-wiring` (id 614, mcp-servers, opened 2026-05-20)
**Parent chain:** `arc-close-filing-review` (closed 2026-05-19) — see [`ARC_CLOSE_FILING_REVIEW.md`](ARC_CLOSE_FILING_REVIEW.md) for the full architecture and [`ARC_CLOSE_FILING_REVIEW_RETROSPECTIVE_2026-05-19.md`](ARC_CLOSE_FILING_REVIEW_RETROSPECTIVE_2026-05-19.md) for the closeout
**Status:** T1 spec — implementation tasks T2-T10 land against this doc; if any downstream task discovers the spec is wrong, that task reopens T1 and patches before resuming

## Mission

Substrate-side trigger listener fires real reviews on `TaskCompleted` /
`BugResolved` / `ChainClosed` (and, once their emitters land,
`CommitLanded` / `RoadmapUpdated`) instead of the v1 no-op
`LogOnlyTriggerObserver`. Cross-fire dispatch reaches a live agent via a
pending-decisions queue picked up by the next Stop hook fire. Two ML
follow-on chains forge once the corpus crosses training threshold.

This doc covers the two new pieces introduced by this chain:

1. **`session_registry`** — SQLite table populated by the Stop hook on
   every fire so the substrate-side listener can resolve "which
   session's transcript do I review when a project-scoped substrate
   event lands?"
2. **`pending_decisions`** — queue persisted between the substrate-side
   listener fire (no live agent) and the next Stop event (live agent
   reads + dispatches).

Together they bridge the asymmetry the parent chain deferred: the
substrate sees events from any source, but only an in-band agent can
execute forges + emit system-reminders. The bridge is a small SQLite
table + a small claim-and-dispatch query.

## Why these two pieces and not three

Two alternatives surfaced during T1 exploration:

- **Direct substrate-side forge execution.** The listener fires the
  review then writes forges directly through the existing
  `work.forge` handler, bypassing the agent. Rejected: parent chain's
  §Filing-dispatch decision (Q5) explicitly keeps decisions
  user-visible — landing forges silently is worse than landing them on
  the next turn. The "agent dispatches the forges" model the parent
  chain locked stays.
- **Reuse `ArcCloseFilingReviewed` event payload as the queue.** A
  query over `events WHERE type='ArcCloseFilingReviewed' AND
  NOT EXISTS (events WHERE type='FilingDecisionsDispatched' ...)`
  could substitute for a `pending_decisions` table. Rejected: the
  events table is append-only and the dispatch operation is mutation
  (which row got claimed by which session at what time). Encoding
  mutable state via correlation events couples consumer state to
  producer state awkwardly — see §Q2 below for the full comparison.

The chosen two-piece design keeps the events ledger as the *training
corpus* (write-once, read-many, immutable per event_substrate
discipline) and the new `pending_decisions` table as the *dispatch
queue* (mutable, has lifecycle, gets evicted). Different lifecycles,
different lifetimes, different tables.

## Sequence diagram

```
  T+0      [BugResolved emitted via bug_resolve handler]
              │   (sync inside the write tx via events.Emit)
              ▼
  T+0      [arcreview.InstallListenerFoldHook chained FoldHook]
              │   recognizes BugResolved as a trigger
              │   calls observer.Observe with SubstrateTriggerEvent
              ▼
  T+0      [observer.Observe — NEW: SubstrateReviewObserver]
              │   kicks goroutine (non-blocking; tx exits cleanly)
              ▼
  T+small  [goroutine: SELECT session_id, transcript_path FROM
              session_registry WHERE project_id = ? ORDER BY
              last_active_at DESC LIMIT 1]
              │
              ├─── no row → drift-log + return (no session active)
              ▼
  T+small  [goroutine: HandleReviewArcForFiling with that
              session_id + transcript_path; debouncer check;
              snapshot; two Qwen calls; partition decisions]
              │
              ├─── status=fired → continue
              ├─── status=debounced → drift-log + return
              ├─── status=skipped → drift-log + return
              ├─── status=qwen_unreachable → drift-log + return
              ▼
  T+1-2s   [goroutine: INSERT INTO pending_decisions(event_id,
              project_id, target_session_id, decisions_json,
              triggers_json, arc_summary)]
              │
              ▼
            [ArcCloseFilingReviewed event emits as part of normal
              handler flow — corpus row lands]

  ... session continues, possibly across multiple Stop events ...

  T+next   [Claude Code Stop event fires for ANY session in the
  Stop      same project (typically but not necessarily the same
              session that produced the original substrate event)]
              │
              ▼
            [arc-close-filing-review-hook.sh:
                first action — UPSERT session_registry
                              (session_id, project_id, transcript_path, now())
                second action — pipe through arc-close-detector.sh as today
                                (counter + user-shape; harness-side path)
                third action — POST work.pending_decisions_claim
                                (project, limit)
                            ← returns claimed rows + marks them
                              dispatched in one tx]
              │
              ▼
            [hook formats claimed rows into a single
              <system-reminder> block on stdout — same format as
              partition.surface_for_confirm today, with an
              "Auto-execute decisions in this block ..." section
              for high-confidence rows and a "Confirm before
              executing ..." section for medium-confidence rows]
              │
              ▼
            [next user turn: agent sees the reminder, dispatches
              forges or asks before applying skill_update
              decisions, etc.]
```

The substrate-side fire and the harness-side dispatch are temporally
decoupled. The substrate event is recorded in the corpus
(`ArcCloseFilingReviewed` row) at fire time; the user-visible decisions
surface at the next Stop event. Bounded staleness = the time until any
Stop event fires for that project (typically seconds to minutes; rarely
longer).

## Q1 — `session_registry` schema, indexes, eviction

### Column shape

```sql
CREATE TABLE session_registry (
    session_id      TEXT PRIMARY KEY,
    project_id      TEXT,
    transcript_path TEXT NOT NULL,
    last_active_at  TEXT NOT NULL,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
```

- `session_id`: the Claude Code session UUID (the same value the Stop
  event passes in `.session_id`). Primary key — one row per session,
  not one per turn.
- `project_id`: nullable. Some sessions don't belong to a single
  project (e.g. cross-repo work, dotfile sessions). The substrate
  listener filters by project_id, so NULL rows are effectively
  unreachable from the project-scoped query, which is correct.
- `transcript_path`: the canonical JSONL path the Stop event passes in
  `.transcript_path`. Load-bearing — the substrate listener feeds this
  directly into `arcreview.ExtractSnapshot`.
- `last_active_at`: ISO-8601 of the most recent Stop event seen for
  this session. UPSERT replaces.
- `updated_at`: DB-side `datetime('now')`. Distinct from
  `last_active_at` to capture clock skew between Stop event timestamp
  and DB write time, which is small in practice but useful for audit.

### Indexes

```sql
CREATE INDEX session_registry_project_active_idx
    ON session_registry (project_id, last_active_at DESC);
```

The hot lookup is "most-recently-active session in project X." A
single composite index serves it; the DESC ordering means SQLite
walks the index in the desired direction without a sort step.

> **Best-effort attribution (bug 947, decided option B).** "Most-recently-active
> session in project X" is a *heuristic* for "which session produced this
> project-level trigger" — and under concurrent same-project sessions
> (multi-agent) it can be wrong: session B's commit can trigger a review of
> session A's transcript when A was active more recently. This is **accepted as
> deliberate best-effort**, not a defect to chase, because the read-side delivery
> scoping (bug 945 — `claimPendingDecisions` filters on `target_session_id`, see
> §Q-claim) guarantees a session only ever *receives* decisions targeted at it. So
> the worst residual cost is a mis-timed review of an *active* session — never the
> cross-session content bleed 945 closed. Precise per-trigger attribution is
> infeasible for the trigger that caused the 945 incident: `CommitLanded` is
> emitted by the post-commit hook (`cmd/commit-landed-emit`), which has no
> Claude-session identity (`CommitLandedPayload` carries no `session_id`). The
> MCP-emitted triggers (`BugResolved`/`TaskCompleted`/`ChainClosed`/
> `RoadmapUpdated`) *could* stamp an originating session_id from
> `MCPSessionIDFromContext` — the reopening condition if precise attribution is
> ever needed; `CommitLanded` would stay best-effort. See
> `observer.go::lookupActiveSession`.

### Eviction policy

Periodic sweep removes rows older than 7 days (the same retention
constant the harness-side counter files use; see
`hooks/arc-close-detector.sh::cleanup_old_counters`). The sweep runs:

- On `toolkit-server` startup (single DELETE statement before the HTTP
  server starts accepting connections).
- Best-effort, in a separate goroutine that runs every 6 hours during
  daemon lifetime.

Sweep query:

```sql
DELETE FROM session_registry
WHERE last_active_at < datetime('now', '-7 days');
```

Rows are also implicitly invalidated when a transcript file moves or
is deleted (the listener's `ExtractSnapshot` call returns an error;
the listener drift-logs and skips). Deleting the row itself is
optional cleanup; the harm of a stale row is one wasted lookup per
substrate event for that project, which is bounded.

### Race semantics

UPSERT on every Stop fire from the hook (`ON CONFLICT(session_id) DO
UPDATE SET ...`). SQLite's single-writer lock serializes — no race
between two Stop events in the same session, and no race between the
hook's UPSERT and the listener's SELECT (which uses a read-only
snapshot). If the same session UPSERTs twice in close succession (e.g.
counter-triggered + user-shape-triggered Stops both update once), the
last write wins; both writes carry the same transcript_path, so no
semantic loss.

## Q2 — `pending_decisions` storage: dedicated table or events query?

**Decision: dedicated table.** Lock for downstream tasks.

### Comparison

| Property | Dedicated table | Query over events |
|---|---|---|
| Schema cost | +1 table, +1 migration | 0 |
| Dispatched-marker | `UPDATE pending_decisions SET dispatched_at = ...` | requires emitting a second event (e.g. `FilingDecisionsDispatched`) referring back to the original `event_id` |
| Claim atomicity | one tx, `UPDATE ... WHERE dispatched_at IS NULL ... RETURNING *` | two writes — read events + emit dispatched event — with a window between |
| Eviction | DELETE old dispatched rows | events are append-only and never deleted; the "undispatched" query has to scan further and further into history |
| Telemetry | join `events ON pending_decisions.event_id = events.event_id` for the full picture | already in events but the projection has to fold two event types |
| Conceptual fit | dispatch queue = operational mutable state | events = immutable corpus row |

The dispatch queue is operational state with a clear lifecycle (born
at substrate-fire, dies at agent-dispatch or eviction). The events
ledger is corpus (born at substrate-fire, never modified). Mixing them
muddies both surfaces.

The dedicated table is one migration and ~50 lines of Go. The events
substitute is structurally awkward for a mutation pattern. We pay the
schema cost.

### Column shape

```sql
CREATE TABLE pending_decisions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id            TEXT NOT NULL,
    project_id          TEXT,
    target_session_id   TEXT NOT NULL,
    decisions_json      TEXT NOT NULL,
    triggers_json       TEXT NOT NULL,
    arc_summary         TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    dispatched_at       TEXT,
    dispatch_session_id TEXT,
    dispatch_error      TEXT
);
```

- `id`: AUTOINCREMENT surrogate. The dispatch-claim query uses it for
  stable ordering when `created_at` ties (rare but possible at sub-
  second granularity).
- `event_id`: the `events.event_id` of the `ArcCloseFilingReviewed` row
  that produced these decisions. Load-bearing for the audit join
  (T8). Not declared as a FOREIGN KEY because the events table is
  append-only and we don't want a row in `pending_decisions` to block
  any operation on events (defensive — events writes don't delete, so
  the FK would never actually fire, but explicit absence beats
  implicit safety in this codebase).
- `project_id`: filter axis for the claim query. Nullable because the
  upstream event may carry a NULL project_id (cross-cutting entities;
  see `events.NewCrossCuttingEntityRef`).
- `target_session_id`: the session_registry hit at substrate-fire
  time. NOT the dispatching session — that lands in
  `dispatch_session_id`. They may differ when the dispatching session
  is a sibling session in the same project (e.g. user opens a second
  Claude Code window).
- `decisions_json`: the full `[]FilingDecision` slice serialized as a
  JSON array. Includes the `payload`, `confidence`, `reasoning` for
  each decision — same shape as the action handler returns.
- `triggers_json`: the triggers array, also from the action handler
  payload. Used to populate the system-reminder block's triggers
  preamble.
- `arc_summary`: optional. Populated when the Qwen pre-call returned a
  non-empty summary. Used for the reminder body's context line.
- `created_at`: DB-side `datetime('now')` at INSERT.
- `dispatched_at`: NULL until a Stop hook claims the row. Set on
  successful claim, in the same UPDATE that returns the row.
- `dispatch_session_id`: the session_id that successfully claimed the
  row. NULL until claim. NOT the same as `target_session_id` when a
  sibling session claims first.
- `dispatch_error`: free-text. Populated only if the hook successfully
  claimed but then failed to emit the reminder (e.g. stdout pipe
  closed unexpectedly). T8's audit query surfaces these so they can be
  re-dispatched manually if needed.

### Indexes

```sql
CREATE INDEX pending_decisions_undispatched_idx
    ON pending_decisions (project_id, created_at)
    WHERE dispatched_at IS NULL;

CREATE INDEX pending_decisions_created_at_idx
    ON pending_decisions (created_at);
```

The partial index over `dispatched_at IS NULL` is the hot path: the
claim query is `SELECT ... WHERE project_id = ? AND dispatched_at IS
NULL ORDER BY created_at, id LIMIT ?`. The partial index stays small
even after many dispatched rows accumulate.

The full `created_at` index serves the eviction sweep and the T8 audit
join.

### Eviction policy

Rows where `dispatched_at IS NOT NULL` and `created_at < now - 30
days` get DELETEd by the same sweep goroutine that handles
`session_registry`. The 30-day window is wider than `session_registry`
because the audit surface (T8) reads dispatched rows and the wider
window keeps recent dispatch history queryable. Undispatched rows
older than 7 days are also DELETEd as garbage — a row that has sat
undispatched for 7 days has lost its dispatch window (the project's
Stops would have picked it up by then if a session was still active).

```sql
DELETE FROM pending_decisions
WHERE dispatched_at IS NOT NULL
  AND created_at < datetime('now', '-30 days');

DELETE FROM pending_decisions
WHERE dispatched_at IS NULL
  AND created_at < datetime('now', '-7 days');
```

## Q3 — Read semantics: claim, dispatch, mark

The Stop hook calls a new MCP action `work.pending_decisions_claim`.
Single-tx semantics:

```sql
BEGIN IMMEDIATE;

-- 1. Find candidates for this project, oldest-first.
SELECT id, event_id, target_session_id, decisions_json, triggers_json, arc_summary
FROM pending_decisions
WHERE project_id = :project
  AND dispatched_at IS NULL
ORDER BY created_at, id
LIMIT :limit;

-- 2. Mark them dispatched in the same tx.
UPDATE pending_decisions
SET dispatched_at = datetime('now'),
    dispatch_session_id = :session_id
WHERE id IN (:ids);

COMMIT;
```

The Go handler does the SELECT, then the UPDATE bound to the returned
IDs, then COMMIT — all under one `pool.WithWrite` call. The IMMEDIATE
mode grabs the writer lock at BEGIN so concurrent claims serialize.

The action signature:

```go
type PendingDecisionsClaimParams struct {
    SessionID string `json:"session_id"`
    Limit     int    `json:"limit"` // default 10 if zero or unset
}

type PendingDecisionsClaimResult struct {
    Claimed []PendingDecisionsRow `json:"claimed"`
}

type PendingDecisionsRow struct {
    ID                int              `json:"id"`
    EventID           string           `json:"event_id"`
    TargetSessionID   string           `json:"target_session_id"`
    Decisions         []FilingDecision `json:"decisions"`
    Triggers          []string         `json:"triggers"`
    ArcSummary        string           `json:"arc_summary,omitempty"`
    CreatedAt         string           `json:"created_at"`
}
```

The hook then formats `Claimed` into a system-reminder text block
(format mirrors today's `surface_for_confirm` block at
`hooks/arc-close-filing-review-hook.sh:262-278` with an extra preamble
naming the originating event_id for traceability) and writes it to
stdout.

If `dispatch_error` is set on a row during the dispatch (e.g. JSON
encoding failure), the hook fires a follow-up action
`work.pending_decisions_mark_error(id, error)` to record the error
without un-marking the row. The audit surface (T8) surfaces these for
manual re-dispatch.

## Q4 — Race between simultaneous Stops

Two scenarios:

### Same-project sibling sessions

User has two Claude Code sessions open in the same project. Both Stop
events fire within milliseconds. Both hooks call
`pending_decisions_claim(project, limit=N)`. SQLite's single-writer
lock serializes the two BEGIN IMMEDIATE statements; whichever wins
grabs all N (or fewer, if there are fewer rows). The loser sees zero
rows.

This is correct semantics: every pending row gets dispatched exactly
once. The user-visible effect is that the reminder lands in whichever
session's transcript happens to win the race. Both sessions are
already in the same project context; the user sees the decisions in
one of them, not both, which avoids the duplicate-reminder failure
mode.

### Same-session double Stop

A single session's Stop event fires twice within the same second
(detector counter + user-shape both triggered at once). Both hook
invocations call `pending_decisions_claim`. The first claims all
available rows; the second sees zero. This is also correct: the
reminder lands once in the session's transcript, not twice.

### Substrate-fire racing Stop-claim

The substrate listener INSERTs into `pending_decisions` at T+1.5s.
A Stop event fires at T+1.0s (before the INSERT lands) — the claim
sees zero rows and exits silently. A subsequent Stop at T+5s sees the
row and dispatches it. Bounded staleness = "next Stop event," which
this design already commits to.

## Q5 — Fail-open behavior

| Failure | Symptom | Recovery |
|---|---|---|
| Substrate listener crashes mid-review | Goroutine dies before INSERT; no pending row, no `ArcCloseFilingReviewed` event | Daemon restart re-folds events but does not re-fire missed reviews (per parent chain §Failure-modes "Event-listener crash" row). The event the listener was reviewing already committed; only the substrate-side review is lost. The next trigger event re-engages the listener. |
| Substrate listener INSERTs but the goroutine dies before COMMIT | Tx rolls back automatically; no row lands | Same as above — review is lost, next trigger re-engages. |
| Stop hook calls `pending_decisions_claim` but MCP unreachable | Hook drift-logs and exits 0; rows stay undispatched | Next Stop fire in any session for that project claims the rows. Bounded staleness = "next Stop." |
| Claim succeeds, UPDATE marks dispatched, but the hook's stdout write fails | Decisions are marked dispatched but the agent never sees them; `dispatch_error` populated | T8 audit surface surfaces these rows; manual re-dispatch via `pending_decisions_redispatch(id)` (deferred to T8 implementation if needed; out of scope for this spec). |
| Eviction sweep deletes an undispatched row at the 7-day mark | The substrate-fired review never reaches an agent | Telemetry surfaces this as a counter (T8 reports "lost-to-eviction-count"). Acceptable: a 7-day-old undispatched row means no session has been active in that project for 7 days; the decisions are stale anyway. |
| `session_registry` empty at substrate-fire time | The listener's lookup returns no row; review never fires | Drift-log and return. The event still committed via the normal handler flow; only the substrate-side review is skipped. |
| `transcript_path` from `session_registry` doesn't exist on disk | `ExtractSnapshot` returns error; listener drift-logs and skips | The session_registry row is stale (transcript was moved or deleted). Eviction will clean it up. |

No failure mode regresses the current discipline. Path A (Stop hook
counter + user-shape detection on a single session) keeps working
underneath; Path B is purely additive.

## Q6 — Telemetry fields per dispatch

The corpus row (`ArcCloseFilingReviewed` event) already captures
per-fire fields per parent chain §Telemetry. The dispatch metadata
joins from `pending_decisions`:

| Field | Source | Use |
|---|---|---|
| `dispatched_at` | `pending_decisions.dispatched_at` | latency from substrate-fire to user-surface |
| `dispatch_session_id` | `pending_decisions.dispatch_session_id` | identify cross-session dispatches |
| `dispatch_session_id != target_session_id` | join | rate of cross-session dispatches |
| `dispatch_error` | `pending_decisions.dispatch_error` | failure-mode rate |
| `user_corrections` | events ledger time-window join (T8 design) | precision of dispatched decisions |

T8 (`arc-review-audit-read-side-action`) builds the read action that
joins these surfaces.

## Migration 049 — locked column list

Migration file: `crates/shared-db/migrations/049_session_registry_and_pending_decisions.sql`
(plus mirror copies in `go/internal/db/migrations/` and
`go/internal/testutil/migrations/` per `CONVENTIONS.md` §Migration
runner ownership).

```sql
-- arc-close-filing-review-substrate-listener-wiring T2: two tables.
-- session_registry bridges harness state to substrate state (the
-- listener resolves "which transcript to review" from this table).
-- pending_decisions queues decisions from substrate-side fires to the
-- next Stop event that can dispatch them. See
-- docs/ARC_CLOSE_FILING_REVIEW_SUBSTRATE_LISTENER.md §Q1, §Q2.

CREATE TABLE session_registry (
    session_id      TEXT PRIMARY KEY,
    project_id      TEXT,
    transcript_path TEXT NOT NULL,
    last_active_at  TEXT NOT NULL,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX session_registry_project_active_idx
    ON session_registry (project_id, last_active_at DESC);

CREATE TABLE pending_decisions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id            TEXT NOT NULL,
    project_id          TEXT,
    target_session_id   TEXT NOT NULL,
    decisions_json      TEXT NOT NULL,
    triggers_json       TEXT NOT NULL,
    arc_summary         TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    dispatched_at       TEXT,
    dispatch_session_id TEXT,
    dispatch_error      TEXT
);

CREATE INDEX pending_decisions_undispatched_idx
    ON pending_decisions (project_id, created_at)
    WHERE dispatched_at IS NULL;

CREATE INDEX pending_decisions_created_at_idx
    ON pending_decisions (created_at);
```

## Section anchors — downstream task references

Each downstream task in this chain anchors back to a section of this
doc. The references are stable and reopen-T1 obligations:

| Task | Slug | Anchored at |
|---|---|---|
| T2 | `migration-049-session-registry-table` | §Q1 column shape + §Migration 049 locked column list |
| T3 | `stop-hook-writes-session-registry` | §Q1 race semantics |
| T4 | `real-trigger-observer-replaces-no-op` | §Sequence diagram (the goroutine path) + §Q5 fail-open behavior |
| T5 | `pending-decisions-dispatch-queue` | §Q2 column shape + §Q3 read semantics + §Q4 race |
| T6 | `commit-landed-event-emitter` | §Mission (the event-type set the listener subscribes to) |
| T7 | `roadmap-updated-event-emitter` | same as T6 |
| T8 | `arc-review-audit-read-side-action` | §Q6 telemetry fields per dispatch |
| T9 | `threshold-and-qwen-prompt-v2-tuning` | parent chain §Thresholds — gate on ≥15 fires per chain `completion_condition` (g) |
| T10 | `forge-ml-follow-on-chains` | chain `completion_condition` (h); both chain slugs are pre-named |

## Non-goals (this chain's design)

- Not re-architecting Path A. The bash detector (`arc-close-detector.sh`) stays narrow.
- Not implementing the ML classifiers themselves — those are their own forged chains (T10).
- Not changing the existing Qwen prompt mid-chain. T9 is the tuning surface, gated on real-fire volume.
- Not introducing a third trigger path. Two paths (harness Stop + substrate event) cover the design space per parent chain §Architecture.
- Not adding `MultipleFileMoves` / `LargeDiff` / `ChainStateTransition` events. The parent chain's "future events" list defers these until volume warrants.

## Out-of-scope dependencies

- `bridge-harness` integration (parent chain §Filing-dispatch bridge-harness section; lives in a future chain).
- Trained classifier promotion (T10 forges the chains; the actual training and A/B promotion lives there).
- `parse_context.recommended_disciplines` cross-fire (parent chain §Forward-compat with chain 17).
