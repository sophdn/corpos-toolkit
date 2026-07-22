# Orchestrator-tier escalation contract — Design

> **Status:** Authoritative. Produced by chain `orchestrator-tier-escalation-contract` T1 (`design-escalation-contract-doc`). Decisions here are durable; downstream tasks T2–T5 bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Reading order:** §1 framing → §2 triggers → §3 EscalationProposed payload → §4 hot-switch → §5 de-escalation hysteresis → §6 threshold tunables → §7 the contract surfaces (event / config / library) → §8 cross-references.
>
> **Companion docs:** `docs/EVENT_SUBSTRATE.md` (the write-side ledger the EscalationProposed event lands through; envelope shape, actor inference, validation). `docs/EVENT_CATALOG.md` (the registered-type catalog this chain adds two rows to). `docs/INFERENCE_DISPATCH.md` (the in-process Go model-dispatch boundary — distinct from this contract, see §1.3).

---

## 1. What this contract is and isn't

### 1.1 The framing

This contract sits on the **agent-loop-driver swap** axis, not the worker-tier-offload axis. The distinction is load-bearing and is the canonical framing from `vault/decisions/2026-05-19_two-axes-of-llm-offload-worker-tier-vs-agent-loop-driver.md`:

- **Worker-tier offload** replaces strong-LLM calls *inside* gate-verified units (atomic-tasks ATs, classification steps, retrieval rerank). Failures are bounded to the unit and recoverable by tier-bump.
- **Agent-loop-driver swap** replaces the harness running the *interactive conversational loop* — the thing reading user messages, calling tools, holding session state, deciding what to do next.

This chain ships **an escalation contract library that any harness can consume** — per that decision, "enabling work for [the driver swap] but a substrate artifact, neither axis on its own." Concretely: a cheap orchestrator-tier model (DeepSeek V4 Pro, Qwen) drives the loop until it hits trouble it can't recover from cheaply, at which point the contract proposes handing the *next* turn to a strong orchestrator-tier model (Opus 4.7), records the proposal as a first-class observable event, and — after the strong tier has stabilised the work — hands control back. The escalation is **advisory and observable**, not a forced in-process switch.

### 1.2 The atomic-tasks lineage

The retry-exhaustion trigger (§2, trigger 1) is the orchestrator-level generalisation of the worker-tier pattern atomic-tasks already runs: gate-verified units execute on a cheap local model (Qwen via llama-server), and on retry-exhaustion the work is **recoverable by tier-bump** — escalate the tier rather than fail the unit (`atomic-tasks/retrospectives/2026-05-18_claude-in-loop-harvest-pre-offload.md` §"Worker-tier failure modes"; `atomic-tasks/README.md` §"Workhorse model"). atomic-tasks bounds failure to one AT and bumps the *worker* tier; this contract bounds failure to one orchestrator turn-window and bumps the *driver* tier. Same shape — a cheap tier with a gated, recoverable escalation edge to a strong tier — applied one level up the stack.

### 1.3 Relationship to the in-process Go dispatch router

`docs/INFERENCE_DISPATCH.md` describes `go/internal/inference/router`: the in-process Go package that selects between local Qwen (llama.cpp) and an optional remote Anthropic client for a single inference call (knowledge rerank, classify, retrieve). **That is worker-tier dispatch and is out of scope here.** This contract operates one layer up: it decides which model *drives the next conversational turn*, lives in a harness-agnostic Python library (the harness, not toolkit-server, runs the loop), and records its decisions through toolkit-server's write-side event ledger. The two never call each other; the Go router has no notion of session state or de-escalation hysteresis, and this contract has no notion of llama.cpp endpoints.

### 1.4 Scope

In scope (this chain): the trigger taxonomy, the EscalationProposed event type, the per-trigger threshold config table + admin read/write actions, the reference Python detector + router-state-machine library, the integration test, and the closing audit event.

Out of scope: building the actual cheap-orchestrator harness loop (that's the driver-swap chain proper — this contract is the *enabling* artifact it will consume); training a classifier to score escalation decisions (a follow-on once the event corpus accumulates); any change to the Go inference router.

---

## 2. Triggers (5 initial)

A trigger is a condition the cheap orchestrator tier evaluates at a turn boundary (or mid-turn — see §4). When a trigger fires and the router is in the `cheap` state, the contract proposes escalation. The five initial triggers, with the threshold knob each consumes:

| # | `trigger_kind` | Fires when | Threshold semantics | Default |
|---|---|---|---|---|
| 1 | `retry_exhaustion` | A gate-verified unit (an AT, a tool-call retry loop) has consumed its retry budget without passing. The atomic-tasks tier-bump edge, lifted to the orchestrator. | `threshold_value` = max consecutive failed retries on the same unit before escalating. | `2` |
| 2 | `low_confidence` | The cheap model's self-reported (or classifier-scored) confidence in its chosen next action falls below the floor. | `threshold_value` = confidence floor in `[0,1]`; fires when observed confidence `<` floor. | `0.35` |
| 3 | `repeated_tool_error` | The same tool call returns a structured error repeatedly within the turn window. | `threshold_value` = max tool errors in-window before escalating. | `3` |
| 4 | `parse_failure` | The cheap model's output fails to parse into the required structured shape (malformed tool call / JSON) repeatedly. | `threshold_value` = max consecutive parse failures before escalating. | `2` |
| 5 | `explicit_handoff` | The cheap model emits an explicit "I need the strong tier" signal (a sentinel token / self-escalation request). | `threshold_value` = count of explicit signals before escalating (normally `1` — fire immediately). | `1` |

**Why these five.** They span the recoverability spectrum: 1 and 3 are *external* failure (the work won't converge), 2 and 5 are *self-assessed* failure (the model knows it's out of its depth), 4 is *protocol* failure (the model can't even emit a valid action). Each is cheaply observable by a harness without a model call. The set is **closed** for v1; adding a sixth trigger is a chain-level decision plus a config-table CHECK-constraint migration (the `trigger_kind` enum is enforced at the DB level — see §6).

**`threshold_value` is a single REAL per trigger.** Counts (triggers 1, 3, 4, 5) store as whole numbers in the REAL column; the confidence floor (trigger 2) stores as a fraction. The detector for each trigger knows how to interpret its own `threshold_value` (count comparison vs. floor comparison) — the config layer stays uniform.

---

## 3. The EscalationProposed payload

EscalationProposed is a new event type registered through the write-side substrate (`docs/EVENT_SUBSTRATE.md` §3). The **envelope** (event_id, ts, actor, span_id, …) is the shared substrate envelope; only the type-specific **payload** is defined here.

```json
{
  "trigger": "retry_exhaustion",
  "from_model": "deepseek-v4-pro",
  "to_model": "claude-opus-4-7",
  "session_id": "orch-7f3a9c2e",
  "turn_index": 14,
  "state_before": "cheap",
  "state_after": "escalated",
  "trigger_detail": "unit=at-3 retries_used=2 last_gate=go_build_failed",
  "threshold_value": 2.0
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `trigger` | string (enum, the 5 §2 kinds) | yes | Which trigger fired. |
| `from_model` | string | yes | The cheap orchestrator model identifier proposing escalation (e.g. `deepseek-v4-pro`, `qwen2.5-32b`). Payload-level, **not** the envelope actor — see the actor note below. |
| `to_model` | string | yes | The strong orchestrator model the next turn is proposed to run on (e.g. `claude-opus-4-7`). |
| `session_id` | string | yes | The orchestrator session the proposal belongs to. Also the entity slug (below). |
| `turn_index` | integer ≥ 0 | yes | Which turn produced the proposal. Lets a reader reconstruct the escalate→de-escalate arc within a session. |
| `state_before` | string (enum: `cheap`/`escalated`/`de_escalated`) | yes | Router state at proposal time. |
| `state_after` | string (enum: same) | yes | Router state after the transition this event records. An escalate edge is `cheap → escalated`; a de-escalation edge (recorded for observability) is `escalated → de_escalated`. |
| `trigger_detail` | string | no | Free-form evidence the detector captured (retries used, confidence value, error kind). Human-readable; not parsed. |
| `threshold_value` | number | no | Snapshot of the `threshold_value` that fired, so a reader sees the contract's config at decision time without joining the (mutable) config table. |

### 3.1 Entity and actor

- **Entity.** `entity.kind = "orchestrator_session"`, `entity.slug = session_id`. Project-scoped (`entity.project_id`) when the harness supplies a project; cross-cutting (`project_id = null`) otherwise — a harness-agnostic library may not be project-aware. This mirrors the substrate's `NewEntityRef` / `NewCrossCuttingEntityRef` split.
- **Actor.** Per `docs/EVENT_SUBSTRATE.md` §4, actor is inferred at the transport boundary, **never caller-supplied** — so an agent cannot forge an identity to dodge the rationale gate. The library emits over the HTTP `POST /mcp/{surface}` route, which stamps no actor → the substrate's `system`-kind sentinel. The *meaningful* model identities therefore live in the payload (`from_model` / `to_model`), not the envelope actor. The proposal's "why" is recorded via a handler-level rationale override (§7.3) so it lands even though `system` actors aren't subject to the rationale gate.

### 3.2 De-escalation is not a separate event type

The escalate edge emits EscalationProposed (`state_after = escalated`). The de-escalation edge is a router-internal transition; for observability the library MAY emit EscalationProposed with `state_after = de_escalated` and `from_model`/`to_model` reversed, but the **closing test asserts the escalate event landed** — de-escalation is verified through the router state machine (§5), not a mandatory second event type. Keeping it one type keeps the catalog small; the `state_before`/`state_after` pair already carries the edge direction.

---

## 4. Hot-switch semantics

"Mid-turn hot-switch" means: the proposal is **recorded the instant a trigger fires**, but the actual model swap takes effect at the **next turn boundary**. The contract never aborts an in-flight generation and never restarts the session.

```
turn N   (cheap model running) ── trigger fires mid-turn ──▶ EscalationProposed emitted
                                                             router state: cheap → escalated
turn N   finishes on the cheap model (in-flight work is not discarded)
turn N+1 begins ── harness calls router.next_model() ──▶ returns the STRONG model
turn N+1 (strong model running)
```

Rationale:

- **No wasted work.** The cheap model's current turn may still produce useful partial output (a tool result, a parse the next tier can build on). Discarding it to switch immediately would burn the work that *detected* the problem.
- **Clean handoff boundary.** The turn boundary is where session state is already serialised (message history, tool results). Switching there means the strong tier picks up a consistent state; switching mid-generation would require defining how to splice two models' partial outputs — unnecessary complexity for no benefit.
- **Advisory, not forced.** `router.next_model()` is what the harness *reads* at the top of each turn. The contract proposes; the harness disposes. A harness that wants to ignore a proposal (cost ceiling, offline strong tier) can — the event still lands for observability, and the router still tracks the proposed state.

The harness contract is therefore exactly two calls: `router.observe(signals)` after each turn (feeds the detectors, may emit an event + transition state), and `router.next_model()` at the start of each turn (reads the current driving model). Everything else — detector thresholds, hysteresis, event emission — is internal to the library.

---

## 5. De-escalation hysteresis

Once escalated, the router does **not** drop back to the cheap tier the moment a turn goes cleanly. It stays escalated until **K consecutive clean turns** (no trigger fired) have elapsed, where K is the `de_escalation_turns` tunable (§6). Only then does `next_model()` return to the cheap model.

```
state=escalated, clean_streak=0
turn clean   ─▶ clean_streak=1
turn clean   ─▶ clean_streak=2   (K=2 reached) ─▶ state: escalated → de_escalated → cheap
                                                  clean_streak reset to 0
```

Any trigger firing while escalated **resets `clean_streak` to 0** — the strong tier keeps driving. A trigger firing while already escalated does not re-emit an escalate event (the router is already escalated); it just resets the streak (and may emit an observability event if the harness opted in).

**Why hysteresis.** Without it, borderline work flaps: the strong tier fixes the immediate problem, control returns to the cheap tier on the very next turn, the cheap tier re-hits the same class of problem, escalate again — a thrash that pays the strong-tier price repeatedly *and* pays the switch-overhead each cycle. Requiring K clean turns means "the work has actually stabilised under the strong tier" before the cheap tier reclaims it. K is tunable per the §6 config because the right value is workload-dependent: tight loops with cheap turns want a larger K (more confirmation), expensive-turn workloads want a smaller K (reclaim the cheap tier sooner).

The transient `de_escalated` state in the enum is the observable marker of the down-edge (it appears in an emitted event's `state_after` if the harness emits the de-escalation event); the router's *resting* states are `cheap` and `escalated`.

---

## 6. Threshold tunables

Per-trigger thresholds + the hysteresis K live in a config table, `escalation_thresholds`, owned by toolkit-server (T2 lands the migration). One row per `(project_id, trigger_kind)`:

| Column | Type | Notes |
|---|---|---|
| `project_id` | TEXT | `''` (empty) is the **global default** row, applied when no project-specific row exists. A project-specific row overrides the global for that `(project, trigger)`. |
| `trigger_kind` | TEXT | CHECK-constrained to the 5 §2 kinds. The DB enforces the closed set — an unknown kind is rejected at write time, not silently stored. |
| `threshold_value` | REAL | The trigger's threshold; semantics per §2 (count or floor). |
| `enabled` | INTEGER | `1`/`0`. A disabled trigger never fires regardless of signals — lets an operator turn off a noisy trigger without losing its tuned threshold. |
| `de_escalation_turns` | INTEGER | The hysteresis K (§5). Router-level conceptually, stored per-row and kept uniform across a project's rows; the library loads K as the value from the rows for a project (they agree by construction of the set action). Default `2`. |
| `updated_at` | TEXT | RFC-3339 stamp of the last write. |
| `last_event_id` / `last_event_ts` | TEXT | Substrate projection-convention columns (default `''`); reserved for a future fold if escalation-config changes become event-sourced. |

The migration **seeds the five global-default rows** so the contract works out of the box (defaults from the §2 table; `de_escalation_turns = 2`).

**Admin read/write.** Two admin MCP actions (T2):

- `escalation_threshold_list` — read. Params `{ project_id? }`; returns the global rows merged with the project-specific overrides (the effective config a harness would load).
- `escalation_threshold_set` — write (upsert on `(project_id, trigger_kind)`). Params `{ project_id?, trigger_kind, threshold_value, enabled?, de_escalation_turns? }`. A mutating action: registered `requires_rationale = true` in `action-manifests/dispatch-policy.toml`, consistent with the other admin write actions.

Why admin (not a new surface): config CRUD is operator-shaped, matching the existing `host_*` / `project_*` admin actions; co-locating the whole escalation surface (config + emit) on `admin` means a harness targets exactly one route, `POST /mcp/admin`.

---

## 7. The three contract surfaces

The contract is realised as three artifacts, each landing in a downstream task:

### 7.1 The event (T2, completion-condition (b) + (f))

`EscalationProposed` (§3) and `EscalationContractAuditCompleted` (the chain's closing audit, `ArchitectureAuditFinding` shape like the other `*AuditCompleted` events). Both register through the standard four-step path (`docs/EVENT_CATALOG.md` §3.2): blueprint JSON at `blueprints/events/<Type>.json`, synced to the Go embed by `scripts/sync-event-schemas.sh`, a typed payload struct in `go/internal/events/payloads.go`, and a catalog row. The audit event is emitted by a one-shot program, `go/cmd/orchestrator-escalation-audit-emit`, mirroring the existing `*-audit-emit` programs — the self-hosting proof that the chain records its own closing audit through the ledger it built.

### 7.2 The config (T2, completion-condition (c))

The `escalation_thresholds` table (§6) + `escalation_threshold_list` / `escalation_threshold_set` admin actions.

### 7.3 The library (T3 + T4, completion-condition (d) + (e))

A harness-agnostic reference Python library at `clients/escalation/`, importable by any harness, **standard-library-only** (no third-party runtime dependency — HTTP via `urllib.request`, JSON via `json`). Three modules:

- `escalation.triggers` (T3) — one pure detector per trigger (§2). Each takes the turn's observed signal + the threshold and returns whether the trigger fired plus its evidence. No I/O, no model calls.
- `escalation.client` (T3) — `EscalationClient(base_url)`: thin toolkit-server MCP client over `POST /mcp/admin`. `propose(...)` emits EscalationProposed (carrying a `reason` the handler records as the event rationale); `list_thresholds(...)` / `set_threshold(...)` read and write the config table.
- `escalation.router` (T4) — `EscalationRouter`: the `cheap ↔ escalated` state machine with K-turn de-escalation hysteresis (§5) and per-trigger threshold consumption. Pure state machine with an optional injected `on_escalation` emit hook (so unit tests run with no server and the integration test wires the real client). Exposes `observe(signals)` and `next_model()` — the two-call harness contract from §4.

### 7.4 The integration proof (T5, completion-condition (e))

A simulated retry-exhaustion trigger fires EscalationProposed, the router switches the next turn's model, de-escalation is observed after K turns, threshold config round-trips through the admin actions, and the EscalationProposed event is confirmed landed in the write-side ledger. Realised as: a Go integration test for the config round-trip + emit-lands-in-ledger (gate-protected via `make -C go test`), Python `unittest` coverage for the detectors + router transitions, and an end-to-end Python test against a live `scripts/worktree-mcp.sh` server (documented run; skipped when no server URL is configured).

---

## 8. Cross-references

- `vault/decisions/2026-05-19_two-axes-of-llm-offload-worker-tier-vs-agent-loop-driver.md` — the framing this contract sits on (driver-swap-enabling substrate artifact). This chain is the "escalation contract library any harness can consume."
- `atomic-tasks/retrospectives/2026-05-18_claude-in-loop-harvest-pre-offload.md` + `atomic-tasks/README.md` — the worker-tier tier-bump lineage the `retry_exhaustion` trigger generalises.
- `docs/EVENT_SUBSTRATE.md` — the write-side ledger (envelope, actor inference, validation, fold contract) EscalationProposed lands through.
- `docs/EVENT_CATALOG.md` — the registered-type catalog; this chain adds `EscalationProposed` + `EscalationContractAuditCompleted`.
- `docs/INFERENCE_DISPATCH.md` — the in-process Go worker-tier model router this contract is explicitly *not* (§1.3).
