# Chain 2 — Success-Model Unification: Retrospective (step 7 / closing audit)

> **Status:** CHAIN COMPLETE (task `retrospective`, position 6). Chain 2 of the telemetry-consolidation program (`TELEMETRY_CONSOLIDATION.md` §3). Verifies the chain `completion_condition` end-to-end with on-disk evidence, records the documented deltas, and resolves the two T1 findings-bugs.

## Commits (refactoring-discipline spine)
| Task | Step | Commit | Branch |
|---|---|---|---|
| T1 scope-inventory-and-characterization-net | 1–2 | `0dc72b25` | merged to main |
| T2 audit-and-findings-ledger | 3 | `1aab54df` | worktree-telemetry-success-model-unification |
| T3 design-both-layers-target-and-triage | 4–5 | `e2d778d0` | ″ |
| T4 behavior-preserving-execution-and-parity | 6 | `fac6f73f` | ″ |
| T5 post-refactor-net-densification | 7 | `933bee1e` | ″ |
| T6 retrospective | — | (this) | ″ |

Docs: `CHAIN2_SUCCESS_MODEL_INVENTORY.md` (T1), `_AUDIT.md` (T2), `_TARGET.md` (T3), `_RETRO.md` (T6).

## completion_condition — item-by-item, with evidence

1. **Call-level success on every `inference_invocations` row.** ✓ Unchanged from Chain 1: migration `077_inference_invocations.sql` — `success INTEGER NOT NULL DEFAULT 1 CHECK (success IN (0,1))`, set at the router emit seam. The both-layers model now carries it onto each per-call projection row too (`proj_inference_call_success.call_success`).

2. **Outcome-level success materialized into `proj_inference_tool_model_performance` via benchmark/grounding joins, replacing the read-time predicate SQL in `inference_v2.go`.** ✓
   - Materialized: migration `082` adds `proj_inference_tool_model_performance.outcome_success_count` (the rollup) + the new per-call `proj_inference_call_success` projection. The classify→`proj_benchmark_results` and vault→`grounding_events` joins are the shared `inferenceOutcomeSuccessExpr` (`projections/inference_call_success.go`), reused by both the per-call projection and the rollup so they cannot drift.
   - Read-time SQL removed: `inference_v2.go` no longer interpolates `pred.Expr` (grep: 0 occurrences). `buildHealthCard` sums `proj_inference_call_success.outcome_success` over the window; `inferenceSparklineBuckets` joins the materialized column. `lookupSuccessPredicate` remains only for the `success_rate_basis` label + the default-fallback log.
   - Tests: `projections` `TestInferenceCallSuccess_*` (per-row outcome by class, any-row-not-latest, vault proximity window, dispatch agreement, rollup count, rebuild byte-identity); `observehttp` `TestInferenceToolModelPerformance_OutcomeSuccessRate`.

3. **RAG `retrieval_success` + inference success reconciled to one model.** ✓ Reconciled as a single **layered vocabulary** (`_TARGET.md` §1.4): call-level liveness → outcome "produced a result" → outcome "result was used". The inference `vault-rerank-retrieve` outcome ("returned results", `results_count>0`) and `proj_retrieval_success_per_query.success` ("result was used", weight≥0.8 OR resolved-from) are named as adjacent layers, NOT collapsed into one number (collapsing would destroy the produced-vs-used distinction — Q6 wrong-abstraction). No behavior change to computation B.

4. **Parity net green, or documented deltas.** ✓ `go test -tags sqlite_fts5 ./internal/observehttp/ ./internal/projections/` → both `ok`; full `scripts/precommit.sh` green on every commit. The step-2 net is unmodified except the dispositioned deltas below.

## Documented deltas (the net's pinned baseline changed only here)
1. **classify `success_rate_basis` wording** "latest …" → "any …" (bug 948). The golden `health_cards.json` basis string is the only changed byte; every numeric `success_rate` is byte-identical (verified by `git diff`). Intended: the SQL never honored "latest" (the `ORDER BY … LIMIT 1` inside `EXISTS` was inert; now dropped).
2. **Outcome freshness** is now as-of-last-fold rather than read-instant — the standard read-side-projection tradeoff (uniform with every other projection), reconciled by a full rebuild or the next inference event for the task. The net (fold-before-read) does not observe it; flagged for honesty.
3. **New additive field** `outcome_success_rate` on `/inference/tool-model-performance` (the Layer-2 rollup rate) + the FE `ToolModelStat` type. Feature-delta, covered by its own test; backward-compatible (additive).

## Findings-bugs disposition
- **948** (classify "latest" vs any-row): RESOLVED as reproduce-any-row + correct-the-claim. Fixed in `fac6f73f` — Description/field-comment now say "any", the inert `ORDER BY/LIMIT` dropped from the materialized arm; the net pins any-row at both the endpoint (`TestCharacterization_ClassifyAnyBenchmarkRowNotLatest`) and projection (`TestInferenceCallSuccess_ClassifyAnyRowNotLatest`) levels. True-latest rejected (no product signal).
- **949** (stale vault-rerank test comment): RESOLVED. Fixed in `fac6f73f` — the comment now points at `TestCharacterization_VaultRerankProximityWindowBoundary`.

## Recorded out-of-scope rejections (do not re-audit)
- **True-latest classify semantics** — new behavior; file a follow-up only if a product need appears.
- **Third RAG success definition** `proj_query_volume_by_source.success_count` (followed/resolved-from) diverges from B on the `cited` class — a real behavior change to `/telemetry/analytics/volume-by-source`, outside Ch2's named A+B pair. Named in the §1.4 vocabulary; **filed as a follow-up bug**, not folded into this chain.

## Downstream unblock
**Chain 3 — data-driven-model-routing** consumes `proj_inference_tool_model_performance`, which this chain finalized: the router can now read BOTH `success_count` (call-level) and `outcome_success_count` (outcome-level) per (task, model) to rank models.

## Deploy
The chain branch is NOT yet merged. Per the worktree workflow + T1's deploy note: a linked-worktree build does not deploy. To go live — merge `worktree-telemetry-success-model-unification` to `main`, `make -C go build` in the main checkout, `/mcp reconnect`. The migration `082` adds tables/columns (additive); the post-merge advisor rebuilds + restarts `:3000`.
