# Substrate Frontend — Design

> **Status:** Draft for review. Produced by chain `agent-substrate-frontend` F1 (`design-frontend-surface`). Decisions here are durable; downstream tasks F2–F5 bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 endpoint catalog → §3 pagination/filter shape → §4 read-source rules → §5 span-link contract → §6 cross-substrate join → §7 per-event detail shapes → §8 component contracts → §9 route plan → §10 fallback matrix → §11 naming-collision disambiguation → §12 open questions.
>
> **Companion docs:** `docs/EVENT_SUBSTRATE.md` (envelope, append-only mechanics, projection contract). `docs/EVENT_CATALOG.md` (per-type payload summaries). `docs/PROJECTIONS.md` (read-side projection fold and rebuild semantics). `docs/OBSERVABILITY.md` (T5 span tree). `docs/BENCHMARKS.md` (T6 provenance bundle).

---

## 1. What this surface is and isn't

The agent-first substrate (chain `agent-first-substrate` T1–T8) ships an append-only `events` table that records every state mutation with rationale, actor, span, and typed payload. Today that ledger is **SQL-only** — no dashboard reader exists for any of it. Operators who want to know "what landed today and why" must `sqlite3 toolkit.db` and write joins by hand.

This chain ships the dashboard surfaces that make the ledger human-readable. Concretely: per-entity timelines slotted into existing detail panels, a top-level audit-ledger page with filters and rationale search, and an admin policy-peek showing which actions enforce rationale.

**Scope (and therefore this doc's scope):**

- HTTP endpoints serving the events table to the dashboard (`go/internal/observehttp/events.go`, F2).
- Reusable `EventTimeline` component slotted into Bug / Chain (and where possible, Task) detail panels (F3).
- Top-level audit-ledger page at `/audit` with filters, cursor pagination, per-event detail drawer, SSE-aware "new events" badge (F4).
- Dispatch-policy peek surface under `/admin/dispatch-policy` showing `requires_rationale` per action, with reload-from-disk (F5).
- Cross-substrate join readiness for `query-telemetry-substrate.query_resolutions.write_event_ids` — null/array distinguishes "not available" from "no related queries".
- Closing self-hosting event `SubstrateFrontendAuditCompleted` emitted at F5.

**Explicit non-goals:**

| Out of scope | Why |
|---|---|
| Phase 4 cutover (retire `bug.resolution_note` / `chain.design_decisions` / `task.handoff_output` from the detail panels) | The legacy free-form fields are still populated as projection cache. Removing them changes the dashboard contract under operator feet; tracked as a follow-on chain after this one bakes in. The EventTimeline is **added below** the legacy fields, not replacing them. |
| Operational live-spans view | T5's `SpansPanel` owns "what is happening now". This chain links to span IDs; the audit ledger surfaces **history**, not motion. |
| Full forensic-investigation tooling (cross-correlation, fan-out queries) | The audit ledger is a viewer for what agents wrote. Power-user correlation stays SQL-side. |
| FTS5 over `events.rationale` | F2 uses unindexed `LIKE` for rationale free-text search. FTS5 is a follow-on if performance demands it; the events table grows slowly enough that LIKE is fine at expected scale. |
| Rich Markdown rendering of rationale | Rationale is verbatim agent text. Whitespace preserved via `white-space: pre-wrap`; no Markdown, no link expansion, no XSS-shaped surface area. |
| Editing events from the UI | Events are append-only by trigger (see `EVENT_SUBSTRATE.md` §3.4). The frontend cannot write. |

---

## 2. Endpoint catalog

Three new endpoints land in `go/internal/observehttp/events.go` (F2). One additional endpoint for the policy peek lands as part of F5 (see §5.4 here for sketch; F5 owns final shape).

### 2.1 `GET /events` — paginated, filterable list

Query parameters:

| Param | Type | Default | Notes |
|---|---|---|---|
| `entity_kind` | string | absent (no filter) | One of `bug`, `task`, `chain`, `benchmark_run`, `benchmark_metric`. |
| `entity_slug` | string | absent | When supplied, filters to the named entity. Usually paired with `entity_kind` (entity slugs aren't globally unique across kinds). |
| `type` | string | absent | Event type, e.g. `BugResolved`. Multiple types via repeated `type=...&type=...`. |
| `project` | string | absent | Project ID (mirrors existing `project` / `project_id` convention from `internal.go::projectFilter`). |
| `span_id` | string | absent | UUIDv4 — narrows to events emitted under one MCP request. |
| `actor_kind` | enum: `agent` / `human` / `system` | absent | |
| `actor_id` | string | absent | E.g. `claude-opus-4-7`. |
| `since` | RFC 3339 timestamp | absent | Lower bound on `ts`, inclusive. |
| `until` | RFC 3339 timestamp | absent | Upper bound on `ts`, exclusive. |
| `q` | string | absent | Free-text LIKE search on `rationale`, case-insensitive. |
| `cursor` | UUIDv7 event_id | absent | Pagination cursor — see §3.1. |
| `limit` | int | 50 | Clamped to `[1, 200]`. |

Response shape (Go struct names use the typed-return convention from `dispatch.Adapt[T any]`; JSON keys are snake_case):

```json
{
  "items": [
    {
      "event_id": "0190f8a3-7b21-7c64-9d83-1f44a2b18cde",
      "ts": "2026-05-17T14:32:00.123Z",
      "actor": { "kind": "agent", "id": "claude-opus-4-7" },
      "type": "BugResolved",
      "entity": { "kind": "bug", "slug": "forge-bug-title-omitted", "project_id": "mcp-servers" },
      "payload_summary": { "kind": "fixed", "commit_sha": "abc1234" },
      "rationale": "Root cause was the bug-schema title field…",
      "span_id": "9f8e7d6c-5b4a-3c2d-1e0f-aabbccddeeff",
      "caused_by_event_id": null,
      "schema_version": 1
    }
  ],
  "next_cursor": "0190f8a4-1c2d-7e6f-8a9b-001122334455",
  "page_size": 50
}
```

`payload_summary` is the full payload object on list responses (events are small, no aggressive truncation needed). `related_entities` is omitted from the list view to keep rows compact; the detail endpoint includes it. The list is ordered `event_id DESC` (newest first) — see §3.1.

### 2.2 `GET /events/{event_id}` — single event detail

Returns one event with full envelope plus the optional cross-substrate join:

```json
{
  "event_id": "0190f8a3-7b21-7c64-9d83-1f44a2b18cde",
  "ts": "2026-05-17T14:32:00.123Z",
  "actor": { "kind": "agent", "id": "claude-opus-4-7" },
  "type": "BugResolved",
  "entity": { "kind": "bug", "slug": "forge-bug-title-omitted", "project_id": "mcp-servers" },
  "payload": { "/* full payload object */": null },
  "rationale": "…",
  "refs": {
    "caused_by_event_id": null,
    "related_entities": [{ "kind": "task", "slug": "fix-it", "project_id": "mcp-servers" }]
  },
  "span_id": "9f8e7d6c-…",
  "schema_version": 1,
  "related_queries": null
}
```

`related_queries` semantics — see §6.

Errors: `404` with `{"error":"event not found"}` when the ID is well-formed but absent; `400` with `{"error":"invalid event_id"}` when the path segment isn't a UUIDv7-shaped value (cheap pre-DB check, avoids gratuitous lookups on garbage).

### 2.3 `GET /entities/{kind}/{slug}/events` — per-entity timeline

Returns events for one entity, ordered chronologically (`event_id ASC` — oldest first; the timeline reads top-down as "this is how it happened"). Same filter superset as `/events` minus `entity_kind` / `entity_slug` (they're in the path). Same response shape as `GET /events`.

Path parameters:
- `kind` — one of `bug`, `task`, `chain`, `benchmark_run`. `benchmark_metric` deliberately not exposed at the path level (those events are sub-events of a benchmark run and surface inside the run's timeline via `caused_by_event_id`; a separate per-metric timeline would be an awkward read).
- `slug` — natural-key identifier per the entity-kind convention.

Required query parameter:
- `project` (or `project_id`) — same as elsewhere. Entity slugs aren't globally unique across projects.

`cursor` + `limit` work the same way; cursor semantics flip to "after this event_id" (ascending pagination).

### 2.4 Endpoint placement

All three mounted in `BuildRouter` in `router.go`:

```go
mux.HandleFunc("GET /events/list", state.eventsList)         // §2.1
mux.HandleFunc("GET /events/{event_id}", state.eventsDetail) // §2.2
mux.HandleFunc("GET /entities/{kind}/{slug}/events", state.entityEvents) // §2.3
```

> **Note on `/events/list` vs `/events`:** the SSE stream is already mounted at `GET /events` (see `router.go:16`: `state.Bus.Handler()`). Mounting a JSON-list endpoint at the same path would shadow the bus handler or require content-type negotiation (fragile). We use `/events/list` for the JSON list to keep the SSE path untouched. `/events/{event_id}` is unambiguous (the path segment after `/events/` distinguishes it from the SSE root). This is the load-bearing reason `q (rationale LIKE search)` filter and friends go on `/events/list`, not `/events`.

CORS rules inherit from `withCORS(mux)`; no per-route overrides needed.

---

## 3. Pagination and filter shape

### 3.1 Cursor-based pagination on `event_id`

`event_id` is a UUIDv7, time-prefixed and lexicographically sortable (see `EVENT_SUBSTRATE.md` §2.1). This means `ORDER BY event_id DESC` and `WHERE event_id < ?` produce stable, total-order pages without an offset.

`/events/list` defaults to descending order (newest first); the audit ledger page expects "recent first". The cursor semantic:

```sql
SELECT … FROM events
WHERE 1=1
  AND (? = '' OR event_id < ?)   -- cursor: '' on first page, prior page's tail otherwise
  -- filters --
ORDER BY event_id DESC
LIMIT ? + 1                       -- read one extra to detect end-of-stream
```

If the query returns `limit + 1` rows, the last row is the next cursor; the response trims it back to `limit` items. If it returns `<= limit`, `next_cursor` is `null`.

`/entities/{kind}/{slug}/events` flips to ascending (`event_id > cursor`, `ORDER BY event_id ASC`) — the entity timeline reads top-down.

**Why not OFFSET.** Offset-based pagination drifts when new rows arrive between calls (the same row can appear on two pages). The events table is append-only and high-volume; cursor-based is the only correct pattern. See vault `decisions/adapter-vs-extend-when-shapes-mismatch.md` for the general principle ("extend the source endpoint, don't reshape on the client").

### 3.2 Cursor stability under concurrent inserts

The events table is append-only by trigger. New rows arrive with `event_id` values strictly greater than every existing row (UUIDv7 timestamp prefix is monotonic per emit-process, and the `pool.WithWrite` mutex serializes cross-process emits). A cursor pinned mid-paginate will not skip or duplicate rows: descending pagination's `event_id < cursor` excludes anything inserted after the cursor was minted; ascending pagination's `event_id > cursor` includes them on the next page.

F2 tests pin this property: seed events, start a paginate, INSERT a new event between calls, verify cursor progress.

### 3.3 Filter compositions

Filters are AND-composed. An empty filter set returns all events (clamped by `limit`). Multi-value filters (multiple `type=…` params) are OR-composed within a key. The handler builds the SQL using `db.NewArgs()` (existing helper, see `bugs.go:53`) — no string concatenation of caller values into SQL.

`q` (rationale LIKE search) is unindexed and uses `LIKE '%' || ? || '%' COLLATE NOCASE`. Performance budget: 100k events scanned in < 200ms on the dev workstation (verified empirically by F2's perf-smoke test). If the table grows past that, FTS5-over-rationale is the follow-on.

`since` / `until` are ISO-8601 strings compared lexicographically against `ts` (which is also ISO-8601 with consistent precision). This is faster than parsing both sides and equivalent because of the format's lexicographic-sort-equals-chronological-sort property.

### 3.4 Response caching

Events are append-only. A list page with `cursor != null` (i.e. anything except the latest page) is **content-stable**: once `next_cursor` is non-null and the cursor is in the page, the response body cannot change. F2 sets `Cache-Control: public, max-age=300` on such responses. The latest page (`cursor == ""`) gets `Cache-Control: no-cache` because new events may arrive.

The single-event endpoint sets `Cache-Control: public, max-age=86400` — events are immutable. The `related_queries` field may transition from `null` → array later (when the sibling chain lands), so the cache TTL is bounded; a 1-day window is fine for an admin surface.

---

## 4. Read-source rules — events vs projections

| Surface | Reads from | Why |
|---|---|---|
| Timeline rows (list, detail, per-entity) | `events` table direct | The ledger IS the source of truth. Projections are denormalized read models; they don't carry the timeline. |
| Entity current-state lookups (e.g. "what's the bug's title for this timeline header?") | `proj_current_bugs` / `proj_chain_status` / etc. | T4 contract: dashboard reads projections, not CRUD. The bug title is current-state, not history. |
| Cross-substrate joins (`related_queries`) | LEFT JOIN to `query_resolutions` when present | See §6. |

The events table is queried via explicit column lists — no `SELECT *`. This mirrors `bugs.go:53`'s SQL discipline and avoids the silent column-add failure mode.

The entity-timeline component (F3) consumes both: the timeline component calls `/entities/{kind}/{slug}/events` for rows, then renders inside an existing detail panel that already has the current-state row from its projection-backed fetch. No new entity-current-state endpoint is needed.

---

## 5. Span-id link contract with T5's SpansPanel

`span_id` on every event is the join key between the events table and T5's structured log + span tree (see `EVENT_SUBSTRATE.md` §6 and `docs/OBSERVABILITY.md`). The audit-ledger UI surfaces `span_id` on every row and event detail; clicking the span id should jump to the operator's view of that span.

### 5.1 Current state of `SpansPanel`

T5's `SpansPanel` (`apps/dashboard/src/pages/Spans/index.tsx`) renders the **live** `/events/spans` SSE stream into a tree grouped by `trace_id`. Two facts shape the link contract:

1. The panel has no per-span deep-link route. Today `/spans` shows the live buffer; there is no `/spans/<id>` or `/spans?span_id=<id>` reader.
2. The buffer is bounded (`useSpanStream(500)`) — spans older than the latest 500 stream events are not in memory.

A naive `<a href="/spans/<id>">` link would break: either the route doesn't exist, or it does but the span has rolled out of the buffer.

### 5.2 Link contract

The `<SpanLink span_id={id} />` component does the following:

1. Renders an `<a>` with `href={\`/spans?span_id=${id}\`}`. This route is added to F4's router work — `/spans` accepts an optional `?span_id=…` query param.
2. The `SpansPanel` reads the query param at mount; when present, it filters the live buffer to spans matching that ID, with an "in live buffer" empty-state when no match exists. The empty state surfaces a "Copy span ID" button as the fallback action.
3. Clicking the link also calls `navigator.clipboard.writeText(id)` (defensive: if the panel renders empty-state, the operator already has the ID in their clipboard to SQL-search). The clipboard write is best-effort (clipboard API may be denied); failure is silent.

This is **the F3/F4 deliverable, not the SpansPanel team's deliverable.** The link points at the existing `/spans` route and degrades gracefully when the target span isn't in buffer. A follow-on (out of scope here) could mint a `/spans/historical/<id>` reader backed by T5's persistent log; until then the live-buffer-filter is the contract.

> **Note for SpansPanel maintainer:** when F4 lands, `SpansPanel` should read `useSearchParams().get('span_id')` and filter the tree. F4 wires the link; the panel-side filter is a 5-line change in `pages/Spans/index.tsx`. See §10 fallback matrix row 1.

---

## 6. Cross-substrate join — `query_resolutions.write_event_ids`

The sibling chain `query-telemetry-substrate` (TT2 `interactions-and-resolutions-tables`) adds a `query_resolutions` table with `write_event_ids` as a JSON-array FK to `events.event_id`. When that table exists, `GET /events/{event_id}` joins to it and surfaces a `related_queries` array on the response; when absent, the field is `null` (not `[]`).

### 6.1 Null vs empty-array semantics

The distinction is load-bearing for the UI:

- `related_queries: null` → "telemetry table not yet available; render 'no telemetry data'".
- `related_queries: []` → "telemetry table present, this event has no associated queries (which is fine — most events don't)".
- `related_queries: [{...}]` → render the rows.

F4's `EventDetailDrawer` switches on this distinction (see §7.6).

### 6.2 Detection

F2's handler probes for the table once at startup:

```go
// sqlite_master detection cached in AppState; admin.schema_reload bumps the bool.
type queryResolutionsState struct {
    present bool
    mu      sync.RWMutex
}
```

A `SELECT 1 FROM sqlite_master WHERE type='table' AND name='query_resolutions' LIMIT 1` on startup populates the bool. The admin reload endpoint (existing pattern) re-probes. Per request the handler reads the cached value under a read-lock; no per-request `sqlite_master` lookup.

When `present` is true and the cross-join SQL errors at request time (table dropped concurrently, schema drift), the handler **logs and continues** — the request returns `related_queries: null` and a warning lands in structured logs. Best-effort, never fails the parent request.

### 6.3 Join shape

When the table is present:

```sql
SELECT qr.interaction_id, qr.query, qr.source_type
FROM query_resolutions qr
WHERE qr.write_event_ids LIKE '%' || ? || '%'
```

`LIKE` on a JSON-array column is the pre-FTS5 contract; F2 documents it and the sibling chain may extend `query_resolutions` with an indexed bridge table later. Best-effort, bounded latency.

---

## 7. Per-event-type detail-panel shape

Every event type in `docs/EVENT_CATALOG.md` gets a per-type renderer module in F3 at `apps/dashboard/src/components/shared/EventTimeline/per-type-renderers.tsx`. Each renderer is a pure function of the payload + actor + ts + entity:

```typescript
type EventTypeRenderer = (props: {
  payload: unknown      // type-discriminated per renderer; the dispatch is by event.type
  actor: Actor
  ts: string
  entity: EntityRef
  refs: Refs
}) => React.ReactNode
```

Unknown types render via a generic fallback (pretty-printed JSON of the payload, plus the rendered envelope fields). The fallback ensures forward compatibility: a new event type registered post-this-chain's-merge surfaces immediately, with a clean degraded view, rather than a hard error.

### 7.1 Bug lifecycle renderers

`BugReported` — surfaces `title`, `severity`, `surface`, `source` chips, problem statement (truncated to 200 chars with click-to-expand). Links the bug slug in the entity header.

`BugTriaged` — diff strip: `severity: high → critical`, `tags: [a, b] → [a, b, c]`. Per-field rows only for fields that changed.

`BugResolved` — discriminated by `kind`. `kind=fixed` shows `commit_sha` (truncated to 7 chars, clickable to copy full SHA). `kind=routed` shows the routed chain/task slug as a clickable deep-link. `kind=dup` shows `dup_of`. `kind=wontfix` shows the rationale as the explanation (rationale is always present on agent-resolved bugs).

`BugReopened` — shows `previous_resolution.kind` and `previous_resolution.commit_sha` (if any).

`BugEdited` — list of `updated_fields` as chips.

`BugStamped` — shows `commit_sha`.

### 7.2 Task lifecycle renderers

`TaskCreated` — chain slug + position + problem statement excerpt.

`TaskAssignedToChain` — diff strip: `chain: from_chain_slug → to_chain_slug`.

`TaskCompleted` — `commit_sha` + `closure_summary` (when present, expanded inline; truncate at 300 chars with expand).

`TaskCancelled` — `reason`, always shown verbatim.

`TaskTransitioned` — `status: from_status → to_status` chip pair; `blocker_slug` rendered as a clickable task deep-link when present.

`TaskEdited` — list of `updated_fields` chips.

`TaskStamped` — `commit_sha`.

### 7.3 Chain lifecycle renderers

`ChainCreated` — number of `tasks`, excerpt of `output` and `design_decisions`.

`ChainClosed` — `closure_summary` (always present, full-text).

`ChainEdited` — list of `updated_fields` chips.

### 7.4 Audit lifecycle renderers

`ArchitectureAuditCompleted` — `audit_doc` link, `summary`, `recommended_next_phase` (if set), then a findings table with one row per item: `item`, `status` (chip), `evidence` (truncated). Sortable by status within the drawer (pass-first or fail-first toggle).

`ConventionAuditCompleted` — same table shape, keyed on `axis` instead of `item` and `status ∈ {honored, partial, absent, n/a}`.

### 7.5 Benchmark lifecycle renderers

`BenchmarkRunStarted` — full T6 provenance bundle exposed as labeled rows:

| Label | Value |
|---|---|
| Scenario | `scenario_id` |
| Model | `provenance.model_id` (`model_version` as a small suffix chip) |
| Prompt template | `provenance.prompt_template_hash` (truncated, clickable copy) |
| Corpus | `provenance.corpus_hash` (truncated, clickable copy) |
| Retriever | `provenance.retriever_version` + `retriever_config_hash` |
| Seed | `provenance.seed` |
| Env | `provenance.env_hash` (truncated, clickable copy) |

This is the **load-bearing reason F1 anchored after T6 closure**: the renderer matches the post-T6 schema, so the UI doesn't need a rewrite when downstream consumers add T6-derived fields.

`BenchmarkRunCompleted` — `score`, `wall_clock_ms`, `input_tokens`, `output_tokens`, `tool_use_tokens`.

`BenchmarkRunFailed` — `error_kind` chip + `error_detail` (verbatim, pre-wrap).

`MetricRecorded` — `step_id`, `metric_name`, `metric_value` with `rationale` italic if present.

### 7.6 `EventDetailDrawer` shape (F4)

The audit-ledger row click opens an `<EventDetailDrawer event_id={id} />` (a drawer panel from the right edge, not a route change — preserves filter state). Layout:

```
┌─────────────────────────────────────┐
│ <type>  ·  <entity.kind>/<slug>  ✕ │
│ <ts>  ·  <actor.kind>:<actor.id>    │
├─────────────────────────────────────┤
│ Rationale                           │
│ ─────────                           │
│ <verbatim, pre-wrap, italic>        │
├─────────────────────────────────────┤
│ Payload                             │
│ ─────────                           │
│ <per-type renderer from §7.1–7.5>   │
├─────────────────────────────────────┤
│ Refs                                │
│ ─────────                           │
│ Caused by: <event link or none>     │
│ Related entities: <chips>           │
├─────────────────────────────────────┤
│ Observability                       │
│ ─────────                           │
│ Span: <SpanLink id=...>             │
├─────────────────────────────────────┤
│ Related queries                     │
│ ─────────                           │
│ <related_queries==null ?            │
│   "no telemetry available" :        │
│   <table of interaction rows>>      │
├─────────────────────────────────────┤
│ Deep links                          │
│ ─────────                           │
│ View entity timeline →              │
└─────────────────────────────────────┘
```

The "Deep links" section is the navigation seam back to F3's per-entity timeline: clicking jumps to `/bugs?slug=<slug>` (or chain equivalent) which scrolls/focuses the timeline section.

---

## 8. Component contracts

Three components, each with its prop shape, loading / error / empty states, and accessibility hooks.

### 8.1 `<EventTimeline />` (F3)

```tsx
interface EventTimelineProps {
  kind: 'bug' | 'task' | 'chain' | 'benchmark_run'
  slug: string
  project?: string
}
```

Behavior:

- Fetches `GET /entities/{kind}/{slug}/events?project=…` on mount and when the SSE event bus reports a matching event for this entity.
- Renders a vertical timeline; each entry runs through the per-type renderer (§7).
- **Loading state:** skeleton rows (3 placeholders) with `data-testid="event-timeline-loading"`.
- **Error state:** `<p role="alert">Failed to load timeline: <message></p>` with `data-testid="event-timeline-error"`.
- **Empty state:** `<p data-testid="event-timeline-empty">No events recorded for this <kind> yet.</p>` — used for entities created before the substrate landed or for kinds that don't emit (rare).
- **Accessibility:** the timeline is `<ol role="list">` (ordered list — chronological), each entry `<li role="listitem">` with `aria-label="{type} by {actor.kind}:{actor.id} at {ts}"`.
- **Performance budget:** > 50 events triggers "Show all (N)" expansion; default render is the latest 50. The "Show all" button increments the limit and re-fetches with no cursor (the entity timeline is short enough that re-fetching is cheaper than paginating client-side).

Integration points:
- `BugDetailPanel.tsx` — appended after the `routed-pointers` block (line 117 in current source).
- `ChainIndex/index.tsx` chain-detail right panel — appended after the existing chain prose blocks.
- Task detail (if `TaskDetail` component supports it; the existing `components/shared/TaskDetail/index.tsx` covers per-task display in the ChainIndex right panel) — appended below `handoff_output`.

### 8.2 `<EventDetailDrawer />` (F4)

```tsx
interface EventDetailDrawerProps {
  eventId: string | null     // null = drawer closed
  onClose: () => void
}
```

Behavior:

- Fetches `GET /events/{eventId}` on `eventId` change.
- Renders the layout in §7.6.
- **Loading state:** drawer opens with a skeleton.
- **Error state:** drawer renders error in place; close button is always functional.
- **Empty state:** N/A (drawer is gated on a specific event_id).
- **Accessibility:** drawer is `<aside role="dialog" aria-labelledby="drawer-title">`; Escape key closes; focus traps inside the drawer until close.

### 8.3 `<DispatchPolicyPeek />` (F5)

```tsx
interface DispatchPolicyPeekProps {
  // No props — page-level component reading from /admin/dispatch-policy.
}
```

Behavior:

- Fetches `GET /admin/dispatch-policy` on mount.
- Renders per-surface tabs (work / knowledge / measure / admin); each tab lists actions with a `requires_rationale` chip (`required` / `not required`).
- A "Reload from disk" button calls the existing `admin.schema_reload` action via the dispatch surface (or the dedicated admin reload endpoint if one exists), then re-fetches.
- **Loading state:** skeleton rows.
- **Error state:** `<p role="alert">Failed to load dispatch policy: <message></p>`.
- **Empty state:** N/A (policy is always present; an empty load means the file is missing — surface as error).
- **Accessibility:** tabs are `role="tablist"` with `role="tab"` children; each action row has `aria-label="{action} requires rationale: {yes|no}"`.

The optional cross-link to `action-docs-corpus` (per chain non-goal §1) is documented as a future hook: F5 leaves a `data-action-key={surface}.{action}` attribute on each row, so a later chain can attach a popover-fetch via DOM querying without re-rendering.

---

## 9. Route plan

The dashboard router (`apps/dashboard/src/router/index.tsx`) currently mounts pages under `/` via `AppShell`. F4 adds:

```tsx
{ path: 'audit', element: <AuditLedgerPage /> },
{ path: 'admin', element: <AdminPage />, children: [
    { index: true, loader: () => redirect('/admin/dispatch-policy') },
    { path: 'dispatch-policy', element: <DispatchPolicyPeekPage /> },
] },
```

`AdminPage` is a thin layout component with a sub-nav; F5 adds the first child route. Future admin sub-pages (action-docs-corpus link, schema-reload UI) slot under the same `/admin/*` umbrella.

Entity timelines are **not** a new route. They live as a section in the existing entity detail panel — the timeline appears whenever the operator opens a bug / chain / task detail in the existing routes.

`AppShell` nav additions (F4 and F5):
- `Audit` (top-level)
- `Admin` (top-level, with `Dispatch policy` sub-link)

**Why `/audit` over `/events`:** the chain framing is "audit ledger". `/events` collides with the existing SSE stream concept (lib/events.ts → `ToolkitEvent`). `/audit` reads as "the operator's review surface" and avoids the collision.

### 9.1 Filter URL encoding (`/audit?…`)

The audit ledger encodes filter state in the URL for shareability and back/forward navigation. Param names are short to keep shared links compact:

| Filter | URL param |
|---|---|
| `entity_kind` | `k` |
| `entity_slug` | `slug` |
| `type` | `t` (repeatable) |
| `project` | `p` |
| `span_id` | `s` |
| `actor_kind` | `ak` |
| `actor_id` | `aid` |
| `since` | `from` |
| `until` | `to` |
| `q` (rationale search) | `q` |
| `cursor` | `c` |

The page reads these via `useSearchParams`; state changes update the URL via `setSearchParams` (replace-state semantics so each filter tweak doesn't pollute history). The drawer's `event_id` lives in a separate `event=…` param so opening / closing the drawer doesn't disturb filter state.

### 9.2 Default view

The audit-ledger page defaults to last-24h events for the active project. The filter bar surfaces this state clearly ("Showing events from the last 24 hours · `from=<iso>`") so the operator doesn't think "nothing happened today" when actually a default narrowed the view. The "Clear all filters" button resets to "all events, no time bound".

---

## 10. Fallback matrix

Each cross-chain seam has a documented graceful-absence shape. The dashboard must boot and render every surface cleanly with the upstream absent.

| Seam | Upstream | Present shape | Absent shape |
|---|---|---|---|
| Span link click | T5 `SpansPanel` exists | `SpansPanel` reads `?span_id=` and filters to the matching span | F3/F4 still emit `<SpanLink>`; absent panel = navigation to `/spans` shows full live buffer; clipboard-copy fallback always fires |
| `related_queries` field | `query-telemetry-substrate` `query_resolutions` table | F2 LEFT JOINs and returns array | F2 returns `null` for the field; F4 drawer renders "no telemetry available" copy |
| Action-docs-corpus cross-link | `action-docs-corpus` chain closed | F5 attaches popover-fetch to `data-action-key` rows | F5 omits the cross-link; rows stand alone |
| Projection tables | T4 of `agent-first-substrate` (already closed) | Entity timelines join to projections for current-state | N/A — T4 is closed and load-bearing for the dashboard generally |
| Events table | T2 of `agent-first-substrate` (already closed) | Everything | N/A — without it the chain is moot |

The fallback contract is: **the dashboard never breaks because an upstream is absent**. F2's startup probe for `query_resolutions`, F4's UI conditional on `related_queries == null`, F5's omit-the-cross-link branch — every contact point with a sibling chain has the absent-case wired before the present-case is exercised.

---

## 11. TypeScript naming-collision disambiguation

The dashboard already uses "event" for the SSE bus concept (`lib/events.ts` → `ToolkitEvent` / `ToolkitEventKind`). This chain's "event" is a different concept — substrate-ledger rows. To avoid the collision, this chain uses the prefix `Audit*` on dashboard-side types:

| Concept | TS type | Source file |
|---|---|---|
| SSE bus event (existing) | `ToolkitEvent`, `ToolkitEventKind` | `apps/dashboard/src/lib/events.ts` |
| Substrate event envelope (new) | `AuditEvent`, `AuditEventListItem`, `AuditEventDetail` | `apps/dashboard/src/lib/auditEvents.ts` (new) |
| API client functions | `listAuditEvents`, `getAuditEvent`, `listEntityAuditEvents` | `apps/dashboard/src/api/auditEvents.ts` (new) |

Backend-side (Go) keeps the term `events` throughout — there's no collision in Go because the SSE bus uses `eventbus.Bus` (clearly distinct). The TypeScript collision is the only one to guard.

The `EventTimeline` component name is preserved (no `Audit` prefix) because the timeline reads naturally on an entity detail panel ("the bug's event history"); the type-level prefix carries the disambiguation where it matters (in code), and the UI label can stay user-friendly.

---

## 12. Open questions for review

Decisions proposed but the user may override before the doc lands.

1. **`/audit` vs `/events` vs `/audit-log`.** I propose `/audit` — short, semantic, no SSE-stream collision. Alternatives `/audit-log` (more descriptive but longer) or `/events` (collides with SSE).
2. **Default page size.** 50 (clamped 1..200). Reasonable for a dashboard list; lower than `bugs` page (200) because each row carries more content (rationale).
3. **`/events/list` vs `/events.json`.** I propose `/events/list` (path-suffix form). Alternative `/events.json` (content-type form). The path-suffix avoids any ambiguity with the SSE stream at `/events` and keeps URL grep-friendly.
4. **Cache TTL on event detail.** 24h. Alternative: forever (events are immutable). Bounded TTL hedges the `related_queries` lazy-population case; revisit when telemetry chain lands.
5. **Last-24h default on `/audit`.** Reasonable for an admin page; alternative is "show last 50 events" (no time bound). The time-bound default is clearer copy ("from last 24h") and the page header advertises it.
6. **`benchmark_metric` not in path-level entity routes.** F1 excludes `/entities/benchmark_metric/{slug}/events` because metric events are sub-events of a benchmark run, accessed via `caused_by_event_id` linking. Alternative: include the path for symmetry. Asymmetry preferred for now.
7. **Reload-from-disk on `<DispatchPolicyPeek />`.** F5 may delegate to `admin.schema_reload` or define a dedicated `GET /admin/dispatch-policy?reload=true` query. The latter avoids cross-action coupling; the former reuses an existing hot-reload primitive. Lean toward the former; F5 may revisit.
8. **`<SpanLink />` clipboard fallback.** I propose: clipboard write on every click (defensive). Alternative: clipboard only when the panel renders empty-state (less surprise but operator has to click twice). Lean toward the always-on defensive write.

---

## 13. Cross-references

- `docs/EVENT_SUBSTRATE.md` — envelope shape, append-only mechanics, projection contract. §2 (envelope), §6 (span_id), §7 (projection contract) are load-bearing for this design.
- `docs/EVENT_CATALOG.md` — per-type payload summaries; §7 here maps to it 1:1.
- `docs/PROJECTIONS.md` — projection fold and rebuild semantics; relevant for §4 read-source rules.
- `docs/OBSERVABILITY.md` — T5 span tree, `obs.SpanStart` API, the SpansPanel design.
- `docs/BENCHMARKS.md` — T6 provenance bundle; §7.5 anchors here.
- `apps/dashboard/src/router/index.tsx` — F4 mounts new routes; §9 here documents the additions.
- `apps/dashboard/src/lib/events.ts` — existing SSE type; §11 disambiguates against this.
- `apps/dashboard/src/components/shared/StatusBadge.tsx` — chip-style precedent; F3 renderers reuse the visual language.
- `apps/dashboard/src/components/shared/TaskDetail/` — integration point for the per-task EventTimeline section.
- `go/internal/observehttp/bugs.go` — F2 endpoint shape mirrors this; SQL discipline (`db.NewArgs`, no `SELECT *`) is the model.
- `go/internal/dispatch/policy/policy.go` — F5 reads from this package's `Registry`; the policy peek surfaces the loaded gates.
- Chain `query-telemetry-substrate` — cross-substrate join sibling; §6 here flags the seam.
- Chain `action-docs-corpus` — cross-link sibling; §8.3 leaves the hook.
- Vault `decisions/adapter-vs-extend-when-shapes-mismatch.md` — informs the F2 endpoint-extension choice over client-side reshape.
