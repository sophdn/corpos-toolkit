# Reference Resolution Frontend — Design

> **Status:** Draft for review. Produced by chain `reference-resolution-substrate-frontend` RF1 (`design-reference-resolution-frontend`). Decisions here are durable; downstream tasks RF2–RF4 bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 three-axis disambiguation + the shape-category superset overlay → §3 endpoint catalog → §4 pagination & filter shape → §5 read-source rules → §6 reuse-vs-extend boundary with sibling frontends → §7 component contracts → §8 route plan → §9 entity-detail integration (the `prompt_id` join) → §10 fallback matrix → §11 forward-compat for T7 ML scores → §12 cross-substrate seam → §13 open questions → §14 cross-references.
>
> **Companion docs:** `docs/REFERENCE_RESOLUTION.md` (the substrate this surface reads from — particularly §2 shape taxonomy, §5 the `resolve_references` action, §6 telemetry integration). `docs/TELEMETRY_SUBSTRATE.md` (TT1 — the read-side telemetry tables and three-layer span hierarchy this surface joins against). `docs/TELEMETRY_FRONTEND.md` (QF1 — the canonical three-axis disambiguation and the `<QueryTrajectoryView />` this surface reuses). `docs/SUBSTRATE_FRONTEND.md` (F1 — the `<EventTimeline />` and the entity-detail integration pattern this surface extends).
>
> **Cross-chain dependencies (all closed or shipped at design-time):** `agent-first-substrate` T1+T5 (event envelope + `span_id`). `query-telemetry-substrate` TT1+TT2+TT3 (telemetry shape, three-layer hierarchy, projections). `reference-resolution-substrate` T1–T9 (substrate; T5 migration 040 widened `query_source` to admit `'reference_resolution'`; T9 migration 041 added `'harness_reminder_interception'`). `agent-substrate-frontend` F1–F5 (audit ledger + `<EventTimeline />` shipped 2026-05-17). `query-telemetry-substrate-frontend` QF1–QF5 (telemetry pages + `<QueryTrajectoryView />` shipped 2026-05-18; live at `apps/dashboard/src/pages/QueryTrajectoryView/`).

---

## 1. What this surface is and isn't

`reference-resolution-substrate` (chain in progress, T1–T9 shipped) added a reference detector + resolver registry + the `knowledge(action='resolve_references', ...)` MCP action + a CHECK-widening migration that makes `'reference_resolution'` a first-class `grounding_events.query_source` value. Today every detection + dispatch lands one `grounding_events` row per detected reference with `query_source='reference_resolution'`, joined to `query_interactions` rows for citation outcomes, in the same telemetry surface that already feeds the QF4/QF5 pages. **No dashboard reader is scoped to it.** Operators who want to know "which references did the agent detect this turn, which got resolved, how, and what did the agent end up surfacing" must `sqlite3 toolkit.db` and write the joins by hand. The reranker-training author who wants to spot-check reference-resolution-sourced training pairs before they ship can filter the existing `/telemetry/training-pairs` page on `query_source='reference_resolution'` — but the per-resolution detail (which resolver fired, what candidates came back, what `PresentedAs` string surfaced) lives in the substrate handler's return envelope and is **not** indexed in the telemetry tables; the dashboard would only see the `grounding_events` shadow.

This chain ships the missing reader: a **Context Pull Inspector** that scopes to `query_source='reference_resolution'`, exposes the detection → resolution → presentation trajectory per reference, and integrates onto Bug/Task detail panels so operators can ask "what references resolved while the agent worked on this entity." It is the third frontend in the substrate-trilogy-frontend family — sibling to `agent-substrate-frontend` (write-side audit ledger) and `query-telemetry-substrate-frontend` (read-side telemetry).

**Scope (and therefore this doc's scope):**

- HTTP endpoints under `/context-pulls/*` serving reference-resolution-specific views to the dashboard (`go/internal/observehttp/context_pulls.go`, RF2).
- `<ContextPullInspector />` page at `/context-pulls` — filterable list of detected references with their resolution outcomes (RF3).
- `<ResolutionDetailDrawer />` — per-resolution detail drawer: the full detection context, the resolver invoked, the candidate list, the chosen result, the `PresentedAs` surfaced to the agent, and a deep-link to the originating query trajectory (RF3).
- Entity-detail integration on Bug + Task panels via `<EventTimeline />` extension: "N references resolved in this prompt" badge under resolution events, click to expand inline (RF3).
- Closing self-hosting event `ReferenceResolutionFrontendAuditCompleted` emitted at RF4 — third closing event in the substrate-trilogy-frontend family.
- Forward-compat hooks for T7 ML scores: `<ConfidenceColumn />` reads `score` from the data when present; absent today.

**Explicit non-goals:**

| Out of scope | Why |
|---|---|
| A "tune the detector" UI | The detector's rule set (shape regexes, catalog match, friction patterns) is code-level. The dashboard is **read-only** — same posture as `<QueryTrajectoryView />` for the telemetry substrate. Tuning is a code change, not a dashboard knob. |
| Re-implementing the trajectory view | `<QueryTrajectoryView />` (QF3, shipped) already renders one (query | span)→results→clicks→resolution leg from the `grounding_events`/`query_interactions`/`query_resolutions` triple. Each reference-resolution call **is** a kind of search trajectory (one `grounding_events` row per detected reference); the inspector drawer **reuses** `<QueryTrajectoryView />` for the lifecycle leg rather than reimplementing it. |
| Surfacing harness-reminder-interception outcomes (T9 fires) | `query_source='harness_reminder_interception'` rows DO exist post-T9 (migration 041), but they're hook-side behavior measuring a Claude Code harness defense — analytically orthogonal to "what did the agent pull from the unified knowledge surface". If/when an operator wants that view, it's a small follow-on (filter the same inspector by source instead of forking the page). v1 inspector defaults to filtering for `reference_resolution` only. |
| Editing or labeling resolutions | Read-only validation, same scope cut as QF1. The substrate gates the data; the dashboard renders it. Any "this resolution was wrong" workflow is a follow-on chain. |
| Exporting context to external tools | Local-only. Mirrors the QF1 / F1 privacy posture for the same homelab-scale reasons. |
| Backfilling pre-substrate resolutions | T5 stamps `query_source='reference_resolution'` starting at migration-040 land time. Rows before that point are agent-initiated knowledge calls that happen to share the resolve_references shape; they are not in scope and the inspector's empty-state copy advertises the forward-only-capture caveat. |
| Per-resolver performance tuning panels | Resolver budgets, dispatcher latency caps, and Cost()-hint values are tuned in code per the substrate doc §3.6. The inspector surfaces `RetrievalCostMs` per resolution as a column but does NOT plot resolver-cost distributions; that's an analyzer concern (out of scope per QF1 §1's "viewers, not analyzers" cut). |

---

## 2. THREE-AXIS DISAMBIGUATION (load-bearing for RF2–RF4)

The substrate-trilogy-frontend convention, established in TELEMETRY_FRONTEND.md §2 and binding here verbatim: every chart, filter, and renderer names which orthogonal column it slices on. Component prop names + filter param names use the exact column name. **No generic `source` / `kind` that conflates the axes.**

| Axis | Column | Values relevant to this surface | What it means | What this chain slices on it |
|---|---|---|---|---|
| **`query_source`** | `grounding_events.query_source` | `reference_resolution` (the inspector's primary filter — every page-load defaults to this), `harness_reminder_interception` (opt-in via URL param), `agent_initiated` / `proactive_hook` / `dashboard_user` / `other` (visible but de-emphasized) | Who/what initiated the search call | The page's "source" filter chip — defaults to `reference_resolution` so the surface is scoped to this chain's emits without operator action. Forward-compat: a new value in migration 042 etc. appears as a chip option automatically (legend reads from data, no UI code change). |
| **`action`** | `grounding_events.action` | For reference-resolution rows the action is always the literal string `resolve_references` today (the handler writes a synthetic action name per substrate doc §6.2). When the kiwix_bridge / vault_candidate resolvers fire, the underlying corpus hits are NOT re-stamped — they remain `resolve_references` because the substrate's emit posture is "one row per detected reference", not "one row per underlying retrieval pass." | Which corpus was hit; or, here, the wrapper action that fanned out to resolvers | Rendered per-result for context; surfaced in the drawer header. Filter dropdown available but typically not load-bearing for this chain (one value dominates). When `action != 'resolve_references'`, the row is shown but the inspector marks it "non-resolution call surfaced because query_source matched" — see §3.1 fallback. |
| **`source_type`** | `knowledge_pointers.source_type` | `vault`, `kiwix`, `library`, `task`, `chain`, `bug` (the existing six, joined to `grounding_events.source_refs` via `knowledge_pointers`) | The candidate-side pointer kind for **resolved** candidates | Per-result rendering in the trajectory leg of the drawer (dispatch to a per-`source_type` sub-renderer); ONE of the two filter dropdowns on the page. |

### 2.1 The fourth, page-local axis: detected-reference `shape`

Reference-resolution adds a structurally distinct axis the sibling chains don't have: the **detector's classification of the input token**, before any resolver runs. This is `refresolve.Reference.Shape` (`go/internal/refresolve/types.go:14-38`) and is **not** a column on `grounding_events` directly — it is recoverable from the row in two ways:

1. **Synthetic `call_id` prefix.** Per substrate doc §6.6 T5 implementation notes, the handler synthesizes `call_id = "<span>#r<i>"` to keep the `(session_id, call_id)` UNIQUE constraint satisfied across N rows from one `tools/call`. Today the `call_id` does NOT encode the shape. RF2 lands a small substrate-side amendment (§3.6 below): emit a side-table `reference_resolution_emits` keyed on `grounding_event_id` with columns `shape`, `confidence_tier`, `presentation_recommendation`. The inspector reads from this side-table; analytics queries that scope by shape join through it.
2. **Source_ref convention.** Per substrate doc §3 every Candidate's `SourceRef` field is prefix-encoded (`chain:<slug>`, `vault:<path>`, `skill:<id>`, etc.). The inspector can recover an *approximate* shape from the source_ref prefix when the side-table row is missing (forward-fill caveat — see §10). This is the fallback, not the primary.

The live `ShapeCategory` enum (authoritative source: `go/internal/refresolve/types.go`) carries seventeen values today:

```
chain_slug, task_slug, bug_slug,
path,
skill_name, project_name, tool_name, forge_schema, library_entry,
domain_term, external_technical,
friction_shape,
skill_trigger, memory_entry, vault_candidate, kiwix_bridge, discipline_skill
```

The inspector's "detected shape" filter renders a multi-select keyed on the enum values returned in the side-table rows (legend-from-data — a new shape added in code appears as a new option without UI churn). The eleven original substrate-doc categories + the friction-shape + the five reference-resolution-migration extensions are all surfaced equivalently; the page does not privilege the "original eleven" over the extensions.

### 2.2 The disambiguation rule, codified

Component prop contract (mirrors `<AnalyticsChartProps>` from QF1):

```tsx
interface ContextPullInspectorProps {
  filterQuerySource: string[]            // grounding_events.query_source — defaults ['reference_resolution']
  filterShape: ShapeCategory[]           // refresolve.Reference.Shape — multi-select, empty = all
  filterSourceType: SourceType[]         // knowledge_pointers.source_type — multi-select, empty = all
  // Never `filterSource` or `filterKind` — those names conflate the axes.
}
```

**Why this matters:** TELEMETRY_FRONTEND.md §2 names the bug pattern explicitly — handlers that lock generic `source` / `kind` references against a schema before the schema lands produce silent-cell rendering. Reference-resolution multiplies the risk: with the page-local `shape` axis layered on the inherited three, **four** orthogonal vocabularies coexist on the inspector. Code that says `result.source` is ambiguous — does it mean `query_source`, `source_type`, or `shape`? The rule: every name carries its axis. `queryResolutionSource`, `resultSourceType`, `detectedShape`. Slightly verbose; not load-bearing-fragile.

### 2.3 Forward-compat with new shapes, sources, and source_types

- **New shape (code-only addition):** add a const to `types.go`, register a resolver in the registry, emit the side-table row. Inspector picks up the new value automatically (filter dropdown reads from the side-table's DISTINCT shapes; the value renders as a chip; per-shape sub-renderer falls through to a generic `<RawShapePill />` until the design doc names a custom renderer for it).
- **New query_source (CHECK-widening migration):** runs the same pattern as TT1's open-enum + QF1's legend-from-data rule. The inspector's source-filter chip set reads from the API response's `available_query_sources` field (RF2 returns it as a query-time enum reflection); the chip set is not a TS hardcoded list.
- **New source_type:** `knowledge_pointers.source_type` is an open enum (no CHECK constraint; TT1 §3.1 confirms). Same legend-from-data treatment.

---

## 3. Endpoint catalog

Six endpoints land in `go/internal/observehttp/context_pulls.go` (RF2). Mounted in `BuildRouter` (`go/internal/observehttp/router.go`) under the existing `mux.HandleFunc("GET ...")` pattern.

The three primary endpoints (§3.1, §3.2, §3.3) cover the inspector page + drawer. The two analytics endpoints (§3.4, §3.5) feed the page's stats banner. The one substrate-side side-table emit (§3.6) is a small extension to the `resolve_references` handler so the shape/tier/recommendation triple is queryable without re-parsing source_refs prefixes.

### 3.1 `GET /context-pulls` — paginated, filterable list

Returns rows from `grounding_events` filtered to `query_source='reference_resolution'` (default; overridable by `?query_source=` param, repeatable), joined to the new `reference_resolution_emits` side-table for shape/tier/recommendation, with the result-set's candidate source_refs unpacked through `knowledge_pointers` for `source_type`.

Query parameters:

| Param | Type | Default | Notes |
|---|---|---|---|
| `query_source` | string \| repeated | `['reference_resolution']` | Names the `grounding_events.query_source` filter. Defaults to the chain's own value; admit any value the API has seen for the unconstrained-power-user case. |
| `shape` | string \| repeated | absent (all shapes) | One of the live `ShapeCategory` enum values; reads from `reference_resolution_emits.shape`. Multi-select. |
| `confidence_tier` | string \| repeated | absent (all tiers) | One of `single_exact`, `fuzzy_multi`, `weak_domain`, `no_hit`. Reads from `reference_resolution_emits.confidence_tier`. Multi-select. |
| `source_type` | string \| repeated | absent (all source_types) | One of the open `knowledge_pointers.source_type` set. Multi-select. **Joins through the first candidate** for the row's results (per-row dispatch is left to the drawer; the list filter is "any candidate matches"). |
| `session_id` | string | absent | UUID — scope to one CLI session. |
| `prompt_id` | string | absent | UUID — scope to one user-prompt arc. |
| `span_id` | string | absent | UUID — scope to one MCP `tools/call`. Reading by span produces every reference-resolution row for that one call (typically 1–5 rows). |
| `project` | string | absent | Mirrors existing project-scoping convention. Reads `grounding_events.project_id`. |
| `q` | string | absent | Free-text `LIKE` on `query_text` (the detected reference token), case-insensitive. Unindexed; budgeted for < 200ms at 10k rows. |
| `reference_text` | string | absent | Alias for `q` for URL-clarity in the "search by detected token" affordance; one of `q`/`reference_text` is honored. |
| `since` | RFC 3339 timestamp | 30 days ago | Lower bound on `created_at`, inclusive. Matches QF1's default time-range. |
| `until` | RFC 3339 timestamp | absent (= now) | Upper bound, inclusive. |
| `cursor` | int | absent | Pagination cursor on `grounding_events.id` (INTEGER AUTOINCREMENT PK; monotonic per QF1 §4.1). |
| `limit` | int | 50 | Clamped `[1, 200]`, matches QF1's training-pair default. |

Response shape:

```json
{
  "items": [
    {
      "grounding_event_id": 42103,
      "ts": "2026-05-17T14:32:00.123Z",
      "project_id": "mcp-servers",
      "session_id": "<uuid>",
      "prompt_id": "1f73b794-...",
      "span_id": "9f8e7d6c-...",
      "parent_span_id": null,
      "action": "resolve_references",
      "query_source": "reference_resolution",
      "query_text": "Tripolar Invariant",
      "shape": "domain_term",
      "confidence_tier": "weak_domain",
      "presentation_recommendation": "mention_as_possibly_relevant",
      "presented_as": "`Tripolar Invariant` may refer to: vault note vault/learnings/glyphs/2026-04-30_tripolar-invariant.md (rank 1, weak)",
      "results_count": 3,
      "first_candidate": {
        "source_ref": "vault:learnings/glyphs/2026-04-30_tripolar-invariant.md",
        "source_type": "vault",
        "position": 1
      },
      "click_kinds_fired": ["mentioned"],
      "ml_confidence_score": null
    }
  ],
  "next_cursor": 41999,
  "page_size": 50,
  "available_query_sources": ["agent_initiated", "proactive_hook", "dashboard_user", "reference_resolution", "harness_reminder_interception", "other"],
  "available_shapes": ["chain_slug", "task_slug", "bug_slug", "path", "skill_name", "project_name", "tool_name", "forge_schema", "library_entry", "domain_term", "external_technical", "friction_shape", "skill_trigger", "memory_entry", "vault_candidate", "kiwix_bridge", "discipline_skill"]
}
```

Notes on field semantics:

- `first_candidate` is the position-1 entry from the row's `source_refs` JSON array, joined to `knowledge_pointers` for `source_type`. Full candidate list lives in the drawer (§3.2) — the list view stays compact. If `results_count == 0`, `first_candidate` is `null` (a no-hit row).
- `click_kinds_fired` is the DISTINCT set of `click_kind` values across `query_interactions` joined on `grounding_event_id`. Empty array = no clicks fired (the resolution surfaced but the agent didn't cite/follow/mention/resolve-from). This is the per-row signal for "did the resolution actually get used?"
- `ml_confidence_score` is the T7 forward-compat field. **Null today** because T7 has not landed; non-null when the trained classifier has scored this detection. The inspector renders `—` when null; renders a value with a colorscale when present. See §11.
- `available_query_sources` + `available_shapes` reflect the rows queryable in the current filter scope (with the source/shape filters removed). The chip dropdown populates from these arrays so newly-added enum values appear without a frontend redeploy.

Errors:
- `400 {"error":"invalid limit"}` on out-of-range `limit`.
- `400 {"error":"invalid cursor"}` on non-integer `cursor`.
- `400 {"error":"invalid since"}` / `"invalid until"` on un-parsable RFC 3339.
- `400 {"error":"invalid shape: <val>"}` on a `shape` query param that isn't in the live enum (whitelist check; the API does not pass garbage downstream). Same gate for `confidence_tier` and `query_source`.

`Cache-Control`: cursor-pinned pages (cursor != 0) → `public, max-age=300` per QF1 §3.6. The first page (cursor unset) → `no-cache` because new rows arrive.

### 3.2 `GET /context-pulls/{grounding_event_id}` — per-resolution detail

Returns the full per-row detail the inspector drawer renders. Synthesizes the response from `grounding_events` + `reference_resolution_emits` + `query_interactions` + `knowledge_pointers` and includes the cross-substrate join to `query_resolutions` (so the drawer can deep-link to the originating query trajectory when one exists).

Path parameter:
- `grounding_event_id` — integer matching `grounding_events.id`. `strconv.Atoi` pre-validation; `400` on parse failure.

Response shape:

```json
{
  "grounding_event": {
    "id": 42103,
    "ts": "2026-05-17T14:32:00.123Z",
    "project_id": "mcp-servers",
    "session_id": "<uuid>",
    "prompt_id": "1f73b794-...",
    "span_id": "9f8e7d6c-...",
    "parent_span_id": null,
    "action": "resolve_references",
    "query_source": "reference_resolution",
    "user_message_id": "<transcript-jsonl-uuid>",
    "results_count": 3
  },
  "detection": {
    "token": "Tripolar Invariant",
    "shape": "domain_term",
    "confidence": 0.78,
    "detection_method": "rubric",
    "start_pos": 88,
    "end_pos": 106,
    "source_message_excerpt": "...check the `Tripolar Invariant` notes."
  },
  "resolver": {
    "name": "domainTermResolver",
    "cost_typical_ms": 700,
    "retrieval_cost_ms": 740,
    "err": null
  },
  "candidates": [
    {
      "position": 1,
      "source_ref": "vault:learnings/glyphs/2026-04-30_tripolar-invariant.md",
      "source_type": "vault",
      "title": "Tripolar Invariant — glyph definition",
      "score": 0.62,
      "debug_notes": "knowledge_search rank 1",
      "ml_confidence_score": null
    },
    { "position": 2, "source_ref": "vault:...", "source_type": "vault", "score": 0.48, "ml_confidence_score": null },
    { "position": 3, "source_ref": "vault:...", "source_type": "vault", "score": 0.31, "ml_confidence_score": null }
  ],
  "outcome": {
    "confidence_tier": "weak_domain",
    "presentation_recommendation": "mention_as_possibly_relevant",
    "presented_as": "`Tripolar Invariant` may refer to: vault note vault/learnings/glyphs/2026-04-30_tripolar-invariant.md (rank 1, weak)"
  },
  "interactions": [
    {
      "interaction_id": 901,
      "source_ref": "vault:learnings/glyphs/2026-04-30_tripolar-invariant.md",
      "candidate_position": 1,
      "click_kind": "mentioned",
      "click_weight": 0.4,
      "was_injected": 0,
      "detected_at": "2026-05-17T14:34:10.000Z"
    }
  ],
  "linked_resolutions": [
    {
      "resolution_id": 8801,
      "entity_kind": "task",
      "entity_slug": "tripolar-invariant-trace",
      "entity_project_id": "mcp-servers",
      "outcome_kind": "completed"
    }
  ],
  "trajectory_deep_link": "/telemetry/trajectories/42103"
}
```

Notes:

- `detection.source_message_excerpt` is the byte-window around `[start_pos, end_pos]` from the substrate's `user_message_id` lookup. The handler fetches the transcript JSONL record by ID and slices `±40 chars` around the detection span. If the user_message_id resolves to an absent transcript file (forward-fill caveat: the transcript was deleted before the side-table was emitted), the field is `null` and the drawer falls back to rendering only `detection.token`.
- `resolver.name` comes from the side-table's `resolver_name` column (RF2 stamps it from `Resolver.Shape()`'s mapping at emit time). Falls back to a derived value (`<shape>Resolver`) when the side-table row is missing.
- `linked_resolutions` is the rows in `query_resolutions` whose `grounding_event_ids` JSON array contains this `grounding_event_id`. The drawer renders one chip per resolution linking to its entity detail. Empty array = no resolutions cite this reference-resolution call; that's the dominant case (most references don't cite-out as terminal resolutions).
- `trajectory_deep_link` is the QF3 deep-link URL — clicking jumps the operator into `<QueryTrajectoryView />` which renders the same row's full read-side leg (results + interactions + write-side resolutions). The inspector drawer is the *reference-resolution-specific* lens; QF3 is the *generic search trajectory* lens. They cross-link in both directions (§6.2).

Errors:
- `404 {"error":"reference resolution not found"}` when the id is well-formed but absent OR when the row's `query_source != 'reference_resolution'` (the endpoint is scoped; non-reference-resolution rows return 404 not 200-with-empty-side-table, so the operator can't accidentally drawer-view an unrelated grounding event).
- `400 {"error":"invalid grounding_event_id"}` on parse failure.

`Cache-Control`: `public, max-age=86400` per F1 §3.4 — events are immutable, and the per-row side-table data is append-only post-emit.

### 3.3 `GET /context-pulls/by-entity/{kind}/{slug}` — entity-scoped reference resolutions

Returns the reference resolutions whose `prompt_id` matches a `prompt_id` of an `events` row resolving this entity. This is **the load-bearing endpoint for §9 entity-detail integration.**

Path parameters:
- `kind` — one of `bug`, `task`, `chain`.
- `slug` — natural-key identifier per the entity-kind convention.

Required query parameter:
- `project` (or `project_id`) — entity slugs aren't globally unique across projects.

Optional query parameters:
- `outcome_kind` — narrow to events with one of `resolved` / `completed` / `cancelled` / `closed`. Default: all of them.
- `limit` — clamped `[1, 200]`, default 50. Cursor pagination same shape as §3.1.

The handler resolves the entity → finds the `prompt_id` set of events resolving it → returns the reference-resolution `grounding_events` rows joined on `prompt_id`. The join is **direct on `prompt_id`** — per TT1 §2 the three-layer hierarchy `session_id ⊇ prompt_id ⊇ span_id` and **`prompt_id` is the user-arc trajectory key**, NOT `span_id`. Span-based joining would miss the cross-span span of a multi-tools-call resolution arc; prompt-based joining captures the full arc.

SQL shape (illustrative):

```sql
-- The entity's resolving prompt_ids (typically 1–3 rows; one BugResolved
-- or TaskCompleted event per entity in the dominant case, plus re-opens).
WITH resolving_prompts AS (
  SELECT DISTINCT
    -- The prompt_id stamped on grounding_events fired in the same session
    -- and within the temporal window of the events row. events does NOT
    -- carry prompt_id directly (per TT1 §2.5); the resolution comes via
    -- the same-session + closest-preceding-grounding-row pattern that
    -- TT2's processResolutions used.
    qr.prompt_id
  FROM query_resolutions qr
  WHERE qr.entity_kind = ?
    AND qr.entity_slug = ?
    AND qr.entity_project_id = ?
    AND (? = '' OR qr.outcome_kind = ?)
)
SELECT
  ge.id, ge.created_at, ge.project_id, ge.session_id, ge.prompt_id,
  ge.span_id, ge.parent_span_id, ge.action, ge.query_source,
  ge.query_text, ge.results_count,
  rre.shape, rre.confidence_tier, rre.presentation_recommendation,
  rre.presented_as
FROM grounding_events ge
LEFT JOIN reference_resolution_emits rre ON rre.grounding_event_id = ge.id
WHERE ge.query_source = 'reference_resolution'
  AND ge.prompt_id IN (SELECT prompt_id FROM resolving_prompts)
ORDER BY ge.id ASC
LIMIT ?;
```

`query_resolutions.prompt_id` is the join key (TT1 §4.1 column; the `prompt_id`-as-trajectory-unit decision flows directly from TT1 §2.4: `prompt_id` source-of-truth is the transcript JSONL `promptId` field stamped by the Stop hook).

Response shape: same envelope as §3.1's `items[]`, prefixed by:

```json
{
  "entity": { "kind": "bug", "slug": "...", "project_id": "..." },
  "matched_prompt_ids": ["1f73b794-...", "..."],
  "items": [ ... per §3.1 item shape ... ],
  "next_cursor": null
}
```

If `matched_prompt_ids` is empty (the entity has no resolving events yet OR was created pre-substrate), `items` is `[]` and the inspector's entity-detail badge renders the absent-state copy (§9.2).

### 3.4 `GET /context-pulls/stats` — banner stats

Returns the distribution banner for the top of the inspector page. Honors the same `query_source` / `shape` / `confidence_tier` / `source_type` / `project` / `since` / `until` filters as §3.1 — distribution updates as the operator narrows.

Response shape:

```json
{
  "total_references": 1247,
  "by_shape": {
    "chain_slug": 124, "task_slug": 89, "bug_slug": 16,
    "path": 211, "skill_name": 72, "project_name": 8, "tool_name": 19, "forge_schema": 12,
    "library_entry": 6,
    "domain_term": 318, "external_technical": 234,
    "friction_shape": 41,
    "skill_trigger": 33, "memory_entry": 19, "vault_candidate": 28, "kiwix_bridge": 11, "discipline_skill": 6
  },
  "by_confidence_tier": {
    "single_exact": 691,
    "fuzzy_multi": 188,
    "weak_domain": 273,
    "no_hit": 95
  },
  "by_source_type": {
    "vault": 412, "kiwix": 211, "library": 39, "task": 89, "chain": 124, "bug": 16
  },
  "by_query_source": {
    "reference_resolution": 1247
  }
}
```

The shape and tier breakdowns are the load-bearing diagnostic. An operator sees at a glance: how many of the agent's references resolved cleanly (`single_exact`) vs needed disambiguation (`fuzzy_multi`) vs were guesses (`weak_domain`) vs missed entirely (`no_hit`). The `no_hit` count is the gap signal — high counts on a specific shape indicate either an index gap or a detector-false-positive pattern.

### 3.5 `GET /context-pulls/stats/timeseries` — trend bars

Returns daily bucketed counts for the page's small trend strip (one row of mini-bars across the top of the inspector). Same filter set as §3.4, plus a required `segment` param:

| Param | Type | Default | Notes |
|---|---|---|---|
| `segment` | `shape` \| `confidence_tier` \| `source_type` | `shape` | The slice axis; reuses the QF1 convention. |

Response shape (mirrors QF1 §3.2):

```json
{
  "segment": "shape",
  "buckets": [
    { "day": "2026-05-16", "segments": { "chain_slug": 12, "domain_term": 28, "path": 7 } },
    { "day": "2026-05-17", "segments": { "chain_slug": 18, "domain_term": 32, "path": 9 } }
  ]
}
```

`recharts <BarChart>` renders the response; one stacked-bar per day. The chart is small (height 80px) and lives in the page header alongside the stats banner — it's a glance-affordance, not a deep-analysis surface. Operators who need deeper analytics drill from `/telemetry` (QF4) which already has the canonical analytics page.

### 3.6 Substrate-side amendment: `reference_resolution_emits` side-table

The inspector's shape / tier / recommendation columns come from a side-table that does NOT exist today. RF2 ships migration 042:

```sql
-- Migration 042 — reference-resolution-substrate-frontend RF2 amendment.
-- Side-table indexing the per-reference detail that doesn't fit cleanly on
-- grounding_events. One row per resolve_references emit; FK to grounding_events.
CREATE TABLE reference_resolution_emits (
    grounding_event_id            INTEGER PRIMARY KEY REFERENCES grounding_events(id),
    shape                         TEXT NOT NULL,
    confidence_score              REAL NOT NULL,        -- detection confidence (Reference.Confidence)
    detection_method              TEXT NOT NULL,
    start_pos                     INTEGER NOT NULL,
    end_pos                       INTEGER NOT NULL,
    confidence_tier               TEXT NOT NULL,
    presentation_recommendation   TEXT NOT NULL,
    presented_as                  TEXT NOT NULL,
    resolver_name                 TEXT NOT NULL,
    retrieval_cost_ms             INTEGER NOT NULL,
    ml_confidence_score           REAL,                 -- nullable; T7 forward-compat
    created_at                    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_rre_shape ON reference_resolution_emits (shape);
CREATE INDEX idx_rre_tier  ON reference_resolution_emits (confidence_tier);
CREATE INDEX idx_rre_resolver ON reference_resolution_emits (resolver_name);
```

The `emitGroundingEvents` helper (`go/internal/refresolve/handler.go` per substrate doc §6.6 T5 implementation notes) gains a sibling INSERT into the side-table inside the same write tx as the `grounding_events` INSERT. RF2 covers the migration sync to the two Go-embed mirror dirs per CLAUDE.md.

**Why a side-table, not extra columns on `grounding_events`:** `grounding_events` is shared across all query sources (agent_initiated, proactive_hook, dashboard_user, reference_resolution, harness_reminder_interception, other). Adding reference-resolution-specific columns to it would either (a) leave most rows with NULLs for the new columns (~5 columns × 99% of rows), or (b) require defaults that don't make sense for non-reference-resolution sources (what's the `confidence_tier` of an agent-initiated vault_search? undefined). The side-table keeps the shared substrate clean and lets this surface evolve its detail shape independently — same posture as `query_interactions` and `query_resolutions` (TT1 §3 + §4 are themselves side-tables of `grounding_events`).

**Forward-compat:** the column shape is closed by RF2 close. T7 ML scores land on the `ml_confidence_score` column (nullable; backfilled when the trained classifier runs). Future detail extensions get new columns ALTER-added; SQLite ALTER TABLE ADD COLUMN works without rebuild because the new column is nullable. **The dashboard reads explicit columns** (no `SELECT *`), so a column addition is opaque to the dashboard until the dashboard opts in.

### 3.7 Endpoint placement

```go
// go/internal/observehttp/router.go — added in RF2.
mux.HandleFunc("GET /context-pulls", state.contextPullsList)
mux.HandleFunc("GET /context-pulls/{grounding_event_id}", state.contextPullsDetail)
mux.HandleFunc("GET /context-pulls/by-entity/{kind}/{slug}", state.contextPullsByEntity)
mux.HandleFunc("GET /context-pulls/stats", state.contextPullsStats)
mux.HandleFunc("GET /context-pulls/stats/timeseries", state.contextPullsStatsTimeseries)
```

CORS inherits from `withCORS(mux)`. SQL discipline mirrors `bugs.go`/`events.go`/`telemetry.go`: `db.NewArgs()` for parameter binds, explicit `SELECT` column lists, `Cache-Control` matches the row's content-stability.

---

## 4. Pagination and filter shape

### 4.1 Cursor pagination

Same shape as QF1 §4.1 and F1 §3.1. `grounding_events.id` is INTEGER AUTOINCREMENT — monotonic by SQLite guarantee — so `WHERE id < ? ORDER BY id DESC LIMIT N+1` produces stable pages on the inspector (descending, newest first). The `/context-pulls/by-entity/{kind}/{slug}` endpoint pages ascending (entity-arc reads chronologically), `WHERE id > ? ORDER BY id ASC LIMIT N+1`.

### 4.2 Empty-state vs absent-state vs forward-fill-empty

Three distinct empty-states, each with distinct copy (matching QF1 §4.2's three-way distinction):

- **Filter narrowed to nothing** — `items: []`, `next_cursor: null`, `total_references > 0` in the stats banner. Copy: "No reference resolutions match the current filters. Try removing the `<filter>` filter."
- **Entity has no reference resolutions yet** — `/context-pulls/by-entity/...` returns `matched_prompt_ids: []`. Copy on the entity-detail badge: "No references resolved while working on this <entity_kind> (yet)."
- **Forward-fill caveat** — entire substrate is empty for the period. Copy: "Reference-resolution telemetry begins at substrate-T5 land time (migration 040, 2026-05-18). Earlier resolves are not in scope."

The three are distinguished by inspecting `total_references` from the stats endpoint (cheap; cached) — if filtered narrows to zero but unfiltered totals are non-zero, the first copy fires; if unfiltered is zero for the date range, the third.

### 4.3 URL-encoded filter state

Filter state encodes into URL per QF1 §4.3. Param names match the API param names (no short-renaming).

| Page | Filters in URL |
|---|---|
| `/context-pulls` (inspector) | `query_source` (repeatable), `shape` (repeatable), `confidence_tier` (repeatable), `source_type` (repeatable), `session_id`, `prompt_id`, `span_id`, `project`, `q`, `since`, `until`, `cursor` |
| `/context-pulls/<id>` (drawer-as-route) | none (the path IS the deep-link) |

The drawer is **route-addressable**: opening it via row-click sets `?event=<id>` in the inspector's URL, so the operator can share "this resolution looked weird" by URL. Closing the drawer removes the param; the rest of the filter state is preserved.

### 4.4 Multi-axis filter UI affordance

Because four orthogonal axes coexist (§2), the filter bar groups them visually:

```
┌─────────────────────────────────────────────────────────────────────┐
│ source: [reference_resolution ✕] [+]    shape: [domain_term ✕] [+]  │
│ tier:   [weak_domain ✕] [+]             source_type: [vault ✕] [+]  │
│ project: [mcp-servers ▾]   since: [30 days ▾]    search: [____]     │
└─────────────────────────────────────────────────────────────────────┘
```

Each axis label uses the exact column name so the operator can read the URL and decode it without lookup. Chip removal updates the URL and re-fetches; chip addition triggers a multi-select popover with the values from the API's `available_*` arrays (legend-from-data).

---

## 5. Read-source rules — projections, side-table, raw substrate

The QF1 §5 read-source pattern carries:

| Surface | Reads from | Why |
|---|---|---|
| `<ContextPullInspector />` row list | `grounding_events` + `reference_resolution_emits` (LEFT JOIN) + `knowledge_pointers` (LEFT JOIN on first source_ref) | Per-row inspector detail. No projection shaped for this view — the side-table IS the projection-equivalent. |
| `<ContextPullInspector />` stats banner | `grounding_events` + `reference_resolution_emits` aggregated server-side | Single-pass aggregate; cheap at homelab scale. |
| `<ContextPullInspector />` trend strip | `grounding_events` + `reference_resolution_emits` aggregated by `DATE(created_at)` | Same as banner; daily buckets. |
| `<ResolutionDetailDrawer />` per-resolution detail | `grounding_events` + `reference_resolution_emits` + `query_interactions` + `query_resolutions` + transcript JSONL (for `source_message_excerpt`) | Point-lookup; same read pattern as F2's `/events/{event_id}` endpoint. |
| `<ResolutionDetailDrawer />` trajectory leg | Delegates to QF3's `<QueryTrajectoryView />` via deep-link, NOT a re-fetch | Trajectory-view is QF3's surface; the drawer cross-links rather than reimplements. |
| Entity-detail badge ("N references resolved in this prompt") | `/context-pulls/by-entity/{kind}/{slug}` | Single endpoint, prompt_id join; see §9. |

Explicit column lists in every `SELECT`, no `SELECT *` — matches the SQL discipline in `bugs.go`/`events.go`/`telemetry.go`/`projections/*.go`. The pattern is load-bearing for the side-table forward-compat: an ALTER TABLE ADD COLUMN is invisible to the dashboard until a handler opts in by adding the column to its `SELECT` list.

---

## 6. Reuse-vs-extend boundary with sibling frontends

This chain's surface inherits from two shipped sibling chains. The boundary table here is more involved than QF1 §6 because reference-resolution sits on top of both substrate-frontend and telemetry-frontend.

### 6.1 What this chain REUSES (binding hard; both upstreams are shipped)

- **`<QueryTrajectoryView />`** (`apps/dashboard/src/components/shared/QueryTrajectoryView/`, shipped 2026-05-18 per QF3 close): used by `<ResolutionDetailDrawer />` for the "see the full trajectory" leg. The drawer's `trajectory_deep_link` field (§3.2) navigates to `/telemetry/trajectories/<grounding_event_id>` — clicking jumps the operator into QF3 with the same row's full read-side leg (results + interactions + write-side resolutions). The drawer itself does NOT re-render the trajectory inline; it cross-links so each surface keeps its own data path. This is the **cross-link, not embed** posture — embedding `<QueryTrajectoryView />` inside the drawer would double-fetch the same row, and the inspector's drawer has a narrower lens (the resolution-specific fields) than QF3's general lens.
- **`<EventTimeline />`** (`apps/dashboard/src/components/shared/EventTimeline/`, shipped 2026-05-17 per F3 close): used on Bug + Task entity-detail panels per §9. Specifically the **per-event-type renderers** (the same `per-type-renderers.tsx` module QF3 extends with its "preceded by N queries" suffix block). This chain adds a second suffix block: "N references resolved in this prompt" — see §9.2.
- **`renderEventPayload`** (`apps/dashboard/src/components/shared/EventTimeline/per-type-renderers.tsx`): NOT directly reused by this chain. Reference-resolution emits do not flow through the events ledger — they emit `grounding_events` rows, not `events` rows. The drawer renders its own per-resolution layout (§7.2) rather than dispatching through the per-event renderer module.
- **API client** patterns from `apps/dashboard/src/api/`: new `apps/dashboard/src/api/contextPulls.ts` mirrors the shape of `apps/dashboard/src/api/auditEvents.ts` and `apps/dashboard/src/api/telemetry.ts` (existence per the dashboard pages already shipped).
- **`<LabelKindBadge />`** + **`<LabelSourcesChips />`** + **`<TrajectoryDeepLink />`** (QF3-defined per QF1 §6.3): conditionally reused. `<LabelKindBadge />` renders on the drawer when the drawer's underlying `query_interactions` rows have label_kind data populated by `proj_training_data_for_reranker` (per QF1 §6.3 it does). `<LabelSourcesChips />` renders on the click_kinds_fired column of the inspector row.
- **Sidebar / AppShell** patterns: a new `Context Pulls` sidebar entry follows the existing `<NavLink>` pattern. Placement: under the `Telemetry` umbrella in the sidebar — sibling to the existing telemetry pages — because the inspector IS a telemetry surface (just scoped). See §8 route plan.
- **Recharts** for the `<TrendStrip />` mini-bars: same `recharts ^3.8.1` import as Benchmarks + Telemetry pages. No new chart-library dep.

### 6.2 What this chain BUILDS NEW

- **`<ContextPullInspector />`** (page) — at `apps/dashboard/src/pages/ContextPulls/index.tsx`. The page is filter-bar + stats banner + trend strip + row list + per-row drawer. The pattern is similar to QF5's TrainingPairBrowser but the rows are different (per-resolution, not per-training-pair).
- **`<ResolutionDetailDrawer />`** — the per-resolution detail drawer. Slides in from the right edge per F1 §7.6's `<EventDetailDrawer />` pattern but renders the §3.2 response shape, not the events-table shape.
- **`<DetectionContextBlock />`** — renders `detection.source_message_excerpt` with the detection span highlighted via `<mark>` tags around `[start_pos, end_pos]`. Falls back to "<token>" only when the excerpt is null (forward-fill caveat).
- **`<CandidateList />`** — the candidate dispatch list inside the drawer. Each row renders via a per-`source_type` sub-renderer: `<VaultCandidateRow />`, `<KiwixCandidateRow />`, `<LibraryCandidateRow />`, `<TaskCandidateRow />`, `<ChainCandidateRow />`, `<BugCandidateRow />`. New source_types render through a `<RawSourceTypePill />` fallback.
- **`<ConfidenceTierBadge />`** — chip with one of `single_exact` / `fuzzy_multi` / `weak_domain` / `no_hit`, color-coded. Matches the visual language of `<StatusBadge />` and `<LabelKindBadge />`.
- **`<ShapeBadge />`** — chip naming the detected shape. Background color keyed to the shape's category group (slug-shapes = blue; filesystem-shapes = green; knowledge-shapes = purple; friction = red; extension-shapes = grey).
- **`<PresentedAsBlock />`** — renders the substrate's `presented_as` string. Verbatim — the substrate produced it; the inspector surfaces it without further processing. Includes a "copy" affordance so the operator can paste the exact wording into a bug filing.
- **`<RecommendationChip />`** — renders the substrate's `presentation_recommendation` (use_directly / ask_user_to_disambiguate / mention_as_possibly_relevant / acknowledge_no_hit_and_ask).
- **`<MlConfidenceCell />`** — T7 forward-compat. Renders `—` when null; renders a value with a 0..1 colorscale when present. See §11.

### 6.3 What this chain EXTENDS

- **`per-type-renderers.tsx`** — the QF3 chain already extended the `BugResolved` / `TaskCompleted` / `ChainClosed` per-type renderers with a "preceded by N queries" suffix block (QF1 §6.2). This chain adds a **sibling** suffix block to the same renderers: "N references resolved in this prompt." The two suffixes coexist; they answer different questions. Source code in the same file; jointly owned by both chains.

```
[ existing renderer output ]
─────────────────────────────
preceded by 3 queries  ▾    ← QF3's suffix (already shipped)
3 references resolved ▾    ← THIS CHAIN's suffix (RF3)
```

Each suffix is independently expandable; expanding the references suffix fetches `/context-pulls/by-entity/<kind>/<slug>?outcome_kind=resolved` and renders a mini-list of `<InspectorRow />` cards inline. Clicking a row jumps to `/context-pulls/<id>` (the drawer's URL form). The mini-list is bounded to the first 5 rows with a "see all N" footer linking to the page-level inspector pre-filtered to the entity.

### 6.4 What this chain DEFERS to the audit-ledger bug

The audit-ledger drawer's `related_queries` field has known field-shape drift (`audit-ledger-related-queries-ts-go-field-drift`, filed per QF1 §10). That bug's fix would add `grounding_event_id` to the wire shape so the drawer can directly cross-link to reference-resolution rows that fed the resolution. **This chain's `<EventTimeline />` extension does not depend on that fix** — the suffix block fetches `/context-pulls/by-entity/...` directly with the entity's `(kind, slug, project)` triple, which is what the entity panel already has. So the cross-substrate seam works without waiting on the audit-ledger bug. The audit-ledger drawer's per-resolution cross-link gains a "see references that resolved here" affordance separately when the bug's fix lands.

---

## 7. Component contracts

### 7.1 `<ContextPullInspector />` (page)

```tsx
// Page-level component, no props. Reads filters from useSearchParams.
```

Behavior:

- Top banner: stats from `GET /context-pulls/stats`. Renders shape + tier breakdowns as horizontal stacked bars, one per axis.
- Trend strip: `<BarChart />` from `recharts`, height 80px, segment defaults to `shape` (changeable via small selector adjacent to the chart). Pulls from `GET /context-pulls/stats/timeseries`.
- Filter bar: chip-set per §4.4. Project picker is the existing component; everything else is new dropdowns over the API's `available_*` arrays.
- Body: paginated list of `<InspectorRow />` cards. Each card shows:
  - `<ShapeBadge />` + `<ConfidenceTierBadge />` chips
  - The detected token (large, bold, with `<DetectionContextBlock />`-styled `<mark>` highlight in context if `source_message_excerpt` is present)
  - The resolver name + cost (small, muted)
  - First-candidate summary: `<VaultCandidateRow />` etc. (truncated to one line)
  - `<RecommendationChip />` + the count of `click_kinds_fired`
  - `<MlConfidenceCell />` (or "—")
  - The session/prompt/span chips (small, clickable to filter)
- Right-side drawer (when `?event=<id>` in URL): `<ResolutionDetailDrawer />`.
- **Loading state:** skeleton cards (5 placeholders) with `data-testid="context-pull-inspector-loading"`.
- **Error state:** `<p role="alert">Failed to load context pulls: <message></p>` with `data-testid="context-pull-inspector-error"`.
- **Empty state:** three-way distinction per §4.2.
- **Accessibility:** filter bar is `<form role="search">`; cards are `<li role="listitem">` inside `<ol>`; chip-set dropdowns are `<details>`-based combo-boxes with `aria-label` per axis.
- **Performance budget:** 50 cards per page, < 300ms server response at 10k rows. The unindexed `LIKE` on `query_text` (the `q` param) is budgeted for < 200ms at 10k rows.

Integration points:
- Sidebar entry added in `apps/dashboard/src/components/layout/Sidebar/index.tsx` under the Telemetry group.
- Route added in `apps/dashboard/src/router/index.tsx` per §8.

### 7.2 `<ResolutionDetailDrawer />`

```tsx
interface ResolutionDetailDrawerProps {
  groundingEventId: number | null    // null = drawer closed
  onClose: () => void
}
```

Behavior:

- Fetches `GET /context-pulls/{groundingEventId}` on prop change.
- Renders the layout below.
- **Loading state:** skeleton blocks per section.
- **Error state:** `<p role="alert">…</p>`; close button always functional.
- **Empty state:** N/A.
- **Accessibility:** drawer is `<aside role="dialog" aria-labelledby="drawer-title">`; Escape closes; focus traps inside until close.

Drawer layout:

```
┌────────────────────────────────────────────────────────────┐
│ <ShapeBadge> <ConfidenceTierBadge>          [ Close ✕ ]    │
│ "Tripolar Invariant"                                       │
│ <ts> · <session_id chip> · <prompt_id chip> · <span chip>  │
├────────────────────────────────────────────────────────────┤
│ Detection                                                  │
│ ─────────                                                  │
│ <DetectionContextBlock> rendering source_message_excerpt   │
│ with the [start_pos, end_pos] span <mark>-highlighted.     │
│ detection_method · confidence (raw score)                  │
├────────────────────────────────────────────────────────────┤
│ Resolver                                                   │
│ ─────────                                                  │
│ resolver_name · retrieval_cost_ms · cost_typical_ms        │
│ <err message if non-null>                                  │
├────────────────────────────────────────────────────────────┤
│ Candidates  (results_count)                                │
│ ─────────                                                  │
│ <CandidateList> — each row dispatches to per-source_type   │
│ sub-renderer; <MlConfidenceCell> per row if present.       │
├────────────────────────────────────────────────────────────┤
│ Outcome                                                    │
│ ─────────                                                  │
│ <RecommendationChip>                                       │
│ <PresentedAsBlock> rendering the verbatim presented_as.    │
├────────────────────────────────────────────────────────────┤
│ Click signals                                              │
│ ─────────                                                  │
│ <InteractionList> — each query_interactions row.           │
│ Empty state: "No clicks fired for this resolution (yet)."  │
├────────────────────────────────────────────────────────────┤
│ Linked resolutions                                         │
│ ─────────                                                  │
│ <ChipList> linking to entity-detail pages.                 │
├────────────────────────────────────────────────────────────┤
│ Deep links                                                 │
│ ─────────                                                  │
│ See full trajectory →   (/telemetry/trajectories/<id>)     │
│ View this prompt's resolutions → (/context-pulls?prompt=)  │
└────────────────────────────────────────────────────────────┘
```

The Deep links section is the navigation seam. "See full trajectory" delegates to QF3. "View this prompt's resolutions" loops back to the inspector filtered to the same `prompt_id` so the operator can see the rest of the prompt's reference activity.

### 7.3 `<EventTimelineExtension />` (the §6.3 suffix block)

```tsx
// Not a top-level component; an addition to per-type-renderers.tsx for
// BugResolved / TaskCompleted / ChainClosed events. Pure-function shape
// matches the renderer module's existing contract.
```

Behavior:

- Renders a `<details>`-based expansion: "N references resolved in this prompt ▾".
- The count comes from a lazy fetch of `/context-pulls/by-entity/<kind>/<slug>?outcome_kind=<event_outcome>&limit=1` (which returns `matched_prompt_ids` array length; no items needed).
- On expansion: fetches `/context-pulls/by-entity/<kind>/<slug>?outcome_kind=<event_outcome>&limit=5` and renders 5 `<InspectorRow />` cards inline.
- Each card clickable to drawer.
- A "See all N" footer when `next_cursor != null`, linking to `/context-pulls?entity_kind=<kind>&entity_slug=<slug>&project=<project>`.
- **Absent shape:** when `/context-pulls/by-entity/...` returns `matched_prompt_ids: []`, the suffix block does not render at all (no copy, no expansion). Same posture as QF3's "preceded by N queries" extension when N=0.

---

## 8. Route plan

Routes added to `apps/dashboard/src/router/index.tsx` (flat shape, matching the existing pattern):

```tsx
{ path: 'context-pulls', element: <ContextPullInspectorPage /> },           // RF3 — list view (drawer via ?event= param)
```

The drawer is **NOT** a separate route — it's controlled by the `?event=<id>` query param on the inspector path. Operators arrive at the drawer via:

- Row click on `/context-pulls` (sets `?event=...`)
- Direct link to `/context-pulls?event=<id>` (shareable)
- Entity-detail expansion click (`<EventTimelineExtension />` rows)
- Deep-link from `/telemetry/trajectories/<id>` (the QF3 trajectory view gains a reverse cross-link to the inspector when the row's query_source is reference_resolution)

**Sidebar nav addition** (`apps/dashboard/src/components/layout/Sidebar/index.tsx`):

```tsx
<NavLink to="/context-pulls">Context Pulls</NavLink>
```

Placement: under the existing Telemetry group (sibling to `<NavLink to="/telemetry">` and `<NavLink to="/telemetry/training-pairs">`).

**Why `/context-pulls` over `/references` or `/telemetry/references`:**

- `/references` would collide with the colloquial "references" (citations, etc.) in vault notes and elsewhere. Too generic.
- `/telemetry/references` is more nested-correct but adds depth without analytical benefit; QF1 §8's path-flat preference applies.
- `/context-pulls` reads as "what context did the agent pull while doing the work" — the operator-facing framing that maps cleanly to the substrate's purpose. The chain's `output` field uses the phrase verbatim. Confirmed.

The QF3 trajectory page (`/telemetry/trajectories/<id>`) gains a small additive change: when the page loads a row with `query_source = 'reference_resolution'`, the page header renders a "View as resolution" cross-link to `/context-pulls/<id>`. This is a 5-line change in QF3's page component, jointly owned with this chain post-RF3.

---

## 9. Entity-detail integration — the `prompt_id` join

The chain's `completion_condition` (c) mandates entity-detail integration: "N references resolved in this session" badges on Bug + Task panels, expanding to inline trajectory. The mechanism is the §6.3 `<EventTimeline />` extension. This section names the join key explicitly.

### 9.1 Why `prompt_id` and not `span_id`

TT1 §2 establishes the three-layer hierarchy: `session_id ⊇ prompt_id ⊇ span_id`. `prompt_id` is the **user-arc trajectory key** — one user input + every subsequent assistant turn + tool call + tool result until the next user input. `span_id` is **per-MCP-`tools/call`** — milliseconds-to-seconds; one model inference's tool dispatch.

A reference-resolution call can fire multiple `tools/call`s across a single user arc:
1. User asks something with several reference-shape tokens.
2. Agent calls `knowledge.resolve_references` — one `tools/call`, one `span_id`, N `grounding_events` rows (one per detected reference), all sharing the same `span_id`.
3. Agent processes the resolutions and may issue follow-up `tools/call`s (`work.task_read`, `knowledge.vault_read`, etc.) in the same `prompt_id`.
4. Eventually the agent emits a write-side event (BugResolved, TaskCompleted, etc.) — different `span_id` than step 2 but **same `prompt_id`**.

The trajectory join is the `prompt_id` link — the user arc connects the resolves (step 2) to the resolution (step 4). Span-based joining would miss the connection because the spans are distinct.

### 9.2 The SQL form (illustrative)

Per §3.3, the by-entity endpoint joins through `query_resolutions.prompt_id` (TT1 §4.1 column on the resolutions side-table):

```sql
-- "Reference resolutions whose prompt_id matches a prompt_id of an
-- events row resolving this Bug" — paraphrased from RF1 acceptance.
WITH resolving_prompts AS (
  SELECT DISTINCT qr.prompt_id
  FROM query_resolutions qr
  WHERE qr.entity_kind = ? AND qr.entity_slug = ? AND qr.entity_project_id = ?
)
SELECT ge.*, rre.*
FROM grounding_events ge
LEFT JOIN reference_resolution_emits rre ON rre.grounding_event_id = ge.id
WHERE ge.query_source = 'reference_resolution'
  AND ge.prompt_id IN (SELECT prompt_id FROM resolving_prompts);
```

`query_resolutions.prompt_id` is stamped by the Stop hook at session end (TT1 §2.5 — `prompt_id` is post-hoc-stamped, not minted live). Consequence: an entity resolved in the current session may show **no reference resolutions** in the inspector badge until the Stop hook runs and stamps the rows. The badge's empty-state copy advertises this caveat ("forward-fill caveat — prompts get stamped at session end").

### 9.3 The integration mechanism

Per §6.3, `<EventTimelineExtension />` is the extension to `per-type-renderers.tsx`. The extension renders **only** on the three resolution-event types — `BugResolved`, `TaskCompleted`, `ChainClosed` — because those are the only event types that produce a `query_resolutions` row. Other event types (`BugReported`, `TaskCreated`, etc.) do not yet produce resolutions, so the extension would always render N=0 there — no-op.

The extension's fetch is **lazy**: only fires when the renderer is mounted AND the suffix block is in viewport (IntersectionObserver). This matches the existing F3 behavior where event detail is not pre-fetched.

### 9.4 Fallback when the events row's `prompt_id` is NULL

Pre-substrate events (events emitted before TT1's `prompt_id` column landed) have `prompt_id = NULL`. The extension's fetch returns `matched_prompt_ids: []` for those (the `NULL IN (SELECT ...)` always-false-in-SQL). The badge does not render — same posture as the absent-shape case in §7.3.

---

## 10. Fallback matrix

| Seam | Upstream | Present shape | Absent shape |
|---|---|---|---|
| `<QueryTrajectoryView />` | QF3 (shipped 2026-05-18) | `<ResolutionDetailDrawer />`'s trajectory deep-link navigates to it | Forward-compat against future deletion; the deep-link button degrades to clipboard-copying the URL. |
| `<EventTimeline />` per-event renderers | F3 (shipped 2026-05-17) | `<EventTimelineExtension />` suffix block adds "N references resolved" to BugResolved/TaskCompleted/ChainClosed | Forward-compat against future deletion; suffix block doesn't render. |
| `reference_resolution_emits` side-table | RF2 (this chain) | Inspector + drawer surface shape/tier/recommendation natively | If the side-table is missing for a row (back-fill caveat: rows emitted by handler.go BEFORE RF2's amendment shipped), the row renders with `shape: 'unknown'` and the drawer's resolver/outcome sections fall back to source_ref-prefix inference. The inspector marks these rows with a "pre-side-table" annotation. |
| `query_resolutions.prompt_id` | TT2 (shipped) | Entity-detail badge fetches via prompt_id join | If the column is NULL for the entity's resolving event (pre-substrate), badge doesn't render. |
| Transcript JSONL for `source_message_excerpt` | Existing path | Drawer renders the detection in-context | If the JSONL file is absent (cleanup, retention), drawer renders only `detection.token` without context. |
| ML confidence scores | T7 (not yet shipped) | `<MlConfidenceCell />` renders the value with colorscale | Renders `—`; column is hide-able via the inspector's column-visibility toggle. |
| `<LabelKindBadge />` etc. (QF3 components) | QF3 (shipped) | Drawer's per-row label badges | Forward-compat; falls back to no badge rendering. |
| `available_query_sources` / `available_shapes` arrays in the API response | RF2 implementation | Filter chip-set populates from data | If the arrays are absent (older API version), fall back to a TS-hardcoded list as a backup. The fallback list is a copy of the live enum; falls behind reality if the enum extends, but never fails closed. |

**The fallback contract** (per QF1 §9 + F1 §10): the dashboard never breaks because an upstream is absent OR because the substrate is empty. Every seam has a designed-in absent shape.

---

## 11. Forward-compat for T7 ML scores

T7 is reference-resolution-substrate's ML classifier integration step (per `docs/REFERENCE_RESOLUTION.md` §10). It is **not** yet shipped at RF1 author time. The design pins how scores will surface when T7 lands.

### 11.1 Where the score lives

`reference_resolution_emits.ml_confidence_score` is the column — REAL, NULLABLE, NULL today. T7 amendment: trained classifier (when the ML capability chain ships per substrate doc §10.1) populates the column post-resolve, possibly asynchronously (the live emit path may not have ML capacity in the latency budget; the trained classifier may run as a post-emit pass).

Per-candidate ML scores: `Candidate.MlConfidenceScore` (the response field per §3.2) is also nullable; populated when the cross-encoder reranker scores the candidate list (substrate doc §10.1 second row).

### 11.2 How the inspector renders it

- **Row list (`<ContextPullInspector />`):** new column "ML confidence" between "Recommendation" and "Click signals". `<MlConfidenceCell value={...} />` renders `—` when null; renders a 2-decimal score with a green-yellow-red colorscale when present.
- **Detail drawer:** the candidate list (`<CandidateList />`) gains a per-candidate ML score column. Reads `Candidate.MlConfidenceScore` from the response.
- **Filter affordance:** new `min_ml_confidence` query param on `/context-pulls`. Defaults to absent (no filter). When set, filters to rows where `ml_confidence_score >= ?`. The page surfaces a small slider in the filter bar that appears only when the stats endpoint reports `ml_confidence_score IS NOT NULL` for at least one row in the current filter scope. The slider is hidden when the column is uniformly null (= pre-T7).

### 11.3 Why the column is NULLABLE not DEFAULT 0.0

A NULL vs 0.0 distinction is load-bearing: `ml_confidence_score = 0.0` would mean "the classifier scored this and decided low confidence"; `ml_confidence_score IS NULL` means "no classifier score yet". Pre-T7 rows must be the latter, not the former, or the analytics conflate "not yet scored" with "scored low". Same posture as `query_interactions.was_injected` (substrate doc §6.2 — proactive_hook forward-compat with the 0 default; reference-resolution rows get 0 because they're agent-initiated, NOT because they weren't classified).

---

## 12. Cross-substrate seam

The substrate-trilogy-frontend trio (this chain + QF1's chain + F1's chain) is structurally symmetric. Each chain reads its substrate's tables, builds its lens, and emits a closing self-hosting audit event through the **write-side** substrate (the events ledger).

### 12.1 Inherited contracts (read-only)

- **`agent-first-substrate`** events ledger: this chain's `<EventTimeline />` extension reads via existing F2 endpoints. No new event types consumed.
- **`query-telemetry-substrate`** `grounding_events` / `query_interactions` / `query_resolutions`: this chain's inspector reads them via the new `/context-pulls/*` endpoints (which do their own joins). No new columns added to these tables — the side-table (`reference_resolution_emits`) carries the reference-resolution-specific detail.
- **`reference-resolution-substrate`** `resolve_references` handler emits: the side-table extension lands as an additive amendment to the existing emit path.

### 12.2 Outgoing audit event from this chain

RF4 (retrospective) emits one write-side event: **`ReferenceResolutionFrontendAuditCompleted`** (schema at `blueprints/events/ReferenceResolutionFrontendAuditCompleted.json`). This is the closing self-hosting audit, third in the substrate-trilogy-frontend family:

- `SubstrateFrontendAuditCompleted` — F5 (agent-substrate-frontend close, 2026-05-17)
- `TelemetryFrontendAuditCompleted` — QF5 (telemetry-frontend close, 2026-05-18)
- `ReferenceResolutionFrontendAuditCompleted` — RF4 (this chain close)

Schema mirrors the prior two: `chain_slug`, `audit_doc_path`, `tasks_completed`, `deferrals` (a JSON array of named deferrals — T7 ML score rendering when ML capability ships, audit-ledger bug cross-link wait, mid-stream system-reminder coverage gap), `closing_summary` (verbatim chain retrospective).

### 12.3 The Reserved namespace

`docs/EVENT_CATALOG.md` already reserves `Reference*` for the parent substrate chain. RF4's event lives under that prefix without a new reservation. The shape declared at chain close.

---

## 13. Open questions for review

Decisions proposed but the user may override before downstream tasks land.

1. **`/context-pulls` vs `/references` vs `/telemetry/references`.** Proposed `/context-pulls` — semantic, matches chain framing, doesn't collide with vault-style "references". Alternatives noted in §8.
2. **Default time range.** 30 days (matches QF1's analytics default). Alternative: 7 days (would catch only the current-week shape but may be too narrow to see the substrate's bake-in pattern post-T5). 30 chosen.
3. **Drawer as URL param vs separate route.** Proposed URL param `?event=<id>`. Alternative: separate route `/context-pulls/<id>` opens the drawer-equivalent as a focused page. URL param chosen for filter-state preservation per QF1 §4.3.
4. **First-candidate rendering on the row list.** Proposed: position-1 candidate, source_type-dispatched, truncated to one line. Alternative: render no candidate (only counts) on the list view, defer to drawer. Position-1 chosen because it's the operator-helpful affordance — "did the first hit look right?" is the dominant audit question. Drawer gives the full list.
5. **Stats banner: stacked bars vs sparkline.** Proposed: stacked horizontal bars for shape + tier; mini-bar trend strip for time-series. Alternative: side-by-side sparklines. Stacked bars chosen because the distribution shape (what % of references are `single_exact` vs `no_hit`) is the load-bearing diagnostic.
6. **Side-table location: amend `grounding_events` vs new table.** Proposed: new `reference_resolution_emits` side-table. Rationale per §3.6.
7. **Per-source_type sub-renderers in the candidate list.** Proposed: one component per source_type with a generic fallback. Alternative: one generic renderer that key-value-tables every field. Per-source_type chosen because the operator's mental model differs by source — vault candidates have a `learnings/<corpus>/<date>_<slug>.md` shape; kiwix candidates have a `<book>/<page>` shape; etc.
8. **`<EventTimelineExtension />` fetch posture: eager count vs lazy on expand.** Proposed: eager fetch of count (small payload), lazy fetch of items on expand. Alternative: fully lazy (no count until expand). Eager-count chosen because the count is the badge's load-bearing affordance — "this entity has 3 reference resolutions" is the operator's reason to expand.
9. **Routes don't carry `:project`.** Project scoping comes from the active `<ProjectPicker />` per the existing dashboard pattern (matches QF1 §11.7).
10. **ML score colorscale.** Proposed: green (≥0.8) — yellow-green (0.6–0.8) — yellow (0.4–0.6) — red-muted (0.2–0.4) — red-strong (<0.2). Matches the `<LabelKindBadge />` palette family. Confirmed only when T7 lands; T7's design may revise.
11. **Cross-link from QF3 trajectory page back to inspector.** Proposed: 5-line change in QF3's page to render a "View as resolution" link when `query_source = 'reference_resolution'`. RF3 owns the QF3 file edit; jointly owned post-chain.
12. **`q` vs `reference_text` URL param.** Proposed: both honored, `q` is canonical (matches QF1 / F1). `reference_text` is the friendly alias for shareable URLs. The page advertises `reference_text` in the UI's filter label; `q` is the wire-compact name.

---

## 14. Cross-references

- `docs/REFERENCE_RESOLUTION.md` — substrate T1 design (§2 shape taxonomy + §5 resolve_references action + §6 telemetry integration + §10 future-ML boundary + §12 worked example).
- `docs/TELEMETRY_SUBSTRATE.md` — TT1 substrate design; §2 three-layer hierarchy is load-bearing for §9's `prompt_id` join.
- `docs/TELEMETRY_FRONTEND.md` — QF1 sibling design; three-axis convention inherited; `<QueryTrajectoryView />` reused via deep-link.
- `docs/SUBSTRATE_FRONTEND.md` — F1 sibling design; `<EventTimeline />` reused + extended via per-type renderer suffix block.
- `docs/PROJECTIONS.md` §6.1 — TT3 LANDED projections; reference-resolution rows flow through `proj_training_data_for_reranker` natively.
- `docs/EVENT_CATALOG.md` — event-type catalog; `ReferenceResolutionFrontendAuditCompleted` registers at RF4 close under the `Reference*` reserved prefix.
- `crates/shared-db/migrations/037_telemetry_substrate.sql` — query_source CHECK base + prompt_id column.
- `crates/shared-db/migrations/038_telemetry_projections.sql` — `proj_training_data_for_reranker` etc.
- `crates/shared-db/migrations/040_grounding_events_reference_resolution_source.sql` — T5 CHECK-widening that admits `'reference_resolution'`.
- `crates/shared-db/migrations/041_grounding_events_harness_interception_source.sql` — T9 CHECK-widening that admits `'harness_reminder_interception'`.
- `crates/shared-db/migrations/042_reference_resolution_emits.sql` — **NEW** RF2 amendment, the side-table this chain reads from.
- `go/internal/refresolve/types.go` — authoritative `ShapeCategory` enum.
- `go/internal/refresolve/handler.go` — `emitGroundingEvents` helper that RF2 extends to also INSERT the side-table row inside the same write tx.
- `go/internal/observehttp/context_pulls.go` — **NEW** RF2 endpoint module.
- `apps/dashboard/src/components/shared/EventTimeline/per-type-renderers.tsx` — the renderer module this chain extends with the second suffix block (jointly owned with QF3).
- `apps/dashboard/src/components/shared/QueryTrajectoryView/` — the QF3 component this chain cross-links to.
- `apps/dashboard/src/pages/QueryTrajectoryView/` + `apps/dashboard/src/pages/Telemetry/` + `apps/dashboard/src/pages/TrainingPairs/` — the QF3/QF4/QF5 surfaces; sidebar siblings to this chain's `/context-pulls`.
- `apps/dashboard/src/pages/ContextPulls/` — **NEW** RF3 page module.
- `apps/dashboard/src/api/contextPulls.ts` — **NEW** RF3 API client module; shape mirrors `auditEvents.ts` + `telemetry.ts`.
- `apps/dashboard/src/router/index.tsx` — route registration site for `/context-pulls`.
- `apps/dashboard/src/components/layout/Sidebar/index.tsx` — sidebar nav addition site under the Telemetry group.
- Bug `audit-ledger-related-queries-ts-go-field-drift` — sibling-chain drift; this chain's §6.4 documents why the seam works without waiting on the fix.
- `~/.claude/vault/learnings/general/2026-05-17_tiered-implicit-feedback-for-rag-telemetry.md` — the four-tier click_kind pattern this chain's drawer renders.
