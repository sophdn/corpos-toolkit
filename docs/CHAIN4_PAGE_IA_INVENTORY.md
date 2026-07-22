# Chain 4 — Telemetry Page IA Unification: Inventory + Characterization Net (refactor steps 1-2)

> **Status:** steps 1-2 artifact for chain `telemetry-page-ia-unification` (308), the 4th chain of
> the telemetry-consolidation program (see `TELEMETRY_CONSOLIDATION.md` §3, Chain 4). Chains 1-3
> (`inference_invocations` substrate, success-model unification, data-driven routing) have landed; the
> read-side projections + endpoints this chain reorganizes already exist. This document pins the
> CURRENT page information architecture and the net that guards it, then proposes the target IA —
> **which Sophi vets before any reorganization** (the dashboard is the user's window; consolidation
> doc §5.4). Nothing in §5 is built until that vetting lands.

---

## 1. Boundary & scope

**In scope:** the dashboard's *page information architecture* — the sidebar nav, the route table, the
page titles, and the shared page-shell conventions (filter bars, empty-states, recharts) — for the
telemetry/observability cluster: Inference, Telemetry Analytics, Query Trajectory, Context Pulls,
Training Pairs, Snapshot Corpus, plus the per-tool-per-model ranking currently embedded as a panel on
the Inference page. The Audit Ledger and Live Spans are touched only insofar as the IA *frames* the
read-side (`/telemetry`) vs write-side (`/audit`) split.

**Out of scope (behavior that must NOT change):** every endpoint's JSON contract (Chains 1-3 own those
and their goldens stay green untouched); the per-page *data* each surface renders; the DB projections.
This chain moves where pages live and what they're called — **not what they compute**. The net (§3)
encodes exactly that boundary: data parity is frozen, structure is the only thing allowed to move, and
each structural move is a deliberate, vetted, diffable change.

---

## 2. Inventory — the telemetry/observability cluster

| Page (current nav label) | Route | Component | Endpoint(s) | Projection / source | What it answers | Has test? |
|---|---|---|---|---|---|---|
| **Telemetry Analytics** | `/telemetry` | `TelemetryAnalyticsPage` | `/telemetry/analytics/volume-by-source`, `/telemetry/analytics/success-rate` | `proj_query_volume_by_day`, `proj_query_success_by_day` | Search **volume** + **retrieval success rate** time-series, by `action` or `query_source` | ✅ (rich) |
| **Qwen Inference** | `/inference` | `InferencePage` | `/inference/health-cards`, `/inference/retrieval-health`, `/inference/tool-model-performance`, `/inference/sparklines` | `inference_invocations`, `proj_inference_call_success`, `proj_inference_tool_model_performance` | Per-task model-call health: latency p50/p95/p99, success%, tokens, sparklines, **+ embedded per-tool-per-model ranking panel** | ✅ |
| **(panel) Per-tool/per-model** | *(none — embedded on `/inference`)* | `ToolModelPerformancePanel` | `/inference/tool-model-performance` | `proj_inference_tool_model_performance` | Per (tool × model) ranking: calls, success%, avg/max latency, avg tokens | via Inference test |
| **Training Pairs** | `/telemetry/training-pairs` | `TrainingPairsBrowser` | `/telemetry/training-pairs`, `/telemetry/training-pairs/stats` | `proj_training_data_for_reranker` | Reranker training-corpus spot-check by `label_kind` / `query_source` | ✅ |
| **Snapshot Corpus** | `/telemetry/snapshot-corpus` | `SnapshotCorpusPage` | `/telemetry/snapshot-corpus/stats` | `arcreview_snapshot_corpus` | Arc-close classifier training-substrate readiness | ✅ |
| **Query Trajectory** | `/telemetry/trajectories/:queryId` | `QueryTrajectoryViewPage` | `/telemetry/trajectories/{id}`, `?span_id=` | `grounding_events` + `query_interactions` + `query_resolutions` | Per-query deep-dive (results, clicks, resolutions); deep-linked detail view | ❌ (gap) |
| **Context Pulls** | `/context-pulls` | `ContextPullInspector` | `/context-pulls`, `/context-pulls/{id}`, `/context-pulls/stats[/timeseries]` | `reference_resolution_emits` + grounding/interactions/resolutions | Reference-resolution audit on 4 axes (query_source / shape / confidence_tier / source_type) | ✅ |
| **Audit Ledger** *(write-side frame)* | `/audit` | `AuditLedgerPage` | `/events/list`, `/events/{id}`, `/entities/{kind}/{slug}/events` | `events` ledger | Append-only state-mutation log (actor + rationale + span) | ✅ |
| **Live Spans** *(write-side frame)* | `/spans` | `SpansPanel` | `/events/spans` (SSE) | in-memory span stream | Live span tree by trace_id | ✅ |

Adjacent (read-side folded projections, candidate for the same section — see §5 vetting): **Knowledge
Index** (`/knowledge`, `KnowledgePage`, no test) and **Memory Substrate** (`/knowledge/memory-substrate`,
`MemorySubstratePage`, ✅). **Benchmarks** (`/benchmarks`) and **Deferred Ports** (`/deferred-ports`)
are ML/work surfaces, not telemetry — left in their own group.

### 2.1 Current nav (the IA being reorganized)

The sidebar (`components/layout/Sidebar/index.tsx`) is a **single flat list of 17 `NavLink`s, no section
headers**, in this order: Chains & Tasks · Roadmap · Bug Index · Suggestion Index · Local LLM Task
Performance · Deferred Ports · Knowledge Index · Memory Substrate · Qwen Inference · Live Spans · Audit
Ledger · Telemetry Analytics · Training Pairs · Snapshot Corpus · Context Pulls · Dispatch Policy ·
Action Docs.

---

## 3. The characterization net (parity oracle) — GREEN before any reorganization

This is a *page-IA* refactor, so the net splits in two:

**(A) DATA parity — frozen, must stay green unmodified across the whole chain.** These pin *what each
page answers* and *what each endpoint returns*; the IA refactor may not change a single assertion here.
- Frontend: the per-page `index.test.tsx` suites (Telemetry, Inference, Training Pairs, Snapshot
  Corpus, Context Pulls, Audit Ledger, Spans, Memory Substrate, Benchmarks) — **464 tests, green.**
- Backend: the Go endpoint goldens in `go/internal/observehttp/` (`/telemetry/*`, `/inference/*`,
  `events`) — `go test ./internal/observehttp/` **green (16.8s).** Endpoints are *not* touched by this
  chain, so these are a stable oracle.

**(B) STRUCTURE parity — the snapshot this chain's deltas land in (NEW, added in step 2).** The route
table and nav were *both untested* — the exact IA this chain reorganizes had no pin. Added:
- `src/router/routes.test.tsx` (24 tests) — pins every concrete route → page-component (acceptance),
  the full route-path set (no silent add/drop), and the three redirect routes (index, `/admin`, `*`).
- `src/components/layout/Sidebar/index.test.tsx` (2 tests) — pins the exact flat link list (label +
  href, in order) and asserts *no section headers exist yet*.

Total: **490 frontend tests green, tsc clean.** When the vetted IA lands, the *expected values* in (B)
are updated **in the same commit as the change** — that diff is the reviewable record of exactly what
moved/renamed — while (A) stays green and unmodified (the proof that no page lost its data).

**Step-2 micro-refactor (make-it-testable):** the route table was extracted from `router/index.tsx`
into `router/routes.tsx` as exported data. Importing a constructed `createBrowserRouter` eagerly
initializes navigation against the host history, which throws under jsdom; exporting the table as data
makes the IA inspectable in isolation. `index.tsx` still exports `router` (App.tsx unaffected).
Behavior-preserving: the same array constructs the same router.

**Pre-existing net gaps (findings, see §6):** `KnowledgePage` and `QueryTrajectoryViewPage` have no
render test. These are *not* filled in step 2 (they're outside the route/nav structure being moved);
they are logged for the step-7 densification pass and built only if the refactor touches them.

---

## 4. The tangle (current IA problems — from `TELEMETRY_CONSOLIDATION.md` §1)

1. **The page literally named "Telemetry" only shows *search* volume/success** — misleading, since
   inference, context-pulls, training-pairs, spans and audit are *all* telemetry.
2. **No grouping.** 17 flat links; the read-side observability surfaces (Inference, Telemetry,
   Context Pulls, Training Pairs, Trajectory) are scattered among work/admin links with no visual
   "these answer the same kind of question" cue.
3. **The per-tool-per-model ranking is buried** as a panel on the Inference page, though the chain's
   completion_condition calls for a ranking *page* over `proj_inference_tool_model_performance`.
4. **The read-side (`/telemetry`) vs write-side (`/audit`) framing is documented but not surfaced** in
   the nav — a newcomer can't see that Audit Ledger is the write-side ledger and the rest is read-side.
5. **"Qwen Inference"** is now a misnomer — Chain 1 made the substrate model-agnostic
   (`inference_invocations` carries Claude + Qwen + future); the nav label still says Qwen.

---

## 5. Proposed target IA — **PENDING SOPHI'S VETTING (the gate)**

A sectioned sidebar that surfaces the read-vs-write framing and groups the read-side observability
cluster. Two grouping granularities are offered for vetting (§5.1 / §5.2); the page-level renames and
the new ranking page (§5.3) are common to both. **None of this is built until vetted.**

### 5.1 Option A — "Telemetry" umbrella (recommended; matches the program's vocabulary)

```
WORK
  Chains & Tasks · Roadmap · Bug Index · Suggestion Index
TELEMETRY                          (read-side: "what the substrate is doing")
  Inference            /inference            (renamed label: "Qwen Inference" -> "Inference")
  Model Ranking        /telemetry/model-ranking   (NEW page over inference_tool_model_performance)
  Retrieval Analytics  /telemetry/retrieval        (RENAMED page: "Telemetry Analytics")
  Context Pulls        /telemetry/context-pulls
  Training Pairs       /telemetry/training-pairs
  Snapshot Corpus      /telemetry/snapshot-corpus
AUDIT                              (write-side: state mutations + rationale)
  Audit Ledger         /audit
  Live Spans           /spans
KNOWLEDGE
  Knowledge Index      /knowledge
  Memory Substrate     /knowledge/memory-substrate
ML / BENCHMARKS
  Local LLM Task Perf  /benchmarks · Deferred Ports /deferred-ports
ADMIN
  Dispatch Policy /admin/dispatch-policy · Action Docs /docs/actions
  (Query Trajectory is a deep-linked detail view, not a top-level nav item)
```

### 5.2 Option B — "Observability" umbrella, read & write as sub-headers under it

```
OBSERVABILITY
  -- read-side --
  Inference · Model Ranking · Retrieval Analytics · Context Pulls · Training Pairs · Snapshot Corpus
  -- write-side --
  Audit Ledger · Live Spans
WORK · KNOWLEDGE · ML/BENCHMARKS · ADMIN  (as in Option A)
```

### 5.3 Page-level changes common to both options
- **Rename the misleading page:** `Telemetry Analytics` -> **`Retrieval Analytics`** (the `<h2>` and
  the nav label). Route `/telemetry` -> `/telemetry/retrieval` (with a redirect from `/telemetry`).
- **Rename nav label** `Qwen Inference` -> `Inference` (model-agnostic post-Chain-1). Route unchanged.
- **Promote the ranking to a page:** new `/telemetry/model-ranking` rendering
  `proj_inference_tool_model_performance` (reuse `ToolModelPerformancePanel`'s data path / endpoint).
  Decision to vet: keep the panel on Inference too, or move it out entirely.
- **Shared shell conventions** applied uniformly across the section: the `ControlsBar` filter/title
  primitive, forward-fill empty-states, recharts, and the three-axis naming discipline
  (`action`/`query_source`/`label_kind` — never bare "source"/"type").

### 5.4 What stays the same (parity promise)
Every page renders the identical data it renders today (net §3.A is the proof). Routes that move keep a
redirect from the old path. Deep links (Query Trajectory from Audit/Training Pairs) keep working.

---

## 6. Findings seeded for the step-3 audit
- **F1 (behavior-preserving):** `Telemetry Analytics` name is misleading — rename to `Retrieval Analytics`.
- **F2 (behavior-preserving):** `Qwen Inference` label is a post-Chain-1 misnomer — drop "Qwen".
- **F3 (behavior-changing, flagged feature):** promote per-tool-per-model ranking to its own page (new
  route + nav). New tests, not parity.
- **F4 (taste/structure):** flat 17-link nav -> sectioned nav surfacing read-vs-write framing.
- **F5 (net gap, densification):** `KnowledgePage` + `QueryTrajectoryViewPage` lack render tests; fill
  in step 7 iff the refactor touches them.
- **F6 (open, vetting):** does Knowledge/Memory join the Telemetry section or stay separate? (Option A
  keeps them separate; the program §1 lists them as read-side folded projections.)
- **F7 (open, vetting):** keep the ranking panel on Inference after promoting it to a page, or remove it?
