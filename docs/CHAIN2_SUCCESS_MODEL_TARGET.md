# Chain 2 — Success-Model Unification: Target Design & Triage (steps 4–5)

> **Status:** STEPS 4–5 COMPLETE (task `design-both-layers-target-and-triage`, position 3). Refactoring-discipline **step 4 (first-principles synthesis)** + **step 5 (triage gate)**. Inputs: `CHAIN2_SUCCESS_MODEL_AUDIT.md` (step 3 ledger), `CHAIN2_SUCCESS_MODEL_INVENTORY.md` (the net), `TELEMETRY_CONSOLIDATION.md` §2.3, the chain `completion_condition`.
>
> This is the picture the execution task (T4) builds to. Every change below is classified **behavior-preserving** or **documented-delta**, and every consumer from the audit is mapped to its post-refactor source.

---

## 1. The both-layers target (first-principles)

The system watches one expensive operation — a model call — and asks two *different* questions, which become two *layers* on the same per-call grain:

- **Layer 1 — call-level liveness:** *did the call return cleanly?* `inference_invocations.success` (`no error AND non-empty output`), set at emit. **Already built (Chain 1).** No change.
- **Layer 2 — outcome quality:** *given ground truth, was the call's output good?* Today computed at **read time** by interpolating predicate SQL (`pred.Expr`) into the health-cards/sparklines aggregate. The refactor **materializes** it as projection data, uniform with the search cluster's `proj_retrieval_success_per_query.success`.

### 1.1 Where each piece lands

```
inference_invocations (source, per call)
    │  .success  ── Layer 1 (emit-time)
    │
    ├─▶ proj_inference_call_success            ⟵ NEW per-call projection (Layer 2 materialized, per row)
    │      (id, task_id, model_name, created_at,
    │       call_success, outcome_success, outcome_basis)
    │        │
    │        ├─▶ /inference/health-cards   success_rate = SUM(outcome_success)/COUNT(*) over 7d window  [PARITY]
    │        └─▶ /inference/sparklines     per-day SUM(outcome_success)/COUNT(*)                          [PARITY]
    │
    └─▶ proj_inference_tool_model_performance  ⟵ +outcome_success_count column (Layer 2 rollup, per task×model)
           .success_count        ── Layer 1 rollup → /inference/tool-model-performance success_rate (call-level) [PARITY]
           .outcome_success_count── Layer 2 rollup → outcome rate (NEW additive field; Chain-3 router consumes)  [FEATURE-DELTA]
```

**Why a per-call projection AND a rollup column (not just the rollup the completion_condition names):** `/inference/sparklines` is inherently **per-day** and `/inference/health-cards` is **7-day-windowed + warmup-gated**. An all-time per-(task,model) rollup *cannot* produce per-day buckets or a rolling window — so reading the rollup directly would be a hard behavior loss (Finding-A1). The per-call projection (one row per `inference_invocations.id`, uniform with `proj_retrieval_success_per_query`) preserves the windowed/bucketed reads while removing the read-time SQL. The rollup column on `proj_inference_tool_model_performance` satisfies the completion_condition's literal "materialized into …" and feeds the Chain-3 router. Both are justified; neither is redundant.

### 1.2 How Layer 2 is computed (the materialized predicates)

The dispatch + the three predicates port **verbatim** from `inference_success_predicates.go` into a single shared SQL fragment in the `projections` package, evaluated per `inference_invocations` row aliased `qi`:

```
outcome_success = CASE
    WHEN qi.task_id = 'vault-rerank-retrieve'           THEN <vault proximity-window EXISTS>
    WHEN substr(qi.task_id,1,9) = 'classify_'           THEN <classify any-benchmark EXISTS>   -- substr, NOT LIKE (see §3.2)
    ELSE                                                     <default: output_tokens NOT NULL AND latency_ms>0>
END
outcome_basis  = CASE … 'vault-rerank-retrieve' / 'classify' / 'default' END   (the predicate Description)
```

- **default** ports exactly (`output_tokens IS NOT NULL AND latency_ms > 0`) — NOT replaced by `call_success`; they differ (default uses token-count + latency, call_success uses error_class + non-empty output), and the net pins the difference (`DefaultPredicateArms/output_null_fails_even_with_latency`).
- **classify** ports as **any-row** `EXISTS(accuracy_score > 0.5)` and the inert `ORDER BY … LIMIT 1` is **dropped** (Finding-948/D1) — behavior-preserving, net-verified.
- **vault-rerank** ports its latency-scaled proximity window verbatim.

### 1.3 The single shared fragment + the dispatch-in-two-places note

The outcome SQL fragment is defined **once** in `projections` and embedded in BOTH the per-call projection fold and the rollup fold (DRY, Q6). The Go `lookupSuccessPredicate` dispatch STAYS in `observehttp` — it still produces the `success_rate_basis` *label* + the "using default predicate" log. So the task_id→predicate dispatch exists in two forms (SQL value-side in `projections`, Go label-side in `observehttp`). They are kept consistent by a **step-7 densification test** that asserts, for representative task_ids, the projection's stored `outcome_basis` equals `lookupSuccessPredicate(taskID).Description`. This is an accepted, recorded tradeoff: SQL and Go cannot share the dispatch, and keeping the label-dispatch in `observehttp` is what lets the step-2 net (`TestLookupSuccessPredicate_Registry` + every `*Predicate.Description` reference) stay **literally unmodified**.

### 1.4 RAG ↔ inference reconciliation = one *vocabulary*, not one number (Finding-R1)

The two clusters answer different questions and are reconciled by a single **layered vocabulary**, applied as naming + documentation (behavior-preserving — no code change to B):

| Layer | Question | Inference column | RAG column |
|---|---|---|---|
| call-level | returned cleanly? | `inference_invocations.success` | (n/a — retrieval has no "call error") |
| outcome: produced | returned a usable result? | `proj_inference_call_success.outcome_success` for `vault-rerank-retrieve` (`results_count>0`) | `grounding_events.results_count` |
| outcome: used | result was actually used? | (n/a for inference) | `proj_retrieval_success_per_query.success` (`weight≥0.8 OR resolved-from`) |

The inference `vault-rerank-retrieve` outcome and the RAG `retrieval_success` are **adjacent layers of the same retrieval**, now explicitly named — NOT collapsed (collapsing destroys the produced-vs-used distinction; rejected per Q6 wrong-abstraction). Cross-referenced in column comments + this doc.

---

## 2. Consumer map — every audit-ledger consumer → post-refactor source

| Consumer | Before | After | Class |
|---|---|---|---|
| `/inference/health-cards` `success_rate` | `SUM(pred.Expr)/COUNT` over `inference_invocations`, 7d | `SUM(outcome_success)/COUNT` over `proj_inference_call_success`, 7d | **behavior-preserving** (same per-row outcome, same window) |
| `/inference/health-cards` `success_rate_basis` | `lookupSuccessPredicate().Description` | unchanged source; classify string corrected | **documented-delta** (classify basis wording, 948) |
| `/inference/sparklines` `success_rate` | `pred.Expr` per row, per-day | `outcome_success` per row from projection, per-day | **behavior-preserving** |
| `/inference/tool-model-performance` `success_rate` | `success_count/call_count` (call-level) | unchanged | **behavior-preserving** |
| `/inference/tool-model-performance` outcome rate | — | NEW `outcome_success_count/call_count` field | **feature-delta** (additive) |
| `/telemetry/analytics/success-rate` (RAG, B) | `SUM(p.success)` | unchanged | **behavior-preserving** (no code change) |
| FE `HealthCard`/`SparklineBucket` types | as-is | unchanged shape | **behavior-preserving** |
| FE `ToolModelStat` | as-is | +optional `outcome_success_rate?` field | **feature-delta** (additive, optional) |
| FE e2e mock basis string | `'classify: latest …'` | corrected to match 948 | **documented-delta** (mock consistency; not a gate) |
| Chain-1 health-cards golden | classify basis = "latest …" | classify basis = "any …"; all numbers identical | **documented-delta** (948; `UPDATE_GOLDEN`, reviewed) |

---

## 3. Triage gate — in/out + sequence

### 3.1 IN scope (this chain's execution, T4)
1. **NEW migration** (auto-numbered): `CREATE TABLE proj_inference_call_success` + `ALTER TABLE proj_inference_tool_model_performance ADD COLUMN outcome_success_count INTEGER NOT NULL DEFAULT 0`. *(behavior-preserving for existing columns; additive)*
2. **NEW projection** `projections/inference_call_success.go` — per-call Layer-2 materialization; register; add to `readSidePrefixes` coverage (already `inference_`). *(new structure)*
3. **Shared outcome SQL fragment** in `projections` (the dispatch CASE), used by the new projection AND the `proj_inference_tool_model_performance` rollup. *(behavior-preserving relocation of the predicate SQL)*
4. **`projections/inference.go`**: add `outcome_success_count` to the rollup INSERT/SELECT (alias `inference_invocations AS qi`, embed the fragment). *(additive)*
5. **`observehttp/inference_v2.go`**: replace `pred.Expr` interpolation in `buildHealthCard` + `inferenceSparklineBuckets` with reads of `proj_inference_call_success.outcome_success`; keep window/warmup/per-day/basis logic. *(behavior-preserving)*
6. **`observehttp/inference_success_predicates.go`**: drop the now-dead `Expr` field from `SuccessPredicate` (keep `Description` + `lookupSuccessPredicate`); correct the classify `Description` ("latest"→"any benchmark row") + field comment (948); move the SQL + its rationale comments to the projection. *(behavior-preserving except the 948 basis-string documented-delta)*
7. **`observehttp/inference_tool_model_performance.go`** + FE `ToolModelStat`: expose `outcome_success_rate` (additive). *(feature-delta, own test)*
8. **Test infra**: add `"inference_call_success"` to `refreshReadSideProjections`' rebuild list (keeps the net's assertions untouched); fix the stale comment at `inference_success_predicates_test.go:27` (949).
9. **Golden update** (`UPDATE_GOLDEN=1`): only the classify `success_rate_basis` string changes; verify all numeric `success_rate` values are byte-identical first.

### 3.2 Implementation guards (carried into T4)
- **`substr(task_id,1,9) = 'classify_'`, NOT `LIKE 'classify_%'`** — `_` is a `LIKE` wildcard; `substr` mirrors the Go `taskID[:9]` dispatch exactly (the 8-vs-9-char + dash-not-underscore net cases must still pass).
- **Alias `inference_invocations AS qi`** in the rollup fold so the ported vault/classify correlated subqueries resolve.
- **Staleness note (minor documented-delta):** materialized outcome reflects ground truth as of the last fold rather than read-instant — identical to every other read-side projection; the net (fold-before-read) does not observe it.

### 3.3 OUT of scope — recorded rejections (do NOT broaden — anti-pattern: while-we're-here)
- **True-latest classify** (vs any-row): new behavior, no product signal. **Rejected**; file follow-up bug only if ever wanted. (948 closes via reproduce+correct-the-claim.)
- **Unifying the third RAG success definition** (`proj_query_volume_by_source.success_count`, Finding-R2): real behavior change to `/telemetry/analytics/volume-by-source`, outside Ch2's named A+B pair. **Deferred** — file a follow-up; name it in the vocabulary (§1.4) so the divergence is documented.
- **Changing computation B's definition**: parity-preserving reconciliation is by vocabulary only. **No B code change.**
- **Percentiles into the projection**: explicitly rejected by Chain-1 design (not foldable); unchanged.

---

## 4. Parity statement

After T4, the step-2 characterization net + Chain-1 goldens pass with: **zero assertion edits**; **one test-infra addition** (`refreshReadSideProjections` list); **one golden string update** (classify basis, 948, documented-delta — all numbers identical); **two comment fixes** (948 field comment, 949 test comment). Every other observable number is byte-identical. New behavior (the `outcome_success_rate` field, the materialized join, the rollup) gets its own tests in step 7 (T5).
