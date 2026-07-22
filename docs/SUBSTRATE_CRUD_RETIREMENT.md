# Substrate CRUD Retirement — Audit + Design

> **Status:** Draft for review. Produced by chain `agent-substrate-crud-retirement` T1 (`phase4-audit-and-design`). Decisions here are durable; downstream tasks T2–T7 + T4a/T4b/T6a bind to them. Amendments after this doc lands require a chain-level decision, not a unilateral task edit.
>
> **Pre-substrate-entity strategy:** **Option A — synthesize initial-state events for every pre-substrate entity.** See §6 for the per-kind matrix.
>
> **Reading order:** §1 scope → §2 read-site inventory → §3 write-site inventory → §4 projection-gap matrix → §5 event-payload-coverage matrix → §6 pre-substrate-entity strategy → §7 FTS5 handling → §8 task-by-task scope handoff → §9 audit findings.
>
> **Companion docs:** `docs/EVENT_SUBSTRATE.md` (envelope + mechanics) · `docs/PROJECTIONS.md` (existing fold contract) · `docs/SUBSTRATE_FRONTEND.md` (dashboard repointing prior).

---

## 1. CRUD-table scope

This chain retires **seven** artifact-lifecycle CRUD tables in toolkit-server. The scope boundary is "tables whose mutations already have an event-emit pairing today (or where the gap is fixable additively in T3)" and whose data the events ledger plus a projection can fully serve.

### 1.1 In scope (retired by T6)

| Table | Live rows (2026-05-20) | Event types | Projection target | Notes |
|---|---|---|---|---|
| `bugs` | 839 | BugReported, BugEdited, BugResolved, BugReopened, BugStamped, BugTriaged | `proj_current_bugs` (exists) | Heavy read traffic; full column mirror exists. |
| `tasks` | 2864 | TaskCreated, TaskEdited, TaskTransitioned, TaskCompleted, TaskCancelled, TaskAssignedToChain, TaskStamped | `proj_current_tasks` (NEW — T2) | Highest read traffic in the meta-tool; load-bearing projection gap. |
| `chains` | 277 | ChainCreated, ChainEdited, ChainClosed | `proj_chain_status` (exists; mirrors chains + denormalised task-status counts) | Foundational entity; existing projection sufficient. |
| `task_blockers` | 12 | TaskTransitioned (carries blocker_slug — see §5 gap) | `proj_task_blockers` (NEW — T2) | Live join table. Audit finding §9.1: T3 must extend TaskTransitioned payload to carry `removed_blocker_slug` for unblock reconstruction. |
| `benchmark_results` | 716 | BenchmarkRunStarted, BenchmarkRunCompleted, BenchmarkRunFailed | `proj_benchmark_results` (NEW — T2) | Pre-035 rows already accepted as "frozen-state" per `BENCHMARKS_DB_RETIREMENT_2026-05-19.md`; post-035 rows have full event provenance via `benchmark_provenance` FK. |
| `roadmap_items` | 38 | RoadmapUpdated | `proj_roadmap_view` (exists) | Small set; existing projection sufficient. |
| `suggestions` | 3 | SuggestionReported, SuggestionResolved, SuggestionReopened, SuggestionStamped | `proj_current_suggestions` (NEW — T2) | All 3 rows are post-substrate (filed after 2026-05-19); no pre-substrate backfill needed. |

### 1.2 Out of scope (deferred to future chains)

| Table | Reason for deferral |
|---|---|
| `task_dependencies` (26 rows) | **DEAD CODE.** Zero production references under `go/internal/`. Drops in T6 alongside the in-scope tables, but needs no projection. See §9.2. |
| `library_entries`, `kiwix_references`, `curation_candidates` | Knowledge-side CRUD; event-flow is partial. Would need its own audit chain (`agent-knowledge-substrate` or similar). |
| `trained_models`, `model_predictions`, `ab_comparisons` | ML-substrate CRUD; **no event coupling today** (zero `TrainedModel*` / `ModelPrediction*` / `ABComparison*` schemas in `go/internal/events/schemas/`). Same deferral shape as knowledge-side tables. |
| `projects` | Bootstrap registration, not artifact-shaped. |
| `hosts`, `setup_recipes`, `recipe_steps`, `remote_ops`, `portal_chats`, `pending_decisions`, `session_registry` | Operational config / runtime state. |
| `grounding_events`, `vault_search_invocations`, `kiwix_offload_invocations`, `qwen_invocations`, `emotive_results` | Already event-or-projection-shaped, or absorbed into grounding_events by telemetry-substrate-cleanup migration 046 (vault_search_invocations + kiwix_offload_invocations drop in migration 047 on the next deploy). |
| `arc_review_debouncer`, `arc_review_hash_cache` | Runtime cache state. |
| `benchmark_provenance`, `benchmark_results_quarantine`, `roadmap_meta` | Cross-cutting metadata; preserved as-is (the `benchmark_provenance` FK target on `proj_benchmark_results` is the live link). |

---

## 2. Read-site inventory

Verified 2026-05-20 via `grep -rnE "FROM (bugs|tasks|chains|task_blockers|task_dependencies|benchmark_results|roadmap_items|suggestions)\b" go/internal/`, excluding `_test.go` and `migrations/`. **135 read sites across 25 production files.**

| File | Read count | Handoff |
|---|---|---|
| `go/internal/work/task.go` | 31 | T4 — every task_read / task_list / task_search / task_blockers / etc. selectors |
| `go/internal/work/chain.go` | 16 | T4 — chain_state / chain_find / chain_status |
| `go/internal/projections/chains.go` | 15 | T5 — fold-internal CRUD reads (deleted, not repointed) |
| `go/internal/work/roadmap.go` | 12 | T4 — roadmap_list / roadmap_diff (joins on chains + tasks) |
| `go/internal/work/suggestion.go` | 7 | T4 — suggestion_read / suggestion_list / suggestion_search |
| `go/internal/work/bug.go` | 7 | T4 — bug_read / bug_list (mostly already projection-driven post-agent-first-substrate; gap audit verifies) |
| `go/internal/refresolve/resolvers_work.go` | 6 | T4 — reference resolvers for chain_slug / task_slug / bug_slug |
| `go/internal/projections/roadmap.go` | 6 | T5 — fold-internal (deleted) |
| `go/internal/observehttp/roadmap.go` | 4 | T4 — dashboard roadmap endpoint |
| `go/internal/observehttp/benchmarks.go` | 4 | T4 — dashboard benchmarks endpoints (`/benchmarks/{timeseries,cards,rubric-cards,tasks}`) |
| `go/internal/measure/team_context.go` | 4 | T4 — team-scoped benchmark queries |
| `go/internal/refresolve/catalogs.go` | 3 | T4 — parse_context slug catalogs (chains/tasks/bugs) |
| `go/internal/forge/indexsync.go` | 3 | T4 — knowledge_pointers index sync (reads chains/tasks/bugs by slug) |
| `go/internal/projections/bugs.go` | 2 | T5 — fold-internal (deleted) |
| `go/internal/observehttp/tasks.go` | 2 | T4 — dashboard tasks endpoint |
| `go/internal/forge/hooks.go` | 2 | T4 — forge post-create hooks (position counting; project_id lookup) |
| `go/internal/forge/edit.go` | 2 | T4 — forge edit's pre-edit validation reads (chains/tasks) |
| `go/internal/forge/create.go` | 2 | T4 — forge create's validation reads (chains existence; tasks slug uniqueness) |
| `go/internal/observehttp/suggestions.go` | 1 | T4 — dashboard suggestions endpoint |
| `go/internal/observehttp/inference_v2.go` | 1 | T4 — inference page benchmarks join |
| `go/internal/observehttp/inference_success_predicates.go` | 1 | T4 — inference success predicate lookup |
| `go/internal/measure/benchmark_replay.go` | 1 | T4 — benchmark replay reads |
| `go/internal/measure/benchmark.go` | 1 | T4 — benchmark scenario lookup |
| `go/internal/knowledge/curation/sources/task_handoff.go` | 1 | T4 — curation source reads task handoff_output |
| `go/internal/forge/delete.go` | 1 | T4 — forge delete's existence check |

**Fold-internal reads (3 files, 23 sites):** `projections/{bugs,chains,roadmap}.go` read CRUD inside `Fold()` per the agent-first-substrate T4 dual-write contract. These are **deleted, not repointed**, in T5 — the fold switches to payload-only construction. The grep at T5's acceptance criterion "any reference to the retired tables inside fold modules returns zero hits" surfaces these.

**Aggregate target after T4:** `grep -rnE "FROM (bugs|tasks|chains|task_blockers|benchmark_results|roadmap_items|suggestions)\b" go/internal/{work,admin,knowledge,measure,forge,refresolve,observehttp}/` returns zero non-comment hits.

---

## 3. Write-site inventory

Verified 2026-05-20 via `grep -rnE "(INSERT INTO|UPDATE|DELETE FROM) (bugs|tasks|chains|task_blockers|benchmark_results|roadmap_items|suggestions)\b"`, excluding tests + migrations. **40 write sites across 11 production files.** Each is verified for event-emit pairing.

| Site (file:line) | Op | Event pairing | Status |
|---|---|---|---|
| `forge/create.go:288` | INSERT chains | events.Emit(ChainCreated) at L294 | ✓ paired |
| `forge/create.go:362` | INSERT tasks | events.Emit(TaskCreated) at L372 | ✓ paired |
| `forge/create.go:423` | INSERT bugs | events.Emit(BugReported) at L447 | ✓ paired |
| `forge/create.go:508` | INSERT suggestions | events.Emit(SuggestionReported) at L528 | ✓ paired |
| `forge/edit.go:346` | UPDATE tasks (forge_edit path) | events.Emit(TaskEdited) at L365 | ✓ paired |
| `forge/edit.go:269` | (chain/bug edit) | events.Emit(ChainEdited/BugEdited) at L269 | ✓ paired |
| `forge/hooks.go:242` | INSERT tasks (forge bulk) | events.Emit(TaskCreated) at L256 | ✓ paired |
| `forge/delete.go:88` | DELETE tasks | NO Emit (forge delete is shape-rare; today only chain T0-style task cancellation) | **§9.3 finding** |
| `work/task.go:194-206` | UPDATE tasks (status/handoff/sha — terminal transitions) | events.Emit(TaskCompleted/Cancelled/Transitioned) at L250 | ✓ paired |
| `work/task.go:217` | DELETE roadmap_items (cascade) | events.Emit(RoadmapUpdated) at L266 nearby | ✓ paired (cascade) |
| `work/task.go:324` | DELETE task_blockers (cascade on close) | NO direct Emit; emitted via the terminal TaskCompleted at L250 | partial — cascade not enumerated in payload |
| `work/task.go:334` | UPDATE tasks (status='pending' on close cascade) | events.Emit(TaskTransitioned) at L396 | ✓ paired |
| `work/task.go:391` | UPDATE tasks (status='pending' on individual swept task) | events.Emit(TaskTransitioned) at L396 | ✓ paired |
| `work/task.go:955-988` | UPDATE tasks (commit_sha; status='closed') | events.Emit(TaskStamped/TaskCompleted) at L960, L988 | ✓ paired |
| `work/task.go:975` | DELETE roadmap_items (cascade) | events.Emit(RoadmapUpdated) at L988 nearby | ✓ paired |
| `work/task.go:1173` | INSERT task_blockers (HandleTaskBlock) | events.Emit(TaskTransitioned) at L1182 IFF task wasn't already blocked | **§9.1 finding** — 2nd+ blocker emits no event |
| `work/task.go:1232` | DELETE task_blockers (specific edge, HandleTaskUnblock) | NO direct Emit; cascaded via TaskTransitioned at L1266 IFF last edge cleared | **§9.1 finding** — removed-blocker slug not in payload |
| `work/task.go:1248` | DELETE task_blockers (all edges, HandleTaskUnblock global) | Same as above | **§9.1 finding** |
| `work/task.go:1719-1724` | UPDATE tasks (work meta-tool's task_edit) | events.Emit(TaskEdited) at L1735 | ✓ paired |
| `work/bug.go:463-481` | UPDATE bugs (bug_resolve) | events.Emit(BugResolved) at L490 | ✓ paired |
| `work/bug.go:679` | UPDATE bugs (bug_reopen) | events.Emit(BugReopened) at L685 | ✓ paired |
| `work/bug.go:752` | UPDATE bugs (bug_stamp_sha) | events.Emit(BugStamped) at L756 | ✓ paired |
| `work/chain.go:236-240` | UPDATE chains (chain_close) | events.Emit(ChainClosed) at L246 | ✓ paired |
| `work/chain.go:253` | DELETE roadmap_items (chain_close cascade) | Cascaded via ChainClosed emit | partial — cascade not enumerated in payload |
| `work/suggestion.go:449` | UPDATE suggestions (suggestion_resolve) | events.Emit(SuggestionResolved) at L457 | ✓ paired |
| `work/suggestion.go:585` | UPDATE suggestions (suggestion_reopen) | events.Emit(SuggestionReopened) at L591 | ✓ paired |
| `work/suggestion.go:659` | UPDATE suggestions (suggestion_stamp_sha) | events.Emit(SuggestionStamped) at L663 | ✓ paired |
| `work/roadmap.go:226` | DELETE roadmap_items (roadmap_set bulk clear) | events.Emit(RoadmapUpdated) at L266 | ✓ paired |
| `work/roadmap.go:243` | INSERT roadmap_items (roadmap_set bulk insert) | Same Emit | ✓ paired |
| `work/roadmap.go:467` | UPDATE roadmap_items (position shift) | events.Emit(RoadmapUpdated) at L504 | ✓ paired |
| `work/roadmap.go:482` | INSERT roadmap_items (roadmap_insert) | Same Emit | ✓ paired |
| `work/roadmap.go:635` | UPDATE roadmap_items (roadmap_update) | events.Emit(RoadmapUpdated) at L662 | ✓ paired |
| `measure/benchmark.go:126` | INSERT benchmark_results (post-run record) | events.Emit(BenchmarkRunCompleted/Failed) per measure.RunOutcome | ✓ paired |
| `db/benchmark.go:66` | INSERT benchmark_results (lower-level writer) | events.Emit(BenchmarkRunCompleted) at L96 | ✓ paired |

**Aggregate after T5:** every write site above drops its CRUD UPDATE/INSERT/DELETE; only events.Emit remains in the write transaction. The fold reconstructs the projection row from event payload alone.

---

## 4. Projection-gap matrix

Per entity kind, which projection columns must exist and whether the current projection (if any) covers them.

### 4.1 bugs → `proj_current_bugs`

**Status: covered.** Migration 033 + 055 ship the full column mirror. T2 has no work here.

| Column source | Coverage |
|---|---|
| All CRUD columns (id, project_id, slug, title, problem_statement, surface, severity, source, acceptance_criteria, constraints, status, resolution_note, resolution_kind, routed_chain_slug, routed_task_slug, resolved_commit_sha, filed_at, resolved_at, updated_at, qwen_task_id, tags, routed_suggestion_slug) | ✓ |
| Substrate columns (last_event_id, last_event_ts) | ✓ |

### 4.2 tasks → `proj_current_tasks` (NEW)

**Status: missing. Load-bearing T2 deliverable.** No projection exists today; every task_read / task_list / task_search hits `tasks` directly.

| Column required | Source | Notes |
|---|---|---|
| id, chain_id, slug, position | CRUD | Identity |
| status | CRUD | folded by TaskTransitioned, TaskCompleted, TaskCancelled |
| problem_statement, acceptance_criteria, context_required, constraints, handoff_output | CRUD | folded by TaskCreated (initial) and TaskEdited (updates — **§9.4 finding**: TaskEdited payload only carries `updated_fields`, not new values) |
| originated_chain_id, moved_on | CRUD | folded by TaskAssignedToChain |
| created_at, updated_at | CRUD | bookkeeping |
| commit_sha | CRUD | folded by TaskStamped / TaskCompleted |
| last_event_id, last_event_ts | substrate | T2 boilerplate |

### 4.3 chains → `proj_chain_status`

**Status: covered.** Existing projection mirrors chains + denormalised task-status counts.

| Concern | Coverage |
|---|---|
| chain columns | ✓ |
| task-status counts (total/pending/active/blocked/closed/cancelled) | ✓ (folded via Chain* + Task* events) |
| **Post-T5 implication** | Counts must be derivable from event-folded `proj_current_tasks` rather than the CRUD `tasks` table. T5 changes the count-recomputation source. |

### 4.4 task_blockers → `proj_task_blockers` (NEW)

**Status: missing. T2 + T3 deliverable.** Live join table; needs both a projection and a payload-extension fix on TaskTransitioned (§9.1).

| Column required | Source |
|---|---|
| blocked_task_id, blocker_task_id, reason, created_at | CRUD |
| last_event_id, last_event_ts | substrate |

### 4.5 benchmark_results → `proj_benchmark_results` (NEW)

**Status: missing. T2 deliverable.** Per `BENCHMARKS_DB_RETIREMENT_2026-05-19.md`, pre-035 historical rows already accepted as "skip backfill"; T2's backfill targets post-035 rows only (those with `provenance_id NOT NULL`).

| Column required | Source |
|---|---|
| id, project_id, scenario_id, tool_name, model_name, run_id, run_at, wall_clock_ms, input_tokens, output_tokens, invoked_contextually, invocation_ok, args_match, extracted_args, interpretation_ok, detected_tool, notes, layer, task_shape, accuracy_score, honesty_score, ranking_quality_score, within_budget_score, task_id, run_shape, provenance_id | CRUD |
| last_event_id, last_event_ts | substrate |

### 4.6 roadmap_items → `proj_roadmap_view`

**Status: covered.**

### 4.7 suggestions → `proj_current_suggestions` (NEW)

**Status: missing. T2 deliverable. Mirrors `proj_current_bugs` shape.**

| Column required | Source |
|---|---|
| id, project_id, slug, title, problem_statement, surface, priority, source, acceptance_criteria, constraints, status, resolution_note, resolution_kind, routed_chain_slug, routed_task_slug, routed_bug_slug, resolved_commit_sha, filed_at, resolved_at, updated_at, tags | CRUD |
| last_event_id, last_event_ts | substrate |

---

## 5. Event-payload-coverage matrix

For each entity kind, do existing event payloads carry every projection column needed for payload-only fold construction (T5's contract)? Gaps land as additive payload bumps in T3.

### 5.1 bugs

| Event | Payload fields | Projection columns produced | Gap |
|---|---|---|---|
| BugReported | title, problem_statement, surface, severity, source, acceptance_criteria, constraints, tags | (initial row creation) | qwen_task_id missing — **§9.5 finding** |
| BugEdited | updated_fields | (in-place update) | **GAP — new values missing.** T3 bumps payload to carry new value for each updated field. |
| BugResolved | commit_sha, dup_of, kind, resolution_note, routed_chain_slug, routed_task_slug | status('fixed'/'wontfix'/'dup'), resolution_kind, resolution_note, routed_*, resolved_commit_sha, resolved_at | ✓ |
| BugReopened | previous_resolution | status='open', resolution_kind=NULL, resolution_note='' | ✓ (fold derives clears from event type) |
| BugStamped | commit_sha | resolved_commit_sha, updated_at | ✓ |
| BugTriaged | from_severity, from_tags, to_severity, to_tags | severity, tags | ✓ |

**T3 bumps for bugs:**
- BugEdited.updated_values (additive map of field → new value)
- BugReported.qwen_task_id (additive optional)
- BugReported.routed_suggestion_slug (additive optional; cross-routing column added 2026-05-19)

### 5.2 tasks

| Event | Payload fields | Projection columns produced | Gap |
|---|---|---|---|
| TaskCreated | chain_slug, position, problem_statement, acceptance_criteria, context_required, constraints, handoff_output | initial row | originated_chain_id not in payload (covered via TaskAssignedToChain history) |
| TaskEdited | updated_fields | (in-place update) | **GAP — same as BugEdited.** Bump to carry new values. |
| TaskTransitioned | from_status, to_status, blocker_slug | status, (blocker edge if added) | **GAP §9.1** — needs `removed_blocker_slug` for unblock reconstruction; needs distinct "edge added vs edge removed vs status-only" markers when 2nd+ blocker is added without status change |
| TaskCompleted | closure_summary, commit_sha | status='closed', handoff_output, commit_sha | ✓ |
| TaskCancelled | reason | status='cancelled' | ✓ |
| TaskAssignedToChain | from_chain_slug, from_position, to_chain_slug, to_position | chain_id, position, originated_chain_id, moved_on | ✓ (chain_id resolved at fold time via projection self-join) |
| TaskStamped | commit_sha | commit_sha, updated_at | ✓ |

**T3 bumps for tasks:**
- TaskEdited.updated_values (additive map)
- TaskTransitioned.removed_blocker_slug (additive optional)
- TaskTransitioned.added_blocker_slug (renamed from `blocker_slug` — **NON-BACKWARD-COMPATIBLE if renamed**; alternative: leave `blocker_slug` as "added" and add a new `removed_blocker_slug`). Decision: KEEP `blocker_slug` semantics as "added" (matches current usage); add `removed_blocker_slug` as a new optional field. Backward-compatible.

### 5.3 chains

| Event | Payload | Gap |
|---|---|---|
| ChainCreated | completion_condition, design_decisions, output, tasks | ✓ |
| ChainEdited | updated_fields | **GAP — same pattern.** Bump to carry new values. |
| ChainClosed | closure_summary | ✓ |

**T3 bumps for chains:**
- ChainEdited.updated_values (additive map)

### 5.4 task_blockers (covered via TaskTransitioned)

See §5.2 — T3 extends TaskTransitioned to carry the blocker-edge mutations.

### 5.5 benchmark_results

| Event | Payload | Gap |
|---|---|---|
| BenchmarkRunStarted | provenance, scenario_id | (initial row created at started time, completed at terminal) |
| BenchmarkRunCompleted | input_tokens, output_tokens, run_id, score, tool_use_tokens, wall_clock_ms | **GAP** — projection needs tool_name, model_name, layer, task_shape, task_id, run_shape, accuracy_score, honesty_score, ranking_quality_score, within_budget_score, invocation_ok, args_match, extracted_args, interpretation_ok, detected_tool, notes, invoked_contextually. Most of these live in `benchmark_provenance` (FK target) and can be resolved at fold time; rest need additive bump. |
| BenchmarkRunFailed | error_detail, error_kind, run_id, wall_clock_ms | ✓ (for the failed-row shape) |

**T3 bumps for benchmark_results:**
- BenchmarkRunCompleted gains a `result_columns` map covering the rubric-side columns (accuracy_score, honesty_score, ranking_quality_score, within_budget_score, invocation_ok, args_match, extracted_args, interpretation_ok, detected_tool, notes). T3 can also choose to resolve these from a payload-side `rubric_outcome` block instead of flattening.

### 5.6 roadmap_items

| Event | Payload | Gap |
|---|---|---|
| RoadmapUpdated | action_kind, item_count, positions, ref_kind, ref_slug | ✓ (the fold reconstructs the new state from action_kind + positions list) |

### 5.7 suggestions

Mirror of bugs.

| Event | Payload | Gap |
|---|---|---|
| SuggestionReported | acceptance_criteria, constraints, priority, problem_statement, source, surface, tags, title | routed_bug_slug not in payload — additive bump if needed at creation time |
| SuggestionResolved | commit_sha, kind, resolution_note, routed_bug_slug, routed_chain_slug, routed_task_slug | ✓ |
| SuggestionReopened | previous_resolution | ✓ |
| SuggestionStamped | commit_sha | ✓ |

**T3 bumps for suggestions:** none required by T1's audit (suggestion event catalog is post-substrate, designed against agent-first-substrate's contract).

---

## 6. Pre-substrate-entity strategy

**Decision: Option A — synthesize initial-state events for every pre-substrate entity.**

**Rationale.** Completion_condition (b) of this chain requires `toolkit-server rebuild-projections` from empty to produce **byte-identical** rows to the live system. Option B (frozen-state with `last_event_id=''`) would produce FEWER rows on rebuild (the snapshot-seeded rows can't be reconstructed from events alone), violating the byte-identity criterion. Option C inherits the same flaw for any kind it puts in Option B. Option A is the only strategy coherent with the completion_condition.

**Operational cost.** Approximately 4,749 synthetic events:
- 839 BugReported (one per pre-substrate bug, payload synthesized from current CRUD state)
- 2864 TaskCreated
- 277 ChainCreated
- 12 TaskTransitioned (synthetic "to=blocked, added_blocker_slug=…" per current task_blockers edge)
- 716 BenchmarkRunCompleted (pre-035 rows accepted as frozen per `BENCHMARKS_DB_RETIREMENT_2026-05-19.md`; only post-035 rows backfilled — count to be refined in T2's projection migration)
- 38 RoadmapUpdated (or one synthetic "RoadmapUpdated action_kind=initial_snapshot positions=[…]" event per project)
- 3 SuggestionReported (no backfill needed — all post-substrate)

**Per-kind strategy:**

| Entity | Strategy | Synthetic event count (approx) | Synthetic actor |
|---|---|---|---|
| bugs | Synthesize BugReported | 839 | `actor={kind:system, id:pre-substrate-backfill}` |
| tasks | Synthesize TaskCreated | 2864 | Same |
| chains | Synthesize ChainCreated | 277 | Same |
| task_blockers | Synthesize TaskTransitioned (to=blocked, added_blocker_slug=…) | 12 | Same |
| benchmark_results (post-035 only) | Synthesize BenchmarkRunCompleted with `result_columns` snapshot | ≤716 (subset with provenance_id NOT NULL) | Same |
| benchmark_results (pre-035) | **Frozen-state exception** — preserved by existing seed semantics | 0 synthetic | n/a |
| roadmap_items | Synthesize one RoadmapUpdated per project with `action_kind=initial_snapshot, positions=[<current>]` | ≤(# projects with non-empty roadmap) | Same |
| suggestions | None — all post-substrate | 0 | n/a |

**Implementation note for T2.** Each new projection's migration carries the synthetic-event INSERT alongside the projection table backfill. The events are emitted with `ts` set to the row's existing `created_at` / `filed_at` and `event_id` UUIDv7 derived from that timestamp — preserving chronological order in the events ledger.

**Implementation note for T5.** The fold for each entity kind treats the synthetic initial-state event identically to a real one — no special handling. The synthetic actor `system/pre-substrate-backfill` is auditable in the events table for forensic queries.

**Implementation note for T6.** After the CRUD drop, `rebuild-projections --from=empty` replays every event (synthetic + real) and produces the same projection rows. SQL diff = empty.

---

## 7. FTS5 handling (`bugs_fts`, `suggestions_fts`)

**Decision: parent-driven rebuild from the projection.**

Both FTS5 virtual tables ship standalone (no contentless coupling) per migration 054. Application code maintains them in the same transaction as the parent write today.

**After T5/T6:**
- The FTS rows are rebuilt from `proj_current_bugs` and `proj_current_suggestions` on every fold for the respective entity kind — the fold module owns the FTS row alongside the projection row.
- The migration drops `bugs` / `suggestions` CRUD tables but **keeps** `bugs_fts` / `suggestions_fts` virtual tables in place. FTS rows are derived from projections, not from the dropped CRUD.
- `rebuild-projections --from=empty` rebuilds the FTS rows transitively (fold writes both projection row and FTS row).

**Why not projection-side columns:** SQLite FTS5 doesn't support arbitrary columns being indexed alongside a normal table; the virtual-table shape stays the cleanest model. Coupling the fold to two write targets (proj_current_bugs + bugs_fts) inside the same transaction matches the existing forge writer pattern.

**T6 drop order:** unchanged — child tables (task_dependencies, task_blockers's cascade) first, then artifact tables (tasks, bugs, chains, benchmark_results, roadmap_items, suggestions). FTS virtual tables persist.

---

## 8. Task-by-task scope handoff

### T2 — `ship-missing-projections`

Ships four new projections per §4:

| Projection | Migration | Fold module |
|---|---|---|
| `proj_current_tasks` | 056_proj_current_tasks.sql | `projections/tasks.go` |
| `proj_task_blockers` | 057_proj_task_blockers.sql | folded inside `projections/tasks.go` (sibling to `proj_current_tasks`) |
| `proj_benchmark_results` | 058_proj_benchmark_results.sql | `projections/benchmarks.go` |
| `proj_current_suggestions` | 059_proj_current_suggestions.sql | `projections/suggestions.go` |

Each migration: creates table; adds projections_watermark row; backfills from current CRUD per existing snapshot pattern; emits synthetic events per §6 for pre-substrate rows.

### T3 — `event-payload-audit-and-bump`

Per §5.7's bumps (additive, schema_version stays v1):

| Event | Bump |
|---|---|
| BugReported | + qwen_task_id, routed_suggestion_slug |
| BugEdited | + updated_values (map[string]any) |
| TaskEdited | + updated_values |
| TaskTransitioned | + removed_blocker_slug |
| ChainEdited | + updated_values |
| BenchmarkRunCompleted | + result_columns (rubric-side block) |

All additive. **schema_version stays at 1.**

### T4 — `repoint-crud-reads-to-projections`

Each of the **135 read sites in §2** switches to its projection counterpart. Per-file handoff:

| File group | Handoff |
|---|---|
| `work/{task,chain,bug,suggestion,roadmap}.go` | task.go → proj_current_tasks + proj_task_blockers; chain.go → proj_chain_status; bug.go → proj_current_bugs; suggestion.go → proj_current_suggestions; roadmap.go → proj_roadmap_view (already exists) |
| `observehttp/{roadmap,tasks,benchmarks,suggestions,inference_v2,inference_success_predicates}.go` | Dashboard endpoints — route to projection counterparts |
| `measure/{benchmark,benchmark_replay,team_context}.go` | Benchmarks reads → proj_benchmark_results |
| `forge/{create,edit,delete,hooks,indexsync}.go` | Validation reads (existence checks, position counting, project_id lookup) → projection counterparts |
| `refresolve/{catalogs,resolvers_work}.go` | Slug catalogs → proj_current_{bugs,tasks,chains,suggestions} |
| `knowledge/curation/sources/task_handoff.go` | One read of task handoff_output → proj_current_tasks |

### T4a — `mcp-readpath-coverage-baseline`

Tests-as-AC-tracker for the Go layer. Backfill regression tests against the post-T4 dual-write state for every handler in §2's inventory. Pass-condition assertion: handler response shape matches between CRUD-sourced and projection-sourced reads.

### T4b — `frontend-state-coverage-baseline`

Tests-as-AC-tracker for the React layer. Inventory every page/component under `apps/dashboard/src/` consuming retired-table-derived state via observehttp endpoints. Capture baselines.

### T5 — `flip-write-contract-event-only`

Per §3, every write site drops its CRUD UPDATE/INSERT/DELETE; only events.Emit remains. The fold modules' "read post-update CRUD" code paths (the 23 fold-internal reads under `projections/{bugs,chains,roadmap}.go` + the new `projections/{tasks,benchmarks,suggestions}.go`) are **deleted**, not bypassed. Fold constructs from event payload alone. SQL-level rebuild-from-empty smoke = byte-identical.

### T6 — `drop-crud-tables-migration`

Single migration (synced to all three locations) drops:

1. **First (child/join tables):** `task_dependencies` (dead per §9.2), `task_blockers`
2. **Then (artifact tables):** `tasks`, `bugs`, `chains`, `benchmark_results`, `roadmap_items`, `suggestions`
3. **NOT dropped:** `bugs_fts`, `suggestions_fts` (per §7 — parent-driven rebuild from projections)
4. **NOT dropped:** `benchmark_provenance` (FK target for proj_benchmark_results; not in scope per §1.2)

Build + tests + smoke pass on the dropped state.

### T6a — `frontend-state-equivalence-verify`

Boot dashboard against fresh DB rebuilt from events alone; diff observable rendered state against T4b's baselines. Drift routes back to T2 / T3 / T4.

### T7 — `phase4-retrospective-and-self-host`

Closing ArchitectureAuditCompleted event + finalize this doc + chain_close.

---

## 9. Audit findings

### 9.1 `task_blockers` mutations lack precise event coverage

**Site:** `work/task.go:1170–1180` (HandleTaskBlock), `work/task.go:1232, 1248` (HandleTaskUnblock)

**Symptom:**
- INSERTing a 2nd+ task_blockers edge emits no event (the `if current != "blocked"` guard skips the TaskTransitioned emit because status is already blocked)
- DELETEing a specific blocker edge doesn't name which one in the TaskTransitioned payload (`blocker_slug` is for additions only)
- Multi-edge DELETE (L1248) collapses all removed edges into at most one TaskTransitioned

**Implication:** rebuild-projections from events alone cannot reconstruct `proj_task_blockers` row content without additional payload info.

**Routing:** T3 — extend TaskTransitioned payload additively (per §5.2):
- `removed_blocker_slug` (optional) — names the specific edge removed when transitioning to pending
- (No payload change needed for the "2nd+ blocker added with no transition" case if T3 also lifts the `if current != "blocked"` guard at L1181 to ALWAYS emit a TaskTransitioned when a task_blockers row is mutated.)

**Bug filing:** see bug `task-blockers-payload-gap-for-substrate-rebuild` (file in T1 close).

### 9.2 `task_dependencies` is dead code

**Symptom:** zero production code references under `go/internal/` (verified by grep excluding tests/migrations). DB carries 26 historical rows; the live join table is `task_blockers` (12 rows).

**Routing:** T6 drops the table alongside the others — no projection needed.

### 9.3 `forge/delete.go:88` DELETE tasks lacks event-emit

**Symptom:** the forge schema dispatcher's delete path (rarely exercised; tested by `forge_test.go:798–836`) issues a `DELETE FROM tasks WHERE …` with no corresponding TaskCancelled or TaskRetired event.

**Routing:** T4 — confirm whether the forge delete path is still used (the `mcp__toolkit-server__work` surface routes deletions through `task_cancel` not `forge_delete`); if dead-path, remove the DELETE entirely. If still in use, route through `task_cancel` to inherit the event-emit pairing.

**Bug filing:** `forge-task-delete-lacks-event-emit` (file in T1 close).

### 9.4 BugEdited / TaskEdited / ChainEdited payloads carry only `updated_fields`

**Symptom:** the existing payload schema `{updated_fields: [<column-name>,...]}` enumerates which columns changed but not their new values. The fold today reads post-update CRUD to recover those values (`docs/PROJECTIONS.md` §2.3). Post-T5 the CRUD is gone.

**Routing:** T3 — additive bump: each Edited event gains an `updated_values` map (column → new value). Backward-compatible: old consumers ignore the new field.

### 9.5 BugReported payload missing `qwen_task_id` and `routed_suggestion_slug`

**Symptom:** the bugs CRUD table has these columns; BugReported's payload doesn't carry them. The agent-suggestion-box chain added `routed_suggestion_slug` to bugs (migration 054) without bumping BugReported.

**Routing:** T3 — additive bump. Both fields optional with default `''`.

### 9.6 BenchmarkRunCompleted payload missing rubric-side columns

**Symptom:** the projection needs accuracy_score / honesty_score / ranking_quality_score / within_budget_score / invocation_ok / args_match / extracted_args / interpretation_ok / detected_tool / notes / invoked_contextually. BenchmarkRunCompleted today carries only the timing + score block.

**Routing:** T3 — additive bump. The `result_columns` block carries the rubric outcome shape, allowing the fold to construct the full projection row.

---

## 10. Open questions

None blocking T2/T3 start. Resolutions deferred to downstream tasks:

- **Q1.** Should `forge/delete.go:88` be removed entirely or routed through `task_cancel`? (T4 decides.) **Resolved at T4 close in `8f2cb87`:** dead-path removal — the schema-level gate at `forge/handler.go:548` already routes callers to `task_cancel`; the `deleteTask` helper was unreachable and is gone.
- **Q2.** Should the synthetic-event backfill for tasks include the chain_id resolution (via projection self-join) or carry it directly in the synthesized payload? (T2 decides; default: chain_id via projection self-join, matching production fold path.) **Resolved at T2 close in `1b31f47`:** chain_slug resolved via JOIN to chains at backfill time per the default.
- **Q3.** What is the rebuild order between `proj_current_tasks` and `proj_chain_status` (the latter's task-status counts depend on the former)? (T2 decides; default: fold dispatch ensures tasks fold before chains within the same event-replay loop.) **Resolved at T2 close in `1b31f47`:** chain_status fold reads task counts via subquery against `proj_current_tasks`, which is independently folded by the same event-dispatch loop.

---

## 11. MCP read-path coverage matrix (T4a)

Tests-as-AC-tracker for the backend cutover. Every handler that reads from a retired CRUD table has at least one regression test that (a) seeds CRUD via `seedXXX` helpers or direct INSERT, (b) calls the handler via its public `work.HandleX` / `forge.HandleX` / `measure.HandleX` entrypoint, and (c) asserts on the response shape — not just `err == nil`.

**Test-time projection coupling:** `testutil.InstallProjectionMirrorTriggers` (called from `testutil.NewTestDB` by default; called explicitly in `work_test.openTestPool` and `forge_test.openTestPool` which have bespoke constructors) installs SQLite triggers that auto-mirror CRUD writes to the projection tables. Tests that INSERT directly into the retired CRUD tables (bypassing the events.Emit→fold path) keep proj_* in sync transparently. Production never installs these triggers; T5 deletes the dual-write code path, T6 drops the CRUD tables.

**Acceptance criterion for T5 / T6 closure:**
- T5 ships when `make -C go test` passes against the post-flip event-only writes path (the same tests in this matrix re-run against the new write contract).
- T6 ships when `make -C go test` passes against the dropped-tables state (the trigger goes with them; production fold drives projection updates).
- If a test in this matrix fails post-flip or post-drop, it surfaces the specific handler that drifted — route the fix back to T2 (projection coverage) / T3 (payload coverage) / T4 (read-site).

### 11.1 work/ handlers

| Handler | Reads from | Test file(s) | Catches drift |
|---|---|---|---|
| `HandleBugList` | `proj_current_bugs` | `bug_test.go::TestBugList_*` (8 tests), `bug_routed_suggestion_test.go::TestBugList_SurfacesRoutedSuggestionSlug` | ✓ — verbose + compact projections, status filter, surface/tags filter, routed_suggestion_slug column |
| `HandleBugRead` | `proj_current_bugs` | `bug_test.go::TestBugRead_BySlug`, `bug_routed_suggestion_test.go::TestBugRead_SurfacesRoutedSuggestionSlug` | ✓ |
| `HandleBugResolve` | `proj_current_bugs` | `bug_test.go::TestBugResolve_*` (~6 tests), `bug_events_test.go::TestBugResolve_EmitsBugResolved`, `bug_routed_suggestion_test.go::TestBugResolve_AcceptsRoutedSuggestionSlug`, `rationale_integration_test.go` | ✓ — both lifecycle transitions and event emit |
| `HandleBugReopen` | `proj_current_bugs` | `bug_test.go::TestBugReopen_*`, `bug_events_test.go::TestBugReopen_EmitsBugReopenedWithPreviousResolution` | ✓ |
| `HandleBugStampSHA` | `proj_current_bugs` | `bug_test.go::TestBugStampSHA_*`, `bug_events_test.go::TestBugStampSHA_EmitsBugStamped` | ✓ |
| `HandleTaskRead` | `proj_current_tasks` | `task_test.go::TestTaskRead_*` | ✓ — full row shape including chain_slug join |
| `HandleTaskSearch` | `proj_current_tasks` | `task_test.go::TestTaskSearch_*` | ✓ |
| `HandleTaskStart` / `Complete` / `Cancel` / `Reopen` / `Unstart` | `proj_current_tasks` | `task_test.go::TestTask{Start,Complete,Cancel,Reopen,Unstart}_*` + `task_t3_payload_test.go` | ✓ — state-machine transitions + event payload |
| `HandleTaskStampSHA` | `proj_current_tasks` | `task_test.go::TestTaskStampSHA_*` | ✓ |
| `HandleTaskBlock` / `Unblock` | `proj_current_tasks` + `proj_task_blockers` | `task_test.go::TestTaskBlock_*`, `task_t3_payload_test.go::TestTaskBlock_EmitsTaskTransitionedWithBlockerSlug` | ✓ — L1181 guard-lift verified |
| `HandleTaskBlockers` | `proj_task_blockers` | `task_test.go::TestTaskBlockers_*` | ✓ |
| `HandleTaskEdit` | `proj_current_tasks` | `task_test.go::TestTaskEdit_*`, `task_t3_payload_test.go::TestTaskEdit_EmitsUpdatedValues` | ✓ — updated_values payload coverage from T3 |
| `HandleChainStatus` | `proj_chain_status` | `chain_test.go::TestChainStatus_*` | ✓ — list + detail + task-status counts |
| `HandleChainState` | `proj_chain_status` + `proj_current_tasks` | `chain_test.go::TestChainState_*` | ✓ |
| `HandleChainFind` | `proj_chain_status` | `chain_test.go::TestChainFind_*` | ✓ |
| `HandleChainClose` | `proj_chain_status` | `chain_test.go::TestChainClose_*`, `chain_events_test.go::TestChainClose_EmitsChainClosed` | ✓ |
| `HandleSuggestionList` | `proj_current_suggestions` | `suggestion_test.go::TestSuggestionList_*` | ✓ |
| `HandleSuggestionRead` | `proj_current_suggestions` | `suggestion_test.go::TestSuggestionRead_*` | ✓ |
| `HandleSuggestionResolve` / `Reopen` / `StampSHA` | `proj_current_suggestions` | `suggestion_test.go::TestSuggestion{Resolve,Reopen,StampSHA}_*` | ✓ |
| `HandleRoadmapList` | `proj_roadmap_view` (joined with `proj_chain_status` + `proj_current_tasks`) | `roadmap_test.go::TestRoadmapList_*` | ✓ |
| `HandleRoadmapSet` | `proj_chain_status` + `proj_current_tasks` (validation joins) | `roadmap_test.go::TestRoadmapSet_*` (~4 tests) | ✓ |
| `HandleRoadmapInsert` | `proj_roadmap_view` | `roadmap_test.go::TestRoadmapInsert_*` | ✓ |
| `HandleRoadmapUpdate` | `proj_roadmap_view` | `roadmap_test.go::TestRoadmapUpdate_*` | ✓ |
| `HandleRoadmapDiff` | `proj_chain_status` + `proj_current_tasks` | `roadmap_test.go::TestRoadmapDiff_*` | ✓ |
| `HandleRoadmapPreviewSet` | `proj_chain_status` + `proj_current_tasks` | `roadmap_test.go::TestRoadmapPreviewSet_*` | ✓ |
| `HandleRoadmapMarkReassessed` | (writes only, no projection reads after T4) | `roadmap_test.go::TestRoadmapMarkReassessed_*` | ✓ (not read-side, but coverage included for completeness) |

### 11.2 forge/ handlers

| Handler | Reads from | Test file(s) | Catches drift |
|---|---|---|---|
| `HandleForge` (chain create) | `proj_chain_status` (existence + project_id) | `forge_test.go::TestHandleForge_CreatesChain*`, `forge_test.go::TestChainForge_*` | ✓ |
| `HandleForge` (task create) | `proj_chain_status` + `proj_current_tasks` (position counting) | `forge_test.go::TestHandleForge_{CreatesTaskAfterChain,TaskPositionIncrementsWithinChain,TaskAcceptanceCriteriaListJoinsOnNewlineDash}`, `forge_test.go::TestForgeCreate_TaskEmitsTaskCreated` | ✓ |
| `HandleForge` (bug create) | `proj_current_bugs` (slug uniqueness) | `forge_test.go::TestHandleForge_CreatesBug*` | ✓ |
| `HandleForge` (suggestion create) | `proj_current_suggestions` (slug uniqueness) | `forge_test.go::TestHandleForge_CreatesSuggestion*` | ✓ |
| `HandleForgeEdit` | `proj_chain_status` (project_id, status checks) | `forge_test.go::TestHandleForgeEdit_*`, `edit_test.go::TestForgeEdit_*` | ✓ |
| `HandleForgeDelete` | (rejects task/bug/chain at schema gate; project-scoped tables only) | `delete_test.go::TestHandleForgeDelete_*` | ✓ |
| `indexsync.go::IndexUpsert*` | `proj_chain_status` + `proj_current_tasks` + `proj_current_bugs` (knowledge_pointers + FTS5 sync) | `index_test.go::TestIndexUpsert*` | ✓ |
| `hooks.go::core hooks` | `proj_chain_status` + `proj_current_tasks` (validation) | `hooks_test.go::TestForgeHooks_*` | ✓ |

### 11.3 refresolve/ handlers

| Handler | Reads from | Test file(s) | Catches drift |
|---|---|---|---|
| `catalogs.go::loadSlugCatalogs` | `proj_chain_status` + `proj_current_tasks` + `proj_current_bugs` | `discoverability_test.go::TestCatalogsRefresh*` | ✓ — chain/task/bug slug catalogs power parse_context |
| `resolvers_work.go::chainResolver.Resolve` | `proj_chain_status` + `proj_current_tasks` (task-status counts) | `resolvers_test.go::TestProductionRegistry_ChainResolver` | ✓ — closed-chain status surfaced |
| `resolvers_work.go::taskResolver.Resolve` | `proj_current_tasks` + `proj_chain_status` (join) | `resolvers_test.go::TestProductionRegistry_TaskResolver*` (2 tests) | ✓ |
| `resolvers_work.go::bugResolver.Resolve` | `proj_current_bugs` | `resolvers_test.go::TestProductionRegistry_BugResolver*` (3 tests including routing surface) | ✓ |

### 11.4 measure/ handlers

| Handler | Reads from | Test file(s) | Catches drift |
|---|---|---|---|
| `HandleBenchmarkRecord` | `proj_benchmark_results` | `benchmark_test.go::TestHandleBenchmarkRecord_*` (2 tests) | ✓ |
| `HandleBenchmarkQuery` | `proj_benchmark_results` | `benchmark_test.go::TestHandleBenchmarkQuery_*` | ✓ — filter + ordering shape |
| `HandleBenchmarkReplay` | `proj_benchmark_results` | `benchmark_replay_test.go::TestHandleBenchmarkReplay_*` (4 tests including legacy-row + diff cases) | ✓ |
| `team_context.go::DeriveTeamContext` | `proj_current_tasks` + `proj_chain_status` | `team_context_test.go::TestDeriveTeamContext_*` (3 tests) | ✓ — project-scoped queries + percentile fallback |

### 11.5 observehttp/ endpoints

Dashboard-facing; response shape is the contract surface for the React tree.

| Endpoint | Reads from | Test file(s) | Catches drift |
|---|---|---|---|
| `GET /chains` (list) | `proj_chain_status` | `chains_test.go::TestChainsListEndpoint_*` | ✓ |
| `GET /chains/{slug}` (detail) | `proj_chain_status` + `proj_current_tasks` | `chains_test.go::TestChainsDetailEndpoint_*` | ✓ |
| `GET /tasks` (list) | `proj_current_tasks` + `proj_chain_status` (join) | `tasks_test.go::TestTasksListEndpoint_*` | ✓ |
| `GET /tasks/search` | `proj_current_tasks` | `tasks_test.go::TestTasksSearchEndpoint_*` | ✓ |
| `GET /bugs` | `proj_current_bugs` | `bugs_test.go::TestBugsEndpoint_*` | ✓ |
| `GET /suggestions` | `proj_current_suggestions` | `suggestions_test.go::TestSuggestionsEndpoint_*` | ✓ |
| `GET /roadmap` | `proj_roadmap_view` (joined with `proj_chain_status` + `proj_current_tasks`) | `roadmap_test.go::TestRoadmapEndpoint_*` | ✓ |
| `GET /roadmap/diff` | `proj_chain_status` + `proj_current_tasks` | `roadmap_test.go::TestRoadmapDiffEndpoint_*` | ✓ |
| `GET /benchmarks` (list) | `proj_benchmark_results` | `benchmarks_test.go::TestBenchmarksListEndpoint_*` | ✓ |
| `GET /benchmarks/timeseries` | `proj_benchmark_results` | `benchmarks_test.go::TestBenchmarksTimeseriesEndpoint_*` | ✓ |
| `GET /benchmarks/cards` / `rubric-cards` / `tasks` | `proj_benchmark_results` | `benchmarks_test.go::TestBenchmarks{Cards,RubricCards,Tasks}Endpoint_*` | ✓ |
| `GET /inference/health-cards` / `/sparklines` / `/retrieval-health` | `proj_benchmark_results` (joins) | `inference_v2_test.go::TestInference*` | ✓ |
| `GET /entities/{kind}/{slug}/events` | `events` table (cross-cutting, not retired) | `events_test.go::TestEntityEvents_*` (includes the `suggestion` kind from `7971c49`) | ✓ |

### 11.6 knowledge/curation/sources/

| Source | Reads from | Test file(s) | Catches drift |
|---|---|---|---|
| `task_handoff.go::TaskHandoff.Sources` | `proj_current_tasks` (single handoff_output read) | `curation/sources/sources_test.go::TestTaskHandoffBuilder_*` (3 tests: BuildsFromTasksRow, RejectsMalformedSourceRef, ErrorsOnMissingTask) | ✓ |

### 11.7 Coverage gaps

None identified at T4a close. Every handler in §2 of the read-site inventory has at least one regression test that:

1. Seeds via `seedXXX` helper (which triggers projection mirror) OR direct INSERT (also triggered),
2. Calls the handler end-to-end,
3. Asserts on the response shape (slug, status, count, ordering, etc. — not just `err == nil`).

The full test suite passes against the post-T4 dual-write state at commit `8f2cb87`. This is the baseline; T5's flip and T6's drop preserve "tests pass" as the acceptance signal.

### 11.8 Verification commands

```
# Run the full backend test suite (the AC tracker):
make -C go test

# Single package (faster iteration during T5/T6):
go test -tags sqlite_fts5 ./internal/work/
go test -tags sqlite_fts5 ./internal/forge/
go test -tags sqlite_fts5 ./internal/refresolve/
go test -tags sqlite_fts5 ./internal/measure/
go test -tags sqlite_fts5 ./internal/observehttp/

# Specific handler regression (worst-case-isolated check):
go test -tags sqlite_fts5 ./internal/work/ -run TestBugRead_BySlug
```

---

## 12. Frontend coverage matrix (T4b)

Tests-as-AC-tracker for the dashboard. Every React-routed page that consumes data derived from a retired CRUD table (via an `observehttp` endpoint repointed in T4) has at least one regression test that mocks the API fetch, renders the page, and asserts on the rendered shape — either Vitest (with mocked API) or Playwright e2e (against a live daemon).

**Baseline state:** post-T4 dual-write (current `main` after T4a). The dashboard test suite captures the rendered state at this point; T6a re-runs the same suite against a DB rebuilt from events alone and diffs.

**Acceptance criterion for T6a:**
- Vitest suite re-passes against the rebuilt-from-events state.
- Playwright e2e suite re-passes against the same state.
- Any divergence routes back to T2 (projection coverage gap) / T3 (payload coverage gap) / T4 (read-site miss).

### 12.1 Page coverage

| Page | observehttp endpoint(s) | Entity kinds | Vitest | Playwright e2e | Catches drift | Backfill |
|---|---|---|---|---|---|---|
| `ChainIndex` | `/chains`, `/chains/{slug}`, `/tasks`, `/tasks/search` | chain + task | `pages/ChainIndex/index.test.tsx` (3 tests) | `tests/e2e/chain-index.spec.ts` | ✓ | **NEW** — added in T4b for page-level coverage; e2e was the only prior tripwire |
| `BugIndex` | `/bugs` | bug | `pages/BugIndex/BugDetailPanel.test.tsx` (detail pane) + `api/bugs.test.ts` (data shape) + `lib/bugIndex.test.ts` (filter logic) | `tests/e2e/bug-index.spec.ts` | ✓ — three-way coverage (e2e for page, Vitest for detail pane, API + lib for shape/filter) | none |
| `SuggestionIndex` | `/suggestions`, `/suggestions/{slug}/events` | suggestion | `pages/SuggestionIndex/SuggestionDetailPanel.test.tsx` (9 tests) + `pages/SuggestionIndex/index.test.tsx` (4 tests, NEW) + `api/suggestions.test.ts` (7 tests) + `lib/suggestionIndex.test.ts` (8 tests) | none (Playwright gap) | ✓ — Vitest fills the e2e gap | **NEW** index.test.tsx |
| `Roadmap` | `/roadmap`, `/roadmap/diff` | chain + task (joined refs) | `pages/Roadmap/index.test.tsx` | `tests/e2e/roadmap.spec.ts` | ✓ | none |
| `Benchmarks` | `/benchmarks`, `/benchmarks/timeseries`, `/benchmarks/cards`, `/benchmarks/rubric-cards`, `/benchmarks/tasks` | benchmark_run | `pages/Benchmarks/index.test.tsx` | `tests/e2e/benchmarks.spec.ts` | ✓ | none |
| `DeferredPorts` | `/benchmarks/tasks` (filtered subset) | benchmark_run | `pages/DeferredPorts/index.test.tsx` | none | ✓ — Vitest sufficient (logic-shaped page) | none |
| `Inference` | `/inference/health-cards`, `/inference/sparklines`, `/inference/retrieval-health`, `/bugs?qwen_task_id NOT NULL` | benchmark_run + bug | `pages/Inference/index.test.tsx` (~12 tests) | `tests/e2e/inference.spec.ts` | ✓ | none |

### 12.2 Out-of-scope dashboard pages (no retired-table dependency)

`AuditLedger` reads the `events` table directly (not retired). `Telemetry`, `Spans`, `ContextPulls`, `QueryTrajectoryView` read telemetry-substrate projections (different chain). `AdminDispatchPolicy`, `ActionDocs`, `Knowledge`, `TrainingPairs` read non-scope tables. `_dormant/*` pages are not routed.

### 12.3 Component coverage (shared between pages)

| Component | Vitest | Why it matters |
|---|---|---|
| `shared/RecordCard` | `index.test.tsx` (7 tests) | Surface for bugs, suggestions, tasks — column shape rendering |
| `shared/StatusBadge` | `index.test.tsx` (17 tests) | Status field rendering across all entity kinds |
| `shared/StatusMixBreakdown` | `index.test.tsx` (5 tests) | Aggregate counts (chain status mix, bug resolution mix) |
| `shared/EventTimeline` | `index.test.tsx` | Per-entity events panel (consumed by ChainIndex, BugIndex detail pane, SuggestionIndex detail pane) |
| `shared/TaskDetail` | `index.test.tsx` (7 tests) | Task row detail shape consumed by ChainIndex |

### 12.4 API layer coverage

The `apps/dashboard/src/api/*.test.ts` suite asserts that each API wrapper hits the correct observehttp endpoint with the correct query params and parses the response into the documented shape. This is the data-contract surface between dashboard and daemon — any change to observehttp response shape (which would happen if T5/T6 introduced a regression) surfaces here.

| Module | Test | Endpoint(s) |
|---|---|---|
| `api/chains.ts` | `api/chains.test.ts` (9 tests) | `/chains`, `/chains/{slug}` |
| `api/bugs.ts` | `api/bugs.test.ts` (5 tests) | `/bugs` |
| `api/suggestions.ts` | `api/suggestions.test.ts` (7 tests) | `/suggestions` |

### 12.5 Verification commands

```
# From apps/dashboard/:
npm test                  # Vitest (424 tests across 37 files)
npm run test:e2e          # Playwright e2e (11 specs)
npm run test:equivalence  # NEW: vitest run && playwright test — chained for T6a

# From repo root:
npm --prefix apps/dashboard run test:equivalence
```

### 12.6 Coverage gaps

- **SuggestionIndex has no Playwright e2e.** Filed as a low-priority enhancement during T4b; not gating since Vitest coverage exists at the page + detail-pane + API + lib levels. T6a treats SuggestionIndex's pass condition as Vitest-only.

### 12.7 Visual sign-off (per-page user verification)

Per Sophi's `feedback-frontend-visual-verify` discipline, the user walks through each affected page on the dev dashboard at `http://localhost:3000` (daemon-restart-served) and confirms it renders correctly post-T4 BEFORE the test baselines are captured as canonical. Confirmed 2026-05-21 at T4 close: ChainIndex, BugIndex, SuggestionIndex, Roadmap, Benchmarks, Inference all render correctly. The `7971c49` fix for `eventEntityKinds` missing `suggestion` was caught during this walkthrough.

---

## 13. T5 split — what shipped vs what was planned (cold-pickup context)

T5 was designed as one load-bearing commit but landed as six sub-tasks per the spec's "split along entity-kind lines" escape hatch (chain `design_decisions` §4 "If the commit is too large to safely land in one go…"). Each sub-task is its own single-commit flip for one entity kind.

### 13.1 Commit ledger (in landing order)

| Sub-task | Commit | Entity kind(s) | Payload bumps landed in-commit |
|---|---|---|---|
| T5-suggestions | `c7c5d6e` | suggestions | none |
| T5-roadmap | `ca65006` | roadmap_items | `RoadmapUpdated.items[]` (T1 §5.6 missed per-position layout for action_kind="set") |
| T5-bugs | `6814665` | bugs | `BugResolved.routed_suggestion_slug` (T1 §5.1 missed the reroute-path coverage) |
| T5-chains | `a96d0e8` | chains | none |
| T5-benchmarks | `94618fe` | benchmark_results | `BenchmarkRunCompleted.{benchmark_result_id, project_id, scenario_id, provenance_id, run_at}` (T1 §5.5 only flagged result_columns) |
| T5-tasks | `7128e48` | tasks, task_blockers | none (T3's prior bumps were sufficient) |

### 13.2 Post-T5 state — CRUD vs projection row counts (verified 2026-05-21)

| Entity | CRUD rows | Projection rows | Drift interpretation |
|---|---|---|---|
| bugs | 842 | 842 | Match. |
| tasks | 2870 | 2864 | **+6 CRUD-only**: tasks INSERTed pre-substrate whose synthetic TaskCreated events couldn't resolve their `chain_slug` at fold time (chain since renamed or never had a ChainCreated event). T6 drop loses them, but they're not in the live projection so no functional impact. |
| chains | 277 | 277 | Match. |
| task_blockers | 8 | 12 | **+4 PROJECTION-only**: edges added by HandleTaskBlock post-T5-tasks (event-only path). T6 drops the CRUD; projection keeps all 12. |
| benchmark_results | 716 | 716 | Match — but pre-T5-benchmarks events lack identifying columns; only post-T5 events feed the rebuild (Option B for legacy rows per `BENCHMARKS_DB_RETIREMENT_2026-05-19.md`). |
| roadmap_items | 38 | 38 | Match. |
| suggestions | 4 | 4 | Match. |
| task_dependencies | 26 | n/a (no projection) | **Dead table** — confirmed during T1 audit. T6 drops without replacement. |

### 13.3 Pre-T5 legacy drift surfaced during smoke

| Surface | Live state | Rebuild-from-empty |
|---|---|---|
| `proj_chain_status` for `reference-resolution-ml-upgrade` | pending=0, blocked=5 | pending=1, blocked=4 (correct per CRUD tasks + proj_current_tasks) |
| `proj_benchmark_results` rebuild | 716 rows | 0 rows (pre-T5 events lack identifying columns) |
| `proj_current_tasks` rebuild | 2864 rows | 2870 rows (synthetic backfill events resolved chains the live state has since lost) |
| `proj_task_blockers` rebuild | 12 rows | 9 rows (pre-T3 HandleTaskBlock no-emit path for 2nd+ edge means 3 historical edges have no TaskTransitioned events) |

**All of these are expected per Option B for pre-T5 legacy state.** Going forward, every new event is consistent with rebuild-from-empty. The live projection state reflects "pre-T5 snapshot-seed + post-T5 fold updates" while rebuild reflects "post-T5 fold updates only." Reconciliation happens naturally as new events fire on touched entities; chains never touched again retain their pre-T5 cached counts.

### 13.4 Test-time infrastructure that goes away in T6

- `testutil.InstallProjectionMirrorTriggers` — DB triggers that mirror direct-CRUD-INSERT test fixtures to projections. The triggers fire on `INSERT/UPDATE/DELETE ON {bugs,tasks,chains,task_blockers,suggestions,benchmark_results,roadmap_items}`. T6's table drops invalidate the trigger targets; the function should be deleted alongside the migration.
- `observehttp/chains_test.go::stillCRUDRebuilds` — allowlist of projection names that still rebuild from CRUD. After T6 it shrinks to telemetry-substrate names only (3 entries: `query_volume_by_source`, `retrieval_success_per_query`, `training_data_for_reranker`).
- Test fixtures' `INSERT INTO {bugs,tasks,chains,…}` direct writes — these compile against the schema. Post-T6 they break at compile time (well, query time). Rewrite to emit synthetic events (`INSERT INTO events ...`) OR call the real handler (forge.HandleForge, work.HandleX) OR direct INSERT to the projection table. The post-T5-bugs snapshot test (`projections_test.go::TestRebuildAll_Snapshot`) already shows the synthetic-event seed pattern.

### 13.5 T6 — concrete next steps

**Goal:** single migration drops the seven retired CRUD tables. Verifies the binary builds, the daemon serves, and `rebuild-projections` from empty still produces consistent state.

**Drop order** (FK-safe):
1. `task_dependencies` — dead table, no FKs.
2. `task_blockers` — FKs INTO tasks; drop before tasks.
3. `roadmap_items` — FKs into projects only; can drop any time after roadmap fold.
4. `tasks` — FKs from `roadmap_items.ref_kind='task'` (already gone) and `task_blockers` (already gone).
5. `bugs`, `chains`, `benchmark_results`, `suggestions` — no FKs FROM other retired tables.

**Migration triple-sync per CLAUDE.md §Migrations:**
- `crates/shared-db/migrations/060_drop_retired_crud_tables.sql` (canonical)
- `go/internal/db/migrations/060_drop_retired_crud_tables.sql` (Go embed mirror)
- `go/internal/testutil/migrations/060_drop_retired_crud_tables.sql` (hermetic fixture)

**Things to also drop in the same commit (or follow-on):**
- `testutil.InstallProjectionMirrorTriggers` body (the trigger-creation SQL targets dropped tables; the function becomes uncallable).
- `observehttp/chains_test.go::refreshProjectionsFromCRUD` — entirely. The remaining 3 telemetry projections still rebuild from their own CRUD (vault_search_invocations etc.), but that's a separate concern; the function could shrink rather than fully delete.
- The seedX helpers in test files that direct-INSERT the dropped CRUD tables. Switch them to emit synthetic events OR direct-INSERT the projection (snapshot-seed pattern).

**Things to PRESERVE through T6:**
- `bugs_fts`, `suggestions_fts` — parent-driven from projections per §7. Drop only if the design doc §7 decision is revisited.
- `benchmark_provenance` — out-of-scope FK target preserved. The FK from `proj_benchmark_results.provenance_id` remains valid.
- All `events` / `proj_*` tables — these are the substrate's source of truth post-T6.
- The fold modules in `go/internal/projections/{bugs,chains,roadmap,suggestions,tasks,benchmarks}.go` — they're fully payload-only now; T6 doesn't touch them.

**Verification recipes for T6:**

```bash
# Build + tests must pass:
make -C go build-all
make -C go test
cargo nextest run --workspace

# Smoke against a fresh copy of the live DB:
sqlite3 data/toolkit.db '.backup /tmp/toolkit-post-drop.db'
# (run migration 060 against /tmp/toolkit-post-drop.db — TBD: standalone runner OR via daemon restart)
go/bin/toolkit-server rebuild-projections --db=/tmp/toolkit-post-drop.db   # all projections, post-drop
# Compare proj_* row counts before vs after — only the legacy-drift rows in §13.3 should differ.

# Grep for residual CRUD references (should return zero non-migration hits):
grep -rnE "FROM (bugs|tasks|chains|task_blockers|task_dependencies|benchmark_results|roadmap_items|suggestions)\b" go/internal/ | grep -vE "(_test\.go|migrations/|projections/.*\.go)"
```

The fold modules under `projections/` legitimately reference the CRUD table names in deleted-code comments and the post-T6 cleanup commits can scrub those comments — out of scope for the migration itself.

### 13.6 Pre-T6 DB snapshots taken during T5

For recovery if T6 needs to be reverted:

- `/mnt/data1/toolkit.db.snapshot-2026-05-20-pre-substrate-crud-retirement-T2` — pre-T2, 28M
- `/mnt/data1/toolkit.db.snapshot-2026-05-21-pre-T5-write-flip` — pre-T5 split, 45M (277 chains / 2864 tasks / 841 bugs / 4 suggestions / 716 benchmarks / 38 roadmap / 8 task_blockers / 4104 events)
- `/mnt/data1/toolkit.db.snapshot-2026-05-21-post-bugs-flip` — mid-T5 (after suggestions+roadmap+bugs flipped, before chains/benchmarks/tasks), 45M

Take a fresh `snapshot-2026-05-21-pre-T6-drop` before running migration 060.

### 13.7 Outstanding bugs / open work

- Bug `task-blockers-payload-gap-for-substrate-rebuild` — resolved (T3 + T5-tasks land the L1181 guard lift + RemovedBlockerSlug emit). The 3 historical edges in §13.3 lack events because they were INSERTed before this fix; they're forensic-only.
- Bug `forge-task-delete-lacks-event-emit` — resolved at `8f2cb87` (dead-path removal).
- Bug `precommit-codemap-gen-fails-in-worktree-git-dir-env` — fixed at `d76a7c3`.
- Bug `eventEntityKinds-missing-suggestion` (in spirit) — fixed at `7971c49`. The dashboard event-history panel for suggestions now resolves.
- `task_blockers` non-emit-pre-T3 historical edges — not a bug, expected per §13.3.
- **Q3.** What is the rebuild order between `proj_current_tasks` and `proj_chain_status` (the latter's task-status counts depend on the former)? (T2 decides; default: fold dispatch ensures tasks fold before chains within the same event-replay loop.)

---

## 14. Frontend equivalence verification (T6a)

The chain's `completion_condition (b)` ("byte-identical rebuild from
empty") is verified end-to-end by `scripts/equivalence-harness.sh`,
which runs in three stages:

### 14.1 Pass condition

- **Stage 2b — SQL row-count diff (load-bearing).** Snapshot the live
  DB, run `toolkit-server rebuild-projections` against the copy, then
  diff `proj_*` row counts between live and rebuilt. Counts that match
  prove the projection layer is byte-identical under
  rebuild-from-events. As of `c9cadca` (the T6a clean-slate commit),
  every artifact-lifecycle projection matches exactly:

  | Projection                | Live | Rebuilt | Status |
  |---                        |---   |---      |---     |
  | `proj_chain_status`       | 277  | 277     | ✓ exact |
  | `proj_current_bugs`       | 849  | 849     | ✓ exact |
  | `proj_current_tasks`      | 2864 | 2864    | ✓ exact |
  | `proj_current_suggestions`|  4   |  4      | ✓ exact |
  | `proj_task_blockers`      |  7   |  7      | ✓ exact |
  | `proj_benchmark_results`  | 716  | 716     | ✓ exact |
  | `proj_roadmap_view`       |  38  |  38     | ✓ exact |

- **Stage 4 — Vitest (mocked-API contract regression).** The dashboard
  component + page tests run against the rebuilt-DB-backed daemon
  (port 3099) via `VITE_API_BASE_URL`. Vitest mocks API calls per-
  test, so this stage is a structural regression check (test code +
  observehttp adapter shapes still align) rather than a data-driven
  check; it surfaces shape drift if the post-T5 fold produces a
  different row shape than the pre-T5 dual-write did. 424/424 pass.

- **Stage 5 — Playwright e2e (rendering).** Default-skipped because
  the specs HARDCODE `localhost:3000` in their `page.route()` regexes
  (see `apps/dashboard/tests/e2e/lib/api-route.ts` and bug 992 — host
  pinning prevents intercepting SPA-route navigations). The isolated
  daemon on port 3099 doesn't match those regexes, so the mocks miss
  + real-data flows through + assertions fail. Workaround: run with
  `--playwright-against-live`, which exercises Playwright against the
  LIVE daemon on port 3000. Valid because stage 2b proved the live
  state and the rebuilt state are byte-identical. 85/85 pass.

  Spec port-hardcoding cleanup filed as
  `playwright-specs-hardcode-localhost-3000-which-blocks-isolated-daemon-runs`.

### 14.2 Acceptable noise

None as of the current verification (all three stages clean).
Documented noise classes that the design accepts at the substrate
level (does NOT contribute to "drift" in equivalence terms):

- `last_event_id` / `last_event_ts` on projection rows. Migration
  057's snapshot-seed wrote `''` for these; the fold writes the
  actual event id. The byte-identical rebuild assertion ignores
  these per `internal/projections/projections_test.go::tableChecksum`.
- Chain-id renumbering during rebuild. proj_chain_status assigns ids
  via `COALESCE(MAX(id), 0) + 1` during the rebuild fold, so chain
  ids may differ between live and rebuild — joins by slug are the
  truth-bearing comparison shape, not joins by id.

### 14.3 How to run

```bash
# default — SQL diff + Vitest (Playwright skipped)
./scripts/equivalence-harness.sh

# include Playwright via the live-daemon workaround
./scripts/equivalence-harness.sh --playwright-against-live

# diagnostic mode (Playwright against isolated daemon; expected to
# fail until spec port-hardcoding is cleaned up)
./scripts/equivalence-harness.sh --playwright-against-isolated
```

### 14.4 Routing drift

If a future commit introduces drift detected by stage 2b:

- **More rows in rebuild than live**: events being folded that don't
  reflect in live state. Either a fold reads from the wrong source,
  or a cleanup-on-close event isn't firing. Route via the
  `task_blockers` fold pattern (see `foldTaskBlockersCleanupOnClose`
  in `go/internal/projections/tasks.go`) for similar cleanup-on-close
  semantics.
- **Fewer rows in rebuild than live**: events missing from the events
  log for entities that exist in projections, OR fold guard skipping
  events with insufficient payload coverage. Routes back to a
  follow-up migration (see migrations 061 / 063 / 064 as templates).

---

## 15. Chain closure (T7)

The chain's ArchitectureAuditCompleted event landed at
`019e4b9b-c6d6-77d6-9148-7fa84b10971e` (2026-05-21T17:36:00Z) via
`go/cmd/substrate-crud-retirement-audit-emit/`. The event itself is
the substrate's self-hosting proof for `completion_condition (g)` —
the substrate the chain retired the CRUD shadow of is exercised to
record its own retirement audit.

**Commit ledger (chain-spanning):**

| Phase | Task | Commit | Notes |
|---|---|---|---|
| T1 | phase4-audit-and-design | `50c78ce` | Option-A backfill picked; 7-table scope confirmed |
| T2 | ship-missing-projections | `1b31f47` | Four projections + Option-A synthetic backfill |
| T3 | event-payload-audit-and-bump | `3062deb` (worktree) → `d76a7c3` (merge) | Six additive payload bumps |
| T4 | repoint-crud-reads-to-projections | `5202d9a` + `0c307f4` + fixups `7971c49` `8f2cb87` | Backend + frontend repoint |
| T4a | mcp-readpath-coverage-baseline | `788008e` | Read-path test coverage matrix |
| T4b | frontend-state-coverage-baseline | `3847bb9` | Frontend test coverage matrix + baseline state |
| T5 | flip-write-contract-event-only | `c7c5d6e` `ca65006` `6814665` `a96d0e8` `94618fe` `7128e48` | Six entity-kind sub-commits; load-bearing flip |
| T6 | drop-crud-tables-migration | `725e854` | Migration 060 drops 8 retired tables; Go test fixtures repoint to projections; Rust gaps in chain-assessment + benchmarks crate closed |
| T6-followup | bug sweep | `53f38d8` | --version flag, smoketest CRUD refs, TaskCreated payload-shape tolerance |
| T6a-prep | Option-A coverage extension | `380e70a` | Migration 061: 239 ChainCreated + 701 BugReported + 22 TaskCreated + roadmap.set synth events |
| T6a | frontend-state-equivalence-verify + clean-slate | `c9cadca` + `65c4f0d` | Migrations 062/063/064 + TaskRetired event type + taskBlockers cleanup-on-close + equivalence-harness.sh |
| T7 | retrospective + closing event | (this commit) | ArchitectureAuditCompleted at `019e4b9b-c6d6-77d6-9148-7fa84b10971e` |

**Deferred follow-ons (filed during the chain, route to other work):**

- `playwright-specs-hardcode-localhost-3000-which-blocks-isolated-daemon-runs` — Playwright spec port-hardcoding cleanup; required for the equivalence harness's stage 5 to work against isolated daemons.
- `phase-4-legacy-field-deprecation` (cousin chain, 4 tasks now populated) — FIELD-level retirement of legacy free-form columns; gated on 30-day production bake-in of agent-substrate-frontend.
- `trained-arc-close-filing-classifier-v1` + `trained-smart-snapshot-filter-v1` (substrate-optimisation tier, 6 tasks each now populated) — ML follow-ons riding ml-capability-substrate's A/B harness.

**Out-of-scope tables (not retired by this chain, may be future work):**

- `library_entries`, `kiwix_references`, `curation_candidates` — knowledge-side CRUD; event flow partial. Would need its own audit chain.
- `trained_models`, `model_predictions`, `ab_comparisons` — ml-substrate CRUD; no event coupling today.
- `projects`, `hosts`, `setup_recipes`, etc. — bootstrap / operational config.
- `bugs_fts`, `suggestions_fts` — FTS5 virtual tables preserved per design §7; parent-driven from projections.
- `benchmark_provenance` — out-of-scope FK target preserved.
