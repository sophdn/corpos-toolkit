# Chain 4 — Telemetry Page IA: Audit Ledger + Locked Target (refactor steps 3-5)

> Companion to `CHAIN4_PAGE_IA_INVENTORY.md` (steps 1-2). T2 = the classified findings ledger (step 3);
> T3 = the first-principles target + triage (steps 4-5), **locked against Sophi's vetting** (recorded
> in §T3.0). Endpoints/projections are out of scope and frozen (Chains 1-3 own them).

---

## T2 — Audit across the axes (classified findings ledger)

Each finding: **[class]** behavior-preserving (BP) / behavior-changing (BC) / taste-only (T), plus a
blast-radius estimate. Conformance (Q2) delegates to `coding-philosophy` + `code-standards` +
`layout-conventions` — not restated here.

| # | Axis | Finding | Class | Blast radius |
|---|---|---|---|---|
| F1 | Naming/clarity | The `/telemetry` page is labelled "Telemetry Analytics" but only shows search volume + retrieval success — misleading, since inference, context-pulls, spans and audit are all telemetry too. | **BP** (rename label + `<h2>`; data unchanged) | 1 page label + 1 heading + 1 nav label + 1 structure-test expectation |
| F2 | Naming/clarity | The Inference nav label says "Qwen Inference"; Chain 1 made the substrate model-agnostic (`inference_invocations` carries Claude+Qwen+future). The "Qwen" qualifier is now a misnomer. | **BP** (nav label only) | 1 nav label + 1 structure-test expectation (page `<h1>` also says "Qwen Inference" — rename for consistency) |
| F3 | Structure/cohesion | The per-tool-per-model ranking is buried as a panel on the Inference page; the chain's completion_condition asks for a ranking *page* over `proj_inference_tool_model_performance`. | **BC** (new page = new capability/route; gets new tests, not parity) | new page + new route + new nav entry; panel removed from Inference (vetted) |
| F4 | Structure (IA) | The sidebar is a flat 17-link list with no sections; the read-side (`/telemetry`) vs write-side (`/audit`) framing is documented but invisible in the nav. | **T** (visual grouping; no route/data change) | Sidebar component + its CSS + the nav structure-test |
| F5 | Edge cases / net coverage | `KnowledgePage` and `QueryTrajectoryViewPage` have no render test (pre-existing gap). | **BP** (net gap, not a code defect) | densification (T5), only if the refactor touches them |
| F6 | DRY / reuse (Q6/Q7) | Knowledge Index + Memory Substrate are read-side folded projections (program §1) — candidate to fold under the telemetry section. | **T** (grouping choice) | nav only — **VETTED: keep separate** (see T3.0) |
| F7 | DRY / redundancy | If the ranking becomes a page, leaving the panel on Inference duplicates the same `/inference/tool-model-performance` read in two places. | **BC/T** | **VETTED: remove the panel** (see T3.0) |
| F8 | Conformance (Q2) | New page + sectioned nav must obey `layout-conventions` (design tokens, raw flex/grid, no wrapper libs) and reuse the shared `ControlsBar` title/filter primitive + recharts + forward-fill empty-state pattern rather than inventing new ones. | **BP** (constraint on how F3/F4 are built) | execution-time guardrail |

**Q4 (dead code):** none found specific to the IA — the route table and nav are all live. **Q5
(error handling):** the new ranking page must handle the empty/cold-projection state (no rows yet)
the same graceful way the embedded panel does (it hides when empty) — pinned in densification.
**Q8 (first-principles):** see T3.

**Rejections / out-of-scope (recorded so they aren't re-audited):**
- Route *moves* for the renamed page (`/telemetry` → `/telemetry/search`) — **rejected**: adds redirect
  machinery + a moved-route test for zero IA benefit (the nav label, not the URL, is the user's window;
  `/inference` and `/context-pulls` already sit outside `/telemetry/*`, so a route-move can't make the
  prefix uniform anyway). Keep `/telemetry`.
- Collapsing Benchmarks/Deferred-Ports into telemetry — **rejected**: they're ML/work surfaces, not
  read-side observability. Own section.
- Reworking the Context Pulls / Training Pairs / Snapshot Corpus *page internals* — out of scope; this
  chain relocates and reframes, it does not redesign page bodies.

---

## T3 — First-principles target + triage

### T3.0 — Vetted decisions (locked; AskUserQuestion, 2026-05-26)

1. **Nav grouping = Option A** — top-level sections: WORK · TELEMETRY (read-side) · AUDIT (write-side)
   · KNOWLEDGE · ML / BENCHMARKS · ADMIN.
2. **Rename** "Telemetry Analytics" → **"Search Analytics"** (nav label + page `<h2>`). **Route
   `/telemetry` kept** (no move).
3. **Remove** the per-tool-per-model panel from the Inference page; the ranking lives **only** on the
   new page.
4. **Knowledge + Memory stay in their own KNOWLEDGE section** (not folded into telemetry).

### T3.1 — Target IA (the from-scratch shape)

```
WORK                         TELEMETRY (read-side)        AUDIT (write-side)
  Chains & Tasks  /tasks/chains   Inference        /inference     Audit Ledger /audit
  Roadmap         /roadmap        Model Ranking    /telemetry/model-ranking   Live Spans /spans
  Bug Index       /bugs           Search Analytics /telemetry  (renamed)
  Suggestion Index /suggestions   Context Pulls    /context-pulls
                                  Training Pairs   /telemetry/training-pairs
KNOWLEDGE                         Snapshot Corpus  /telemetry/snapshot-corpus
  Knowledge Index /knowledge
  Memory Substrate /knowledge/memory-substrate
ML / BENCHMARKS                   ADMIN
  Local LLM Task Perf /benchmarks   Dispatch Policy /admin/dispatch-policy
  Deferred Ports /deferred-ports    Action Docs /docs/actions
```
Query Trajectory (`/telemetry/trajectories/:queryId`) remains a deep-linked detail view, not a nav item.

### T3.2 — Section delta vs current flat nav
- **NEW:** section headers (6) in the Sidebar; the new "Model Ranking" nav entry + route + page.
- **RENAMED:** "Telemetry Analytics" → "Search Analytics"; "Qwen Inference" → "Inference".
- **MOVED (visual only):** Inference + Context Pulls join the TELEMETRY section (routes unchanged);
  Audit Ledger + Live Spans become the AUDIT section.
- **REMOVED:** `ToolModelPerformancePanel` from the Inference page body.

### T3.3 — Triage (value × risk) → execution order for T4
1. **Sectioned Sidebar (F4)** — high value (surfaces the whole framing), low risk (presentational; nav
   structure-test pins it). *Commit 1.*
2. **Rename Search Analytics (F1) + drop "Qwen" (F2)** — high value, low risk (label/heading +
   structure-test). *Commit 2.*
3. **New Model Ranking page (F3) + remove panel (F7)** — high value, medium risk (new route/page; the
   removed panel's data must still be reachable — now on the new page). New tests for the page;
   Inference's data-parity test updated only where the panel assertion lived (that assertion moves to
   the new page's test — it is *relocated*, not weakened). *Commit 3.*
4. **Conformance pass (F8)** — fold into each commit above (use ControlsBar/recharts/tokens), not a
   separate step.

### T3.4 — Parity contract for T4
- DATA-parity net (per-page index.test.tsx for surfaces whose *body* is unchanged: Search Analytics,
  Training Pairs, Snapshot Corpus, Context Pulls, Audit, Spans + the Go endpoint goldens) stays green
  and **unmodified**.
- Inference page test: the only allowed edit is **removing** the assertions that targeted the embedded
  ranking panel (because the panel is deliberately removed) — those assertions are **re-homed** on the
  new Model Ranking page's test, so coverage of that data does not drop. This is a vetted
  behavior-change (F3/F7), not a silent weakening.
- STRUCTURE net (routes.test.tsx + Sidebar/index.test.tsx) updated in the same commit as each change;
  the diff is the record of what moved.

---

## T5 — Post-refactor net densification (step 7)

The new `ModelRanking` page is the seam the refactor exposed; the parity net never targeted it as a
*page*. Densified `pages/ModelRanking/index.test.tsx` over its own input-classes (strictly additive —
no parity assertion modified):

- **Acceptance:** ranking renders; remote-model row first-class; null `avg_tokens` → em-dash (re-homed).
- **Grouping boundary (the `firstOfGroup` branch the embedded panel never isolated):** tool label shown
  only on the first row of a group; blank on subsequent same-tool rows; **re-printed** at a new-tool
  boundary; single-row group prints its label.
- **Ordering:** the page preserves the server's row order (does not re-sort).
- **Transient/edge states (new as a page):** loading branch before resolve; explanatory empty-state when
  the projection is empty (vs the panel's hide-when-empty); error branch on fetch failure.

This exercises every branch of the new component: `loading | error | empty | populated`,
`firstOfGroup true|false`, `avg_tokens null|non-null`. **Coverage/mutation tooling note:** the dashboard
has no `@vitest/coverage-v8` (or mutation) dependency installed, so the gate here is the enumerated
input-space matrix above, not a coverage %. 498 dashboard tests green.

### F5 net-gap decision (Knowledge + QueryTrajectoryView render tests)
**Not filled — logged.** The "extend if touched" rule applies: this refactor did **not** touch either
page's body. `KnowledgePage` stayed in its own KNOWLEDGE nav section (no code change); Query Trajectory
is a deep-linked detail view, not a nav item, and was untouched. Filling those nets is out of scope for
a relocation/rename refactor; they remain a standing pre-existing gap for whichever chain next modifies
those pages.

---

## T6 — Retrospective + completion_condition verification

**completion_condition (verified clause-by-clause):**
1. *One telemetry nav section with consistent layout/format* — ✅ the Sidebar groups Inference, Model
   Ranking, Search Analytics, Context Pulls, Training Pairs, Snapshot Corpus under one **TELEMETRY**
   (read-side) section, beside the **AUDIT** (write-side) section; pinned by `Sidebar/index.test.tsx`.
2. *Misleading "Telemetry" page renamed* — ✅ "Telemetry Analytics" → "Search Analytics" (label + `<h2>`);
   no shipped page/nav source still carries the old label.
3. *Per-tool-per-model ranking page renders inference_tool_model_performance* — ✅ new
   `/telemetry/model-ranking` page reads `/inference/tool-model-performance` (which serves
   `proj_inference_tool_model_performance`); pinned by `pages/ModelRanking/index.test.tsx`.
4. *Endpoint/visual parity preserved* — ✅ Go `observehttp` endpoint goldens green & untouched; the
   per-page data-parity vitest suites green & unmodified (the only edit was relocating the ranking
   panel's assertions onto the new page's test); production `vite build` succeeds.
5. *Page-IA mockups vetted by user before build* — ✅ vetted via AskUserQuestion (Option A nav, "Search
   Analytics", remove panel, keep Knowledge separate) **before** the T4 execution commits.

**What moved:** flat 17-link nav → 6 sectioned groups; 2 renames; 1 promoted page; 1 removed panel.
**What didn't:** every endpoint, every projection, every page's data. Net proof: 498 dashboard tests +
the Go goldens, green throughout; the data-parity assertions were never weakened.

**Lesson (filed to vault):** for a *page-IA* refactor the characterization net splits cleanly into
**data-parity (frozen)** and **structure-parity (where the deltas land)** — and the route table + nav
were the exact unpinned structure. Pinning them first (with a make-it-testable extraction of the route
table to data) turned each IA change into a reviewable test diff instead of a silent move.
