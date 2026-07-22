# Work Batching and Forge Templates — Plan + Design Audit

**Chain:** work-batching-and-forge-templates (id 281)
**Task:** T1 — design-pass
**Date:** 2026-05-23

This doc is the load-bearing output of T1. It does three things:

1. Audits each of the 8 chain-level design decisions for internal consistency and load-bearing rationale (PASS / TIGHTEN / OVERTURN).
2. Resolves the three open questions T1's acceptance lists (`work.batch` op surface; `forge(chain, tasks=full-objects)` back-compat; `measure` vs `work` for `bench_run`).
3. Sketches the composite-event payload shapes (`BatchExecuted`, `TaskHandoff`, `ChainAndTasksForged`) at field-set granularity so T2/T3/T7 can land their event-emit code without re-deriving the field sets.

No implementation code lands in T1. The audit produces zero chain `forge_edit` calls — every decision either stands as written or gets a tightening note that the implementing task picks up.

---

## 0. Vault context applied to this audit

Three vault notes are load-bearing for the design and were consulted before the audit pass:

- **`memory/feedback/no-batching-in-chains.md`** — *chain task design* must stay 1-task-per-step. This does NOT conflict with T2 (`work.batch`). `work.batch` is dispatcher-level batching of independent ops within one tool round-trip; the chain-design rule is about chain-task granularity. The two operate at different scopes and never trade off against each other. Call this out so the design isn't misread as a reversal.
- **`memory/feedback/feedback-work-surface-rationale-envelope.md`** — `rationale` is envelope-level on every work mutating action (next to `action`/`project`/`params`, not inside `params`). T2's per-op shape `{op, params, rationale}` is an *inner envelope* that reproduces the envelope-level convention at the op level. The outer envelope still carries a batch-level `rationale` for the batch-as-a-whole (see Decision 1 tightening).
- **`decisions/2026-05-20_forge-schema-parameter-overloading-trap.md`** — when authoring the four new forge schemas (retrospective, report-card, migration, bench), scan field names against reserved top-level envelope keys (`project`, `slug`, `rationale`, `schema_name`, `commit_sha`/`sha`, `chain_slug`). For any collision, rename the schema field (preferred) or set `cross_project = true` to disable auto-injection. Return `RoutingNote` on the create-result for any caller-influenced routing.
- **`learnings/mcp-servers/2026-05-09_silent-drop-pattern-recurring-by-shape.md`** — every new action (`work.batch`, `work.lifecycle_step`, `measure.bench_run`) must reject unknown params at dispatch with the action's accepted-params list named in the error. This adds an explicit smoke-test row in T2/T3/T6.

---

## 1. Audit of the 8 chain-level design decisions

Decision text is summarized from the chain's `design_decisions` field (`chain_state` on chain id 281).

### Decision 1 — `work.batch` as a new action with per-op rationale invariant

**Verdict: PASS, with one tightening.**

The per-op rationale shape is required for the events ledger to remain audit-grade across batches: a single envelope-level rationale would collapse the "why" for ops with genuinely distinct intents (close-task vs start-next-task vs forge-bug — three batched ops, three reasons).

**Tightening:** the batch *also* carries an envelope-level `rationale` for the batch-as-a-whole ("why am I batching these"). Both grains are kept — envelope rationale and per-op rationale — and the `BatchExecuted` event payload records both. The chain decision text says "per-op rationale stamped on the events ledger" but is silent on the batch-level rationale; T2 acceptance should make explicit that *both* are recorded.

### Decision 2 — `work.lifecycle_step` as sugar over `work.batch`

**Verdict: PASS, with one tightening.**

`lifecycle_step` is named at the abstraction level the chain operates at (a chain-task lifecycle handoff) rather than the implementation level (`close_and_start`). This stays correct as future task-lifecycle transitions get added — `unstart → start_next` could share the same sugar without renaming.

**Tightening:** the sugar expands to two ops under the hood (`task_complete` + `task_start`), and the composite `TaskHandoff` event emits *in addition to* the per-op `TaskCompleted` + `TaskStarted` events, not in lieu of them. Reason: downstream listeners that subscribe to `TaskCompleted` or `TaskStarted` individually must not regress when callers switch to `lifecycle_step`. The composite is the affordance event; the per-op events are the data events. Same shape as the existing `ChainCreated → cascade TaskCreated` pattern in `blueprints/events/ChainCreated.json`.

### Decision 3 — `forge(retrospective)` + `forge(report-card)` as distinct schemas

**Verdict: PASS, with one tightening.**

The two artifacts have distinct lifecycle positions (retro lands at chain-close time; report-card lands post-close, written by a fresh sub-agent). Folding into one schema with a `kind=retro|report-card` discriminator would invite the parameter-overloading trap from the 2026-05-20 vault decision (kind being a reserved-key alias for `schema_name`).

**Tightening:** the section skeletons (heading + default field set) are schema-resident, not caller-supplied. T4 acceptance should specify the default sections for each:

- **Retrospective:** Outcome → What worked → Friction surfaced → Decisions revisited → Per-task adoption telemetry → Token-budget delta.
- **Report card:** Per-task grades (A/B/incomplete + one-sentence rationale) → Independent reproduction → Measurement (call count delta vs predecessor chain).

Callers pass `sections=[]` to use defaults or a custom list to override.

### Decision 4 — `forge(migration)` owns numbering + canonical/mirror sync

**Verdict: PASS, with one tightening.**

Auto-numbering past sequence gaps closes a real footgun — the `047_drop_per_handler_telemetry_tables.sql.skeleton` file proves agents have miscounted in the past. Owning canonical+mirror sync at create-time prevents the silent-drop pattern from re-firing on the migration pair.

**Tightening:** idempotency dedup key is `(migration_name, content_hash)`. Same name + same content returns the existing row (idempotent). Same name + different content errors with `MigrationNameCollision`. Different name + same content writes a new row (rename is a legitimate change). T5 acceptance should name this rule.

### Decision 5 — `forge(bench)` + `measure.bench_run` (separate surfaces)

**Verdict: PASS — resolves Q3 in advance.**

The split is correct: bench registration is metadata (slug, binary, flags, baseline-path) → `forge` on work surface; running and diffing is operational → `measure` (which already owns the benchmark vocabulary via `benchmark_replay`).

**Tightening:** baseline files live at `bench/baselines/<slug>.json` by convention. Storing JSON files (not DB rows) for baselines is intentional — they need to be hand-editable and diff-reviewable when a metric's expected value legitimately shifts. The `--update-baseline` flag on `bench_run` overwrites the file in-place and emits a `BenchmarkBaselineUpdated` follow-on event (see Decision 7 tightening for the event-set).

The schema field `baseline_json_path` does NOT collide with any reserved top-level key (checked against the trap-decision's list: `project`, `slug`, `rationale`, `schema_name`, `commit_sha`/`sha`, `chain_slug`).

### Decision 6 — Implementation order (T2 batch → T3 lifecycle → T4 retro/report-card → T5 migration → T6 bench → T7 chain+tasks)

**Verdict: PASS.**

T2 must precede T3 (T3 is sugar over T2). T4 must precede T8/T9 (T8/T9 dog-food the retro and report-card schemas). T7 sensibly goes last — it's the most-reach change, benefits from T2/T3 learnings on composite-event shape.

T5 (migration) and T6 (bench) are independent of T2-T4-T7 and could parallelize. Sequential is fine; flagging the parallelization opportunity for the implementer if they want to spawn worktree agents.

### Decision 7 — Per-action event types

**Verdict: PASS, with one tightening.**

Eight new event types match the existing one-event-per-action convention (`blueprints/events/` already carries 50+ types; the catalog grows by 8 to ~58).

**Tightening:** the `BenchmarkBaselineUpdated` event surfaces as a 9th type when `--update-baseline` fires on `bench_run`. Without it, baseline updates leave no trace in the events ledger. T6 acceptance should add this event-type to the registration list.

Final event set (T6 update increments the count from 8 to 9):

| Event | Emitted by | Composite? |
|---|---|---|
| `BatchExecuted` | `work.batch` | Yes (cascades per-op events) |
| `TaskHandoff` | `work.lifecycle_step` | Yes (cascades `TaskCompleted` + `TaskStarted`) |
| `MigrationForged` | `forge(migration)` | No |
| `RetrospectiveForged` | `forge(retrospective)` | No |
| `ReportCardForged` | `forge(report-card)` | No |
| `BenchmarkForged` | `forge(bench)` | No |
| `BenchmarkDiff` | `measure.bench_run` | No |
| `BenchmarkBaselineUpdated` | `measure.bench_run --update-baseline` | No (T6 tightening) |
| `ChainAndTasksForged` | `forge(chain, tasks=[full-objects])` | Yes (cascades `ChainCreated` + N `TaskCreated`) |

### Decision 8 — chain+tasks atomic create as separate shape from `work.batch`

**Verdict: PASS.**

Folding chain+tasks into `work.batch` would force the batch to learn "use op1's result as op2's input" plumbing, which is exactly the kind of complexity `work.batch` should NOT take on. The chain-create returns the chain id, and tasks need that id as foreign key — a parent-child relation, not independent ops.

`ChainAndTasksForged` as a singular composite event is consistent with this split.

---

## 2. Open questions resolved

### Q1 — `work.batch` op surface: ONLY existing work_* ops, or generic `(action, params)` pair?

**Resolution: Generic dispatch shape, allowlist filter at the batch entrypoint.**

The batch handler accepts `{op: <action-name>, params: <object>, rationale: <string>}` and routes through the existing work-surface dispatch table (the same `BuildTable` map in `go/internal/work/table.go`). An allowlist filter at the batch entrypoint rejects any op that isn't a **work-surface mutating action**: no nested `batch`, no `forge_schemas`/`__actions__` reads (allowed individually but pointless inside a batch), no admin/knowledge/measure crossover.

Rationale: generic-shape + allowlist-filter is the union of the two options. Reuses the existing handler table (no per-op explicit registration), prevents privilege escalation through batch (no calling admin actions), keeps the schema discoverable through one path. Allowlist is mechanical: a single `isBatchAllowedAction(name string) bool` predicate.

The allowlist is the load-bearing safety: without it, the batch becomes a way to bypass per-action policy gates (`requires_rationale`, future per-action rate limits). With it, the per-op envelope rationale flows through the same gate the per-call envelope rationale flows through today.

### Q2 — `forge(chain, tasks=[full-objects])`: back-compat for pipe-delimited, or deprecate?

**Resolution: Keep pipe-delimited shape accepted byte-for-byte. No deprecation in this chain.**

The pipe-delimited shape is the only shape callers have ever seen. The smoke fixture in the chain's completion criteria explicitly tests **mixed mode** (some entries pipe-delimited, some full-object) — that fixture would be unbuildable if pipe-delimited weren't supported. Deprecation can ship as a separate chain once telemetry shows the full-object shape has displaced pipe-delimited in practice.

T7 acceptance should explicitly include a smoke-test row: "existing pipe-delimited shape stays accepted byte-for-byte" (the chain already lists this; flagging it as load-bearing here so it doesn't get dropped in implementation).

### Q3 — `bench_run`: `measure` or `work`?

**Resolution: `measure`.**

Precedent settles it: `measure.benchmark_replay` already lives in `go/internal/measure/benchmark_replay.go` and does the conceptually-adjacent thing (re-run a recorded benchmark, emit `BenchmarkRunStarted` with `caused_by_event_id` chained). `bench_run` is the same shape: take a registered harness, execute, compare to baseline, emit a diff event. Same surface, same vocabulary.

The alternative ("work owns chain-tracking") doesn't fit. `bench_run` doesn't necessarily run in chain context; it can be invoked ad-hoc (CI, manual smoke, post-commit advisor). Chain-tracking is orthogonal to where the runner lives.

`forge(bench)` stays on `work` (it's a forge schema, and forge lives on work). The split — registration on work, execution on measure — mirrors the existing `forge(setup-recipe)` on work + `admin.recipe_run` on admin precedent.

---

## 3. Composite-event payload sketches

The envelope structure stays the same as every other event (per `blueprints/events/_envelope.json`). What follows is the `payload` field for each composite event, named to field-set granularity so T2/T3/T7 can write the blueprint JSON without re-deriving the shape. Per-op cascade events use the existing payload schemas (`TaskCompleted`, `TaskStarted`, `TaskCreated`, `ChainCreated`) with `refs.caused_by_event_id` set to the composite event's `event_id`.

### 3.1 `BatchExecuted` payload (T2)

```json
{
  "op_count": 5,
  "succeeded": 4,
  "failed": 1,
  "continue_on_error": false,
  "rolled_back": true,
  "ops": [
    {
      "position": 0,
      "action": "task_complete",
      "ok": true,
      "rationale": "<verbatim per-op rationale>",
      "event_id": "<cascade event id>"
    },
    {
      "position": 1,
      "action": "task_start",
      "ok": false,
      "rationale": "<verbatim per-op rationale>",
      "error_kind": "TaskNotFound",
      "error_message": "<verbatim error>"
    }
  ]
}
```

Field rules:

- `op_count`, `succeeded`, `failed` — counts always present.
- `continue_on_error` — mirror of the caller's flag (default false).
- `rolled_back` — true iff abort-on-first-error fired AND the SQL transaction rolled back. Distinct from `failed > 0` because `continue_on_error=true` can succeed at envelope level with failed ops.
- `ops[]` — one entry per op, in caller-supplied order. `event_id` is the cascade event's id (omitted for failed-and-rolled-back ops). `rationale` is verbatim. `error_kind`/`error_message` present only when `ok=false`.

### 3.2 `TaskHandoff` payload (T3)

```json
{
  "chain_slug": "work-batching-and-forge-templates",
  "closed": {
    "task_slug": "T1-design-pass",
    "commit_sha": "<sha>",
    "handoff_output": "<verbatim>",
    "event_id": "<cascade TaskCompleted event id>"
  },
  "started": {
    "task_slug": "T2-work-batch",
    "event_id": "<cascade TaskStarted event id>"
  }
}
```

Field rules:

- `chain_slug` — present always (denormalized from both tasks; they must share a chain — T3 enforces this at dispatch).
- `closed.commit_sha` — may be `"unversioned"` per existing `mcp__toolkit-server__work` convention.
- `closed.handoff_output` — verbatim copy of the handoff field from the closing call. Distinct from envelope.rationale.
- `closed.event_id` / `started.event_id` — cascade event ids (always present; lifecycle_step is atomic and either both ops emit or the transaction rolls back).

### 3.3 `ChainAndTasksForged` payload (T7)

```json
{
  "chain_slug": "...",
  "task_count": 9,
  "tasks": [
    {
      "position": 1,
      "slug": "T1-design-pass",
      "shape": "full_object",
      "event_id": "<cascade TaskCreated event id>"
    },
    {
      "position": 2,
      "slug": "T2-work-batch",
      "shape": "pipe_delimited",
      "event_id": "<cascade TaskCreated event id>"
    }
  ]
}
```

Field rules:

- `chain_slug` — the just-created chain (matches the cascade `ChainCreated` event's `entity.slug`).
- `task_count` — `len(tasks)`.
- `tasks[].shape` — `"full_object"` or `"pipe_delimited"` per the mixed-mode design. Lets observers detect the split without re-parsing the input.
- `tasks[].event_id` — cascade `TaskCreated` event id (always present; the whole forge is one transaction).

The cascade `ChainCreated` event emits with `refs.caused_by_event_id = <ChainAndTasksForged event_id>`; each cascade `TaskCreated` ALSO points at `<ChainAndTasksForged event_id>` (not at the `ChainCreated` event). Single parent, N+1 children. This keeps the causal graph rooted at the composite, not at the chain row.

---

## 4. What the audit did NOT change

For completeness, items NOT requiring tightening:

- The eight decision names and verbs stay as the chain wrote them.
- The implementation order stays sequential as listed (T2→T3→T4→T5→T6→T7), with the parallelization observation as an option not a structural change.
- The completion-condition criteria (a) through (j) all hold without revision.
- No `forge_edit` calls are needed against the chain's `design_decisions` field — every tightening is small enough to live in the relevant per-task acceptance, and T1's job (per its own constraints) is to surface tightenings as guidance for the implementing task, not to rewrite the chain text.

---

## 5. Handoff to T2

Pre-implementation checklist T2 inherits from this audit:

- [ ] `work.batch` accepts generic `{op, params, rationale}` triples, allowlist-filtered to work-surface mutating actions only (Q1 resolution).
- [ ] Envelope-level batch rationale is REQUIRED in addition to per-op rationale (Decision 1 tightening).
- [ ] Unknown-param rejection at dispatch with accepted-list named in the error (silent-drop vault learning).
- [ ] `BatchExecuted` payload schema lands at `blueprints/events/BatchExecuted.json` per the §3.1 sketch.
- [ ] Per-op events emit with `refs.caused_by_event_id = <BatchExecuted event_id>` (`ChainCreated → TaskCreated` precedent).
- [ ] Smoke covers: 3-op happy path; 1-op degenerate; 1-op-failure-mid-batch abort+rollback; 1-op-failure with `continue_on_error=true`; unknown-action rejection; non-allowlisted action rejection.

T3 inherits: composite `TaskHandoff` event in addition to per-op `TaskCompleted`/`TaskStarted` (Decision 2 tightening + §3.2 sketch). T7 inherits the §3.3 ChainAndTasksForged shape and the rooted-at-composite causal graph rule.
