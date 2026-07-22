# Chain 3 — Data-Driven Model Routing: Target Design & Triage (steps 4–5)

> **Status:** STEPS 4–5 COMPLETE (task `design-target-and-triage`). First-principles target + triage gate, from the T2 audit. This is the picture T4 builds. Every change classified behavior-preserving (cold-start path) vs behavior-changing (the data-present ranking). **The ranking rule below is the user-vettable surface** (roadmap: "you vet the ranking rule + the fallback boundary") — T6 flags it for review.

---

## 1. The target shape

```
Router.GenerateWithOpts(ctx, …)                       proj_inference_tool_model_performance
   │  model := r.resolveModel(ctx)  ───────────────▶  (read by the injected selector, 60s cache)
   │     selector nil / !ok / cold → r.modelName (qwen2.5-32b, the static default)
   │
   ├─ model == remoteModelName && r.remote != nil ──▶  r.generateRemote(ctx, …)   (extracted)
   └─ else ─────────────────────────────────────────▶  r.generateLocal(ctx, …)    (extracted; today's body)
```

### 1.1 The selector seam (router stays db-free)
```go
// ModelSelectorFunc returns the model to use for the task in ctx. ok=false
// (cold-start / insufficient data / read error) → the router uses its static
// default. Injected post-construction, mirroring SetInvocationRecorder.
type ModelSelectorFunc func(ctx context.Context) (modelName string, ok bool)
func (r *Router) SetModelSelector(fn ModelSelectorFunc)
```
- `Router` gains a `selectModel ModelSelectorFunc` field (nil default).
- `resolveModel(ctx)`: if `selectModel != nil`, call it; if `ok && modelName != ""` → that model; else → `r.modelName` (the static local default). **Nil selector ⇒ always `r.modelName` ⇒ identical to today** (the cold-start oracle stays green untouched).
- `GenerateWithOpts` branches on `resolveModel`: the remote model name (only when `r.remote != nil`) → `generateRemote`; anything else → `generateLocal`.
- **Refactor (behavior-preserving, pinned by the existing net):** extract today's local-dispatch body into `generateLocal` and the remote body into `generateRemote`; `GenerateRemote` (public, explicit) and the Generate remote-branch both call `generateRemote` (DRY, Finding-S1/Q6). No assertion changes — the existing dispatch/telemetry tests pass unchanged.

### 1.2 The selector implementation — new package `internal/inference/modelrank`
DB-aware, so it lives outside the router (Finding-A1). Two parts:
- **`Ranker`** (stateful): holds `*db.Pool`, a 60s TTL cache, and a mutex. `Select(ctx) (string, bool)` reads the task_id from ctx (via the same qwenctx accessor the router's `stampTaskID` uses), checks the cache, else queries `proj_inference_tool_model_performance` for that task's per-model rows, calls `rank`, caches the result (keyed by task_id), and returns. **Best-effort:** any query error → return `("", false)` (→ static default), never cached, never surfaced (mirrors the fail-open recorder at `main.go:330`). Thread-safe (the daemon hosts concurrent sessions — the `DriftFireTracker` mutex precedent).
- **`rank` (PURE function, the vettable core, hermetically testable):**
  ```
  rank(rows []ModelStat, defaultModel string) (chosen string, switched bool)
  ```
  `ModelStat{ModelName, CallCount, OutcomeSuccessCount, SuccessCount, TotalLatencyMS}`.

### 1.3 The ranking rule (success-rate / latency / cost) — concrete + conservative
Constants: `warmupMinCalls = 20` (health-cards precedent), `qualityMargin = 0.10`.
```
warmed   = { m in rows : m.CallCount >= warmupMinCalls }
dStat    = warmed[defaultModel]
if dStat is absent:            return defaultModel, false   # default not warmed → COLD-START
quality(m) = m.OutcomeSuccessCount / m.CallCount            # Layer-2; call_count>0 guaranteed by warmup
dQ       = quality(dStat)
# candidates that MATERIALLY beat the default (the cost-asymmetry guard):
cand     = { m in warmed, m != defaultModel : quality(m) >= dQ + qualityMargin }
if cand is empty:              return defaultModel, false   # nobody clears the margin → stay local/free
# among margin-clearers, pick highest quality; tie-break on LOWER avg latency:
best = argmax over cand of (quality, then -avgLatency(m));  return best.ModelName, true
```
- **success-rate** = the primary signal (`quality`, outcome-level). When `outcome_success_count` is degenerate (a task with no ground-truth join — all-zero outcome), `quality` is 0 for every model, so the margin never clears and the default holds (safe). *(Call-level `success_count/call_count` is available as a documented future fallback for the all-zero-outcome case; T4 keeps it simple — all-zero → stay default.)*
- **latency** = tie-break among margin-clearing candidates (lower `total_latency_ms/call_count` wins).
- **cost** = the `qualityMargin` itself: the local default (`qwen2.5-32b`, ~free) is only abandoned when another model (e.g. paid remote `claude-sonnet-4-6`) is **materially** better (≥0.10 absolute outcome-success-rate), never on a thin edge. This is the load-bearing guard (Finding-R1). Finer per-token cost weighting is **deferred** (documented), since the dominant cost lever is local-vs-remote and the margin captures it.

### 1.4 Cold-start boundary (the parity guarantee, §3.6 of the audit)
The default path selects `qwen2.5-32b` whenever ANY of: selector nil; query error; the task has no rows; the default model's row is below warmup (`<20` calls); or no candidate clears the quality margin. Only when data exists AND the default is warmed AND a candidate materially wins does traffic move. So with an empty/young projection, routing is **byte-identical to today**.

---

## 2. Consumer map — post-refactor source
| Consumer | Before | After | Class |
|---|---|---|---|
| `GenerateWithOpts` / `Generate` (all local callers) | always local qwen2.5-32b | `resolveModel`-gated: local unless selector materially prefers another model | **behavior-preserving at cold-start**; data-present = new |
| `GenerateRemote` (explicit; session-routing escalation) | remote claude | unchanged (now shares `generateRemote` helper) | **behavior-preserving** |
| `ModelName()` | qwen2.5-32b | unchanged (the static default identity) | **behavior-preserving** |
| `main.go` router wiring | `NewWithClients` + `SetInvocationRecorder` | + `SetModelSelector(ranker.Select)` | additive wiring |
| session-routing escalation | caller-level fallback | unchanged (OUT of boundary, T2) | **behavior-preserving** |

---

## 3. Triage gate

### 3.1 IN scope (T4 execution)
1. Router: `ModelSelectorFunc` + `SetModelSelector` + `resolveModel`; extract `generateLocal`/`generateRemote`; `GenerateWithOpts` branches via `resolveModel`. *(behavior-preserving; net green unchanged)*
2. New `internal/inference/modelrank`: `Ranker` (cached, best-effort DB read of `proj_inference_tool_model_performance`) + pure `rank`. *(new behavior)*
3. `main.go`: construct the `Ranker` with `pool` + wire `inferRouter.SetModelSelector(ranker.Select)`. *(additive)*
4. Tests: a new router test that `GenerateWithOpts` with a nil selector dispatches local (cold-start parity at the dispatch level), and that an injected selector returning the remote model routes remote — proving the seam without a DB. The T1 oracle + existing dispatch net stay green and unmodified.

### 3.2 OUT of scope (recorded rejections)
- **Session-routing escalation** — OUT (T2 §2.1); untouched.
- **Per-token cost weighting** — deferred; the local-vs-remote margin is the dominant cost lever.
- **Call-level fallback when outcome is all-zero** — T4 stays-on-default (simplest safe behavior); a richer fallback is a future refinement, filed if wanted.
- **Latency-primary routing** (switch to a faster model at equal quality) — deferred; latency is a tie-break only. Staying on the free local default is the safe bias.
- **No migration** — this chain only *reads* the Chain-2 projection; no schema change.

### 3.3 Behavior-change flag
The data-present ranking path is **new behavior**, not parity. It activates only once `proj_inference_tool_model_performance` has ≥20 calls for a task's default model AND a candidate clears the 0.10 margin. Its tests are step-7 (T5), seeded hermetically. The ranking rule (§1.3) is the **user-vettable** surface — T6 surfaces it for review (margin value, warmup threshold, outcome-vs-call-level quality, the deferred axes).

---

## 4. Parity statement
After T4: the T1 cold-start oracle + the existing router dispatch/telemetry net pass with **zero assertion edits** (the local/remote extraction is behavior-preserving; nil-selector ⇒ today's behavior). New seam behavior (selector-routes-remote) gets a new additive test in T4; the ranking rule + cache get hermetic tests in T5.
