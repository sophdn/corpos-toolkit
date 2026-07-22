# Chain 1 — Inference Telemetry onto the Substrate: Detailed Design (T10)

> **Status:** DESIGN FOR VETTING. Produced by chain `per-tool-per-model-observability` (264) task `audit-and-target-design` (T10 = refactor-discipline steps 3–5: audit → first-principles target → triage). The characterization net (T9, commit `d6440884`) is GREEN and is the parity oracle this design must reproduce. Nothing here is built yet.
>
> **Companion:** `TELEMETRY_CONSOLIDATION.md` (the program; this is its §4 "Chain 1 in detail" expanded). `TELEMETRY_SUBSTRATE.md` (the read-side substrate this generalizes). `PROJECTIONS.md` (projection contract).
>
> **Plain-language promise (for the non-analyst review):** every number the Inference page shows today is pinned by the T9 golden net. This design moves *where the data lives* without changing those numbers — and the one place a naive move WOULD change a number (latency percentiles) is called out explicitly in §3 with the chosen fix.

---

## 1. Classified findings ledger

From scope-inventory (T8, verified) + the characterization net (T9). Each finding is classed **behavior-preserving** (relocation/delete with identical observable output), **behavior-changing** (adds or alters observable output — needs new tests, not parity, and may route to a later chain), or **taste** (structure/docs only).

| # | Finding | Class | Blast radius | Disposition |
|---|---|---|---|---|
| F1 | `qwen_invocations` is a direct sink read at query-time by `inference_v2.go`; never got the emit→fold→projection treatment the RAG cluster has | taste (structure) | `inference_v2.go` (3 handlers), 1 table | **Chain 1**: relocate onto the read-side substrate |
| F2 | `qwen_invocations` is Qwen-named but stores `model_name` per row; **remote Claude is never recorded** (`GenerateRemote` does not emit) | behavior-changing (adds coverage) | `router.go::GenerateRemote`, `main.go` recorder | **Chain 1**: rename → `inference_invocations`, emit at the remote path. New rows = **flagged feature delta** (new tests, not parity) |
| F3 | "success" is computed at read-time via the interpolated SQL **predicate registry** (`inference_success_predicates.go`: default / classify→benchmark / vault-rerank→grounding) | taste (structure) + behavior-changing (if redefined) | `inference_v2.go`, predicates file | **Chain 1** relocates the read unchanged (parity). **Success-model REWORK** (materialize predicates into the projection + call-level layer) → **Chain 2** |
| F4 | No `success` / `error_class` column — call-level outcome is invisible; "error" calls only show as latency rows | behavior-changing (adds columns) | table, emit payload | **Chain 1**: add both, populated at emit (call-level). *Recorded* in Ch1; *consumed by the success_rate computation* in Ch2 |
| F5 | `work_tool_calls` is **dead** (zero readers in Go + TS) and migration 075's comment **falsely** claims it is this chain's per-tool-per-model precursor (it is per-**action**, not per-**tool**) | behavior-preserving (delete) | migration 075, `main.go` `dispatch.SetCallObserver`, the false comment | **Chain 1**: DROP table + remove the `CallObserver` write + delete the false comment |
| F6 | Data-format drift (`input_tokens` vs the chain text's `tokens_in`; `error_class` vs predicate) across telemetry tables | taste | cross-table | **Chain 5** (data-format unification) |
| F7 | `inference_v2.go:127-135` — the nil-`last_call_at` sort branches are **unreachable** (`inferenceListTaskIDs` only returns task_ids with in-window rows ⇒ `last_call_at` is always populated) | taste (dead code) | `inference_v2.go` sort | **Chain 1**: the repoint rewrites this sort; drop the dead branches then (parity net guards it) |
| F8 | `inference_v2.go:244-248` — `buildHealthCard` `n==0` branch unreachable via the list→build path (a built task was listed ⇒ ≥1 in-window row) | taste (dead/defensive) | `inference_v2.go` | **Chain 1**: leave + document, or drop on repoint (low value either way) |
| F9 | `inference_retrieval.go:53` comment says `rate` is "capped at 1.0 for display" but the code does **not** cap (`count/denom`) | taste (comment bug) | one comment | Opportunistic fix (comment-only, behavior-preserving). retrieval-health is otherwise **out of Chain-1 scope** (see §4) |
| F10 | The router picks models by static rules; it doesn't read performance data | behavior-changing (new capability) | `router.go` | **Chain 3** (data-driven routing) |
| F11 | The curation scorer bypasses the router ⇒ its model calls are uncaptured | behavior-changing (coverage) | curation scorer (CLI/offline) | **Follow-on** chain `curation-scorer-telemetry-coverage` |
| F12 | The page literally named "Telemetry" shows only *search* volume/success | taste (IA) | dashboard | **Chain 4** (page IA) |

---

## 2. First-principles target (step 4)

The shape the search cluster already proves: **per-call telemetry table → read-side projection keyed by (purpose × variant) → consumers read the projection.** Bring inference onto it.

### 2.1 `inference_invocations` — per-call read-side table (supersedes `qwen_invocations`)

Same **per-call grain** as `qwen_invocations` (honors `TELEMETRY_SUBSTRATE.md` §9.2 / bug-1328: one model call ≠ one grounding event; do NOT merge into `grounding_events`). Model-agnostic name; `success`/`error_class` added; remote rows now flow.

```sql
CREATE TABLE inference_invocations (
    id            INTEGER PRIMARY KEY,
    task_id       TEXT    NOT NULL,                       -- the "tool" / inference purpose (qwenctx.TaskID);
                                                          --   'unattributed' when the caller didn't stamp one
    model_name    TEXT    NOT NULL,                       -- local ('qwen2.5-32b') OR remote ('claude-sonnet-4-6'); per-row
    latency_ms    INTEGER NOT NULL,
    input_tokens  INTEGER,                                -- NULL when the upstream model omits usage
    output_tokens INTEGER,
    success       INTEGER NOT NULL DEFAULT 1,             -- CALL-LEVEL: 1 = (no upstream error AND non-empty output)
    error_class   TEXT    NOT NULL DEFAULT '',            -- closed enum (see below); '' on success
    created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_inference_invocations_task_created ON inference_invocations (task_id, created_at);
CREATE INDEX idx_inference_invocations_task_model   ON inference_invocations (task_id, model_name);
```

- **`error_class` closed enum:** `'' | upstream_error | empty_response | not_configured | timeout`. Mapped at the router emit seam from the existing branches: local `Complete` error → `upstream_error`; empty `Choices`/`Content` → `empty_response`; remote nil-client → `not_configured`; ctx deadline → `timeout`. `success = (error_class == '' AND output non-empty)`.
- **No `project_id`** — `qwen_invocations` has none and inference isn't project-scoped; adding one would diverge from parity. (The bug_count join is the only project-scoped read and it joins `proj_current_bugs`, untouched here.)
- **`success`/`error_class` are recorded in Chain 1 but NOT yet consumed** by the `success_rate` the endpoints emit (that stays the predicate registry for parity — F3). Chain 2 switches `success_rate` onto the call-level + materialized-outcome model. Laying the columns now means Ch2 is a read-path change only.

### 2.2 `proj_inference_tool_model_performance` — read-side projection keyed (task_id, model_name)

Stored as **running totals** so (a) rates/averages compute on read and (b) the read-side invariant `Fold == RebuildFromEmpty` holds **vacuously** (re-snapshot from source; byte-identical rebuild is automatic — the `query_volume_by_source` pattern).

```sql
CREATE TABLE proj_inference_tool_model_performance (
    task_id             TEXT    NOT NULL,                 -- the tool / purpose
    model_name          TEXT    NOT NULL,
    call_count          INTEGER NOT NULL DEFAULT 0,
    success_count       INTEGER NOT NULL DEFAULT 0,       -- SUM(success) — call-level
    total_latency_ms    INTEGER NOT NULL DEFAULT 0,       -- → avg = total/call_count
    max_latency_ms      INTEGER NOT NULL DEFAULT 0,
    total_input_tokens  INTEGER NOT NULL DEFAULT 0,
    total_output_tokens INTEGER NOT NULL DEFAULT 0,
    calls_with_tokens   INTEGER NOT NULL DEFAULT 0,       -- denom for "avg tokens where usage known"
    last_invoked_at     TEXT    NOT NULL DEFAULT '',
    last_event_id       TEXT    NOT NULL DEFAULT '',       -- watermark convention (carries MAX(id); empty-string baseline)
    last_event_ts       TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (task_id, model_name)
);
CREATE INDEX proj_itmp_task_idx ON proj_inference_tool_model_performance (task_id);
```

Fold (`= RebuildFromEmpty`, re-snapshot; mirrors `queryVolumeBySourceSQL`):

```sql
INSERT INTO proj_inference_tool_model_performance
    (task_id, model_name, call_count, success_count, total_latency_ms, max_latency_ms,
     total_input_tokens, total_output_tokens, calls_with_tokens, last_invoked_at,
     last_event_id, last_event_ts)
SELECT task_id, model_name,
       COUNT(*), SUM(success), SUM(latency_ms), MAX(latency_ms),
       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
       SUM(CASE WHEN input_tokens IS NOT NULL OR output_tokens IS NOT NULL THEN 1 ELSE 0 END),
       MAX(created_at), CAST(MAX(id) AS TEXT), MAX(created_at)
FROM inference_invocations
GROUP BY task_id, model_name
```

- **Namespace:** add `"inference_"` to `projections.readSidePrefixes` (`{query_, retrieval_, training_, injection_, offload_}` → `+ inference_`). Clearer than reusing reserved `offload_` (which implied Qwen-only; inference spans local + remote).
- **Emit→fold wiring:** the recorder closure in `main.go` writes the row, then the same emit→`FoldAllReadSide` seam the RAG path uses refreshes the projection (re-snapshot is cheap at homelab volume).

---

## 3. The one place a naive move changes a number — and the fix (vet this)

**Latency percentiles are not foldable into a totals projection.** The endpoints emit `p50/p95/p99` per task (`HealthCard`) AND `p95` per model (`ModelStat`) AND `p95` per day (`SparklineBucket`). A percentile needs the *full sorted latency vector*; you cannot reconstruct it from `SUM`/`MAX`/`COUNT`. Storing the vector in the projection would just be a second copy of the table.

**Chosen resolution (proposed):**

- **Percentile / distribution-shaped reads stay on the per-call table** `inference_invocations`. Because Chain 1 is a *rename + additive columns*, percentiles over `inference_invocations` are **byte-identical** to today's over `qwen_invocations` (same rows, same `nearestRank`). Parity is trivially preserved.
- **The projection `proj_inference_tool_model_performance` backs the foldable, per-(tool,model) aggregates** — `call_count`, `success_count`/rate, **average** and **max** latency, token totals — which is exactly what the chain's new capability (per-tool-per-model **ranking**) and the Chain-3 router need. Ranking by avg/max latency + success-rate + cost does not need percentiles.

So "the existing endpoints read the projection" is true for the **per-model breakdown's count + the volume/token/success aggregates**, while the **percentile health signals read the per-call table**. Both halves preserve today's numbers; the projection *adds* the ranking surface. This split is the load-bearing decision; the alternative (percentiles in the projection) is rejected because it is not foldable without duplicating the table.

> If you'd rather the per-model breakdown also show percentiles (it shows `p95` per model today), those `p95`-per-model values likewise come from the per-call table, not the projection — same rule. The projection's per-model row carries avg/max, not p95.

---

## 4. Triage (step 5): Chain-1 scope vs routed-out

**In Chain 1 (this chain):**
1. **Relocate** `qwen_invocations` → `inference_invocations` (rename + `success`/`error_class` columns). *Behavior-preserving* for all existing reads (repoint table name).
2. **Add** `proj_inference_tool_model_performance` + register under `inference_` + verify byte-identical rebuild-from-empty.
3. **Repoint** the `/inference/health-cards` + `/inference/sparklines` reads: percentiles from the table, foldable aggregates + per-model ranking from the projection. **Golden parity proven** against the T9 net.
4. **Flagged feature deltas (new tests, not parity):** remote-Claude rows now appear (F2); per-(tool,model) ranking becomes first-class (F4 columns recorded).
5. **Delete** `work_tool_calls` + its `CallObserver` writer + the false migration-075 comment (F5).
6. Opportunistic: fix the F9 retrieval comment (behavior-preserving).

**Routed out (recorded so they're not re-audited):**
- **Success-model rework** (call-level success consumed + predicate registry materialized into the projection) → **Chain 2**. *Why:* changing how `success_rate` is computed is a behavior change; Chain 1 holds parity by leaving the read-time predicate in place.
- **Data-driven routing** (router reads the projection) → **Chain 3**. *Why:* new capability, depends on the projection existing.
- **Page IA / "Telemetry" rename / unify inference+RAG nav** → **Chain 4**. *Why:* the dashboard is the user's window; mockups before build.
- **`qwen_invocations` table DROP** → **Chain 5**. *Why:* Chain 1 supersedes the write path and repoints readers; the empty table soaks until the legacy-sink retirement chain drops it (matches the chain completion condition: "write path removed; table DROP deferred to Ch5").
- **Data-format unification** → **Chain 5**. **Curation-scorer coverage** → follow-on chain.

### retrieval-health is NOT relocated in Chain 1
`/inference/retrieval-health` reads `grounding_events` + `query_interactions` (the RAG substrate), NOT `qwen_invocations`. Its numbers are independent of this swap. The T9 net still pins it (proving Chain 1 doesn't disturb it), but no read of it changes here. (It lives on the Inference page only by UI grouping; the page-IA question is Chain 4.)

---

## 5. Execution shape for T11/T12 (preview; built after this design is vetted)

- **T11 (write path):** migration 077 — `CREATE TABLE inference_invocations` + indexes (canonical + testutil mirror); `db.RecordInferenceInvocation`; extend `router.InvocationRecord` with `Success`/`ErrorClass`; emit at `GenerateWithOpts` (all branches, as today) **and** `GenerateRemote` (new); map `error_class`. **Dual-write** `qwen_invocations` during transition is unnecessary — readers move in T12 within the same chain — but the write to the old table is removed only once readers are repointed. New-coverage tests for the remote path.
- **T12 (read path + cutover):** migration adds `proj_inference_tool_model_performance`; register the projection; repoint the endpoints per §3/§4.3; prove golden parity (re-run the T9 net unmodified); delete `work_tool_calls` + false comment. Drop the `qwen_invocations` write last.
- **T13:** post-refactor densification (coverage + mutation on the new structure; characterize the new projection + remote-row classes) + chain close (`ObservabilityAuditCompleted` event through the write-side ledger).

**Open decisions worth your explicit call** (defaults proposed, will proceed on them unless you redirect): table name `inference_invocations`; projection name `inference_tool_model_performance`; the §3 percentile split; the `error_class` enum set.
