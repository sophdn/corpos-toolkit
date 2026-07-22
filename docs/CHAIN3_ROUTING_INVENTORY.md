# Chain 3 — Data-Driven Model Routing: Scope, Inventory & Characterization Net

> **Status:** STEPS 1–2 (this task: `scope-inventory-and-characterization-net`). Chain 3 of the telemetry-consolidation program (`TELEMETRY_CONSOLIDATION.md` §3). Refactoring-discipline **step 1 (scope & inventory)** + the **step 2 (characterization net)** manifest. It does NOT contain the audit (step 3), target (step 4), or triage (step 5) — those are the forged downstream tasks. The net pinned here is the parity oracle the data-driven path must reproduce **at cold start**.
>
> **Companions:** `CHAIN2_SUCCESS_MODEL_*.md` (Chain 2 finalized `proj_inference_tool_model_performance`, this chain's data source — both `success_count` (call-level) and `outcome_success_count` (outcome-level) per (task, model)), `TELEMETRY_CONSOLIDATION.md` §2.4 + §3.


> **SUPERSEDED IN PART (2026-07-22).** The session-routing escalation this document inventories and audits — `no-trigger` + `containsRoleVocab` → `GenerateRemote` — **no longer exists.** It was compensation for Qwen under-detecting role invocations, and the role/persona system was retired wholesale, so the escalation, `RoleVocab`, `containsRoleVocab`, and the `fallback_used` field went with it. Any claim below that this is *the only* data-conditional model escalation now reads: there are **none**. The rest of the inventory stands.

---

## 1. Scope & boundary

**Chain goal (completion_condition):** the inference router makes data-driven model choices by reading `proj_inference_tool_model_performance` (60s in-process cache), ranked by success-rate/latency/cost, **with a cold-start fallback to today's static routing**; cold-start routing proven identical to today's.

**In scope (the behavior this chain refactors):** the **router-level model-selection decision** — which model identifier an inference call dispatches to (`go/internal/inference/router`). Today that decision is *static and data-blind*; the chain adds a data-backed selection step with a cold-start fallback equal to the static default.

**Adjacent / context (inventoried, boundary-flagged for the step-3 audit):**
- **The session-routing remote-escalation rule** (`measure/classify.go::HandleClassifySessionRoutingTrigger`): a *caller-level* conditional model escalation (see §2.3). It is a correctness fallback, not a performance-routing choice. **Decision for step 3:** whether the data-driven router subsumes / interacts with it, or it stays an orthogonal caller concern (the inventory's working assumption: orthogonal — out of the router-selection boundary).
- `proj_inference_tool_model_performance` (the data source) — read-side only; its computation is Chain 2's, already characterized; this chain consumes it.

**Out of scope:** the telemetry recording path (Chain 1), the success-model materialization (Chain 2), the dashboard ranking page (Chain 4 IA), and the per-call dispatch mechanics (local/remote HTTP, token capture, error_class) — already pinned and unchanged by a selection-layer addition.

---

## 2. Inventory of today's static routing

### 2.1 The router has no internal model selection
`router.Router` (`router.go`) exposes two dispatch methods; **the caller picks the method, the router does not pick the model from data**:

| Method | Model | Selection logic |
|---|---|---|
| `Generate` / `GenerateWithOpts` | local llama.cpp, `modelName` = **`"qwen2.5-32b"`** (hardcoded in `New()`) | unconditional — the default for *all* inference (classify, retrieve, arcreview, domain-term-classifier, session-routing pass-1) |
| `GenerateRemote` | remote Anthropic, const `remoteModelName` = **`"claude-sonnet-4-6"`** | unconditional when invoked; requires `ANTHROPIC_API_KEY` (else `not_configured` error). Invoked explicitly by the caller |

There is **no** task→model map, **no** name-prefix routing table, and **no** read of any performance/telemetry source in the selection path. (The roadmap's "static name-prefix rules" is loose framing for "the static model assignment" — the concrete static rule is simply *local-`qwen2.5-32b`-by-default*.) `ModelName()` returns the constant local identifier; it is the **cold-start baseline** the data-driven path must reproduce when no data exists.

### 2.2 The data source this chain will consume
`proj_inference_tool_model_performance`, keyed `(task_id, model_name)`, carries per-model `call_count`, `success_count` (call-level), `outcome_success_count` (outcome-level, Chain 2), `total_latency_ms` / `max_latency_ms`, token totals, `last_invoked_at`. The ranking rule (success-rate/latency/cost) will read these on a 60s cache. Today **nothing in the router reads it** — that read is the chain's central addition.

### 2.3 The one conditional model escalation (caller-level, boundary-flagged)
`HandleClassifySessionRoutingTrigger` (`measure/classify.go` ~L307–326): after a local `Generate`, **if** the parsed label is `no-trigger` **and** `containsRoleVocab(userInput)`, it calls `GenerateRemote` (claude) as a second opinion; on remote success the label is replaced and `fallback_used=true` is recorded; on remote error the local `no-trigger` is kept. This is the **only** data-conditional model escalation in the codebase today, and it is hardcoded (not data-driven). It is *caller-level*, not router-level. Existing coverage: `measure/classify_test.go::TestHandleClassifySessionRoutingTrigger_WritesStructuredNotes` pins the **non**-escalation path (`fallback_used=false`). **Flagged for step-3 audit** (in/out of the data-driven-selection boundary).

---

## 3. Characterization net (step-2 gate) — GREEN

The model-selection input space is, today, **trivial and constant** (no task/data input branches the choice), so the net is judged complete against that small space: the static model identifiers + the cold-start default + data-blindness. `go test -tags sqlite_fts5 ./internal/inference/router/` is GREEN.

### 3.1 Pre-existing pins (dispatch + selection contract) — `router_test.go`
| Input class | Pinned by |
|---|---|
| local dispatch success (text + tokens) | `TestGenerate_LocalSuccess`, `…OmitsTokensWhenUsageAbsent` |
| local failure, **no silent remote fallback** | `TestGenerate_LocalFailureNoFallback` |
| remote dispatch success | `TestGenerateRemote_Success` |
| remote unavailable (nil client) → error, not local | `TestGenerateRemote_NilClientReturnsError` |
| **remote model identifier = `claude-sonnet-4-6`** | `TestGenerateRemote_RecordsRemoteModelOnSuccess` |
| local model identifier via injected name | `TestModelName` (uses `NewWithClients` — injects the name) |
| call-level success + error_class branches (upstream / empty / not_configured) | `TestGenerate_RecordsCallLevelSuccess`, `…RecordsUpstreamErrorClass`, `…RecordsEmptyResponseOnEmptyText`, `TestGenerateRemote_Records{UpstreamError,EmptyResponse,NotConfigured}…` |
| task-id stamping / recorder wiring | `TestGenerate_Invokes…Recorder…`, `…UnstampedCtxRecordsAsUnattributed` |

### 3.2 Chain-3 densification added by this task
| Input class | Pinned by |
|---|---|
| **cold-start static default**: the production constructor `New("")` yields `ModelName() == "qwen2.5-32b"` with no performance data, and the identifier is **stable across calls** (data-blind). This is the explicit parity oracle for "cold-start == today's static routing." | **`TestStaticRouting_ColdStartModelSelectionIsStaticDefault`** (NEW) |

**Why this is the right net:** the existing suite already pins the per-call dispatch contract exhaustively; the only *selection*-specific invariant the data-driven change must preserve is "with no data, pick the static default, deterministically." The new test pins exactly that via the production constructor (`TestModelName` only pins an *injected* name, not the production default). When the data-driven `SelectModel` lands (step 6), its cold-start branch must keep this test green; its data-present branch is new behavior with its own tests (step 7).

---

## 4. Acceptance for this task (steps 1–2)
- [x] Boundary + inventory written (this doc): the static selection contract, the data source, the caller-level escalation (boundary-flagged).
- [x] Characterization net GREEN, with the cold-start static-default oracle pinned explicitly via the production constructor.
- [x] Downstream tasks (steps 3–7) forged before close (so the chain does not stall at the gate — the anti-pattern remediated for Chain 2 on 2026-05-26).
