# Chain 3 — Data-Driven Model Routing: Retrospective (closing audit)

> **Status:** CHAIN COMPLETE (task `retrospective`). Chain 3 of the telemetry-consolidation program. Verifies the `completion_condition` item-by-item with on-disk evidence, flags the user-vettable surface, and notes the downstream unblock.

## Commits (refactoring-discipline spine)
| Task | Step | Commit |
|---|---|---|
| T1 scope-inventory-and-characterization-net | 1–2 | `373579d7` |
| T2 audit-and-findings-ledger | 3 | `97e4a2ad` |
| T3 design-target-and-triage | 4–5 | `4c2b9278` |
| T4 behavior-preserving-execution-and-parity | 6 | `acfca570` |
| T5 post-refactor-net-densification | 7 | `37bb4a9a` |
| T6 retrospective | — | (this) |

Docs: `CHAIN3_ROUTING_INVENTORY.md` (T1), `_AUDIT.md` (T2), `_TARGET.md` (T3), `_RETRO.md` (T6). Branch `worktree-data-driven-model-routing` (unmerged).

## completion_condition — item-by-item, with evidence

1. **Router reads `proj_inference_tool_model_performance` with a 60s cache + static-rules cold-start fallback.** ✓
   `internal/inference/modelrank.Ranker.Select` reads the projection for the ctx task through a 60s TTL cache (`cacheTTL`, thread-safe, injectable clock), best-effort (read error / unattributed / no-switch → static default). Wired in `main.go` via `inferRouter.SetModelSelector(modelrank.NewRanker(pool, "qwen2.5-32b").Select)`. The router itself stays db-free (the selector is an injected seam, mirroring `SetInvocationRecorder`).
   - Evidence: `modelrank/modelrank.go`; `main.go` wiring; `TestSelect_RanksFromSeededProjection`, `TestSelect_CacheHitThenExpiry`.

2. **Ranking rule (success-rate / latency / cost) implemented + tested.** ✓
   Pure `rank()`: warmup gate (`warmupMinCalls=20`); quality = outcome-level success-rate (`outcome_success_count/call_count`); a `qualityMargin=0.10` cost-asymmetry guard (never displaces the free local default unless a candidate is materially better — the dominant local-vs-remote cost lever); latency (`total_latency_ms/call_count`) as the tie-break among margin-clearers.
   - Evidence: `TestRank_DefaultWarmupBoundary`, `TestRank_QualityMarginBoundary`, `TestRank_HighestQualityAmongClearers`, `TestRank_LatencyTieBreak`, `TestRank_SubWarmupCandidateIgnored`, `TestRank_AllZeroOutcomeStaysDefault` (+ the T4 sanity set).

3. **Cold-start routing proven identical to today's static routing.** ✓
   A nil selector ⇒ `resolveModel` always returns the static default; cold-start (no rows / default below warmup / no margin-clear / read error / unattributed) ⇒ the local `qwen2.5-32b` default. The T1 oracle is unchanged and green.
   - Evidence: `TestStaticRouting_ColdStartModelSelectionIsStaticDefault` (T1, unmodified); `TestRouting_NilSelectorDispatchesLocal`; `TestRouting_SelectorNotOkUsesLocalDefault`; `TestSelect_NoRowsReturnsDefaultNotSwitched`.

4. **Parity net green** — `go test -tags sqlite_fts5 ./internal/inference/...` green; full `scripts/precommit.sh` green on every commit. The existing router dispatch/telemetry net passed **unchanged** through the local/remote extraction (behavior-preserving refactor).

## Behavior delta (flagged new behavior, not parity)
The data-present path is **new behavior**: once a task's default model is warmed (≥20 calls) AND a candidate clears the 0.10 outcome-success margin, the default `Generate` path routes that task to the better model (e.g. local→remote). This activates only with data; it is covered by its own tests (not parity). The remote path now also forwards the caller's `MaxTokens` (so a routed retrieve isn't truncated to 64) — the explicit `GenerateRemote` is unchanged (still 64).

## USER-VETTABLE surface (per the roadmap: "you vet the ranking rule + the fallback boundary")
Recommend a review of `docs/CHAIN3_ROUTING_TARGET.md §1.3` before this routes meaningful traffic:
- `qualityMargin = 0.10` (the cost-asymmetry guard) — the single knob that gates free-local → paid-remote shifts.
- `warmupMinCalls = 20` — when a model becomes trustworthy.
- Quality = **outcome-level** success-rate (vs call-level) — the chosen quality signal.
- **Deferred** (recorded out-of-scope): per-token cost weighting, latency-primary routing, a call-level fallback when outcome data is all-zero, and folding the session-routing escalation into the data-driven path. File follow-ups if any are wanted.

## Downstream unblock
This finalizes the data-driven router-cache the **Phase-D orchestrator-swap (bridge-harness)** depends on: the router now picks models from telemetry with a safe static fallback. Per the program plan, Chain 4 (telemetry-page-IA) and Chain 5 (legacy-sink-retirement) follow.

## Deploy
Branch unmerged; a linked-worktree build does not deploy. To go live: merge `worktree-data-driven-model-routing` → main, `make -C go build` in the main checkout, `/mcp reconnect`. No migration (reads the Chain-2 projection). The selector is fail-open, so a deploy cannot break inference even if the projection is empty/unavailable — it degrades to today's static routing.
