# Telemetry Consolidation — Program Plan

> **Status:** DRAFT FOR REVIEW (uncommitted). Produced while working chain `per-tool-per-model-observability` (264), after the user reframed that chain as the first stage of a full telemetry consolidation. Nothing here is built yet; this is the "measure twice" artifact. Each stage is a chain in **refactor-discipline** format (net-first, behavior-preserving, parity-gated).
>
> **Audience note:** §1–§3 are written to be vettable *without* data-analytics expertise — they describe what each surface answers in plain language, what's tangled, and what the consolidated shape is. §4 onward is the engineering detail. The load-bearing promise (§6): a **characterization net** pins every current number before anything moves, so consolidation provably loses nothing — you don't have to eyeball an analytics spec to trust it.
>
> **Companion docs (the program this extends):** `TELEMETRY_SUBSTRATE.md` (the read-side substrate design this consolidation generalizes), `TELEMETRY_FRONTEND.md` (the dashboard IA conventions), `PROJECTIONS.md` (the projection contract), `OBSERVABILITY.md` (span_id substrate), `EVENT_CATALOG.md` (write-side event types).

---

## 1. The tangle, in plain language

The toolkit-server has ~10 telemetry/observability dashboard pages backed by a dozen tables. Grouped by the question each answers:

| Question it answers | Page(s) | Where the data lives | Architecture |
|---|---|---|---|
| "How's my **model/inference** layer doing?" | Inference | `qwen_invocations` table | **direct-sink → page reads raw + computes success at read-time** |
| "How's my **search/retrieval** doing?" | Telemetry, Query Trajectory, Context Pulls, Training Pairs | `grounding_events` + `query_interactions` + `query_resolutions` → 3 folded projections | **emit → folded projection → page** (the good pattern) |
| "What happened inside one **request**?" | Spans, Audit Ledger | `span_events` (24h), `events` ledger | streaming / append-only ledger |
| "How are **benchmarks / ML training** doing?" | Benchmarks, Snapshot Corpus, Training Pairs | `benchmark_results`, training corpora | mixed |
| "**Knowledge / memory** health?" | Knowledge, Memory Substrate | `proj_memories` etc. | folded projection |

**The core finding: the search/retrieval cluster is a deliberately-designed substrate (documented, conventioned, projection-backed). The inference cluster is the one piece that never got moved onto it.** Everything that feels tangled flows from that single asymmetry:

1. **Two architectures for the same kind of thing.** Search telemetry: emit an event per search → a projection folds it into an aggregate → the page reads the aggregate. Inference telemetry: raw rows in a sink table the page reads directly, computing "success" in hand-written SQL at read-time. Same problem shape, two solutions.
2. **Two definitions of "did it work."** Search uses *did the agent click/cite/use the result* (a tiered click model). Inference uses a *bespoke SQL rule per task* ("classification accuracy > 0.5", "vault search returned hits"). Two mechanisms for one concept.
3. **`work_tool_calls` is dead** — no reader — and its migration comment *falsely* claims it's the foundation for the per-tool-per-model chain. That false comment is itself a confusion-bug; it gets deleted.
4. **Half the model calls are invisible.** Only *local Qwen via the router* is recorded. Remote Claude calls aren't recorded at all; the curation scorer bypasses the router. So "per-model" can't even be answered today — there is only ever one model in the data.
5. **The page literally named "Telemetry"** only shows *search* volume/success — misleading, since everything in this doc is telemetry.
6. **Data-format drift** — the same concept under different column names across tables (`input_tokens` vs the chain's `tokens_in`, `error_class` vs a success predicate), plus two telemetry tables already mid-retirement (`vault_search_invocations`, `kiwix_offload_invocations`).

---

## 2. The target shape (one architecture)

The system has exactly **two kinds of "expensive operation" worth watching per-variant**:

- **Model calls** — *which model* did *which task (purpose)*, how fast, how many tokens, did it succeed.
- **Retrievals** — *which source* answered *which query*, how many results, was it used.

Both deserve the **same** pattern — the one the search cluster already proves: *emit a telemetry record per operation → fold it into a "performance by (purpose × variant)" projection → one page surfaces the ranking → consumers (router, dashboard) read the projection.*

So consolidation = **move the inference cluster onto the search cluster's substrate pattern, then unify the seams (success model, page IA, data format) across both.**

### 2.1 Read-side substrate, NOT the write-side ledger (load-bearing)

`TELEMETRY_SUBSTRATE.md` §1.1 reserves the immutable `events` ledger for **agent mutations** (each carries an actor + rationale + schema validation; it's the audit trail and it's replayed to rebuild domain state). A model-call *measurement* is **telemetry, not a domain mutation** — it belongs in the **read-side substrate** (mutable analytical tables + read-side projections, driven by an emit→fold seam), exactly where search telemetry lives. "Moved to events" means *moved to the emit-and-fold substrate pattern* (away from `work_tool_calls`'s dead raw-insert), **not** "put measurements in the audit ledger." Chain-close/audit-completed milestones DO go to the ledger (matching the existing `Telemetry*` event prefix); per-call measurements do not.

### 2.2 Honoring the `qwen_invocations`-stays decision (§9.2)

`TELEMETRY_SUBSTRATE.md` §9.2 says `qwen_invocations` STAYS — but scoped to *"don't merge it into `grounding_events`"* (wrong grain: one Qwen call serves many grounding events; merging re-triggers the bug-1328 failure). This consolidation does **not** merge it into `grounding_events`. It **replaces the sink mechanism with a clean per-call telemetry table at the same per-call grain**, plus the projection layer it never had. That preserves bug-1328's actual lesson (don't conflate grains/sources) while removing the off-pattern sink. The grain is unchanged; only the mechanism (raw sink → emit+fold) and the coverage (local-only → all models) change.

### 2.3 The "both-layers" success model (your selected option)

- **Call-level success** is recorded on every telemetry row at emit time: `success = (no error AND non-empty output)`, with a small closed `error_class` enum (`'' | upstream_error | empty_response | not_configured | timeout`). Uniform across every model and task — no per-task authoring needed.
- **Outcome-level success** is layered *in the projection* where ground truth exists, by joining the same sources today's predicate registry reads: `classify_*` → latest `proj_benchmark_results.accuracy_score`; `vault-rerank-retrieve` → matching `grounding_events.results_count > 0`. This **preserves today's predicate richness but materializes it as projection data instead of read-time SQL** in `inference_v2.go` — so it's rebuildable, testable, and uniform with the search cluster's success columns.

This is the unification of tangle #2: one place, two layers, every task gets the call-level layer for free and the outcome layer where we have truth.

### 2.4 Naming / namespace decisions (proposed, vettable)

| Thing | Today | Proposed | Why |
|---|---|---|---|
| Per-call model telemetry table | `qwen_invocations` (local-Qwen-only, misnamed) | `inference_invocations` (model-agnostic) | Carries Claude + Qwen + future; same per-call grain |
| Read-side projection | (none) | `inference_tool_model_performance`, keyed `(tool, model)` | Per-(purpose × model) ranking; `tool` = the purpose/task label the router routes on |
| Projection namespace prefix | reserved: `query_*`, `injection_*`, `offload_*`, `bench_*` | add `inference_*` to `readSidePrefixes` | Clearer than reusing `offload_*` (which implied Qwen-only); inference spans local+remote |
| "tool" dimension | — | the inference **purpose** (`qwenctx.TaskID`: `classify_<rubric>`, `vault-rerank-retrieve`, …) | This is what routing keys on, NOT the MCP surface.action; surface/action carried alongside for cross-reference |

---

## 3. The chain sequence (each in refactor-discipline format)

Five chains, dependency-ordered, each independently shippable and leaving the system working. Every chain follows the seven-step refactor backbone — **scope+inventory → characterization net (the gate) → audit/findings-ledger → first-principles target → triage → behavior-preserving execution+parity → post-refactor densification** — so each has a net pinning current behavior *before* a line moves, and a parity gate proving nothing was lost. Where a chain adds *new* capability (per-model rows, router consumption), that delta is flagged explicitly as behavior-changing and gets its own new tests (not parity).

The existing chain `per-tool-per-model-observability` (264) is **reframed as Chain 1**; its current 7 tasks are re-cut onto the refactor backbone.

### Chain 1 — Inference telemetry onto the substrate *(the architectural refactor; reframes 264)*
- **What:** Replace `qwen_invocations` (sink) with `inference_invocations` (clean per-call read-side table: model-agnostic, +`success`/`error_class`, +remote-Claude coverage at the router seam). Add read-side projection `inference_tool_model_performance`. Repoint the existing `/inference/*` endpoints + Inference page to read the projection. Delete dead `work_tool_calls` + its false comment.
- **Net (gate):** golden snapshots of `/inference/health-cards`, `/inference/sparklines`, `/inference/retrieval-health` JSON over fixed DB fixtures, captured BEFORE any change.
- **Parity:** the Inference page shows identical numbers afterward.
- **Flagged feature delta (post-parity, new tests):** per-model becomes first-class (not a sub-array); remote-model rows now appear; curation coverage noted as a follow-on.
- **You vet:** the new table/projection shape, and the parity proof that the Inference page is unchanged.

### Chain 2 — Success-model unification *(both-layers)*
- **What:** Net over today's success computations (the inference predicate registry + RAG `retrieval_success`). Refactor to §2.3: call-level `success` recorded on the row; outcome-level layered in the projection (classify→benchmark, vault→grounding) — moving the predicate logic out of read-time SQL into the projection. Reconcile the two clusters' "did it work" semantics.
- **Parity:** success numbers match today's (or documented, justified deltas).
- **You vet:** the reconciled success definition and any number that intentionally changes.

### Chain 3 — Data-driven model routing
- **What:** The router reads `inference_tool_model_performance` (60s in-process cache) to pick the best model per task, **with a cold-start fallback to today's static rules**. Net pins current static routing; new behavior only activates once data exists.
- **Parity:** cold-start routing == today's routing.
- **You vet:** the ranking rule (how "best" is chosen from success-rate/latency/cost) and the fallback boundary.

### Chain 4 — Telemetry page IA unification
- **What:** One consistent telemetry information architecture: extend the documented `/audit` (write-side) vs `/telemetry` (read-side) framing, rename the misleading generic "Telemetry" page, group inference + RAG + retrieval under one nav section with shared layout/filter/format conventions (the three-axis naming discipline, forward-fill empty-states, recharts).
- **Net:** endpoint + visual parity — every page still answers what it answered, in the unified shell.
- **You vet:** the page map / nav structure (this is your window — mockups before build).

### Chain 5 — Legacy-sink retirement & data-format unification
- **What:** Retire remaining legacy sinks — coordinate with the already-planned `telemetry-substrate-cleanup` (vault_search_invocations / kiwix_offload_invocations → grounding_events), and drop `qwen_invocations` now that Chain 1 supersedes it. Unify column-naming/data-format conventions across the substrate.
- **Net:** rebuild parity — no data loss; the substrate is strictly larger/cleaner.
- **You vet:** the retirement order + soak windows.

### Folds in: existing T7 `forge-shape-liveness-reaudit`
Per-forge-shape call telemetry + `trained_model` first-use marker + liveness re-audit. Rides the same per-call substrate once Chains 1–2 land; sequenced after them (it was already marked "do NOT block the core").

---

## 4. Chain 1 in detail (the anchor)

*(Steps map 1:1 to the refactor-discipline playbook. Full schema/SQL detail lands in Chain 1's own design task; this is the shape for vetting.)*

> **Detailed design (T10):** the classified findings ledger, the exact `inference_invocations` + `inference_tool_model_performance` schemas, the latency-percentile decision, and the triage now live in **`CHAIN1_INFERENCE_TELEMETRY_DESIGN.md`**.

**1. Scope & inventory.** Boundary = the inference telemetry cluster: `qwen_invocations` (write path in `router.GenerateWithOpts` + `db/qwen_telemetry.go`; read path in `observehttp/inference_v2.go` + `inference_success_predicates.go` + `inference_retrieval.go`), the Inference dashboard page (`apps/dashboard/src/pages/Inference/` + `api/inference.ts`), and the dead `work_tool_calls` (`migration 075` + the `CallObserver` in `main.go`). Out of scope: the RAG substrate (untouched here), curation-scorer coverage (follow-on).

**2. Characterization net (the gate).** Golden JSON of every `/inference/*` endpoint over a fixed seeded DB (local-only Qwen rows today). Plus Go-level tests pinning the success-predicate outputs. Green before the first refactor commit.

**3. Audit / findings ledger.** Classify each finding behavior-preserving / behavior-changing / taste. (e.g. "remote calls uncaptured" = behavior-changing→feature; "work_tool_calls dead" = behavior-preserving delete; "success computed at read-time" = taste/structure → moved to projection in Chain 2, not here).

**4. First-principles target.** `inference_invocations` table + `inference_tool_model_performance` projection (§2.4); endpoints read the projection; per-model first-class.

**5. Triage.** Chain 1 does the *relocation* (behavior-preserving) + the *cheap features that don't change existing numbers* (remote coverage adds new rows; per-model view adds a column). Success-model rework routes to Chain 2; routing to Chain 3; page-IA to Chain 4.

**6. Behavior-preserving execution + parity.** Small commits: (a) add `inference_invocations` + emit at router (local+remote) writing BOTH old + new during transition; (b) add projection + fold; (c) repoint endpoints to projection, prove golden parity; (d) delete `work_tool_calls` + false comment; (e) drop the old write path. Parity net green at every step.

**7. Post-refactor densification.** New branches (remote path, empty-token nulls, cold projection) pinned; the newly-clean projection's own input-classes characterized.

---

## 5. Decisions for you / open questions

Most are mine to make and I've proposed defaults above; these are the ones worth your explicit call:

1. **Sequence & granularity** — is the 5-chain cut (§3) the right grain, or do you want it coarser/finer? Any chain you want resequenced?
2. **Table name** `inference_invocations` (vs `model_invocations`) and prefix `inference_*` (vs reusing reserved `offload_*`). (§2.4)
3. **Curation coverage** — the curation scorer bypasses the router and is CLI-only/offline. Default: leave it out of Chain 1, file as a follow-on. Confirm, or pull it in.
4. **Page IA (Chain 4)** — you'll get mockups to vet before any frontend build, since the dashboard is your window. Flagging now so you know it's a checkpoint, not a fait accompli.

---

## 6. Why this is safe to vet without being a data analyst

The refactor discipline's gate is: **pin current behavior in a test net before changing anything.** Concretely — before `qwen_invocations` is touched or any page is repointed, the *exact JSON each telemetry endpoint produces today* is captured as golden snapshots over fixed sample data. The rule for every consolidation step is then mechanical: the new version must reproduce those goldens, or we consciously record that a specific number changed and why. **That is what guarantees consolidation doesn't silently drop a metric you rely on** — the proof is the green net, not an analytics review you'd have to perform. Each chain ships with its net; each chain's parity is a checkpoint you can trust at a glance.
