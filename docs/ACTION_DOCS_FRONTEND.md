# Action-Docs Frontend — Design

> **Status:** Approved contract. Produced by chain `action-docs-corpus-frontend` AF1 (`design-action-docs-frontend`). AF2 implements against this doc; AF3 verifies against it. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 data source → §3 endpoint contract → §4 route + URL contract → §5 listing layout → §6 detail-view layout → §7 cross-link contract → §8 caching + reload semantics → §9 accessibility / responsive → §10 testing surface → §11 closure event → §12 open questions.
>
> **Companion docs:** `go/internal/actiondocs/corpus/_schema.toml` (per-action TOML schema), `go/internal/actiondocs/corpus/README.md` (authoring conventions), `docs/SUBSTRATE_FRONTEND.md` §8.3 (dispatch-policy peek the cross-link binds to), `docs/EVENT_CATALOG.md` (where `ActionDocsFrontendAuditCompleted` will be registered at AF3).

---

## 1. What this surface is and isn't

Chain `action-docs-corpus` (closed 2026-05-17) shipped the per-action documentation corpus and the `admin.action_describe(surface, action)` MCP getter. Today the consumer is the agent — a human collaborator asking "what does `forge_edit` accept on a `chain`?" either greps `main.go`'s TOC, opens `go/internal/actiondocs/corpus/work/forge_edit.toml`, or calls MCP directly.

This chain ships the dashboard surface that makes the corpus human-browseable. Concretely: a new `/docs/actions` index grouped by surface, a per-action detail view at `/docs/actions/<surface>/<action>` that renders the typed TOML fields, and a bidirectional cross-link with the dispatch-policy peek so the rationale gate and the action prose deep-link to each other.

**Scope (and therefore this doc's scope):**

- New observe-HTTP endpoint serving the parsed corpus to the dashboard (`go/internal/observehttp/actiondocs.go`, AF2).
- `apps/dashboard/src/pages/ActionDocs/` page with list + detail routes (AF2).
- Bidirectional cross-link with `/admin/dispatch-policy` — outbound from detail → policy entry, inbound from each policy row → action-docs detail (AF2).
- `requires_rationale` state shown on detail view, sourced from `action-manifests/dispatch-policy.toml` via the existing `/admin/dispatch-policy` endpoint.
- Closing self-hosting event `ActionDocsFrontendAuditCompleted` emitted at AF3.

**Explicit non-goals:**

| Out of scope | Why |
|---|---|
| In-dashboard corpus editing | The TOML files are the source of truth; editing is a PR + restart workflow. Same discipline as `action-manifests/dispatch-policy.toml` (per `SUBSTRATE_FRONTEND.md` §8.3) and `EVENT_SUBSTRATE.md` §3.2's closed-catalog principle. |
| Qwen-mediated Q&A endpoint over the corpus | Action-docs-corpus T5 spike concluded DEFER for both Shape B (`action_ask`) and Shape C (proactive hook). If usage data later flips the recommendation, a separate chain opens — this chain's surface stays scoped to browsing. |
| Auto-rendered Markdown narrative | The corpus IS the documentation — the typed TOML structure (`purpose`, `params`, `param_aliases`, `value_aliases`, `errors`, `examples`, `notes`) is what renders. No prose templating, no field-merging into a synthetic sentence. |
| Cross-surface action search ("which action takes a `sha` param?") | Out of scope for AF2; punt to a follow-on if browsing reveals the friction. The corpus is small enough (85 chunks at AF1 time) that Cmd-F over a single surface's list page is the working fallback. |

---

## 2. Data source — observe-HTTP wrapper, not direct MCP

**Decision: pre-aggregated observe-HTTP endpoint, NOT direct MCP from the browser.**

The corpus has 85 chunks across four surfaces. The list view needs every chunk; the detail view needs one. Two access shapes were considered:

| Shape | What it looks like | Why rejected / accepted |
|---|---|---|
| Direct `admin.action_describe` per chunk | Browser calls MCP HTTP bridge 85 times for the list view, once per detail nav. | **Rejected.** Auth + CORS shape for browser → MCP isn't established; the existing dashboard reads exclusively through `observehttp`. Adding browser→MCP for one page is a contract drift. Also wasteful — 85 round-trips for a 150KB corpus that fits in one response. |
| Pre-aggregated observe-HTTP endpoint | Single `GET /admin/action-docs` returns the full corpus indexed by `(surface, action)`. Browser caches the response; detail-view lookup is a client-side map access. | **Accepted.** Mirrors the dispatch-policy peek shape (`GET /admin/dispatch-policy` returns the entire policy in one shot). Reuses the established observe-HTTP boundary. One response handles list + every detail view in the same session. |

The observe-HTTP handler reads from the in-process `actiondocs.Registry` populated at startup (`main.go:316–323`). The registry is read-once-at-startup by design (per chain `action-docs-corpus` closure summary); reload-from-disk semantics are described in §8.

---

## 3. Endpoint contract — `GET /admin/action-docs`

### 3.1 Request

```
GET /admin/action-docs              → full corpus
GET /admin/action-docs?surface=work → one surface
GET /admin/action-docs?surface=work&action=forge_edit → one chunk
```

| Param | Type | Default | Notes |
|---|---|---|---|
| `surface` | enum: `work` / `knowledge` / `measure` / `admin` | absent | When absent, every surface's chunks are returned. Unknown surface returns 200 with `surfaces: {}` (not 404 — the surface list shape is the truth). |
| `action` | string | absent | When supplied without `surface`, returns 400. Unknown action under a known surface returns 200 with that surface's `actions` map omitting the requested key. |

Query-param filters are **convenience for the dashboard**, not gates — the page always fetches the full corpus on mount and filters client-side for the listing. The filters exist so external consumers (curl, scripts) can target one chunk without parsing the full payload.

### 3.2 Response

```ts
interface ActionDocsResponse {
  // Total chunks served (includes _general entries).
  count: number
  // Sorted surface names; UI iteration order is wire-stable.
  surfaces: string[]
  // surfaces[i] → action_name → ActionDoc | null
  // The literal action name "_general" is present here for any surface
  // that has a _general.toml chunk. It's surfaced because the detail
  // view renders it; the list view filters it out.
  actions: Record<string, Record<string, ActionDoc>>
  // Set membership for "is this a write action?" — pulled from
  // action-manifests/dispatch-policy.toml. Keys are "surface.action"
  // (same format as policy.Registry.Actions()). Absence ⇒ read action.
  // Empty object when the policy file is not loaded; the dashboard
  // degrades to "kind: unknown" rather than failing the page render.
  write_actions: Record<string, true>
  // Absolute path of the corpus dir on disk (for the footer "loaded
  // from: …" affordance — same shape as /admin/dispatch-policy's path).
  // Empty string when the corpus is not loaded.
  corpus_path: string
  // Parse errors surfaced at load time (chunks present on disk but
  // unparseable). Empty array means a clean load. Rendered as a
  // collapsed banner on the list view; expansion shows the source
  // file + reason.
  parse_errors: ParseError[]
}

interface ActionDoc {
  surface: string
  action: string
  purpose: string
  params?: Param[]
  param_aliases?: ParamAlias[]
  value_aliases?: ValueAlias[]
  errors?: ErrorCondition[]
  examples?: Example[]
  notes?: string
}

interface ParseError {
  source_file: string
  err: string
}
```

The `ActionDoc` shape is exactly `actiondocs.ActionDoc` from `go/internal/actiondocs/registry.go` — JSON tags match field-for-field. **The wire shape and the disk shape are one schema, not two.** Same commitment as `admin.action_describe`'s hit branch.

### 3.3 Failure modes

| Condition | Status | Body |
|---|---|---|
| Corpus not loaded (registry is nil) | 200 | `{count: 0, surfaces: [], actions: {}, write_actions: {}, corpus_path: "", parse_errors: []}` |
| `action` supplied without `surface` | 400 | `{error: "action filter requires surface"}` |
| Dispatch policy unloaded (only affects `write_actions`) | 200 | Response is still 200; `write_actions` is `{}`. Dashboard degrades to "kind: unknown" badge. |
| Parse error on one chunk | 200 | Successful chunks load; the broken chunk is in `parse_errors`. Same non-fatal model as the registry's `ParseErrors()`. |

---

## 4. Route + URL contract

### 4.1 Routes

| Route | Page | Purpose |
|---|---|---|
| `/docs/actions` | `ActionDocsPage` index | Grouped list, intra-surface alphabetical. |
| `/docs/actions/:surface` | `ActionDocsPage` with surface tab active | Same page; URL-driven tab selection. |
| `/docs/actions/:surface/:action` | `ActionDocsPage` with detail open | Same page; detail view rendered alongside the list (or full-bleed on narrow viewports). |

A single page component owns all three URLs; React Router `:surface` and `:action` params drive the rendered state. This keeps deep-linking, browser-back, and intra-page tab switching on the same code path. Mirrors the `/telemetry/trajectories/:queryId` shape (`router/index.tsx:39–42`).

### 4.2 URL format is load-bearing

`/docs/actions/<surface>/<action>` is the **stable deep-link target** that cross-links from `/admin/dispatch-policy` rely on (see §7). The format is pinned here so the inbound link from policy peek can be constructed without round-tripping to the action-docs page.

`_general` chunks: `/docs/actions/<surface>/_general` resolves correctly. The detail view renders `_general` chunks (they're surface-wide prose, not real actions) — but they're excluded from the list view's enumeration, same as `actiondocs.Registry.List()`'s contract.

### 4.3 Sidebar nav

A new nav entry under the existing flat sidebar: **"Action Docs"** → `/docs/actions`. Placed below the existing "Dispatch Policy" entry so the two related admin/reference surfaces sit adjacent. No nav grouping or section header in this chain — the sidebar is flat today; restructuring it is a separate concern.

---

## 5. Listing layout

The list view is grouped by surface, alphabetical within surface. Four surfaces today (`admin`, `knowledge`, `measure`, `work`); rendered as a horizontal tab strip mirroring `AdminDispatchPolicyPage`'s tab pattern.

```
┌─ Action Docs ──────────────────────────────────────────┐
│  Loaded from: embedded                 [Reload]        │
│  [admin] [knowledge] [measure] [work]                  │  ← tabs, URL-driven
├────────────────────────────────────────────────────────┤
│  ▸ _general                — surface-wide conventions  │  ← _general row, dimmed
│  ▾ bug_list                — read    Bug list query…   │  ← purpose excerpt (one line)
│  ▾ bug_read                — read    Read one bug…     │
│  ▾ bug_resolve  rationale  — write   Resolve a bug…    │  ← "rationale" badge when requires_rationale
│  …                                                     │
└────────────────────────────────────────────────────────┘
```

| Column | Source | Notes |
|---|---|---|
| Action name | `ActionDoc.action` | Monospace, clickable — navigates to `/docs/actions/<surface>/<action>`. |
| Rationale chip | `write_actions["<surface>.<action>"]` truthy | Same color as the dispatch-policy "required" chip. Absent for read actions. |
| Kind chip | derived from `write_actions` | `read` for absence, `write` for presence. Single chip — kind and rationale-requirement are 1:1 today; if they diverge in the future, split the chips. |
| Purpose excerpt | `ActionDoc.purpose`, truncated at 100 chars | One-line preview; click action name for full detail view. |

`_general` rows render at the top of each surface with a subdued styling (no kind chip, no rationale chip, distinct "surface-wide conventions" label). They're surfaced because they document conventions the actions below depend on.

---

## 6. Detail-view layout

The detail view renders one `ActionDoc` chunk fully. Layout is top-down through the TOML's field order — the schema's ordering is the doc's ordering, no creative re-arrangement.

```
┌─ work.forge_edit ──────────────────────────────────────┐
│  [← back to work]                                      │
│  ┌─ Kind ─┐ ┌─ Rationale ─────┐ ┌─ Dispatch policy ──┐ │
│  │ write  │ │ required        │ │ See /admin/dispatch-│ │  ← cross-link
│  └────────┘ └─────────────────┘ │ policy#work-forge_  │ │     to policy peek
│                                  │ edit                │ │
│                                  └─────────────────────┘ │
│                                                          │
│  Purpose                                                 │
│  Edit fields of an existing artifact in place…           │
│                                                          │
│  Parameters                                              │
│  ┌──────────┬────────┬─────────┬──────────────────────┐ │
│  │ name     │ type   │ required│ description          │ │
│  ├──────────┼────────┼─────────┼──────────────────────┤ │
│  │ slug     │ string │ true    │ Artifact slug…       │ │
│  │ fields   │ table  │ true    │ Edits to apply…      │ │
│  └──────────┴────────┴─────────┴──────────────────────┘ │
│                                                          │
│  Param aliases                                           │
│  • id → slug                                             │
│                                                          │
│  Value aliases                                           │
│  • resolution_kind: fix → fixed                          │
│                                                          │
│  Errors                                                  │
│  • not_found — artifact missing                          │
│                                                          │
│  Examples                                                │
│  ┌─ Sugar shape ───────────────────────────────────────┐│
│  │ {schema_name: 'bug', slug: '…', title: 'new title'}││
│  └────────────────────────────────────────────────────┘│
│                                                          │
│  Notes                                                   │
│  Lorem ipsum dolor sit amet…                             │
└──────────────────────────────────────────────────────────┘
```

### 6.1 Section rules

| Field | Render | Empty-state handling |
|---|---|---|
| `purpose` | Plain paragraph, `white-space: pre-wrap` | Required by schema; never empty. |
| `params` | Sortable table (default order: schema order) | Section omitted entirely when array is absent/empty. |
| `param_aliases` | Unordered list `from → to`, optional `notes` inline | Section omitted when empty. |
| `value_aliases` | Unordered list `param: from → to`, optional `notes` inline | Section omitted when empty. |
| `errors` | Unordered list `condition — message` | Section omitted when empty. |
| `examples` | Per-example card with `description` header + `<pre>` code block | Section omitted when empty. |
| `notes` | Plain paragraph, `white-space: pre-wrap` | Section omitted when string is empty. |

**No Markdown rendering anywhere.** The corpus is verbatim agent text; `white-space: pre-wrap` preserves newlines and indentation without opening a Markdown surface. Same discipline as `EventDetailDrawer`'s rationale rendering (`SUBSTRATE_FRONTEND.md` §7).

### 6.2 Header chips

The detail-view header carries three chips in fixed order:

1. **Kind** — `read` / `write` / `unknown`. Sourced from `write_actions` membership; `unknown` when policy is unloaded.
2. **Rationale** — `required` / `not required` / `unknown`. Same source.
3. **Dispatch policy** — clickable link to `/admin/dispatch-policy` deep-anchored to the row (see §7.2). This chip is the **outbound** half of the cross-link.

---

## 7. Cross-link contract with dispatch-policy peek

### 7.1 Both directions

Chain `agent-substrate-frontend` F5 closed 2026-05-17 with the dispatch-policy peek shipped at `/admin/dispatch-policy`. Its retrospective (`docs/SUBSTRATE_FRONTEND_RETROSPECTIVE_2026-05-17.md`) explicitly deferred the cross-link to this chain:

> `action-docs-corpus` cross-link in policy peek | F1 §8.3 designed a `data-action-key` hook on each policy row. Wiring the popover-fetch waits for the action-docs-corpus chain to close.

Each policy peek row carries `data-action-key="${surface}.${action}"` (verified at `apps/dashboard/src/pages/AdminDispatchPolicy/index.tsx:160`). This is the binding surface the inbound link uses.

| Direction | From | To | Implementation |
|---|---|---|---|
| Outbound | `/docs/actions/<surface>/<action>` detail-view header | `/admin/dispatch-policy#<surface>.<action>` | Header chip 3 in §6.2. Anchor format is `<surface>.<action>` — matches the policy peek's `data-action-key` value so a sibling `id="<surface>.<action>"` attribute on each row provides the scroll target. |
| Inbound | Each row in `/admin/dispatch-policy` | `/docs/actions/<surface>/<action>` | A "docs" link rendered on every policy row, after the rationale chip. Uses the existing `data-action-key` attr to build the URL. |

### 7.2 Anchor format on the dispatch-policy page

AF2 adds `id="${surface}.${action}"` to each policy-peek row (extending the existing `data-action-key`-only attribute). The anchor target is the row element so browser scroll-to-anchor lands on the right row. Anchor format is the same `surface.action` shape as the policy registry's keys — one format, three uses (registry key, data-action-key, URL anchor).

### 7.3 Graceful degradation

If `write_actions` is empty (policy not loaded), the cross-link chip still renders but points to `/admin/dispatch-policy` without the anchor (the page handles missing anchors by ignoring them; same browser default).

If a policy row points to an action that has no corpus chunk (an orphan-from-the-other-side, like `work.work_actions` per the corpus closure summary), the inbound "docs" link still renders but resolves to the `/docs/actions/<surface>/<action>` page's "action not found" state. AF2 implements that empty-state explicitly (see §10 test e).

---

## 8. Caching + reload semantics

The `actiondocs.Registry` is loaded once at server startup (per chain `action-docs-corpus` T2). Unlike `policy.Load` which the dispatch-policy peek calls fresh per request, the corpus is larger (85 chunks vs. ~20 policy entries) and authoring is rarer (PR + restart workflow).

**Decision: serve from the in-process registry by default; on `?reload=1`, load fresh from disk for that response.**

The handler accepts a `?reload=1` query param. When present, the handler calls `actiondocs.Load(corpusDir)` afresh and serves the resulting registry for this response only (no swap of shared state — the per-request fresh load is sufficient because each operator reload independently re-reads the file). When absent (default), the handler reads from the startup-loaded registry without I/O. The startup registry is shared with `admin.action_describe` (MCP) — operators who want MCP-side to also pick up corpus changes call `admin.schema_reload`, the same affordance that exists for dispatch-policy.

**Why not "fresh per request" like dispatch-policy:**

- Corpus is larger; 85 file reads per request is wasted I/O when authoring frequency is "rare PR".
- The closing principle is the same — the file is the source of truth — but the cadence justifies different mechanics.
- The reload button mirrors the dispatch-policy "Reload from disk" UX, so operator muscle memory is preserved.

The dashboard's HTTP layer sets `Cache-Control: no-cache` on the response so the browser doesn't bake stale chunks into back-button caches.

---

## 9. Accessibility / responsive

- **Semantic HTML**: `<nav>` for the surface tab strip, `<main>` for the list+detail body, `<table>` for the parameter rows (not divs-styled-as-rows), `<dl>` for the param-alias / value-alias / error lists.
- **ARIA**: `role="tab"` + `aria-selected` on surface tabs (mirrors `AdminDispatchPolicyPage`). `aria-current="page"` on the active detail-view row in the list. Anchored cross-link target rows carry no extra ARIA — they're regular table rows.
- **Keyboard nav**: Tab order is sidebar → surface tabs → action list (one tab stop per row) → detail-view scroll region. Enter on a list row opens the detail; the back-link in the detail header is focusable and Enter-activates.
- **Responsive**: Two-pane layout (list left, detail right) on viewports ≥ 1024px. Below 1024px, the detail view is full-bleed and the list view is reachable via the back button. The CSS module follows the same pattern as `ChainIndex` (left-list + right-detail) — concrete breakpoint pinned in AF2.

---

## 10. Testing surface (AF2 must cover all)

| Test | What it pins |
|---|---|
| (a) page renders given mocked corpus | Happy path — `vi.mock` the `getActionDocs` API as in `AdminDispatchPolicy/index.test.tsx`. |
| (b) listing groups by surface, alphabetical within surface | List view ordering. |
| (c) detail view renders all TOML fields cleanly | `purpose`, `params`, `param_aliases`, `value_aliases`, `errors`, `examples`, `notes` all render; absent fields omit their section. |
| (d) deep-link `/docs/actions/<surface>/<action>` resolves for every entry | Driven by router params; one parameterized test that walks every (surface, action) pair from a fixture corpus. |
| (e) `requires_rationale` chip reflects dispatch-policy correctly | Mock both `getActionDocs` (returning `write_actions`) and verify chip text/class for one read action + one write action. |
| (f) empty-state when corpus is empty | `{count: 0, surfaces: [], actions: {}, write_actions: {}, corpus_path: "", parse_errors: []}` → "no docs loaded" message. |
| (g) accessibility — semantic HTML, ARIA on tabs | `getByRole('tab')`, `getByRole('table')`, `getByRole('main')` queries pass. |
| (h) outbound + inbound cross-link with `/admin/dispatch-policy` resolves correctly for a sample action | Click cross-link chip → assert navigation URL ends in `#<surface>.<action>`. From policy page (separate test in `AdminDispatchPolicy/index.test.tsx`), click docs link → assert URL is `/docs/actions/<surface>/<action>`. |

Go-side endpoint test (`go/internal/observehttp/actiondocs_test.go`):

- Empty registry returns 200 + zero-value response.
- Loaded registry returns full corpus.
- `?surface=work` filters to one surface.
- `?surface=work&action=forge_edit` filters to one chunk.
- `?action=forge_edit` without `surface` returns 400.
- `?reload=1` invokes `Load` and the new corpus is reflected on the next un-reloaded GET.
- Parse errors propagate into `parse_errors`.
- `write_actions` reflects the dispatch-policy file (test fixture supplies both).

---

## 11. Closure event

AF3 emits `ActionDocsFrontendAuditCompleted` through the events ledger this chain made readable. Same payload shape as `SubstrateFrontendAuditCompleted` (audit_doc / summary / recommended_next_phase / findings), keyed to this design doc's sections and the chain's completion_condition items (a)–(e).

Registration steps owned by AF3:

1. Add type to `docs/EVENT_CATALOG.md` reserved-namespace table.
2. Add `blueprints/events/ActionDocsFrontendAuditCompleted.json` schema (mirror `SubstrateFrontendAuditCompleted.json`).
3. Add `ActionDocsFrontendAuditCompletedPayload` to `go/internal/events/payloads.go`.
4. Add to `go/internal/events/events_test.go`'s registered-types list.
5. Add `go/cmd/action-docs-frontend-audit-emit/main.go` mirroring `substrate-frontend-audit-emit/`.
6. Run the binary against `data/toolkit.db`; verify the event lands.
7. Reference the event ID + binary in the closure_summary.

---

## 12. Open questions

None at AF1 close. The chain is small; ambiguity surface is correspondingly small. AF2 may discover surprises in the responsive breakpoint (pinning concrete numbers) or in the `_general`-row styling, but neither rises to a design-level question.
