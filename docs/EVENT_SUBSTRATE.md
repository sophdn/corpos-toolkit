# Event Substrate — Design

> **Status:** Draft for review. Produced by chain `agent-first-substrate` T1 (`design-event-substrate`). Decisions here are durable; downstream tasks T2–T7 bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 envelope → §3–§6 mechanics → §7 cross-chain seam → §8 audit-checklist coverage. §9 is open questions.
>
> **Companion docs:** `docs/AGENT_AUDIT_AND_MIGRATION.md` (source-of-truth audit framing; this doc takes positions on every §4.1–§4.5 checklist item — see §8). `docs/AGENT_AUDIT_CONVENTIONS.md` §1 / §11 (CODEMAP + structured-log rationale).

---

## 1. What this substrate is and isn't

The toolkit-server data layer today is CRUD: `bugs`, `tasks`, `chains`, `benchmark_results`, and friends are mutable rows that agents update via the `work` meta-tool. Reasoning lives in free-text fields (`resolution_note`, `design_decisions`, `constraints`) — recoverable to a human reader, opaque to programmatic recall, lost on overwrite.

This substrate adds **one append-only `events` table** as the source of truth for every state mutation. Existing CRUD tables remain in place; every mutation dual-writes — **row first, event second** (post-T4; see history note below) — inside a single SQLite transaction. The events table is the authoritative ledger; the CRUD tables become a denormalized projection cache.

> **History note (dual-write order):** T2 of this chain originally chose *event first, row second* with the rationale "if Emit fails (schema reject) the UPDATE never runs." T4 (projection-subsystem) reversed it because the projection fold inside `events.Emit`'s hook needs to read the post-update CRUD row to populate projection columns (`BugEdited` carries field names without values, so payload-only fold can't reconstruct the new state). Schema-reject failure mode is preserved: a failed `Emit` after the CRUD update still rolls the whole tx back via `pool.WithWrite`'s rollback path. See `docs/PROJECTIONS.md` §2.3 for the projection-fold reason in detail.

**Scope of this chain (and therefore this doc):**

- Event envelope, type catalog, payload-schema convention.
- Event-emit helpers + dual-write transaction discipline (T2).
- Rationale enforcement at the dispatch boundary (T3).
- Projection subsystem with rebuild-from-empty (T4).
- Span-id-shared structured observability (T5).
- Benchmark provenance as a typed payload (T6).
- Go package discoverability + CODEMAP (T7).

**Explicit non-goals (deferred to follow-on chains or excluded entirely):**

| Out of scope | Why |
|---|---|
| Dropping CRUD tables; frontend cut over to projections | Phase 4 of `AGENT_AUDIT_AND_MIGRATION.md` §6; needs projection bake-in time before legacy tables retire. Tracked separately, not this chain. |
| Retrofitting events for pre-2026-05-16 state | Forward-only capture. Pre-substrate history stays in `git log` + commit messages. |
| Dashboard frontend rewrite | Only read paths switch from CRUD tables to projection tables; the React tree is untouched. |
| RAG corpus management events (audit §4.4) | toolkit-server does not own a RAG corpus. Kiwix is read-only docs; vault is filesystem-backed text. The audit's RAG event types (`CorpusIngested`, `ChunkMarkedStale`, etc.) belong in the kiwix or vault subsystems, not here. See §8.4. |
| Seed-packet (or any other consumer project) | The substrate lives in `mcp-servers`; every project mounting `toolkit.db` inherits it transparently. No per-project work. |

---

## 2. Event envelope

The envelope is closed: every event has exactly these top-level fields, in this order. Adding a field requires a chain-level decision and a migration.

```json
{
  "event_id": "0190f8a3-7b21-7c64-9d83-1f44a2b18cde",
  "ts": "2026-05-16T14:32:00.123Z",
  "actor": {
    "kind": "agent",
    "id": "claude-opus-4-7"
  },
  "type": "BugResolved",
  "entity": {
    "kind": "bug",
    "slug": "forge-bug-title-omitted",
    "project_id": "mcp-servers"
  },
  "payload": { "/* type-specific, JSON Schema-validated */": null },
  "rationale": "Root cause was the bug-schema title field defaulting to empty in the forge writer path; fixed in commit abc1234. Verified by re-running the smoke harness against a fresh DB.",
  "refs": {
    "caused_by_event_id": null,
    "related_entities": []
  },
  "span_id": "9f8e7d6c-5b4a-3c2d-1e0f-aabbccddeeff",
  "schema_version": 1
}
```

### 2.1 Field rules

| Field | Type | Required | Notes |
|---|---|---|---|
| `event_id` | UUIDv7 | yes | Time-ordered, globally unique. Sortable by `event_id` gives time order. Stable for FK targeting (see §7). |
| `ts` | RFC 3339 with TZ, ms precision | yes | Redundant with the time-prefix in `event_id`; kept for human readability and queries that want a separate index. UTC. |
| `actor.kind` | enum: `agent` / `human` / `system` | yes | Inferred at dispatch from transport (see §4). Not caller-supplied. |
| `actor.id` | string | yes | For `agent`: model identifier (e.g. `claude-opus-4-7`, `qwen2.5-32b`). For `human`: portal session identifier. For `system`: cron job / hook / CLI subcommand name. |
| `type` | string | yes | Must match a registered type with a schema in `blueprints/events/<type>.json`. Closed set; agents cannot invent. |
| `entity.kind` | string | yes | One of: `bug`, `task`, `chain`, `benchmark_run`, `benchmark_metric`, or a future registered kind. |
| `entity.slug` | string | yes | The natural-key identifier for the entity. Toolkit-server uses slugs as primary IDs throughout; no numeric IDs in event payloads. |
| `entity.project_id` | string | yes for project-scoped kinds, `null` otherwise | Mirrors the project-scoping convention from `forge-schemas/bug.toml` / `chain.toml` / `task.toml`. Cross-cutting events (e.g. `BenchmarkRunStarted` for the regression suite) carry `null` and document why in the type's schema. |
| `payload` | JSON object | yes (may be `{}`) | Validated against the per-type JSON Schema at write time. Unknown fields are an error, not silently dropped. No PII, no large blobs — store hashes/refs, keep blobs content-addressed elsewhere. |
| `rationale` | string | yes for `actor.kind == "agent"`; optional otherwise | Enforced at dispatch boundary, not in handlers. Empty or whitespace-only is rejected with `InvalidInput` before the handler runs. See §5. |
| `refs.caused_by_event_id` | UUIDv7 or `null` | yes (may be `null`) | Compensating events (`BugTriageReversed`) point at the event they undo. Cascade-emitted events (e.g. `BugResolved` triggers `RoadmapItemRemoved`) point at the parent. |
| `refs.related_entities` | array of `{kind, slug, project_id}` | yes (may be `[]`) | Cross-entity references that aren't the primary `entity`. Example: `BugRoutedToTask` has `entity = bug`, `related_entities = [{kind: task, slug: …}]`. |
| `span_id` | UUIDv4 | yes | Per-MCP-request identifier; shared by every event and every log line emitted while serving that request. See §6. |
| `schema_version` | integer | yes | Envelope version. Currently `1`. Bumping requires a migration that backfills or maps. |

### 2.2 Storage shape

The `events` table is one row per event, with the envelope columns indexed for queries and the payload as a JSON blob:

```sql
-- crates/shared-db/migrations/032_events.sql (T2 will land this; sketched here for design clarity)
CREATE TABLE events (
    event_id           TEXT PRIMARY KEY,             -- UUIDv7
    ts                 TEXT NOT NULL,                -- RFC 3339 with TZ
    actor_kind         TEXT NOT NULL,
    actor_id           TEXT NOT NULL,
    type               TEXT NOT NULL,
    entity_kind        TEXT NOT NULL,
    entity_slug        TEXT NOT NULL,
    entity_project_id  TEXT,                         -- nullable for cross-cutting events
    payload            TEXT NOT NULL,                -- JSON
    rationale          TEXT,                         -- NULL only for non-agent actors
    caused_by_event_id TEXT REFERENCES events(event_id),
    related_entities   TEXT NOT NULL DEFAULT '[]',   -- JSON array
    span_id            TEXT NOT NULL,
    schema_version     INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX events_entity_idx ON events(entity_kind, entity_slug, ts);
CREATE INDEX events_type_ts_idx ON events(type, ts);
CREATE INDEX events_span_idx ON events(span_id);
CREATE INDEX events_project_ts_idx ON events(entity_project_id, ts);

-- Append-only enforcement: reject UPDATE/DELETE on the events table at the trigger level.
CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'events table is append-only'); END;

CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'events table is append-only'); END;
```

The triggers are the structural guarantee. A mistake in handler code that tries to "correct" an event by overwriting it fails at the SQL level — not at code review.

---

## 3. Event types are closed

### 3.1 The blueprints directory

Every event type has a schema at `blueprints/events/<type>.json`. The schema validates **payload-only**; the envelope is shared and validated by a single envelope schema at `blueprints/events/_envelope.json`. This mirrors the existing convention in `blueprints/forge-schemas/` where one TOML per entity-kind defines the per-kind validation.

Why JSON Schema (not TOML, not Go structs):

- **JSON Schema is the lingua franca for JSON payloads.** TOML is fine for static config (like `forge-schemas/`) where the data is read at startup; events are JSON in flight and need a JSON-native validator.
- **Same shape as `_envelope.json`.** One validator library, one error format.
- **Tooling.** Many JSON Schema validators exist for Go (the chosen one is documented in T2). Re-inventing validation by hand-rolling structs would re-introduce the "agent can send a typo and it gets silently coerced" failure mode the audit calls out.

### 3.2 Registration is a PR, not a runtime action

Adding a new event type means:
1. Add `blueprints/events/<type>.json` with a `description` field at every level explaining what each payload key means and when it's set.
2. Add a row to `docs/EVENT_CATALOG.md`.
3. Wire the emit-helper in the handler that produces it (T2 provides the helper signature).
4. Commit.

There is no runtime `register_event_type(...)` call. Emit-time validation reads the schema from disk (or the embed at build time); an unknown `type` value rejects at the dispatcher with `InvalidInput("unknown event type: <type>")` before any DB write. **An agent cannot ship a new event type by writing one.**

### 3.3 Migration for type-shape evolution

When a type's payload shape changes:

- **Additive change** (new optional field): bump nothing. Old events have the field absent; readers tolerate it. The schema's `description` for the new field records the date it was added.
- **Breaking change** (required field added, field type changed, field removed): bump the type. `BugResolved` becomes `BugResolvedV2`. The old type stays registered; old events stay readable. Projection-fold functions handle both.

There is no envelope-schema-version-per-type. Type-name versioning is the explicit, ledger-friendly form of evolution.

---

## 4. Actor inference

`actor.kind` and `actor.id` are not caller-supplied. They are inferred at dispatch from the transport that delivered the call.

| Transport | `actor.kind` | `actor.id` source |
|---|---|---|
| stdio JSON-RPC (MCP from a `claude` session) | `agent` | The connecting client's `clientInfo.name + "-" + clientInfo.version` from the MCP `initialize` handshake. For Claude Code: `claude-opus-4-7`. For other agent clients: their declared name. |
| Portal HTTP (`/portal/chat`, `/portal/write/*`) | `human` | A session identifier minted at portal-login, stored in a signed cookie. The portal does not currently have authentication; for this chain, `actor.id = "portal-anonymous-<session-uuid>"` is acceptable, with the understanding that real auth is a follow-on. |
| CLI subcommand (`toolkit-server rebuild-projections`, `toolkit-server install-into`, etc.) | `system` | `cli-<subcommand-name>` (e.g. `cli-rebuild-projections`). |
| In-transaction hook (cascade emits) | inherits parent | A cascade-emitted event inherits `actor.kind` and `actor.id` from the event that triggered it, with `refs.caused_by_event_id` pointing at the parent. |
| Cron / background job | `system` | `cron-<job-name>`. |

**Why inference, not caller-supply.** If the caller sets `actor.kind`, an agent can forge a `human` identity to bypass rationale enforcement (§5). Inference at the dispatch boundary closes that gap structurally.

---

## 5. Rationale at the dispatch boundary

The audit (§4.2 item 2, §5.2 rule 3, §8) says: rationale is required for agent actors, rejected when empty or boilerplate, enforced at write time.

### 5.1 Where the check lives

The check lives in **the dispatcher**, before the handler runs. Concretely, the Go dispatch package (`go/internal/dispatch/`) gets a pre-handler middleware:

```go
// Sketch — T3 lands the real code.
func enforceRationale(call ActionCall, manifest ActionManifest, actor Actor) error {
    if !manifest.RequiresRationale {
        return nil
    }
    if actor.Kind != "agent" {
        return nil
    }
    if strings.TrimSpace(call.Rationale) == "" {
        return ErrInvalidInput{Field: "rationale", Reason: "rationale required for agent actor on action " + manifest.Name}
    }
    return nil
}
```

**Why not in handlers.** Per-handler rationale checks scatter the same `if rationale == ""` boilerplate across every mutating action, with the obvious drift: one handler forgets, the gap goes unnoticed until an audit. Putting the check at the boundary means it's impossible to land a new mutating action that doesn't enforce it — the dispatcher reads the manifest, applies the policy, no opt-out for handler authors.

**Why a manifest flag (not "every action enforces").** Some actions are read-only (`bug_read`, `chain_status`); requiring rationale on every read would be ceremony for no audit value. Some actions are non-state-changing observers (`task_blockers`); same. The manifest's `requires_rationale = true` declares the action mutates state worth auditing.

### 5.2 Manifest flag location — actual landed shape (T3)

The pre-T3 sketch imagined `action-manifests/<surface>.toml` with per-action subsections. T3 surfaced two facts about the on-disk reality:

1. The existing per-skill manifests (`action-manifests/start-task.toml`, `action-manifests/complete-task.toml`, …) are agent-facing skill discoverability content, not dispatcher policy. They use verb-first names (`start-task`) that don't 1:1 match the dispatcher's entity-first action names (`task_start`).
2. Loading 30+ per-skill TOMLs for the dispatcher just to extract one boolean per file would tightly couple the dispatch boot path to file naming conventions that have nothing to do with policy.

T3 therefore introduces a **single source-of-truth policy file**:

```toml
# action-manifests/dispatch-policy.toml
[work.bug_resolve]
requires_rationale = true

[work.bug_read]
requires_rationale = false

[work.task_complete]
requires_rationale = true
```

Loaded by `go/internal/dispatch/policy/` at server startup; consulted by `dispatch.DispatchWithOptions` before each handler runs. The per-skill TOMLs in `action-manifests/` are untouched and continue to serve their agent-facing role.

The default for an unset entry remains **`false`** — read-only ergonomic, fail-open so a typo doesn't block production calls. T3 swept every mutating action in the agent-first-substrate scope to explicit `true` (chain `completion_condition` item (a)).

### 5.3 What counts as invalid rationale

T3 landed three rejection conditions, all applied only when `actor.kind == "agent"`:

1. **Empty / whitespace-only.** `strings.TrimSpace(rationale) == ""` → `reason: empty`.
2. **Too short.** `len([]rune(strings.TrimSpace(rationale))) < 6` → `reason: too short`. Six characters is past every single-word boilerplate ("ok", "done") but well below substantive short rationales ("fix typo", "unblock CI").
3. **Boilerplate stop-list.** A small, closed, case-insensitive set in `go/internal/dispatch/policy/policy.go` rejects rationales matching common filler ("ok", "as requested", "lgtm", "n/a", "todo", "because", …). The list is closed; expansion requires a chain-level decision.

The pre-T3 design called for "empty only" rejection and deferred boilerplate handling to the sibling read-side chain. T3 acceptance criteria revised that: a tight, conservative stop-list at the write boundary makes boilerplate uncomfortable at the call site (cheap, immediate feedback), while a separate quality-judgment surface in `query-telemetry-substrate` remains the right home for *substantive-but-uninformative* rationale flagging (which needs more nuance than a stop-list).

Human and system actors are not subject to length or stop-list checks — their rationale is recorded verbatim when supplied and NULL when absent. The discipline is agent-targeted because the failure mode it addresses (single-word filler under context pressure) is agent-shaped.

---

## 6. Span ID and trace contract

The audit (§4.3 of the conventions companion doc, §11) frames structured observability as "logs and events share identity." This substrate makes that concrete with `span_id`.

### 6.1 One span per MCP request

When the dispatcher receives an MCP `tools/call`, it mints a `span_id` (UUIDv4, random — no need for time order) before dispatching. That `span_id` is attached to:

- Every event emitted while serving the request.
- Every structured log line emitted while serving the request.
- Every child operation: in-transaction hooks, FTS5 sync calls, SSE broadcasts on the `/events` stream.

The `span_id` is the join key between the events table and the (future, T5) structured-log store.

### 6.2 Child spans for multi-step ops

A handler that performs multiple distinct operations under one request mints child `span_id`s and records the parent. T5 landed the concrete API as `obs.SpanStart(ctx, name) (ctx, end)` in `go/internal/obs/`; see `docs/OBSERVABILITY.md` for the full handler-author guide. The contract is:

- Parent span: the request-scoped `span_id`.
- Child span: a fresh UUIDv4, with `parent_span_id` recorded in the log entry / event's `refs.caused_by_event_id` chain. (Events use `caused_by_event_id` not `parent_span_id` — the event ID chain is the canonical causal graph; span IDs are observability metadata.)

Example: `forge(bug, ...)` request — one parent span. Within it: the row INSERT emits `BugReported` (parent span). The in-transaction hook that re-builds the FTS5 index emits a `FTSIndexRefreshed` event with `caused_by_event_id` pointing at `BugReported` and the same `span_id`. The SSE broadcast logs an entry with the same `span_id`.

### 6.3 Cross-substrate span sharing

The sibling chain `query-telemetry-substrate` is the read-side observability surface. Its design (TT1) depends on the span_id contract being **agent-request-scoped, not handler-scoped**. Concretely:

A typical agent session does: `vault_search(...)` → reads results → `task_complete(...)`. The vault_search is a read-side query (sibling chain's territory). The task_complete is a write-side mutation (this chain's territory). The sibling chain joins query → resolution by **shared span_id**: both calls run under the same agent-request span.

> **Note for sibling-chain authors:** the span_id is minted at the *MCP request boundary*, one per `tools/call`. A single agent turn typically issues multiple `tools/call` requests; each gets its own span_id. The query-resolution join therefore needs a *session-scoped* identifier above the span, which is the MCP session ID from the `initialize` handshake. The sibling chain's design doc (T1 deliverable) should specify whether it joins by span or session, and the schema should support both.

This is a flagged seam, not a closed decision. The sibling chain may need an additional identifier surfaced by this substrate; if so, the amendment lands here.

---

## 7. Projection contract

Projections are denormalized read-models folded from events. The frontend reads projections; agents never read or write them directly.

### 7.1 Projection table shape

Every projection table carries two structural columns:

```sql
-- Sketch; T4 lands real projection schemas.
CREATE TABLE current_bugs (
    -- domain columns: slug, title, status, severity, project_id, etc. --
    last_event_id  TEXT NOT NULL,   -- the most recent event folded into this row
    last_event_ts  TEXT NOT NULL,   -- denormalized for query convenience
    PRIMARY KEY (slug, project_id)
);

CREATE TABLE projection_watermarks (
    projection_name  TEXT PRIMARY KEY,    -- e.g. 'current_bugs'
    last_event_id    TEXT NOT NULL,       -- highest event_id folded into this projection
    last_folded_ts   TEXT NOT NULL,       -- when the fold ran
    schema_version   INTEGER NOT NULL     -- bumped when the fold function changes shape
);
```

The per-row `last_event_id` enables row-level "when did this last change" queries cheaply. The `projection_watermarks` table is the resume-point for incremental folds.

### 7.2 Fold function contract

Each projection has a fold function with signature:

```go
// Sketch.
type Fold func(ctx context.Context, tx *sql.Tx, event Event) error
```

Three invariants:

1. **Idempotent.** Folding the same event twice produces the same state. The fold reads the projection row first; if `last_event_id >= event.event_id`, it's a no-op return.
2. **Pure-with-respect-to-events.** The fold reads only the event passed in + the current projection state. No external API calls, no filesystem reads outside the DB. This is what makes drop-and-rebuild safe.
3. **Type-dispatched.** Each fold function handles a specific set of event types relevant to its projection. Unknown types are *ignored*, not errored — a new event type doesn't break existing projections.

### 7.3 Rebuild semantics

Two rebuild modes:

- **Incremental.** `toolkit-server rebuild-projections [--projection=name]` resumes from each projection's watermark; processes new events in `event_id` order; updates watermark + row(s) per event in a single transaction per event. Safe to run continuously (T4 will land a cron-style continuous runner; for this design the manual subcommand is sufficient).
- **Full.** `toolkit-server rebuild-projections --from-scratch [--projection=name]` truncates the projection table(s), resets the watermark to `null`, replays every event in `event_id` order. The CLI prompts for confirmation; without `--yes`, prints the projected row counts and exits.

A successful full rebuild followed by an incremental run from the same event log must produce byte-identical projection state to an incremental-only run. This is the closing test for T4 (see chain `completion_condition` item (b)).

### 7.4 Initial projection set

T4 ships three projections; more are deferred until a concrete consumer surfaces.

| Projection | Folds events of type | Used by |
|---|---|---|
| `current_bugs` | Bug* | dashboard `/bugs`, `bug_list` action |
| `chain_status` | Chain*, Task* | dashboard `/chains`, `chain_status` action |
| `roadmap_view` | Chain*, Task* + roadmap-mutating events (future) | dashboard `/roadmap`, `roadmap_list` action |

`benchmark_trends` and `rag_freshness` from the audit's suggested set are not in T4. Benchmarks get partial coverage via T6 (provenance), which is enough for re-running but doesn't add a trends projection; that's deferred. RAG freshness is out-of-scope per §1.

---

## 8. Audit checklist coverage (§4.1 – §4.5)

Every checklist item in `docs/AGENT_AUDIT_AND_MIGRATION.md` §4.1–§4.5 has a stated position below. Positions:

- **will-pass-this-chain**: the substrate as designed addresses the gap and the closing retrospective (T8) will verify it.
- **will-not-address-this-chain**: the gap is real but the chain's scope excludes it; pointer to where it gets addressed.
- **out-of-scope-this-codebase**: the audit item assumes a domain the toolkit-server does not own.

### 8.1 Data layer audit (§4.1)

| Audit item | Current state | Position | Reason |
|---|---|---|---|
| (1) Single mutable table per entity, agents `UPDATE` directly | FAIL — `bugs`, `tasks`, `chains` are all single mutable tables | **will-pass-this-chain** | Dual-write keeps the mutable tables in place but the events ledger is the source of truth. Phase 4 (out-of-scope here) retires the legacy tables; in the meantime the audit's spirit is met — every UPDATE is preceded by an event INSERT in the same txn. |
| (2) Status/priority/assignment overwritten without history | FAIL | **will-pass-this-chain** | Every status transition action (`bug_resolve`, `task_complete`, `task_cancel`, etc.) emits a typed `*Resolved` / `*Completed` event capturing old + new state. T3 enforces rationale on each. |
| (3) Benchmark results updated rather than append-only | PARTIAL — `benchmark_results` rows are mostly INSERT-only but lack provenance | **will-pass-this-chain** | T6 makes runs fully append-only with typed provenance payloads (`BenchmarkRunStarted` / `Completed` / `Failed`). Any historical UPDATE path gets removed in T6. |
| (4) `notes` / `comments` / `rationale` as dumping ground | FAIL — `bug.resolution_note`, `chain.design_decisions`, `task.constraints` all carry free-form agent reasoning | **will-pass-this-chain** | Rationale moves to envelope-level required field, enforced at dispatch (T3). The legacy text fields stay populated as projection cache (so the dashboard doesn't break), but the canonical reasoning lives in `events.rationale`. |
| (5) FK relationships as nullable columns rewritten in place | FAIL — `bug.routed_chain_slug` / `routed_task_slug` are nullable strings updated in place | **will-not-address-this-chain** | Schema reshape into linking events (`BugRoutedToTask` event with the bug → task linkage in `refs.related_entities`) is the right end state, but the legacy columns survive as projection cache. The events ledger captures every routing decision with rationale; column shape becomes display-only. Reason: minimal disruption, correctness via events. Full schema reshape is a follow-on. |
| (6) Hard deletes | PARTIAL — most surfaces don't delete; `roadmap_items` DELETE on close is *intentional* per CONVENTIONS.md §Polymorphic-ref | **will-pass-this-chain** for events table (append-only enforced by trigger); **will-not-address-this-chain** for existing tables (the existing delete semantics are by-design and the closing event captures the lifecycle end). |

### 8.2 Agent interface audit (§4.2)

| Audit item | Current state | Position | Reason |
|---|---|---|---|
| (1) Generic CRUD endpoints rather than domain verbs | PARTIAL — `forge` / `forge_edit` look generic but are shape-validated against `blueprints/forge-schemas/`; per-domain transitions (`bug_resolve`, `task_complete`, `task_cancel`) exist as verbs | **will-pass-this-chain** | T3 sweep includes auditing every action manifest for verb-shape. Any remaining `set_*` / `update_*` / `patch_*` actions get a verb-shaped replacement or get the `requires_rationale = true` flag (or both). |
| (2) Rationale optional or absent | FAIL — `task_complete` has no rationale parameter; `bug_resolve.resolution_note` is optional in some kinds | **will-pass-this-chain** | T3 makes rationale a dispatch-boundary required field (§5). |
| (3) Two paths for the same action (REST + direct SQL) | PASS — dashboard is read-only; portal HTTP routes through `dispatch::handle_work` (same code path as MCP); no direct SQL writes | n/a | Documented in CONVENTIONS.md §Architecture and Migration Strategy. |
| (4) Closed schema of event/action types | FAIL — no events table | **will-pass-this-chain** | T2 enforces via the `blueprints/events/` convention and emit-time schema validation (§3). |

### 8.3 Read path audit (§4.3)

| Audit item | Current state | Position | Reason |
|---|---|---|---|
| (1) Frontend queries same tables agents write to | FAIL — dashboard reads `chains` / `tasks` / `bugs` / `benchmark_results` directly via observe HTTP | **will-pass-this-chain** *partially*: T4 introduces projections and updates observe-HTTP to read from them. Full frontend cutover is Phase 4, out of scope. After T4, the *substrate* offers the alternative; the dashboard *switch* is the follow-on chain's work. |
| (2) Roadmap / dashboard / trend views computed on-the-fly | PARTIAL — `roadmap_items_v_with_status` is a view (computed); some dashboard endpoints do joins | **will-pass-this-chain** | T4 materializes `chain_status` and `roadmap_view` as projections folded from events; views become projection reads. |
| (3) Can projections be rebuilt without re-running agent work | n/a — no projections yet | **will-pass-this-chain** | §7.3 mandates drop-and-rebuild as a first-class operation. Closing test for T4 (chain `completion_condition` item (b)). |

### 8.4 RAG-specific audit (§4.4)

| Audit item | Position | Reason |
|---|---|---|
| (1) Chunk staleness as boolean vs event-sequenced | **out-of-scope-this-codebase** | toolkit-server does not own a RAG corpus. Kiwix is read-only doc fetches (we don't write the ZIM); vault is filesystem-backed markdown (rebuild is a script, not an agent action). If a RAG corpus joins this codebase later, its own chain adopts the substrate. |
| (2) Corpus hash + index version recorded per retrieval eval | **out-of-scope-this-codebase** | Same reason. T6 captures `corpus_hash` for benchmark runs that consume vault/kiwix, which is the closest analog — see §8.5 item (1). |

### 8.5 Benchmarking-specific audit (§4.5)

| Audit item | Current state | Position | Reason |
|---|---|---|---|
| (1) Each benchmark run records full config (model, prompt hash, retriever, corpus, seed) | PASS — T6 closed via `BenchmarkProvenance` 8-field bundle on every `BenchmarkRunStarted` event; trigger on `benchmark_results` rejects post-cutover rows without a `provenance_id` FK | **passed-this-chain (T6)** | Schema landed at `blueprints/events/BenchmarkRunStarted.json`; storage at `benchmark_provenance` table per migration 035. T1 drafted the 7-field shape; T6 detail-filled by adding `retriever_config_hash` (distinct from `retriever_version` — version pins the retriever, config_hash pins its runtime knobs). `wall_clock_start` lives on the envelope's `event_time`; `wall_clock_end` lives on `BenchmarkRunCompleted.wall_clock_ms` — neither needs a payload field. |
| (2) Two runs comparable; surrounding state pinned | PASS — T6 makes drift visible (full provenance bundle joined 1:1) and exposes a first-class replay action | **passed-this-chain (T6)** | `measure.benchmark_replay` spawns `benchmarks --replay <row_id>` which re-emits `BenchmarkRunStarted` with `refs.caused_by_event_id` chained to the original; the Go handler returns a per-field diff over the score columns. Chain `completion_condition` item (d) verifies replay against deterministic models. |
| (3) Intermediate agent decisions during a run logged | FAIL | **will-pass-this-chain** | `MetricRecorded` events fire at every per-step rubric judgment, with `caused_by_event_id` chaining back to `BenchmarkRunStarted`. The span tree (§6) reconstructs the run. |

---

## 9. Open questions for review

These are decisions I am proposing but the user may want to override before the doc lands.

1. **Storage of `payload` and `related_entities` as TEXT JSON, not as native JSON1.** SQLite JSON1 supports indexed JSON columns; TEXT + manual `json_extract` is simpler and matches existing toolkit-server convention (`bugs.tags`, `events`-style fields elsewhere are stored as TEXT). Confirm.
2. **`schema_version = 1` is envelope-wide.** When the envelope itself evolves (e.g. add a top-level `correlation_id` for distributed tracing), the version bumps and a migration backfills. Per-type evolution is via type renaming (`BugResolvedV2`), not per-type version. Confirm both forms.
3. **`actor.id` for portal HTTP without auth.** I propose `portal-anonymous-<session-uuid>` as a placeholder until portal auth lands. Alternative: reject portal writes entirely until auth is in place. The portal currently does write (chat turns); rejecting them breaks the dashboard. Confirm placeholder is acceptable.
4. **The `_envelope.json` lives at `blueprints/events/_envelope.json` (leading-underscore convention to sort first and signal "not an event type").** Alternative: a separate `blueprints/event-envelope/envelope.json`. Single-file is simpler; the underscore prefix is the visual signal. Confirm.
5. ~~**`requires_rationale` defaults to `false` during T3 rollout, then T3 sweeps every existing mutating action to explicit `true`.**~~ **RESOLVED (T3 close):** Default-false confirmed. T3 swept every mutating action listed in the chain's acceptance criteria (bug_resolve, bug_reopen, task_start, task_complete, task_cancel, task_reopen, task_block, task_unblock, task_edit, chain_close, forge, forge_edit, forge_delete) plus the SHA-stamp + roadmap-mutating + library-* + admin-mutating set to explicit `true` in `action-manifests/dispatch-policy.toml`. See §5.2.
6. **Span-id is per-MCP-request, not per-session.** Sibling-chain (§6.3) may need session-id surfaced separately. Flagged as a seam, not closed. Confirm before sibling-chain T1 starts.

---

## 10. Glossary

| Term | Meaning |
|---|---|
| **Substrate** | The events table + envelope + emit helpers + dispatch enforcement + projection contract, taken together as one architectural layer. |
| **Envelope** | The 11-field top-level shape every event shares. Closed; bumping is a migration. |
| **Payload** | The type-specific JSON object inside the envelope; validated against `blueprints/events/<type>.json`. |
| **Fold** | The function that updates a projection table by applying one event. Idempotent, pure with respect to events. |
| **Projection** | A denormalized read-model table produced by folding events. Disposable; rebuildable from the events ledger. |
| **Watermark** | A projection's record of the highest `event_id` it has folded. Used to resume incremental folds. |
| **Span** | A request-scoped identifier shared by every event and log line emitted while serving one MCP `tools/call`. |
| **Dual-write** | One SQLite transaction that updates the legacy CRUD row and then inserts the event (post-T4 order; see §1 history note). The projection fold inside `events.Emit`'s hook reads the post-update CRUD row, so the order matters. Failure of either rolls back both. |
| **Cascade emit** | A child event triggered by a parent event (e.g. an in-transaction hook emitting a derived event). Inherits actor; records `caused_by_event_id`. |

---

## 11. Cross-references

- `docs/AGENT_AUDIT_AND_MIGRATION.md` — source-of-truth audit framing. §3 = target architecture; §5 = envelope shape; §7 = local-first practical notes; §10 = closing audit-event format (used by T8).
- `docs/AGENT_AUDIT_CONVENTIONS.md` — companion audit, human-vs-agent conventions. §1 (CODEMAP) informs T7. §11 (structured logging) informs T5.
- `docs/EVENT_CATALOG.md` — the per-type catalog table; one row per registered event type.
- `blueprints/events/*.json` — per-type payload schemas; sibling shape to `blueprints/forge-schemas/*.toml`.
- `CONVENTIONS.md` — house style. §Architecture lists the substrate's home as the Data layer (events sit alongside CRUD tables, both managed by `crates/shared-db/migrations/`). §Migration Strategy / §Migration runner ownership applies — `032_events.sql` lands in canonical + both Go embed mirrors via `scripts/sync-migrations.sh`.
- Chain `query-telemetry-substrate` — read-side counterpart. Shares span_id; depends on UUIDv7 `event_id` for FK targeting.
