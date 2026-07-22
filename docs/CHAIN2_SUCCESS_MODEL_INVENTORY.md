# Chain 2 — Success-Model Unification: Scope, Inventory & Characterization Net

> **Status:** STEPS 1–2 COMPLETE (this task: `scope-inventory-and-characterization-net`). Produced by chain `telemetry-success-model-unification` (306) — Chain 2 of the telemetry-consolidation program (`TELEMETRY_CONSOLIDATION.md` §3, §2.3). This document is the refactor-discipline **step 1 (scope & inventory)** + the manifest for **step 2 (characterization net)**. It deliberately does **NOT** contain the audit/findings-ledger (step 3), the first-principles target (step 4), or the triage (step 5) — those are the next task(s). The net pinned here is the parity oracle the later unification must reproduce or consciously deviate from with a documented delta.
>
> **Companions:** `CHAIN1_INFERENCE_TELEMETRY_DESIGN.md` (Chain 1 routed the success-model rework here — see its findings F3/F4), `TELEMETRY_CONSOLIDATION.md` §2.3 ("the both-layers success model"), `TELEMETRY_SUBSTRATE.md` (the read-side substrate + click model).

---

## 1. Scope & boundary

**In scope (the two success computations this chain unifies):**

- **A — the inference success-predicate registry.** Read-time SQL fragments in `go/internal/observehttp/inference_success_predicates.go`, interpolated into the per-task aggregate queries in `inference_v2.go` (the `/inference/health-cards` and `/inference/sparklines` handlers). Computes a per-task `success_rate` = `SUM(predicate)/COUNT(*)`.
- **B — the RAG retrieval success model.** The `success` column of `proj_retrieval_success_per_query`, computed in `go/internal/projections/query_success.go` as a projection-time `CASE` over the tiered click model.

**Adjacent / context (NOT the subject of this net, but inventoried so the unification doesn't lose track):**

- **C — call-level success.** `inference_invocations.success` (added in Chain 1, migration 077), summed into `proj_inference_tool_model_performance.success_count` (`projections/inference.go`) and surfaced as a call-level `success_rate` by `/inference/tool-model-performance` (`inference_tool_model_performance.go`). Per Chain 1's design it is **recorded but not yet consumed** by the predicate-registry `success_rate`. The "both-layers" target (§2.3 of the program) layers A's outcome logic *on top of* C. C's own computation is already characterized by `inference_tool_model_performance_test.go` and is not re-pinned here.

**Out of scope:** the `/inference/retrieval-health` aggregate (reads `grounding_events` + `query_interactions` directly; its numbers are independent and already pinned by `inference_retrieval_test.go` + the retrieval-health golden), the dashboard/page IA (Chain 4), and the call-level emit path (Chain 1, landed).

---

## 2. Inventory of computation A — the inference predicate registry

### 2.1 Dispatch (`lookupSuccessPredicate`)

`task_id` → predicate, resolved as: **exact registry match** → **`classify_` 9-char prefix** → **default** (returning `hadCustom=false`, which makes the handler log a "using default predicate" line). The registry currently holds one exact entry (`vault-rerank-retrieve`) and one prefix family (`classify_*`).

### 2.2 The three predicates (SQL fragments, evaluated per `inference_invocations` row aliased `qi`)

| Predicate | Fires for | Expression (semantics) | Ground-truth source |
|---|---|---|---|
| **default** | any unregistered task_id | `output_tokens IS NOT NULL AND latency_ms > 0` → 1 | the row itself (liveness floor) |
| **classify** | `task_id` prefixed `classify_` | `EXISTS(proj_benchmark_results row for task with accuracy_score > 0.5)` → 1 | `proj_benchmark_results` |
| **vault-rerank-retrieve** | `task_id == "vault-rerank-retrieve"` | `EXISTS(grounding_events row, action='vault_search', results_count > 0, within latency-scaled proximity window)` → 1 | `grounding_events` |

- **Proximity window** (vault-rerank): `ABS(ge.created_at − qi.created_at) ≤ latency_ms/1000 + 2` seconds. The latency scaling exists because `vault_search` runs two Qwen passes (two `inference_invocations` rows) but writes one grounding row at pass-2 exit, so the pass-1 row lands ~`latency` seconds before the grounding it should match.
- **Consumption / warmup:** `success_rate` is `NULL` (warming up) when a task has `< 20` rows in the window (`warmupMinCallsForSuccessRate`). The per-day sparkline `success_rate` is `NULL` for any day with `< 5` calls.

### 2.3 Output contract

`HealthCard.success_rate` (float, nullable) + `HealthCard.success_rate_basis` (the predicate's description string) per task; `SparklineBucket.success_rate` per (task, day).

---

## 3. Inventory of computation B — `proj_retrieval_success_per_query.success`

One row per `grounding_events.id` (LEFT JOIN `query_interactions`, GROUP BY `ge.id`). The `success` column:

```
success = max_click_weight >= 0.8  OR  had_resolved_from = 1
```

- `max_click_weight` = `MAX(qi.click_weight)` over the event's interactions (0.0 when none).
- Tier default weights (`telemetry.DefaultClickWeights`): `followed`=1.0, `cited`=0.8, `mentioned`=0.4, `resolved-from`=1.0. `click_weight` may be set explicitly per interaction.
- The OR's second arm is **load-bearing and intentionally inclusive**: a `resolved-from` interaction marks the event successful even when its weight is below 0.8 (the documented "resolved-from is the canonical positive signal for a closed entity" rule). With the default weight (1.0) this arm is masked by the weight arm; it only changes the result for a `resolved-from` row weighted `< 0.8`.

---

## 4. Characterization net manifest (step-2 gate)

The net combines pre-existing pins (built for Chain 1's relocation) with the Chain-2 densification added by this task. **Baseline + densified net is GREEN.**

### 4.1 Computation A

| Input class | Pinned by |
|---|---|
| Dispatch: exact / `classify_` prefix / 9-char boundary / dash-not-underscore / empty / unregistered + `hadCustom` | `TestLookupSuccessPredicate_Registry` (pre-existing) |
| default — acceptance (tokens set, latency>0) | golden `aaa-default-mixed` even rows; **`TestCharacterization_DefaultPredicateArms/both_satisfied_succeeds`** (latency=1 boundary) |
| default — **output_tokens-NULL arm in isolation** (latency>0, output NULL → 0) | **`…DefaultPredicateArms/output_null_fails_even_with_latency`** (NEW) |
| default — **latency=0 arm in isolation** (output set, latency=0 → 0) | **`…DefaultPredicateArms/zero_latency_fails_even_with_output`** (NEW) |
| classify — acceptance (>0.5) | golden `classify_delta` (0.9 → 1.0) |
| classify — rejection (≤0.5 floor; no benchmark row) | `TestCharacterization_ClassifyPredicateRejections` (pre-existing) |
| classify — **multi-row selection** (newer fail + older pass) | **`TestCharacterization_ClassifyAnyBenchmarkRowNotLatest`** (NEW) — see Finding §5.1 |
| vault-rerank — acceptance (proximate vault_search, results>0) | `TestVaultRerankPredicate_HitsOnProximateVaultSearchGrounding`; golden |
| vault-rerank — wrong action; no grounding | `TestVaultRerankPredicate_IgnoresNonVaultSearchGrounding`; `TestCharacterization_VaultRerankNoGroundingMiss` |
| vault-rerank — **results_count=0 boundary** | **`TestCharacterization_VaultRerankZeroResultsMiss`** (NEW) |
| vault-rerank — **proximity-window boundary** (2s inside / 5s outside the latency-scaled 3s window) | **`TestCharacterization_VaultRerankProximityWindowBoundary`** (NEW) — see Finding §5.2 |
| warmup boundary (19 warms / 20 computes); bug_count join; project-scope quirk | `TestCharacterization_SuccessRateWarmupBoundary`, `…BugCountJoin`, `…BugCountProjectScope` (pre-existing) |

### 4.2 Computation B

| Input class | Pinned by |
|---|---|
| acceptance via weight (followed 1.0 → success 1); empty event → success 0; proactive flag; per-tier flags; kinds_fired | `TestRetrievalSuccessPerQuery_FoldEmitsRowPerEvent`, `…TelemetryEmitTriggersFold` (pre-existing) |
| **`>= 0.8` boundary exactly** (cited-only 0.8 → success 1, weight arm) | **`TestCharacterization_RetrievalSuccess_WeightBoundaryAtPoint8`** (NEW) |
| **rejection band** 0<w<0.8, no resolved-from (mentioned 0.4 → success 0) | **`TestCharacterization_RetrievalSuccess_BelowBoundaryNoResolvedFromFails`** (NEW) |
| **resolved-from rescue arm** (explicit weight 0.5 + resolved-from → success 1) | **`TestCharacterization_RetrievalSuccess_ResolvedFromRescuesLowWeight`** (NEW) |

### 4.3 Test-infra delta

`testutil.WithGroundingCreatedAt` added (`telemetry_seeders.go`) so the proximity-window boundary can place a grounding row a known delta from the inference rows. Additive: when unset, the schema's `datetime('now')` default still applies, so every existing caller is unchanged.

---

## 5. Findings surfaced by the net (route to step-3 audit — the NEXT task)

These are divergences the characterization process exposed. They are **recorded, not resolved** here; the unification (step 3+) decides reproduce-for-parity vs. fix-as-documented-delta. Both are filed as bugs.

### 5.1 The classify predicate is "ANY benchmark row > 0.5", not "latest"

The `Expr` is `EXISTS(SELECT 1 … WHERE accuracy_score > 0.5 ORDER BY run_at DESC LIMIT 1)`. The `ORDER BY … LIMIT 1` **inside `EXISTS` is inert** — `EXISTS` only tests for ≥1 matching row, so the actual semantics are "any benchmark row for the task scores > 0.5." The struct's `Description` ("latest benchmark_results.accuracy_score for the task > 0.5") and the field comment both claim *latest*. `TestCharacterization_ClassifyAnyBenchmarkRowNotLatest` pins the actual behavior (newer 0.3 + older 0.9 → success 1.0). **Decision for step 3:** when materializing classify into the projection join, either reproduce any-row (parity) or implement true-latest as a documented delta — but the description must stop claiming a behavior the SQL never had.

### 5.2 The vault-rerank proximity-window math was previously unpinned

The comment at `inference_success_predicates_test.go:27` states the predicate's time math "is covered by `inference_retrieval_test`." It is not — that file tests `/inference/retrieval-health` (a `grounding_events` aggregate), which shares no code with this predicate's latency-scaled window. `TestCharacterization_VaultRerankProximityWindowBoundary` now pins both sides of the 3s window. The stale comment should be corrected.

---

## 6. Acceptance for this task (steps 1–2)

- [x] Boundary + inventory written (this doc): computations A, B inventoried; C + out-of-scope recorded.
- [x] Characterization net exhaustive across A's and B's input classes (acceptance × rejection × boundary), with each previously-conflated arm isolated.
- [x] Net is GREEN (`./internal/observehttp/` + `./internal/projections/`), with the pre-existing goldens **unmodified** (densification is strictly additive).
- [x] Findings recorded (§5) and filed as bugs for the step-3 audit; not resolved here.
