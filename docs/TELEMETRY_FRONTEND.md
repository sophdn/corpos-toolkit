# Telemetry Frontend — Design

> **Status:** Draft for review. Produced by chain `query-telemetry-substrate-frontend` QF1 (`design-telemetry-frontend-surface`). Decisions here are durable; downstream tasks QF2–QF5 bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 three-axis enum disambiguation → §3 endpoint catalog → §4 pagination & filter shape → §5 read-source rules → §6 reuse/extend boundary with substrate-frontend → §7 component contracts → §8 route plan → §9 fallback matrix → §10 cross-substrate seam with audit ledger → §11 open questions.
>
> **Companion docs:** `docs/TELEMETRY_SUBSTRATE.md` (TT1 — substrate data model). `docs/TELEMETRY_LABEL_SPIKE.md` (TT1.5 — frozen click_kind 4-tier and label_kind 5-value enums). `docs/PROJECTIONS.md` §6.1 (TT3 LANDED — the three `query_*` projections this chain renders). `docs/TELEMETRY_RETROSPECTIVE_2026-05-17.md` (TT4 — chain-close audit, forward-fill caveats, seam-friction notes). `docs/SUBSTRATE_FRONTEND.md` (sibling chain — the audit-ledger surface this chain cross-links to).

---

## 1. What this surface is and isn't

`query-telemetry-substrate` (chain closed 2026-05-17) shipped the read-side audit substrate: `grounding_events` (the search calls), `query_interactions` (per-tier click signals), `query_resolutions` (terminal `BugResolved` / `TaskCompleted` events with JSON-array FKs back to `events.event_id`), and three `query_*` projections. Today that ledger is **SQL-only** — no dashboard reader exists for the read-side telemetry. Operators who want to know "what searches did the agent run today, and which ones led somewhere" must `sqlite3 toolkit.db` and write joins by hand. The ML pipeline author who needs to spot-check the reranker training corpus before fine-tuning has the same problem.

This chain ships the dashboard surfaces that make the read-side telemetry human-readable. Concretely: per-query trajectory view (the full agent-turn audit — query → results → clicks → resolution), an analytics page over the two analytics projections, and a training-pair browser the ML pipeline author can use to validate `proj_training_data_for_reranker` before training.

**Scope (and therefore this doc's scope):**

- HTTP endpoints serving the three `query_*` projections and the trajectory aggregator to the dashboard (`go/internal/observehttp/telemetry.go`, QF2).
- `<QueryTrajectoryView>` rendering one (query_id | span_id) deep-link page — the read-side leg (query + results + clicks) is new; the write-side leg (the resolution events) reuses substrate-frontend's per-event renderers (QF3).
- `/telemetry` analytics page with two recharts time-series surfaces over `proj_query_volume_by_source` and `proj_retrieval_success_per_query` (QF4).
- `/telemetry/training-pairs` read-only browser over `proj_training_data_for_reranker` with label-kind distribution stats + per-pair link to the originating trajectory (QF5).
- Entity-detail integration: `<EventTimeline>`'s per-event-type renderers (substrate-frontend F3) gain a "preceded by N queries" affordance under resolution events that expands to the per-query trajectory (QF3 + QF5).
- Closing self-hosting event `TelemetryFrontendAuditCompleted` emitted at QF5 — mirrors substrate-frontend's `SubstrateFrontendAuditCompleted` close pattern.

**Explicit non-goals:**

| Out of scope | Why |
|---|---|
| Training any ML model | Chain non-goal §1. The training-pair browser is read-only corpus validation. The cross-encoder fine-tune is `local-ml-roadmap.md` §1.1, a future ML chain. |
| Mutating UI ("label this resolution good/bad" editor) | The audit-doc framing names `query_interactions` as mutable (per-click_kind refinement) but the editor is feature-extend that follows from observed need. v1 read-only. |
| Surfacing query content for cloud-replication or external sharing | Privacy-by-design — query text is local-only at homelab scale per parent chain `design_decisions` §5. A cloud-export with redaction is a future chain. |
| Backfilling pre-substrate trajectories | TT1 §1.3 names this an explicit non-goal. Pre-substrate `grounding_events` rows lack `prompt_id` / `query_source` / `query_text`. Forward-only-capture; surfaces render empty for the period before 2026-05-15 with a one-line copy explaining. |
| Forensic correlation tooling (cross-prompt fan-out, statistical anomaly detection) | Power-user investigation stays SQL-side. The chain ships viewers, not analyzers. |
| FTS5 over `proj_retrieval_success_per_query.query_text` | Free-text search across queries is unindexed `LIKE` in v1. Promote to FTS5 if the table grows past ~10k rows / latency budget breached. |
| Replacing `proj_training_data_for_reranker` consumer SELECTs with a UI exporter | The pipeline reads the table directly. The browser is for spot-checking; bulk export is a script (`scripts/extract_reranker_training.py` already exists). |

---

## 2. THREE-AXIS DISAMBIGUATION (load-bearing for QF2–QF5)

Migration 037 + TT1 establish three orthogonal axes that the frontend MUST NOT conflate. Every chart, filter, and renderer names which axis it slices on, in the doc AND in the code. Legend rendering reads values from the data (no hardcoded enum) so forward-compat across all three holds.

| Axis | Column | Values today | What it means | What the frontend slices on it |
|---|---|---|---|---|
| **`action`** | `grounding_events.action` | `vault_search`, `kiwix_search`, `knowledge_search` (no CHECK; open enum) | Which search corpus was hit | Analytics chart **"Volume by corpus"** — line per `action` |
| **`query_source`** | `grounding_events.query_source` | `agent_initiated`, `proactive_hook`, `dashboard_user`, `other` (CHECK pinned in migration 037) | Who/what triggered the search | Analytics chart **"Volume by initiator"** — line per `query_source`. Training-pair filter (segregate models on different traffic shapes). |
| **`source_type`** | `knowledge_pointers.source_type` | `vault`, `kiwix`, `library`, `task`, `chain`, `bug` (no CHECK; open enum) | Kind of returned candidate | Per-result rendering in the trajectory view — dispatches to a per-source_type sub-renderer for the "what was returned" leg |

**Component prop contract:** every chart names the axis in a typed prop:

```tsx
interface AnalyticsChartProps {
  projection: 'query_volume_by_source' | 'retrieval_success_per_query'
  timeRange: { since: string; until: string }    // ISO-8601
  segmentBy: 'action' | 'query_source'           // load-bearing — never inferred from projection
}
```

The chart NEVER guesses which axis to segment on; the page renders one `<AnalyticsChart>` per (projection, segmentBy) pair, and the URL encodes the choice (`/telemetry?seg=action` vs `?seg=query_source`).

**Why this matters:** the live audit retrospective (TT4) calls out specifically the substrate-frontend chain locking column names in handlers before the substrate had committed the schema — and shipping `interaction_id` / `query` / `source_type` references that didn't match the canonical `resolution_id` / `entity_kind` / `entity_slug` shape (see bug `audit-ledger-related-queries-ts-go-field-drift`, filed against the audit ledger drawer during this design's drift sanity-check). The "name the axis in code" rule is the structural fix that prevents the same kind of silent-cell rendering in this chain.

**Forward-compat with new query_source values:** the canonical CHECK constraint on `grounding_events.query_source` is `('agent_initiated', 'proactive_hook', 'dashboard_user', 'other')` per migration 037. Chains like `reference-resolution-substrate` and `toolsearch-rerank-hook` produce searches that fall in the `other` bucket until they add their value to the CHECK via a schema migration. The legend reads `query_source` values from the projection rows, so a new value (`reference_resolution`, `toolsearch_rerank`) appears as a new line on the chart with no UI code change — but until the migration lands, those searches show up under `other`. QF4's legend copy explicitly names `other` as the "unbucketed" line so the operator isn't surprised.

**Forward-compat with new action values:** `grounding_events.action` has no CHECK constraint; new corpora can land without a schema migration. Same legend-from-data contract applies — a new `xrefs_search` value would render as a new line automatically.

---

## 3. Endpoint catalog

Five new endpoints land in `go/internal/observehttp/telemetry.go` (QF2). Mounted in `BuildRouter` (`go/internal/observehttp/router.go`) under the existing pattern.

### 3.1 `GET /telemetry/trajectories/{query_id}` — per-query full audit

Returns the complete agent-turn audit for one search call: query metadata, the returned result set, every `query_interactions` row that fired, and the IDs of any `query_resolutions` whose `grounding_event_ids` array contains this query.

Path parameter:
- `query_id` — integer matching `grounding_events.id`. Cheap pre-DB validation (`strconv.Atoi`); 400 with `{"error":"invalid query_id"}` on parse failure.

Alternate access pattern (for span-deep-links): `GET /telemetry/trajectories?span_id=<uuid>` — same response shape, lookup-by-`span_id` (which is unique per tools/call per migration 034). If the span has multiple `grounding_events` rows (rare but legal — one span could fire vault_search and kiwix_search), the response wraps them in `trajectories: [...]`; the single-query path returns one trajectory inline.

Response shape (`/telemetry/trajectories/{query_id}`):

```json
{
  "query": {
    "query_id": 42,
    "span_id": "9f8e7d6c-5b4a-3c2d-1e0f-aabbccddeeff",
    "prompt_id": "1f73b794-2a1b-4e59-a0d1-85477ed43b27",
    "session_id": "<uuid>",
    "parent_span_id": null,
    "project_id": "mcp-servers",
    "action": "vault_search",
    "query_source": "agent_initiated",
    "query_text": "telemetry projection rebuild semantics",
    "results_count": 5,
    "created_at": "2026-05-17T14:32:00.123Z"
  },
  "results": [
    {
      "position": 1,
      "source_ref": "learnings/mcp-servers/2026-05-17_substrate-rebuild-semantics.md",
      "source_type": "vault",
      "candidate_pointer_id": 1287
    }
  ],
  "interactions": [
    {
      "interaction_id": 901,
      "source_ref": "learnings/mcp-servers/2026-05-17_substrate-rebuild-semantics.md",
      "position": 1,
      "click_kind": "followed",
      "click_weight": 1.0,
      "citation_kind": null,
      "dwell_ms_estimate": 4200,
      "was_injected": 0,
      "detected_at": "2026-05-17T14:33:10.000Z"
    }
  ],
  "resolutions": [
    {
      "resolution_id": "019e3778-...",
      "entity_kind": "task",
      "entity_slug": "telemetry-substrate-cleanup-design",
      "entity_project_id": "mcp-servers",
      "outcome_kind": "completed",
      "write_event_ids": ["019e3778-0001-...", "019e3778-0002-..."],
      "detected_at": "2026-05-17T14:35:00.000Z"
    }
  ]
}
```

`results` is reconstructed from `grounding_events.source_refs` JSON column (the original result list) joined to `knowledge_pointers` for `source_type` lookup. `interactions` is the rows from `query_interactions` filtered by `grounding_event_id = query_id`. `resolutions` is the rows from `query_resolutions` whose `grounding_event_ids` JSON array contains `query_id` (SQL: `json_each` + `EXISTS`).

Errors:
- 404 `{"error":"query not found"}` when the integer is well-formed but absent.
- 400 `{"error":"invalid query_id"}` on parse failure.
- 400 `{"error":"either query_id (path) or span_id (query param) required"}` on the `?span_id=` form when no value is supplied.

### 3.2 `GET /telemetry/analytics/volume-by-source` — query volume time-series

Returns rows from `proj_query_volume_by_source` filtered by time range, ready to feed `<AnalyticsChart>`.

Query parameters:

| Param | Type | Default | Notes |
|---|---|---|---|
| `since` | RFC 3339 date (YYYY-MM-DD bucket-aligned) | 30 days ago | Lower bound on `day` column, inclusive. |
| `until` | RFC 3339 date | today | Upper bound on `day`, inclusive (bucket end). |
| `project` | string | absent | Mirrors existing project-scoping convention. |
| `segment` | `action` \| `query_source` | `action` | Names the slice axis; affects ONLY the response key shape, not the rows. |

Response shape:

```json
{
  "segment": "action",
  "buckets": [
    { "day": "2026-05-15", "segments": { "vault_search": 12, "kiwix_search": 3, "knowledge_search": 1 } },
    { "day": "2026-05-16", "segments": { "vault_search": 8, "kiwix_search": 5, "knowledge_search": 0 } }
  ],
  "totals_by_segment": { "vault_search": 20, "kiwix_search": 8, "knowledge_search": 1 }
}
```

For `segment=query_source`, the `segments` keys are `agent_initiated` / `proactive_hook` / `dashboard_user` / `other`. The response key shape is the same; only the keys differ. Forward-compat: any new value found in the projection appears as a new key without server-side enum churn.

### 3.3 `GET /telemetry/analytics/success-rate` — retrieval success time-series

Returns aggregated rollups over `proj_retrieval_success_per_query`. Bucketed by day server-side (the projection is row-per-query, not pre-aggregated by day — server does the GROUP BY).

Same `since` / `until` / `project` / `segment` params as §3.2. `segment` here is also `action` | `query_source`.

Response shape:

```json
{
  "segment": "action",
  "buckets": [
    { "day": "2026-05-15", "segments": {
        "vault_search": { "query_count": 12, "success_count": 9, "success_rate": 0.75 },
        "kiwix_search": { "query_count": 3,  "success_count": 1, "success_rate": 0.33 }
    } }
  ],
  "totals_by_segment": {
    "vault_search": { "query_count": 20, "success_count": 14, "success_rate": 0.70 }
  }
}
```

`success_rate` is `success_count / query_count`, computed server-side (single source of truth — the chart reads, doesn't compute). `success` per `proj_retrieval_success_per_query.success` is `max_click_weight >= 0.8 OR had_resolved_from = 1` (migration 038 + TT1 §6.2).

### 3.4 `GET /telemetry/training-pairs` — paginated training-pair browser

Returns rows from `proj_training_data_for_reranker` for the browser. Cursor-paginated on `training_id` (the autoincrement PK — monotonic).

Query parameters:

| Param | Type | Default | Notes |
|---|---|---|---|
| `label_kind` | string \| repeated | absent (all kinds) | One of `positive`, `weakly_positive`, `negative`, `hard_negative`, `unlabeled`. Repeatable. |
| `query_source` | string \| repeated | absent (all sources) | One of `agent_initiated`, `proactive_hook`, `dashboard_user`, `other`. Repeatable. |
| `project` | string | absent | Project scope filter. Reads through `grounding_events.project_id` via JOIN on `grounding_event_id`. |
| `q` | string | absent | Free-text `LIKE` on `query_text`, case-insensitive. Unindexed; budgeted for < 200ms at 10k rows. |
| `cursor` | int | absent | Pagination cursor on `training_id`. |
| `limit` | int | 50 | Clamped `[1, 200]`. |

Response shape:

```json
{
  "items": [
    {
      "training_id": 8801,
      "grounding_event_id": 42,
      "query_text": "telemetry projection rebuild semantics",
      "candidate_pointer_id": 1287,
      "source_ref": "learnings/mcp-servers/2026-05-17_substrate-rebuild-semantics.md",
      "candidate_position": 1,
      "label_kind": "positive",
      "weight": 1.0,
      "label_sources": ["followed", "resolved-from"],
      "query_source": "agent_initiated",
      "was_injected": 0,
      "prompt_id": "1f73b794-...",
      "span_id": "9f8e7d6c-..."
    }
  ],
  "next_cursor": 8901,
  "page_size": 50
}
```

`label_sources` is the JSON array column already on the projection — every `click_kind` that fired for this (query, candidate) pair. The browser surfaces it as chips so the spot-checker can see "this was labeled positive because `followed` AND `resolved-from` fired."

### 3.5 `GET /telemetry/training-pairs/stats` — corpus-shape banner

Returns the label distribution + per-segment counts for the corpus banner at the top of the training-pair browser. Honors the same `project`, `query_source`, and `label_kind` filters as §3.4 — distribution updates as the user narrows.

Response shape:

```json
{
  "total_pairs": 105,
  "by_label_kind": {
    "positive":         8,
    "weakly_positive":  3,
    "negative":        84,
    "hard_negative":   21,
    "unlabeled":        0
  },
  "by_query_source": {
    "agent_initiated":  98,
    "proactive_hook":    0,
    "dashboard_user":    0,
    "other":             7
  },
  "by_action": {
    "vault_search":     85,
    "kiwix_search":     20,
    "knowledge_search":  0
  }
}
```

**Five-value enum is canonical.** The corpus banner, the filter dropdown, and the per-row badge ALL enumerate all five values. The pre-TT1.5 four-value proposal (clicked / cited / resolved-from / ignored) is OBSOLETE and must not appear anywhere in QF2–QF5 code or tests.

### 3.6 Endpoint placement

```go
// go/internal/observehttp/router.go — added in QF2.
mux.HandleFunc("GET /telemetry/trajectories/{query_id}", state.telemetryTrajectory)
mux.HandleFunc("GET /telemetry/trajectories",            state.telemetryTrajectoryBySpan) // ?span_id=...
mux.HandleFunc("GET /telemetry/analytics/volume-by-source", state.telemetryVolumeBySource)
mux.HandleFunc("GET /telemetry/analytics/success-rate",     state.telemetrySuccessRate)
mux.HandleFunc("GET /telemetry/training-pairs",       state.telemetryTrainingPairs)
mux.HandleFunc("GET /telemetry/training-pairs/stats", state.telemetryTrainingPairsStats)
```

CORS inherits from `withCORS(mux)`. SQL discipline mirrors `bugs.go`/`events.go`: `db.NewArgs()` for parameter binds, explicit `SELECT` column lists (no `SELECT *`), `Cache-Control` matches the row's content-stability (cursor-pinned trajectory and training-pair pages are immutable → `public, max-age=300`; analytics buckets for past days are immutable → `public, max-age=300`; today's bucket and the latest training-pair page get `no-cache`).

---

## 4. Pagination and filter shape

### 4.1 Cursor pagination — two flavors

The chain spec calls for cursor-based pagination on two axes:

- **Write-side leg** (resolution events inside a trajectory): cursor on `event_id` (UUIDv7 lexicographic), descending or ascending per substrate-frontend §3.1. The trajectory view doesn't paginate its resolution list — a single query rarely emits > 5 resolutions; the list renders inline.
- **Read-side leg** (training-pair browser, trajectory query lists): cursor on `training_id` (`proj_training_data_for_reranker`) or `grounding_events.id`. Both are INTEGER AUTOINCREMENT PKs — monotonic by definition (SQLite AUTOINCREMENT guarantees monotonic), so `WHERE id < ? ORDER BY id DESC LIMIT N+1` produces stable pages.

### 4.2 Empty-state vs absent-state

- Empty array (`items: []`, `next_cursor: null`) means "filter narrowed to nothing" — render a one-line empty copy keyed to the active filters.
- A 200 with a wrapping summary banner ("0 of 0 pairs match") OR an empty arrange-the-page block: both are acceptable; QF5 picks one and sticks to it.
- The substrate being entirely empty (pre-substrate, no rows accumulated yet) is the same wire shape as a narrow filter — copy distinguishes via the projection-totals fetch (§3.5). If `total_pairs == 0` for the unfiltered case, the banner says "Telemetry substrate is live but no agent activity has been folded yet (forward-fill caveat — see `TELEMETRY_RETROSPECTIVE_2026-05-17.md` §forward-fill)" with a link to the doc.

### 4.3 URL-encoded filter state

The analytics and training-pair pages encode filter state in the URL for shareability + back/forward navigation. Param names match the API param names (no short-renaming as substrate-frontend's audit ledger does — these surfaces are narrower and don't need compact links). QF4/QF5 read via `useSearchParams`, write via `setSearchParams` with replace-state semantics.

| Page | Filters in URL |
|---|---|
| `/telemetry` (analytics) | `seg` (action\|query_source), `since`, `until`, `project` |
| `/telemetry/trajectories/{id}` | none (the path IS the deep-link) |
| `/telemetry/training-pairs` | `label_kind` (repeatable), `query_source` (repeatable), `project`, `q`, `cursor` |

---

## 5. Read-source rules — projections, not raw tables

| Surface | Reads from | Why |
|---|---|---|
| `<QueryTrajectoryView>` — query metadata + results + interactions | `grounding_events` + `query_interactions` + `knowledge_pointers` (direct JOIN) | The trajectory is per-query detail — not what projections are shaped for. Direct reads. |
| `<QueryTrajectoryView>` — resolutions list | `query_resolutions` (direct) | Same — per-query lookup. |
| `<QueryTrajectoryView>` — write-side leg (resolution event payloads) | `events` table via `GET /events/{event_id}` (substrate-frontend F2 endpoint) | Reuses substrate-frontend's per-event renderers; see §6. |
| `<AnalyticsChart>` (`/telemetry`) | `proj_query_volume_by_source`, `proj_retrieval_success_per_query` | TT3 projections shaped exactly for these charts. |
| `<TrainingPairBrowser>` (`/telemetry/training-pairs`) | `proj_training_data_for_reranker` | TT3 projection shaped for this surface. |
| Training-pair stats banner | `proj_training_data_for_reranker` + GROUP BY in handler | Single-table aggregate; cheap at homelab scale. |

The trajectory view reads raw substrate tables because there's no projection shaped per-query — projections are for analytics/training. Per-query reads are point lookups (`WHERE id = ?`), not scans, so the cost is the same.

Explicit columns in every `SELECT`, no `SELECT *` — matches the SQL discipline in `bugs.go`/`events.go`/`projections/*.go`.

---

## 6. Reuse-vs-extend boundary with substrate-frontend

`docs/SUBSTRATE_FRONTEND.md` describes the agent-substrate-frontend component family. This chain's surfaces consume those components rather than reinventing them, but only where the data axis matches.

### 6.1 What this chain REUSES

- **`renderEventPayload`** (`apps/dashboard/src/components/shared/EventTimeline/per-type-renderers.tsx`, exported pure function): used by `<QueryTrajectoryView>` to render the write-side leg of the trajectory. For each event_id in a `query_resolutions.write_event_ids` JSON array, the trajectory view calls `GET /events/{event_id}` (substrate-frontend §2.2) to hydrate the full event detail, then dispatches the rendering to `renderEventPayload(eventDetail)`. No new per-event renderer for telemetry — we get the bug-resolved / task-completed / chain-closed rendering for free.
- **API client** `getAuditEvent(eventId)` from `apps/dashboard/src/api/auditEvents.ts`: the trajectory view's resolution-leg fetcher.
- **Type `AuditEventDetail`** from `apps/dashboard/src/lib/auditEvents.ts`: the trajectory view holds an array of these for the write-side leg. (Note: this type's `related_queries` field has known field-shape drift — see §10 — but the rest of the envelope is stable and used as-is.)
- **`<EventTimeline>` component**: NOT reused at the trajectory-view level — `<EventTimeline>` takes `{kind, slug, project}` and fetches the entire timeline for one entity; the trajectory view fetches by `event_id` list. They're different access patterns. `<EventTimeline>` IS reused on entity-detail panels (Bug/Task), and this chain EXTENDS its per-type renderers (see §6.3) to surface the cross-substrate "preceded by N queries" affordance.
- **Sidebar / AppShell** patterns: a new `Telemetry` sidebar entry follows the existing `<NavLink>` pattern in `apps/dashboard/src/components/layout/Sidebar/index.tsx`.
- **Recharts** as the charting library: `proj_query_volume_by_source` and `proj_retrieval_success_per_query` use the same `recharts` import surface that `pages/Benchmarks/index.tsx` already pulls in (`recharts ^3.8.1` in `apps/dashboard/package.json`). No new chart-library dep.

### 6.2 What this chain EXTENDS (per-event-type renderers)

The substrate-frontend per-type renderers (`per-type-renderers.tsx`) emit one chip / line / block per event payload. This chain adds a **suffix block** at the bottom of each `BugResolved` / `TaskCompleted` / `ChainClosed` renderer:

```
[ existing renderer output ]
─────────────────────────────
preceded by 3 queries  ▾    ← click to expand the trajectory
```

The expansion calls `GET /telemetry/trajectories?event_id=<event_id>` (a new wrinkle on §3.1 — server-side does the JSON-array contains lookup) and renders an inline collapsed trajectory. The "preceded by N queries" count is fetched lazily on first expansion (cheap; bounded result set).

This extension is **additive**, not invasive: substrate-frontend's per-type renderers continue to render whether telemetry is present or absent. The "preceded by N queries" affordance is conditional on the response being non-empty; when no queries lead to a resolution, the suffix block doesn't render.

Cross-chain coordination: the extension lives in the same file (`per-type-renderers.tsx`) so the cross-substrate join is co-located with the write-side renderer. QF3 takes ownership of the file edit; substrate-frontend's chain is closed and the file is now jointly owned.

### 6.3 What this chain BUILDS NEW

The read-side leg (the agent's query + the result set + the click signals) has no analog in the audit ledger and gets new components:

- **`<QueryHeader>`** — query metadata block (text, action, query_source chip, timestamp, span_id deep-link)
- **`<ResultList>`** — ordered result list; each row dispatches to `<ResultRow source_type="vault|kiwix|library|task|chain|bug">` for source-type-specific rendering (e.g., `vault` rows render a markdown-style "learnings/<corpus>/<date>_<slug>.md" pill; `bug` rows render a bug-slug chip linking to `/bugs?slug=<slug>`)
- **`<InteractionList>`** — chronological list of `query_interactions` rows; each row shows the click_kind chip, weight, source_ref, dwell_ms_estimate, and was_injected badge
- **`<LabelKindBadge>`** — chip with one of `positive` / `weakly_positive` / `negative` / `hard_negative` / `unlabeled`, color-coded; used in `<TrainingPairBrowser>` AND in the trajectory view's per-result decoration ("this candidate was labeled `positive` because `followed` AND `resolved-from` fired")
- **`<LabelSourcesChips>`** — renders the `label_sources` JSON array as a stack of click_kind chips
- **`<TrajectoryDeepLink>`** — `<a href="/telemetry/trajectories/{query_id}">` with title attribute showing the query text; used in entity-detail expansions and in the training-pair browser to jump to the originating trajectory

---

## 7. Component contracts

### 7.1 `<QueryTrajectoryView />`

```tsx
interface QueryTrajectoryViewProps {
  queryId?: number     // grounding_events.id
  spanId?: string      // alternative lookup; mutually exclusive with queryId
}
```

Exactly one of `queryId` / `spanId` must be set; the other is `undefined`. TypeScript can't express the XOR at the type level cleanly without a discriminated union; QF3 uses a runtime assert + a discriminated-union type internally.

Behavior:
- Fetches `GET /telemetry/trajectories/{queryId}` or `?span_id=…` on mount.
- Renders four sections top-to-bottom: `<QueryHeader>`, `<ResultList>`, `<InteractionList>`, write-side resolutions block.
- The resolutions block iterates `query_resolutions[]`; for each, it expands `write_event_ids` and fetches each event via `getAuditEvent(eventId)`, then renders via `renderEventPayload`.
- **Loading state:** skeleton blocks per section (4 placeholders) with `data-testid="trajectory-loading"`.
- **Error state:** `<p role="alert">Failed to load trajectory: <message></p>` with `data-testid="trajectory-error"`.
- **Empty state:** the query exists but has zero interactions and zero resolutions — render the header + result list + a "No clicks fired and no resolution attached to this query yet" copy. This is a real state (in-flight queries) and not an error.
- **Accessibility:** the four sections are `<section aria-labelledby="…">`; each section has a heading; result rows are `<li>` inside `<ol role="list">`.
- **Performance:** result list expected < 25 rows (typical search returns 5); interaction list expected < 10 rows. No client-side pagination at the trajectory level.

### 7.2 `<AnalyticsChart />`

```tsx
interface AnalyticsChartProps {
  projection: 'query_volume_by_source' | 'retrieval_success_per_query'
  timeRange: { since: string; until: string }
  segmentBy: 'action' | 'query_source'
}
```

Behavior:
- Fetches `GET /telemetry/analytics/volume-by-source` or `…/success-rate` on mount and on prop change.
- Renders a multi-line `recharts` `<LineChart>` — one line per segment value, x-axis is `day`, y-axis is `query_count` (volume chart) or `success_rate` (success-rate chart). The legend lists segments in alphabetical order.
- **Loading state:** chart container with a skeleton shimmer.
- **Error state:** `<p role="alert">…</p>`.
- **Empty state:** `<p>No queries in this time range for the selected project.</p>` — distinguishes from forward-fill-empty (a meta-empty surfaced by the page wrapper, not the chart).
- **Accessibility:** chart container is `role="img" aria-label="Query volume by {segmentBy} from {since} to {until}"`. The recharts `<Tooltip>` exposes hover data; a textual data table is rendered below the chart at `<details>`-collapsed default so screen readers can access the bucket values.
- **SSE-aware:** subscribes to `useEventBus(['grounding_event_recorded'])` (NOTE: this event type does not exist yet in the events ledger; if it doesn't ship by QF4, the chart polls on a 30s timer instead — see §9 fallback matrix).

### 7.3 `<TrainingPairBrowser />`

```tsx
// page-level component, no props. Reads filters from useSearchParams.
```

Behavior:
- Top banner: stats from `GET /telemetry/training-pairs/stats`. Renders label_kind distribution as a 5-cell mini-bar.
- Filter bar: `<select multiple>` for `label_kind`, `<select multiple>` for `query_source`, free-text input for `q`, project picker (existing component).
- Body: paginated list of training-pair cards. Each card shows query_text + the candidate (source_ref + source_type) + `<LabelKindBadge>` + `<LabelSourcesChips>` + `<TrajectoryDeepLink>`.
- **Loading state:** skeleton cards (5 placeholders).
- **Error state:** `<p role="alert">…</p>` in body; banner falls back to "—" cells.
- **Empty state:** distinguishes forward-fill-empty (no rows at all) from filter-narrowed-empty (rows exist, filter eliminated them); see §4.2.
- **Accessibility:** filter bar is `<form role="search">`; cards are `<li role="listitem">` inside `<ol>`; the label_kind badge has `aria-label="label kind: positive (max click weight 1.0)"`.
- **Performance budget:** 50 cards per page, < 300ms server response at 10k rows.

---

## 8. Route plan

Routes added to `apps/dashboard/src/router/index.tsx` (flat shape, matching the existing pattern — no nested children block):

```tsx
{ path: 'telemetry', element: <TelemetryAnalyticsPage /> },                       // QF4
{ path: 'telemetry/trajectories/:queryId', element: <QueryTrajectoryViewPage /> }, // QF3
{ path: 'telemetry/training-pairs', element: <TrainingPairBrowserPage /> },        // QF5
```

The `?span_id=` form of trajectory lookup uses the same `:queryId` page; QueryTrajectoryViewPage checks for an absent path param + present `span_id` query param and switches its fetch path.

**Sidebar nav additions** (`apps/dashboard/src/components/layout/Sidebar/index.tsx`):

```tsx
<NavLink to="/telemetry">Telemetry Analytics</NavLink>
<NavLink to="/telemetry/training-pairs">Training Pairs</NavLink>
```

The trajectory view is NOT in the sidebar — it's a deep-link target only (operators arrive via the analytics page, the training-pair browser, or the audit-ledger event-detail expansion).

**Why not `/audit/queries/<id>` (the chain spec's alternative):** keeping `/telemetry/*` as its own top-level segment preserves the read-side / write-side framing the substrate ships under. `/audit` is the write-side ledger; `/telemetry` is the read-side ledger. Operators don't have to think about whether their question is a "query about audit" or "audit about a query" — the URL path picks the lens.

**Why not fold into `/knowledge`:** `/knowledge` is a single-card overview of the knowledge-pointer index — wrong granularity for the multi-page telemetry surface. The two pages are sibling top-level peers.

**Why not move `<EventTimeline>` to `/telemetry`:** `<EventTimeline>` is the entity-detail timeline (chronological history of one bug/task/chain); it's the write-side audit and lives where the entities live (BugDetailPanel, ChainIndex). Telemetry pages don't render `<EventTimeline>` directly; they reuse only the per-event-type renderer functions for the cross-substrate suffix block (§6.2).

---

## 9. Fallback matrix

| Seam | Upstream | Present shape | Absent shape |
|---|---|---|---|
| `renderEventPayload` per-type renderers | substrate-frontend F3 (closed 2026-05-17) | `<QueryTrajectoryView>` write-side leg renders full payloads | Falls back to JSON-stringified payload in a `<pre>` block. The substrate-frontend chain is closed, so this is forward-compat against future deletion, not present-day absence. |
| `<EventTimeline>` suffix block on resolution events | substrate-frontend F3 + this chain's extension in §6.2 | "preceded by N queries" affordance appears under resolution events | If the cross-substrate join returns empty (forward-fill caveat: the substrate is live but no agent activity has been folded yet), the suffix block doesn't render. No error, no copy — just absent. |
| `getAuditEvent` API client | substrate-frontend F2 (closed) | Trajectory view's write-side leg fetches event details | Forward-compat against future deletion; the trajectory view's resolutions block degrades to showing `resolution_id` + `outcome_kind` only, no event payload. |
| `<SpansPanel>` `?span_id=` deep-link | `agent-first-substrate` T5 + substrate-frontend F4 (both closed) | `<QueryHeader>` span_id chip jumps to `/spans?span_id=<id>` | Same graceful absence as substrate-frontend §5.2 — the link clipboard-copies the span_id on click as defensive fallback. |
| `grounding_event_recorded` SSE event | NEW event type — not yet emitted | Analytics chart re-fetches on every new query | Falls back to a 30s `setInterval` poll. QF4 ships polling first; SSE invalidation lands as a follow-on when the event type is added to the grounding-events processor. |
| `proj_training_data_for_reranker` rows | TT3 (closed) | Training-pair browser shows real corpus | If the projection is empty (forward-fill caveat), the banner explains and the body shows the empty state with the forward-fill-context link. |
| `query_resolutions.grounding_event_ids` JSON array | TT2 (closed) | Trajectory view shows the resolutions; entity-detail expansion shows preceded queries | If the array is `'[]'`, the resolutions block renders "No resolutions linked to this query (yet — forward-fill caveat)". |

**The fallback contract:** the dashboard never breaks because an upstream is absent OR because the substrate is empty. The forward-fill caveat (TELEMETRY_RETROSPECTIVE_2026-05-17.md §forward-fill) is structurally absorbed by the empty-state copy on every page.

---

## 10. Cross-substrate seam — known drift with substrate-frontend's audit ledger

The substrate-frontend audit ledger drawer has a `related_queries` field on `GET /events/{event_id}`. That field's wire shape today (Go `relatedQuery` struct in `go/internal/observehttp/events.go:102-108`) is `{resolution_id, entity_kind, entity_slug, outcome_kind, prompt_id}` — a per-RESOLUTION record. The dashboard side (`apps/dashboard/src/lib/auditEvents.ts:43-47`) still declares the obsolete shape `{interaction_id, query, source_type}`, and `AuditLedger/index.tsx` renders columns that read fields that don't exist. **This is a real shipped bug** filed as `audit-ledger-related-queries-ts-go-field-drift` during this design's drift sanity-check (2026-05-17). Sibling-chain concern; this chain does not fix it.

**Implication for this chain:** the trajectory view's deep-link affordance from the audit-ledger drawer (clicking "this event has related queries" should jump to a trajectory view) cannot use the existing `related_queries` field today, because the wire shape lacks `grounding_event_id`. Two paths forward:

- **Path A** (this chain's default): the entity-detail "preceded by N queries" suffix (§6.2) is the integration point — it's a NEW endpoint (`?event_id=<id>`) that does its own JSON-array containment lookup against `query_resolutions.grounding_event_ids`, so it doesn't depend on the existing `related_queries` field. The audit-ledger drawer's `related_queries` block continues to render the resolution-shape rows (after its bug is fixed), and gains a "see trajectory" link per row in a separate follow-up.
- **Path B** (deferred): extend `relatedQuery` struct on the Go side to include `grounding_event_id`, fix the TS shape mismatch, and let the drawer cross-link directly. Bigger change, owned by the bug's fix; not blocking.

QF1 commits to Path A. The seam works without waiting on the bug; the bug's fix adds a cross-link separately.

---

## 11. Open questions

Decisions proposed but the user may override before downstream tasks land.

1. **`/telemetry` vs `/audit/telemetry` vs `/read-audit`.** I propose `/telemetry` — short, semantic, sibling to `/audit`. Alternatives `/audit/telemetry` (nested under audit) collide with the read-side / write-side framing the substrate ships under.
2. **Default time range on analytics.** 30 days. Alternative: 7 days (matches the forward-fill cadence in TELEMETRY_RETROSPECTIVE §forward-fill). 30 chosen as the larger window so the operator sees the substrate's bake-in shape; the page header advertises the range.
3. **SSE event for analytics chart refresh.** A new `grounding_event_recorded` SSE event would let the chart re-render reactively. Today the chain's `completion_condition` (c) says "charts re-render reactively on new query_volume_by_source values without page reload" — this is the only way to satisfy that with the substrate as-is. Polling is the v1 fallback; SSE is a small follow-on the Go grounding-events-processor adds (one emit at the end of each `processFile`).
4. **Training-pair browser pagination size.** 50 cards per page (matches the audit-ledger default). Alternative: 20 (cards are larger than audit rows). 50 chosen for parity; QF5 may revisit after first usability pass.
5. **`<LabelKindBadge>` color palette.** Lean on existing `<StatusBadge>` patterns (`apps/dashboard/src/components/shared/StatusBadge/`): positive=green, weakly_positive=yellow-green, negative=red-muted, hard_negative=red-strong, unlabeled=gray. Confirmed: matches the visual language already on the dashboard.
6. **Per-result decoration in trajectory view.** Should each result row show its `<LabelKindBadge>`? Pro: makes the labeling outcome visible at trajectory inspection time. Con: re-fetches the projection for one query just to decorate. I propose YES — the trajectory endpoint (§3.1) does the JOIN server-side and returns `label_kind` per result, so no extra fetch.
7. **Routes don't carry `:project`.** Project scoping comes from the active `<ProjectPicker>` selection (header-bar component) rather than the URL — matches the existing dashboard pattern (no other page puts project in the path). Alternative: include `project_id` in the path for `/telemetry/trajectories/{project}/{queryId}`. Rejected: trajectory query_ids are globally unique INTEGER PKs, so the project segment is redundant; the project picker filters analytics + browser pages by reading from context.
8. **Free-text search latency budget on `/telemetry/training-pairs`.** 200ms at 10k rows. If we cross 10k pairs and latency degrades, promote to FTS5 over `query_text`. Captured as a future-watch, not a current decision.

---

## 12. Cross-references

- `docs/TELEMETRY_SUBSTRATE.md` — TT1 substrate design.
- `docs/TELEMETRY_LABEL_SPIKE.md` — TT1.5 enum closures (click_kind 4-tier CONFIRM; label_kind 5-value REVISE).
- `docs/PROJECTIONS.md` §6.1 — TT3 LANDED projections; the cross-encoder reranker worked-example consumer SELECT.
- `docs/TELEMETRY_RETROSPECTIVE_2026-05-17.md` — TT4 closing audit; forward-fill caveats; cross-chain seam-friction history.
- `docs/SUBSTRATE_FRONTEND.md` — sibling chain's design doc; this doc cross-links to §2.2 (`/events/{event_id}`), §7 (per-event renderers), §8.1 (`<EventTimeline>`).
- `docs/EVENT_CATALOG.md` — event-type catalog; `TelemetryFrontendAuditCompleted` registers here at QF5 (mirrors `SubstrateFrontendAuditCompleted`).
- `crates/shared-db/migrations/037_telemetry_substrate.sql` — query_interactions + query_resolutions + new grounding_events columns + click_kind / query_source CHECK constraints.
- `crates/shared-db/migrations/038_telemetry_projections.sql` — proj_query_volume_by_source + proj_retrieval_success_per_query + proj_training_data_for_reranker + label_kind 5-value CHECK.
- `go/internal/projections/query_volume.go` / `query_success.go` / `query_training.go` — the projection implementations this chain's endpoints read.
- `apps/dashboard/src/components/shared/EventTimeline/per-type-renderers.tsx` — the per-event-type renderer module this chain extends (§6.2).
- `apps/dashboard/src/lib/auditEvents.ts` / `apps/dashboard/src/api/auditEvents.ts` — the substrate-frontend type and client this chain reuses.
- `apps/dashboard/src/pages/Benchmarks/index.tsx` — `recharts` precedent for chart construction.
- `apps/dashboard/src/router/index.tsx` — route registration site.
- `apps/dashboard/src/components/layout/Sidebar/index.tsx` — sidebar nav addition site.
- Bug `audit-ledger-related-queries-ts-go-field-drift` — known drift in sibling chain's surface; not blocking this chain.
- `~/.claude/vault/learnings/general/2026-05-17_tiered-implicit-feedback-for-rag-telemetry.md` — the four-tier click_kind pattern referenced by TT1 §5.
