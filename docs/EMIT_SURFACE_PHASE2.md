# `record` — Event-Submission Surface (forge v2) — Phase 2 Design

> **Status:** DRAFT FOR HANDOFF. Produced 2026-05-25 in conversation, as the "phase 2" successor to the telemetry-consolidation program (`TELEMETRY_CONSOLIDATION.md`). This is a north-star spec, not an implementation — a fresh session picks it up starting from the forge-feature inventory (the first task). **The surface is named `record` (§Naming); the `emit` filename + chain/task slugs are retained for now and migrate incrementally — read `emit` throughout as `record`.**
>
> **Discipline:** This is a **`code-migration-discipline`** effort — an isolated **v2 built beside `forge`**, not an in-place refactor. forge keeps working throughout; `record` must achieve **parity with every guarantee forge encodes** before forge is migrated off and archived.
>
> **Companion docs:** `TELEMETRY_CONSOLIDATION.md` (the sibling program; phase 2 converges with it — see §Observability), `EVENT_CATALOG.md` + the `events` ledger (`go/internal/events/`), `PROJECTIONS.md` (fold contract), `go/internal/forge/` (the surface being superseded), `go/internal/work/batch.go` (the embryo of `record` — already an ordered, atomic, partial-success op-array in one tx).

---

## Naming — the surface is `record`

The write action is **`record`** — `record(events[])`. Read side: **`read`** (the projection, fast) + **`recall`** (the event log, for provenance). Decided 2026-05-27; supersedes `forge` (the retired surface) and `emit` (this doc's earlier working name).

`forge` is a metaphor and `emit` is producer-jargon — both hide the mechanic. `record` exposes it: you write a durable, append-only **fact** about what happened, and all state is a projection of those facts. It reframes the agent from CRUD ("create a bug") to event-sourcing ("record that a bug was filed"), and reads plainly to a cold reader — what `forge` never did. `record` / `read` / `recall` is self-documenting as a set.

**The rename is incremental** ("slowly, properly"): the chain slug `emit-surface-forge-v2`, the task slugs (`emit-local-draft-surface`, …), and this filename keep the `emit` identifier for now and migrate as touched — but **the built surface is `record`**. Read every `emit` below as `record`.

---

## 1. One sentence

A new surface in the `work` family that supersedes `forge` / `forge_edit` / the lifecycle actions with a single **`record(events[])`** call: the agent submits a timestamped, heterogeneous array of typed events; valid ones append to a **hot, freely-mutable local draft** ledger and fold into projections; rejected ones become **ghost-TODOs**; the draft mirrors asynchronously to a **CI-gated, immutable, git-backed canonical registry** (Gitea on the mini-PC) that is the published source of truth and the disaster-recovery authority.

## 2. The call & return

- **Input:** `record(events: [{type, payload, ts?}, …])` — heterogeneous (a bug, a chain-and-tasks primitive, several bugs, a memory, …). One create is a single-element array.
- **Behavior:** per-event validation against a finite type enum → append the valid + fold projections in one local tx → ghost the rejected → **return consolidated per-event results immediately** (ok / ghost + descriptive reason + enough context to rewrite), with partial-success semantics inherited from today's `work.batch`.
- **Async tail:** new events mirror to the registry; CI re-validates + stamps; **a completion ping re-enters the agent's context** with the verdict (blessed, or descriptive failure). The agent never blocks on network or CI for the primary return. (Feedback latency is acceptable because the local path is all-Go and fast, and the ping closes the loop.)
- **`ts`:** server-authoritative by default; a caller-supplied `ts` is accepted only under monotonic/clamp rules (ordering is load-bearing — see §Invariants).

## 3. Two-layer truth

- **Hot local draft** — intentionally hot, fast, forgiving. Freely mutable *for unpublished events*: slot in place, merge two bad events into one real one, squash. The agent's working surface; **disposable** because it is rebuildable from the registry.
- **Canonical registry (Gitea, mini-PC)** — append-only, immutable, **fast-forward-only**: CI refuses any insertion / edit / rebase over *previously-published* events (git's non-fast-forward refusal); **only new events survive local rewriting** through to publish. This is the source of truth, the DR / reconstruction authority, and the two-way artifact-diff substrate (git content-addressing *is* the deterministic diff base that earlier diff-on-commit reasoning needed).

## 4. Validation — tiered, not removed

`forge`'s strictness has been compensating for the local DB being the only copy with no net. With a CI-gated durable registry, the local DB stops being precious, so the gate can relax — but it **tiers**, it does not vanish:

- **Thin-fast-local:** the cheap, high-ROI, instant guards that keep the draft usable and catch the 90% fumble immediately → ghost. (shape, obvious illegality, the must-reject cases.)
- **Thorough-CI:** the expensive / subtle / global-consistency checks, run on the registry as the validity stamp + integrity backstop (catches drift, direct pushes, anything the local gate let through).

Choosing the split — which checks are local vs CI — is core design work.

## 5. Ghosts

A rejected event is neither dropped nor folded into entity projections. It is a persistent **TODO + session-anchor**: "you tried X, rejected because Y, here's enough to rewrite it," surfaced to the operating agent via the existing `pending_decisions` → Stop-hook seam and counted in a **rejection / fumble projection**. Squashing a draft never loses the fumble record — the ghosts preserve it. (This natively closes the `forge-shape-liveness-reaudit` "success AND rejection counts per shape" gap.)

## 6. Observability falls out

Per-tool-per-model performance, per-forge-shape liveness, and operator-fumble rate become **projections over the event + ghost stream** — not separate instrumentation. This is where phase 2 *converges* with the telemetry-consolidation program rather than duplicating it.

## 7. Invariants (non-negotiable)

1. The published (canonical) ledger is **append-only-immutable + fast-forward-only**.
2. The local draft is **always rebuildable** from the canonical registry (disposable).
3. Validation runs **before-or-at append**, never after; **ghosts never fold into entity projections**.
4. **`ts` is authoritative for ordering** (server-set or clamped).

## 8. Build approach & sequencing (load-bearing)

Isolated v2 via `code-migration-discipline`; forge untouched until parity is proven.

**Order matters — the local loosening is only safe after the net exists:**
0. **Inventory forge** → the exhaustive forge-derived AC (list B); **also reconcile the §11 cross-cutting design questions** (they sit outside forge's tests, so the inventory alone misses them).
1. **Build the canonical registry + CI FIRST** (the net — fast-forward-only, the validity stamp) — **this includes defining the shared event-type enum + per-type validators (§11 Decided); the CI is the full enum-backed validity-stamp tier**.
2. **Then** the local `record` surface (the thin-fast-local validation tier built *on* the T2 enum, hot draft, fold, consolidated returns).
3. **Then** ghosts + async mirror + completion ping.
4. **Then** parity audit against forge's characterization net.
5. **Then** migrate callers, promote `record`, archive forge.

Loosening forge before the registry + CI exist would remove the net before stringing the new one.

## 9. Acceptance criteria — two sources

### A. Our AC (this conversation's design decisions)
1. Single `record(events[])` surface in the work family; heterogeneous, timestamped array; one-create = single-element array.
2. Finite event-type enum, one per-type validator each — **defined in T2's shared `go/internal/events/` package (§11 Decided); the CI tier (T2) and the local surface (T3) both import the one canonical definition**.
3. Instant local validate → append → fold → return; consolidated per-event results; partial-success.
4. Ghosts: persistent TODO + session-anchor; descriptive reason + rewrite-context; kept out of entity projections; Stop-hook-surfaced; counted in a rejection projection.
5. Hot local draft, freely mutable for unpublished events (slot / merge / squash); rebuildable from registry.
6. Canonical Gitea registry: immutable, fast-forward-only (rejects rewrite of published events), CI-gated.
7. Async mirror + CI validity stamp + completion ping into agent context.
8. Tiered validation (thin-fast-local + thorough-CI).
9. DR: workstation loss → clone registry → replay → rebuild projections + materialize artifact files (two-way diff).
10. Multi-machine (workstation + mini-PC) push / pull / rebuild.
11. `ts` ordering authority; observability as projections over the event + ghost stream.
12. Two-layer truth + the four §7 invariants.

### B. forge-derived AC — *EXHAUSTIVE (T1 complete, 2026-05-27)*

Mined per `code-migration-discipline` step 1 from forge's **full test suite** (33 `*_test.go` files, ~200 test fns — each test fn name is a feature label), `validate.go` / `placeholder_guard.go` / `chain_tasks.go` guard code, `work/batch.go` (the `record` embryo), the `events` ledger contract, and the three cross-referenced bugs. Each AC is tagged with its source (test name / bug slug / file). `record` must reproduce every one of these or document a deliberate delta (§9 honest-boundary rule).

> **Naming:** these are *forge's* guarantees; `record` honors them through the events-array surface. Where forge had `forge` / `forge_edit` / `forge_delete` as three actions, `record` folds create+amend+retract into typed events (see §11 amend-vs-compensate).

**Envelope, dispatch & param handling**
- **B-E1.** Two envelope shapes accepted: top-level "sugar" keys *and* nested `fields:{}`. *(TestHandleForge_SugarShape, _StructuredFieldsShape)*
- **B-E2.** Mixed-envelope rejection — a call may not mix sugar + `fields:{}`. Per-schema nuance: task accepts `chain_slug` alongside a `fields` object (not "true mixed"); a true mixed envelope still rejects. *(TestHandleForge_MixedEnvelopeRejection, TestHandleForgeEdit_TaskAcceptsChainSlugAlongsideFieldsObject, _TaskStillRejectsTrueMixedEnvelope)*
- **B-E3.** Unknown-field rejection on **both** paths — sugar keys *and* `fields:{}` block. *(TestHandleForge_UnknownFieldYieldsB10Envelope, _RejectsUnknownTopLevelKeyOnFieldsEnvelope, TestHandleForgeEdit_UnknownFieldRejected)*
- **B-E4.** Unknown top-level `op` key specifically rejected (was silently swallowed on the fields path → create-instead-of-update corruption) with a hint pointing at the update/delete path. *(bug `forge-silently-swallows-unknown-op-key-on-fields-envelope-path` d17f2786; TestHandleForge_RejectsUnknownOpOnFieldsEnvelope, TestHandleForgeInTx_RejectsUnknownOp)*
- **B-E5.** Missing `schema_name` rejected. *(TestHandleForge_RejectsMissingSchemaName)*
- **B-E6.** Schema-not-found returns the registered-schema list. *(TestHandleForge_SchemaNotFoundReturnsRegisteredList)*
- **B-E7.** Project-required gate for project-scoped schemas; omitted project falls back to the CWD/default resolver; rejection hint names the action. *(TestHandleForge_RejectsMissingProject, _OmittedProjectFallsBackToResolver, _RejectsMissingProject_HintNamesAction)*
- **B-E8.** Rationale gate at the envelope level for agent actors (+ wrong-nesting hint). For `record`: per-event rationale, generalizing `work.batch`'s per-op rationale (mandatory, pre-execution reject naming the offending index). *(dispatch-policy; batch.go pre-flight)*

**Field validation** *(validate.go — all violations collected, not short-circuited)*
- **B-V1.** Required-field-missing → `missing_required`, message names the field; the envelope-level message names which envelope was inspected (sugar vs fields). *(TestValidate_RequiredFieldMissing, TestHandleForge_MissingRequiredNamesFieldsEnvelope, _MissingRequiredNamesSugarEnvelope)*
- **B-V2.** Empty-required → `empty_required`. *(TestHandleForgeEdit_RequiredFieldEmptyRejected)*
- **B-V3.** Type mismatch (list vs string) → `type_mismatch`. *(TestValidate_TypeMismatchListInsteadOfString)*
- **B-V4.** `string_or_list` coercion: a single string lifts to a 1-element list. *(TestValidate_CoercesSingleToList)*
- **B-V5.** Enum violation → `enum`, message names the accepted set. *(TestNet_Validate_EnumRejection)*
- **B-V6.** Pattern violation → `pattern` (regex compiled at schema-load time). *(validate.go; pattern path)*
- **B-V7.** Malformed-field → `malformed_field`: an object-array where a scalar / homogeneous string-list is expected (bug 1398's `tasks:[{...}]` collapse) is caught at parse time, naming the field + the bad shape, before any row write. *(TestNet_Create_MalformedScalarFieldRejected, _FieldValue_CoercionAndRejectionShapes, _MalformedField_EditAndInTx)*
- **B-V8.** List-join canonicalization: `surface` + `tags` join on comma; `acceptance_criteria` joins on newline-dash. AC list persists at forge time for bug / suggestion / task. *(TestHandleForge_BugSurfaceAndTagsListJoinOnComma, _TaskAcceptanceCriteriaListJoinsOnNewlineDash, bug_list_shape_test.go)*

**Guards**
- **B-G1.** Placeholder-shaped value `{{NAME}}` rejected by default (whole-value match only; an embedded `{{X}}` substring passes); opt-out via `allow_placeholder=true`. *(placeholder_guard.go; suggestion `forge-edit-reject-placeholder-shaped-values-by-default`; TestHandleForgeEdit_RejectsPlaceholderShapedValueByDefault, _AllowPlaceholderOverrideLetsThrough, _EmbeddedPlaceholderSubstringPasses)*
- **B-G2.** Double-dated-slug guard: a date-prefixed slug rejects for schemas whose output path already adds a date (both `YYYY-MM-DD_` and `YYYY-MM-DD-` separators); allowed for non-double-dating schemas. *(TestHandleForge_RejectsDatePrefixedSlugForDoubleDatingSchema, _RejectsDatePrefixedSlugWithDashSeparator, _AllowsDatePrefixedSlugForNonDoubleDatingSchema)*
- **B-G3.** Slug auto-derived from title when absent (chain + others); a derived chain slug is not rejected. *(TestHandleForge_SlugAutoDerivedFromTitle, _ChainSlugAutoDerivedFromTitleNotRejected, slugify_test.go)*

**Duplicate-slug semantics (per-schema policy — the corruption-lineage core)**
- **B-D1.** Duplicate-slug **create rejects** for chain / bug / suggestion / task, naming the update path — never a silent content overwrite. *(bugs `forge-chain-create-on-existing-slug-overwrites-instead-of-rejecting` + `...task-create-silently-upserts...` d17f2786; TestHandleForge_ChainCreateOnExistingSlugRejects, _BugCreateOnExistingSlugRejects, _SuggestionCreateOnExistingSlugRejects, _TaskCreateOnExistingSlugRejects, TestHandleForgeInTx_BugDuplicateRejects)*
- **B-D2.** vault-note + memory **deliberately re-forge-as-update** ("policy A"), returning `action=updated`; a scope/kind change relocates + cleans the old file. This must NOT be flattened into B-D1. *(TestHandleForge_VaultNoteReforgeNotRejected, TestForgeVaultNote_SameSlugReforgeAutoUpdates, _ScopeChangeReforgeCleansOldFile)*
- **B-D3.** Defaults applied at create: bug `severity`, suggestion `priority`. *(TestHandleForge_CreatesBugWithSeverityDefault, _CreatesSuggestionWithPriorityDefault)*

**Chain+tasks atomic fan-out**
- **B-C1.** `forge(chain, tasks=[…])` emits `ChainCreated` + N `TaskCreated` + `ChainAndTasksForged` atomically, order preserved. *(TestForgeChain_FullObjectTasks_AtomicCreate, forge_events_test.go)*
- **B-C2.** `tasks` accepts pipe-delimited (legacy), full-object (T7), per-entry-mixed, and single-string shapes. *(ParseChainTasks; TestForgeChain_PipeDelimitedOnly_BackCompat, _FullObjectTasks, _MixedModeTasks)*
- **B-C3.** A full-object task missing its mandatory per-task rationale rejects the **whole** forge call pre-write (atomic with the rejection). *(TestForgeChain_FullObjectMissingRationale_RejectsPreWrite; chain_tasks.go parseFullObjectEntry)*
- **B-C4.** No `tasks` field → only `ChainCreated` emits. *(TestForgeChain_NoTasksField_OnlyChainCreatedEmits)*
- **B-C5.** Task `position = MAX(position)+1` within the chain; sequential within one tx (read-through-tx). *(TestHandleForge_TaskPositionIncrementsWithinChain, TestNet_InTx_TaskPositionsSequentialInOneTx)*

**Fold invariants (parity-critical — the data-integrity core)**
- **B-F1.** `ChainCreated` fold **preserves the existing chain PK** on `(project,slug)` conflict — never reassigns (the freshly-computed MAX+1 id is used only on a genuine insert). Orphaning child tasks via PK-reassign is the canonical corruption class. *(bug `chain-create-fold-reassigns-pk-on-conflict-orphaning-tasks` ae90da55)*
- **B-F2.** `chain_id` resolved by slug; idempotent re-fold (re-firing a `ChainCreated` for an existing slug does not mutate state); **byte-identical rebuild** from the event log. *(migration dual-write byte-identical; projections rebuild)*
- **B-F3.** Index / FTS upsert on create **and** edit (knowledge_pointers); suggestion + memory have **no** knowledge pointer; vault-note routes by kind/scope; delete-then-insert FTS parity. *(indexsync_test.go, shape_matrix IndexSync tests, TestPointersUpsert_DeleteThenInsertParity)*

**Event emission (the closed enum forge exercises)**
- **B-EV1.** Per-schema event types: chain→`ChainCreated`, task→`TaskCreated`, bug→`BugReported`, edit-bug→`BugEdited` (with updated-fields/values map), edit-chain→`ChainEdited`, memory→`MemoryWritten`, retrospective→`RetrospectiveForged`, report-card→`ReportCardForged`, bench→`BenchmarkForged`, migration→`MigrationForged`, suggestion→`SuggestionReported`. *(forge_events_test, forge_t3_payload_test, strategy_test, memory_test, retrospective_test, bench_test, migration_test)*
- **B-EV2.** Silence where forge is silent: non-tracked-table edit emits nothing; vault-note create writes the file but emits **no** event; suggestion create is not indexed. *(TestForgeEdit_NonTrackedTableEmitsNothing, TestStrategy_VaultNote_CreateWritesFileNoEvent, _Suggestion_CreateEmitsAndNotIndexed)*
- **B-EV3.** Event ledger contract inherited from `go/internal/events`: closed type enum (unknown type / schema-failing payload = hard error at write), `(ts, event_id)` chronological ordering authority, append-only enforced by BEFORE-UPDATE/DELETE triggers, `schema_version` stamped, dual-write order (row update then `Emit`, both-or-neither), fold hook runs in the same tx. *(events/doc.go, emit.go, validator.go, events_test.go)*

**forge_edit semantics → `record` amend events**
- **B-ED1.** Edit updates only the provided field; unknown field rejected; required-empty rejected; not-found on unknown slug. *(edit_test.go)*
- **B-ED2.** Lifecycle-owned fields rejected from a plain edit with a "use the lifecycle action" message: bug `resolution_note`, bug `status`, suggestion `status`, suggestion `resolution_note`. *(edit_setby_test.go)*
- **B-ED3.** Markdown-doc edits (vault-note / retrospective / report-card): partial title/body update; tags update; kind-change relocates the file; preserves pre-canonical body + `created` alias; preserves embedded H2 headings; round-trips non-declared frontmatter keys; `drop_extras` removes a non-declared key (accepts single-string form); locates an undated filename via fallback; no-op edit preserves the path. *(edit_markdown_test.go)*

**forge_delete semantics → `record` retract events**
- **B-DEL1.** `forge_delete` rejects bug / chain / task / suggestion by default (no hard delete — lifecycle actions own terminal state); schema-not-found fires before the project guard; empty-project rejected for project-scoped schemas; missing schema_name or slug errors; id-without-slug points at the lifecycle actions; not-found when the schema supports delete but no row matches. *(delete_test.go)*

**Fail-open / fault-injection**
- **B-FO1.** Telemetry / index / markdown-**emit** writes are **fail-open** — they never abort the underlying op (surface on the public result, but the op succeeds). A markdown **file-write** failure IS a hard error. An index-upsert failure surfaces after a create error. *(fault_injection_test.go, TestNet_IndexSync_FailOpenOnPointerWriteError)*

**Agent-UX affordances**
- **B-UX1.** Burst nudge: a sequential forge burst within a window nudges toward `batch`/`record`; no nudge for a non-batchable schema; no nudge without a session_id. *(forge_burst_hint_test.go, forge_burst_tracker_test.go)*
- **B-UX2.** Deferral nudge: a task deferred without captured context nudges. *(deferral_nudge_test.go)*
- **B-UX3.** Retrospective follow-on capture: an orphaned candidate auto-files as a suggestion; the `none` sentinel and all-candidates-captured produce no suggestion. *(retrospective_followon_capture_test.go)*

**Hooks**
- **B-H1.** Pre-op hook can **block** (aborts the create); a hook failure rolls back the chain insert; the skeleton-task-inserter hook fires on chain forge. *(hooks_test.go)*

**Introspection**
- **B-I1.** `forge_schemas` returns all registered schemas (empty array when degraded); `forge_schema` returns fields, accepts name/kind aliases, exposes call-envelopes for create + update, and returns an error envelope for unknown/missing. The `record` analogue documents the event-type enum + per-type schema. *(introspect_test.go)*

**In-tx (batch) parity → `record`'s native shape**
- **B-IX1.** `HandleForgeInTx`: rejects unknown op / non-batch schema / empty fields / missing-required; bug create succeeds; the scope gate rejects a non-allowlisted schema; edit-markdown is rejected in-tx. *(open_bug_fixes_test.go, shape_matrix InTx tests)*
- **B-IX2.** Partial-success / atomicity (the `work.batch` contract `record` generalizes): default abort-on-first-error rolls back the whole outer tx (prior ops UNDONE); `continue_on_error=true` commits each op and reports per-op outcomes; a rolled-back batch produces **zero** events and the per-op `event_id`s are stripped from the response. *(batch.go, batch_test.go)*

**Schema-specific (migration / bench)**
- **B-S1.** `forge(migration)`: dual-write byte-identical (canonical + testutil mirror), steps past the substrate number, sequence-gap → next is MAX+1, idempotency (existing slug returns existing), mirror-write-fails → canonical rolls back. *(migration_test.go)*
- **B-S2.** `forge(bench)`: happy path writes row + emits, idempotency (existing slug returns existing), non-JSON `parse_output_as` rejects. *(bench_test.go)*
- **B-S3.** Markdown-root resolution: via project path, falls back to CWD, env override wins, honors project path for forge-migration; atomic write refuses to escape outside the guard dir. *(markdown_root_test.go, render_test.go)*
- **B-S4.** Bug `qwen_task_id` round-trips with no collision warning. *(forge_qwen_task_id_test.go)*

→ This list is the parity net for T6. A `record` behavior that drops any B-* AC without a documented §9-honest-boundary delta is a blocker.

## 10. Honest boundaries

- List A is the converged design; List B is **seeded, not exhaustive** — pretending otherwise would undercut the "match every hard-won forge guarantee" goal.
- The local projection DB does not disappear — it becomes a **derived read replica** of the registry (reads stay fast + local), so this commits to a sync + fold pipeline.
- Pick **local-hot / remote-authority** deliberately (vs "remote is canonical, local must reach it to be real," which re-introduces latency/availability coupling). Append-only logs are merge-friendly in git, which is what makes the two-machine case tractable.
- The dashboard window will show **draft** state, not blessed state, until a later "draft vs published" distinction is added (accepted; author has a plan).

---

## 11. Cross-cutting design questions (resolve in T1) — from event-sourced-agent-os-notes.md

These are substrate-level decisions the forge-parity AC (§9.B) does **NOT** cover — they sit outside forge's test suite, so the T1 inventory will miss them unless explicitly reconciled. Surfaced by `~/Documents/files/ideas-to-process/event-sourced-agent-os-notes.md` (§Reconciliation, 2026-05-27). Resolve or explicitly scope each **before** the registry/emit build begins.

**Load-bearing (costly to retrofit):**
- **Three-state event lifecycle.** §3 models two states (hot draft / canonical) and treats the draft as freely mutable while unpublished. But editing a local event that *another consumer (or a past self) already read as input* corrupts that consumer's reasoning even pre-publication. Model the frozen middle state — local-private (freely editable) / local-shared-but-not-canonical (compensate-only) / canonical (immutable) — and surface the amend-vs-compensate distinction in the `record` API.
- **Snapshot strategy.** §10 commits to a fold pipeline but not to snapshots; replay-from-zero degrades within weeks of event volume. Decide periodic-materialization + replay-from-snapshot now (even if implemented later); point-in-time queries ride on it.
- **Causal ordering.** §7 orders by `ts` + `event_id` — single-writer-safe, but the two-machine case (workstation + mini-PC both appending, §10 / AC-10) needs `parent_event_id` (or vector clocks) so CI can distinguish concurrent from sequential and reject events claiming a non-canonical parent.

**Scope decisions (settle first — they bound the event-type enum):**
- **Artifacts in or out of v2?** The plan reads forge-entity-only (db + memory mutations). The notes assume file/artifact diff-events too (the "diff-as-primitive vs intent-as-primitive" framing). The diff+intent question only applies if artifacts are in scope.
- **Speculative branching** — first-class primitive (fork local timeline → project → evaluate → reconcile-or-abandon) or out of v2 scope? The local-draft + ff-only-canonical model affords it nearly free; decide deliberately rather than let it be an accidental emergent.

**Refinements (inform existing plan work, not blockers):** CI rule taxonomy (structural / causal / semantic / projection-coherence) to inform the §4 local-vs-CI split; agent read-side dual API (projection-default + event-log-for-provenance); schema evolution (upcasting vs versioning — the `events` ledger already carries `schema_version`).

**Decided (2026-05-27) — the event-type enum + per-type validators are a T2 deliverable, defined in a shared location.** So T2 can implement the *full* enum-backed CI (the validity-stamp tier) rather than a structural-only stub, the finite event-type enum + per-type schemas/validators are **defined in T2**, placed in the shared `go/internal/events/` package (extending the existing event-definition home — `emit.go` / `payloads.go` + the `blueprints/events/*.json` schemas) so the registry CI (T2) and the local `record` surface (T3) both **import one canonical definition**. T3 builds its thin-fast-local tier *on top of* that shared enum; it does not redefine it. This resolves the T2-before-T3 ordering (the enum exists when CI needs it) and removes the parallel-source-of-truth risk. The CI-rule-taxonomy split above still applies — it governs which *categories* of check run in the CI tier vs the local tier, on top of the one shared enum.

Full per-item reconciliation + the considered divergences (e.g. typed-events vs the Datomic tuple primitive) live in the notes file's §Reconciliation.

---

## 12. Resolutions (T1, 2026-05-27) — the §11 decisions, settled

The §11 questions are now resolved. Each is binding on T2–T7; deltas require a doc update + rationale.

**Load-bearing:**
- **Three-state lifecycle — MODEL IT.** `record` events carry a lifecycle state: `local-private` (only this worktree has it, no other consumer has read it → freely amendable), `local-shared-not-canonical` (read by another agent/process as input, or pushed but not yet CI-blessed → **compensate-only**, no in-place amend), `canonical` (CI-blessed + ff-only → immutable forever). The free-edit window is "written → first read as input." The `record` API surfaces the **amend-vs-compensate** distinction explicitly: amend mutates an unpublished `local-private` event; once an event leaves `local-private`, correction is a *new* compensating event (supersede / retract), never an in-place edit. This is the safety invariant — editing an already-observed event corrupts the consumer's reasoning + destroys the audit trail.
- **Snapshot strategy — COMMIT NOW, implement in the fold pipeline.** Projections materialize periodic snapshots tagged "valid as of event N"; replay restores from the latest snapshot + folds forward (never replay-from-zero, which degrades within weeks of event volume). Snapshots are content-addressed + stored alongside events. Point-in-time queries ("what did the projection look like at event N") ride on this. T2's DR proof (clone → replay → byte-identical rebuild) validates the snapshot+fold path, not just zero-replay.
- **Causal ordering — `parent_event_id` for the two-machine case.** Single-writer keeps `(ts, event_id)`. The events ledger gains an optional `parent_event_id` (single-parent sequential; the existing `caused_by_event_id` ref is the seam). CI's causal tier rejects any event claiming a parent that is not on canonical at merge time, distinguishing concurrent (needs reconciliation) from sequential (don't). Vector clocks are deferred — single-parent refs catch the workstation+mini-PC class.

**Scope decisions:**
- **Artifacts — OUT of v2 (forge-entity-only), captured as a follow-on chain.** v2 records DB + memory/vault mutations only — the existing ~70-type enum. File/artifact diff-events (the "diff-as-primitive vs intent-as-primitive" framing) are **deferred but fully designed** in a dedicated follow-on chain authored at T1 time (`record-artifact-diff-events`, see chain note) so the design context isn't lost. The diff+intent question, content-addressed file storage, and replay-materializes-files DR are that chain's concern, not v2's.
- **Speculative branching — OUT of v2, affordance preserved, captured as a follow-on chain.** v2 does not build the fork/project/evaluate/reconcile-or-abandon API. The hot-draft + ff-only-canonical model affords branching near-free, so the draft-ledger design must **not foreclose** it (a draft is already a divergent local timeline). The first-class primitive — branch declaration, "project this branch and tell me what's true," cross-agent branch sharing, abandoned-speculation-as-learning — is **fully designed** in a dedicated follow-on chain authored at T1 time (`record-speculative-branching`, see chain note).

**Refinements (fold into the relevant task):**
- **CI rule taxonomy** governs the §4 local-vs-CI split: *structural* (schema valid, refs resolve, no dup ids, required present) + cheap *semantic* run thin-fast-local; *causal* (parent on canonical, no descent from non-canonical) + expensive *projection-coherence* (HEAD projection internally consistent, rebuilt from snapshot forward) run thorough-CI. T2 implements the taxonomy; T3 picks the local subset.
- **Read-side dual API:** `read` (projection-default, fast) + `recall` (event-log, provenance — "this fact came from event N by actor A, superseded by event M"). Per the §Naming set.
- **Point-in-time queries** ride the snapshot + `parent_event_id` work; not a separate deliverable.
- **Schema evolution:** the `events` ledger already carries `schema_version`; v2 uses **upcasting** for additive changes (projection code sees latest shape) + **versioning** when semantics genuinely change. No decision locked beyond "both, as the existing `schema_version` allows."

**Unified-tuple primitive (Datomic `[e,a,v,tx,asserted?]`):** conscious divergence — v2 keeps typed heterogeneous events. Recorded, not adopted.

---

## 13. Parity audit (T6, 2026-05-27) — `record` vs forge

The T6 parity audit, run against the §9.B AC list. **Headline finding:** `record`
is the *event-submission substrate* (raw typed events → ledger), while forge is a
*construction layer* (schema + fields → constructed event). So `record` reproduces
forge's **substrate-level** guarantees but deliberately does **not** reproduce its
**construction-layer** sugar — those are not lost, they live *above* `record` and
are the T7 migration's concern. This is a documented, deliberate delta (§10
honest-boundary), per the decision to *document deltas, not force 1:1 parity now*.

DR (§9 AC-9) is proven (`verify-dr`, byte-identical, incl. from a real Gitea clone);
multi-machine (§9 AC-10) is proven on the real mini-PC (push → pull → rebuild,
2026-05-27). This section covers the §9.B forge-derived parity.

### A. Substrate-level — reproduced by `record` (the shared events core)
These hold for `record` because it emits through the same `go/internal/events`
ledger forge uses; tested in `record_test.go` / `ghosts_test.go` / the events suite:
- **B-EV3** — event-ledger contract: closed type enum, `(ts, event_id)` ordering,
  append-only triggers, `schema_version`, fold-in-same-tx. (Shared verbatim.)
- **B-EV1** — per-entity event *types* are emittable via `record` (BugReported,
  ChainCreated, TaskCreated, BugResolved, …) and fold into the same projections.
- **B-F1/F2/F3** — fold invariants (PK-stable-on-conflict, chain_id-by-slug,
  byte-identical rebuild, index/FTS) — these live in the projection folds, which
  `record` drives identically (proven: `verify-dr` byte-identical; chain-319 fan-out).
- **B-IX2** — partial-success / atomicity: `record` generalizes `work.batch`
  (per-event ok/reject, abort-or-continue, rolled-back→event_ids stripped).
- **§7 ts authority** — server-set / future-clamped (`record_test.go`).
- Plus `record`-native additions beyond forge: **ghosts** (T4), **dry-run**,
  **event_schema** discovery, entity-kind inference.

### B. Construction-layer — NOT in raw `record` (documented deltas → T7)
These are forge's schema+fields→event *construction* behaviors. `record` takes a
*pre-formed typed event*, so it does not perform them; a caller migrating off forge
either constructs the event itself or goes through a thin forge-parity layer over
`record`. Deltas, deferred to T7:
- **B-E1/E2/E3/E4** — sugar-vs-`fields` envelope shapes, mixed-envelope rejection,
  unknown-field / unknown-`op` rejection. (Envelope ergonomics of forge's input.)
- **B-V1…V8** — field validation/coercion/enum/pattern/malformed-field, list-joins.
  (`record` validates the *event payload* against its JSON Schema, not forge's
  field-envelope rules.)
- **B-G1/G2/G3** — placeholder guard, double-dated-slug guard, slug-auto-derive.
- **B-D1/D2/D3** — duplicate-slug reject (per-schema policy), vault-note/memory
  re-forge-as-update, create-time defaults.
- **B-C1…C5** — chain+tasks atomic fan-out *sugar* (`forge(chain, tasks=[…])`).
  Note: `record` *can* express it (ChainCreated + N TaskCreated in one call —
  proven, chain 319) but does not do forge's slug-derivation / skeleton-hook /
  position-sugar; the events + fold are equivalent.
- **B-ED1/2/3, B-DEL1** — forge_edit / forge_delete lifecycle semantics
  (set-by-lifecycle field rejection, markdown-doc edits, no-hard-delete).
- **B-FO1** — fail-open telemetry/index writes.
- **B-UX1/2/3** — burst/deferral/retrospective nudges (agent-UX affordances).
- **B-H1** — pre-op hooks (skeleton inserter, block-on-hook).
- **B-I1** — forge schema introspection (`record` has the analogous **event_schema**
  for event types, but not forge-schema introspection).
- **B-S1…S4** — migration/bench schema specifics, markdown-root, qwen_task_id.

### C. forge's characterization net
forge's `*_test.go` net exercises forge's **construction layer** (bucket B), which
`record` intentionally does not reproduce — so the net is a **forge gate, not a
`record` parity gate**. The *shared* substrate it depends on (event emission + folds,
`go/internal/events` + `go/internal/projections`) is exercised by both and stays green.

### Conclusion + what this means for T7
`record` does **not** today drop-in-replace forge: it replaces forge's
*event-emission substrate*, and forge's *construction sugar* (bucket B) is the
migration's responsibility. T7 (migrate callers + archive forge) is therefore not a
one-line cutover — it must either (a) port forge's construction sugar into a thin
parity layer over `record`, or (b) migrate each caller to construct events directly
(using `record`'s validation + `event_schema`). That choice is the substantive T7
design decision; archiving forge is gated on it (and on not-merging this session).

---

## 14. T7 migration plan (started 2026-05-27) — decision + inventory + the real gate

### Caller inventory (the migration surface — smaller than feared)
- **MCP-action wiring** (`work/table.go`): registers `forge` / `forge_edit` /
  `forge_delete` / `forge_schema(s)` as agent-facing actions.
- **`work/batch.go`**: dispatches `forge` + `forge_edit` ops in-tx.
- **arcreview auto-filer** (`arcreview/fallback.go` + `handler.go`, wired in
  `cmd/toolkit-server/main.go`): the F2/fallback pipeline auto-files bugs/
  suggestions via an injected `ForgeFn = forge.HandleForge` (schema+fields).
  **This is the dominant real Go caller, and it RELIES on forge's construction
  sugar** (slug-derive, dedupe, defaults, validation).
- **CLIs**: `chain-replay-verify` (byte-identity verify tool), the arc-fallback
  path in `main.go`.
- **Dashboard**: NONE — the frontend reads projections, never calls forge.

### Decision: (a) construction layer over `record`
Per §13, forge's construction sugar (slug-derive, dup-reject, defaults,
field-validation/coercion, guards, chain-fan-out) is **load-bearing** — the
arcreview auto-filer and agent ergonomics depend on it. So **(a)**: forge's
construction is preserved as a layer that emits through the `record` path,
rather than **(b)** rewriting each caller to build raw events (which would drop
the sugar — a real behavior regression, especially for the auto-filer). (a) is
the behavior-preserving strangler; forge's char-net keeps passing.

### What "archive forge" then means (reframe)
forge's construction *logic* is not deleted — it becomes (or is wrapped as) the
construction layer over `record`. What's retired is the forge *surface* as the
canonical write path: agents + callers use `record` (for raw events) and the
construction helper (for schema+fields convenience), both emitting through the
one `record`/events substrate. "Archive forge" = retire the standalone forge
action/strategy path in favor of that, moving the superseded code to `archive/`.

### Cutover plan + rollback
1. Build the construction layer (forge-sugar → `record` emission). *The big piece.*
2. Re-point callers: arcreview `ForgeFn`, `work.batch` forge ops, the table wiring.
3. Prove parity: forge's characterization net + the §9.B substrate ACs, green.
4. Move forge's superseded code to `archive/` per repo convention.
5. Update docs (CONVENTIONS, CLAUDE.md, TELEMETRY_CONSOLIDATION, this doc).
Rollback: single revert of the cutover commit; forge code restorable from `archive/`.

### The gate (REAL — and distinct from T6's false gate)
Unlike T6 (which I wrongly called gated and then simply did), T7 is genuinely
blocked on three real things: (1) **building the construction layer** is
substantial new work; (2) it rewires the core write path, so it wants
**explicit sign-off**; (3) **archiving forge lands by merging to `main`**.
Forcing the cutover before full parity would be the archive-before-parity
anti-pattern (`code-migration-discipline`).

---

## 15. T7 full-removal execution stages (2026-05-27, approved)

Sign-off + merge are now granted (full removal). forge is ~18.9K lines; this is
a multi-stage, multi-session, production-write-path project. Approach:
**re-home, don't re-implement** (reuse forge's helpers — e.g. the exported
`SlugifyTitle` — so the layer can't drift), **incremental, parity-gated, merge
per stage**, and **forge is archived only at the final stage, when its full
characterization net is green against the new layer.**

**Architectural note (chain 321 retroactive cleanup, 2026-05-27).** The
construction layer lives in **`go/internal/construct/`** as its own package,
not in `work/`. The single agent-facing public entry is
**`construct.Create(ctx, deps, schema, project, in construct.Input)`** — one
call that internally dispatches by schema name and orchestrates the full
sequence (validate → dup-check → build typed event(s) → submit through record →
index-sync). Per-schema builders are package-private; per-schema typed Inputs
ride the discriminated-union `construct.Input{Bug *BugInput, Chain *ChainInput,
ChainWithTasks *ChainWithTasksInput, Task *TaskInput, Memory *MemoryInput,
Retrospective *ChainAnchoredDocInput, ReportCard *ChainAnchoredDocInput,
Migration *MigrationInput, Suggestion *SuggestionInput}` struct (the canonical
Go pattern over bare `any` — see vault
`reference/2026-05-15_go-mcp-dispatch-typed-returns-pattern.md`). Stage 3-6
plans below are written against this architecture.

- **Stage 1 — construction layer for bug + suggestion.** DONE: `construct.Create`
  with `Input.Bug` / `Input.Suggestion` applies forge's sugar (required-
  validation, slug-derive via `forge.SlugifyTitle`, severity/priority=medium
  defaults) and emits via `record`; parity tests vs `forge(bug)`/`forge(suggestion)`
  (byte-identical projection) + slug-derive parity for both. Additive.
- **Stage 2 — remaining event-emitting create schemas.** DONE: chain (with
  fan-out via `Input.ChainWithTasks`), task, memory, retrospective, report-card,
  migration; plus cross-cutting guards re-homed (B-D1 duplicate-slug,
  B-F3 index-sync, B-G2 double-dated-slug — all run *inside* `construct.Create`
  for the schemas they apply to). The deltas vault-note (no event), bench /
  trained_model (direct-write), roadmap (no schema), and the B-G1 placeholder
  guard (edit-path) routed to Stage 3+ explicitly — see §15 boundary section
  below.
- **Stage 3 — edit + delete.** Add `construct.Update(ctx, deps, schema, project, in Input) (UpdateResult, error)`
  and `construct.Delete(ctx, deps, schema, project, slug string) (DeleteResult, error)`,
  mirroring `Create`'s dispatch + orchestration shape. Internal sequence per
  arm: validate Input→schema → build the typed edit/delete event → run the
  edit-path guards (**B-G1** placeholder guard for edits — re-home via
  `forge.firstPlaceholderShapedField`; **B-ED1/2/3** set-by-lifecycle-field
  rejections; **B-DEL1** no-hard-delete policy) → submit through `record` →
  index-sync where applicable. Per-schema typed Inputs reuse the existing
  fields where possible (e.g. `BugInput.Severity` works for update too) or
  add edit-shape variants (`BugEditInput` if needed for set-but-empty
  semantics). Edit-path markdown-doc semantics (B-ED3): retrospective /
  report-card / memory edits update the file + the projection. Parity-test
  every schema's edit / delete vs `forge_edit` / `forge_delete`.
- **Stage 4 — migrate the Go callers.** arcreview `ForgeFn`, `work.batch`'s
  forge ops, the `work` table forge action, and the CLIs route through
  `construct.Create` / `construct.Update` / `construct.Delete` (NOT the
  package-private `buildX` functions). Each caller becomes a one-call site:
  caller hands in the schema name + the typed `Input`; the umbrella does the
  rest. Behavior-preserving (the forge char-net + the construct parity tests
  guard).
- **Stage 5 — agent surface + docs.** Layer a forge-shaped MCP affordance on
  `construct.Create` so the agent's external call shape doesn't change when
  forge archives — the existing `record` action gains a forge-equivalent input
  mode that accepts `{schema_name, fields}` and internally constructs the
  matching `Input.X` then calls `construct.Create`. The raw `events[]` mode
  stays available for the unusual cases (lifecycle events, multi-event
  sequences) where it earns its complexity. Update agent guidance
  (CONVENTIONS.md, CLAUDE.md, this doc).
- **Stage 6 — archive forge.** Move the 8 exported forge seams the construct
  package depends on into `construct/` (the dependency inverts): `SlugifyTitle`,
  `RejectDuplicateBySlug`, `IndexSyncFromProjection`, `WriteMemoryArtifact`,
  `WriteChainAnchoredDoc`, `WriteMigrationArtifact`, `CheckDoubleDatedSlug`,
  `substrateMaxMigrationNumber`. Re-point forge's full 33-file characterization
  net at `construct.Create` / `Update` / `Delete`; the char-net green is the
  archive gate. Move forge to `archive/`. Includes the deferred cutover
  decisions: vault-note's file + knowledge_pointer logic re-homes into
  `construct.Create` (it becomes the layer per §14); bench / trained_model
  event-source-or-keep decision is made here. Final docs.

Stages 1–5 are additive (forge stays live) and merge safely as they land;
Stage 6 is the cutover, gated on the full char-net passing against the layer.

### Resuming T7 cold (start here in a fresh session)

**State (as of chain 321 closure):** T1–T6 + T7 Stages 1-2 are merged to
`main`. The record-construction layer lives in **`go/internal/construct/`** as
a dedicated package, with **`construct.Create`** as the public agent-facing
umbrella. forge is fully intact (Stage 4 hasn't routed callers through
construct yet). The `record` surface is live.

**The pattern to copy when extending the layer (e.g. Stage 3's `Update`/`Delete`,
or a new create schema):**

1. Add a typed Input struct in the right bucket file:
   - **`go/internal/construct/event_sourced.go`** for event-folded schemas
     (bug/suggestion/chain/task) — pure builder, no I/O.
   - **`go/internal/construct/file_schemas.go`** for markdown / SQL artifact
     schemas (memory/retro/report-card/migration) — writes a file via the
     re-homed forge helper, returns the event.
   - **`go/internal/construct/guards.go`** for cross-cutting guards.
   - **`go/internal/construct/index.go`** for B-F3 sync.

2. Add the per-schema build function (package-private, e.g. `buildEditX`):
   validate required fields, derive the slug via `forge.SlugifyTitle`
   (reuse — B-G3), apply defaults, marshal the typed `events.XPayload`,
   return a `work.RecordEvent`. For file schemas, write the artifact via the
   exported forge helper first; for chain+tasks fan-out style cases, return
   `[]work.RecordEvent`.

3. Wire the schema into the umbrella (`construct/create.go` or the parallel
   `update.go`/`delete.go` for Stage 3): add the `case "<schema>"` arm to
   `dispatchBuild`/`dispatchUpdate`/`dispatchDelete`, the `requireExactly`
   line in `validateInputMatchesSchema`, and (if applicable)
   `shouldDupCheck` / `needsIndexSync`.

4. Add a parity test in the matching `_test.go` file calling
   `construct.Create(ctx, deps, "<schema>", project, construct.Input{X: &...})`
   alongside `forge(schema, {fields})`, comparing projection / file / pointer
   byte-identically.

5. Add a build-logic-isolation test in `builders_internal_test.go` if the
   shape concerns (slug-derive, rejection-message, optional-field omission)
   need pinning at the per-builder level.

**Discipline:** re-home (reuse forge's helpers via the exported seams);
additive (do NOT touch forge's behavior — only export new seams as needed);
parity-gate every schema; run `scripts/precommit.sh` before each commit
(`golangci-lint`'s `forbidigo` rule rejects bare `any` outside `internal/db`
+ `internal/dispatch` — use typed sum-struct Inputs); merge per coherent
slice. Do NOT archive forge until Stage 6 (its full char-net green against
the construct layer is the archive gate).

**Gotchas:** the Go module is at `go/` (`make -C go test`, or
`cd go && go test -tags sqlite_fts5 ./...`). If you spawn subagents in a
worktree, pin them to the worktree path + forbid the CLAUDE.md `cd ~/dev/...`
reflex (see docs/MULTI_AGENT_WORKTREE_WORKFLOW.md §0). After a Go change,
deploy is `make -C go build` on `main` + `/mcp reconnect`. Live MCP version
check: `admin.server_version` vs `git rev-parse HEAD`.

### Stage 2 — the create-schema boundary (settled 2026-05-27)

Stage 2 split into the parent chain's first slice (event-sourced creates) +
its own follow-on chain **`record-layer-stage2-additive-remainder`** (chain
320) for the additive remainder. Building it surfaced that the create schemas
are **not uniform** — they fall into five buckets by *how the artifact is
written*, and the record construction layer's defining remit is **create
schemas that EMIT an event** (those route through `record`). What landed and
where the boundary sits:

**COVERED — event-emitting create schemas, all dispatched via
`construct.Create(ctx, deps, "<schema>", project, construct.Input{...})`
(each parity-gated, byte-identical vs forge; merged to `main`):**
- **bug, suggestion** — `Input.Bug` / `Input.Suggestion`. Stage 1.
- **chain, task (+ chain+tasks fan-out)** — `Input.Chain` (bare) or
  `Input.ChainWithTasks` (atomic fan-out: ChainCreated + N TaskCreated +
  optional ChainAndTasksForged grouping signal) / `Input.Task`; projection-row
  parity.
- **memory** — `Input.Memory`. Build writes the file via
  `forge.WriteMemoryArtifact`; FILE byte-identical + `proj_memories` parity.
- **retrospective, report-card** — `Input.Retrospective` / `Input.ReportCard`.
  Build writes the file via `forge.WriteChainAnchoredDoc`; FILE +
  `knowledge_pointer` parity. (Documented delta: the retro
  `captureOrphanedFollowons` next-chain capture gate is NOT re-homed — forge
  still runs it when invoked directly; a negative-pin test in
  `construct/file_schemas_test.go` catches a future re-home accident.)
- **migration** — `Input.Migration`. Build writes via
  `forge.WriteMigrationArtifact`; canonical+mirror `.sql` byte-identical,
  EXPLAIN-check + idempotency. Idempotent re-build returns `idempotent=true`
  in the payload and writes no new file.

**Cross-cutting guards run automatically inside `construct.Create`:** **B-D1**
duplicate-slug reject (bug/suggestion/chain/task — via the exported
`forge.RejectDuplicateBySlug`); **B-F3** knowledge-index sync (Indexed DB
schemas: chain, task, bug — including each task in a chain+tasks fan-out; via
`forge.IndexSyncFromProjection`); **B-G2** double-dated-slug
(`construct.RejectDoubleDatedSlug` is available as a standalone primitive but
the `Create` dispatch table doesn't currently call it because the only
existing double-dating schema, vault-note, is a §15 delta below). **B-G1**
placeholder guard is **edit-path only** → Stage 3.

The guards + `construct.SyncCreateIndex` stay exported as composable
primitives — callers that want different ordering can compose them, but the
default path through `construct.Create` runs them in the canonical order
internally.

**DELTAS — out of the Stage-2 record-construction-layer remit (forge stays the
writer; these are §10 honest-boundaries, sequenced — NOT dropped):**
- **vault-note** — emits **no event** (B-EV2), so nothing routes through
  `record`; it is the one create schema outside the layer's event-emitting
  remit. Its file + knowledge_pointer logic is re-homeable but belongs to the
  **Stage 6** cutover (where forge's construction logic *becomes* the layer),
  not Stage 2. (Its B-G2 guard is already re-homed above, ready for it.)
- **bench, trained_model** — write their DB row **directly** (no projection
  fold); `trained_model` emits no event at all. Event-sourcing them needs NEW
  substrate (a `BenchmarkForged`→`bench_harnesses` fold; a new
  `trained_model` event + fold) AND stopping forge's direct write — which is
  **non-additive**, so it needs an explicit decision before Stage 6, not an
  additive Stage-2 slice.
- **roadmap** — has **no forge schema** (handled by `roadmap_insert/set/update`);
  excluded entirely.

Net: every event-emitting forge CREATE schema now has a parity-gated record
construction-layer entry. The deltas are bounded + documented; Stage 3
(edit/delete) and Stage 4 (caller migration) build on this set.

## 16. P2-C.2 — the forge archive: execution design (chain 311 T7 Stage 6, 2026-05-28) — ✅ DONE (2026-05-29)

> **STATUS — DONE.** forge's top-level package is archived to `archive/forge/`
> (out of the `go/` build); the `forge/registry` + `forge/fieldvalue` subpackages
> stay. The agent write surface is now **construct** (`HandleForgeCreate/Edit/
> Delete` → `PrepareForge*` → `Create`/`UpdateFromForge`/`Delete` → `FinalizeForge*`)
> reached via the work-table `forge`/`forge_edit`/`forge_delete`/`forge_schema(s)`
> actions (the action NAMES survive — only the implementation moved) and the
> `record` forge-shaped sugar. §15's residual delta survivors (vault-note
> create/edit, bench/trained_model edit) re-homed into construct. Pipe-delimited
> chain tasks are rejected (deprecated). A precommit forbidden-pattern guard bans
> new bare `toolkit/internal/forge` imports. The forge characterization net moved
> dormant into `archive/forge/` with its subjects; construct's own suite +
> `construct/handle_test.go` are the live gate.

This section is the precise execution guide for the archive commit — written
after the P2-C.1 minimal sever landed (`5f2e13ea`), once a full scope inventory
revealed the archive's true shape. **Read this before touching code.** It
resolves the two mechanical decisions the earlier plan left open and gives a
per-file disposition + ordered sequence.

### 16.0 The decisive structural fact: the archive is ONE unstageable commit

The 8 seams + construction logic **cannot** be moved out of `forge` incrementally:
`forge`'s own `handler.go` / `strategy.go` / markdown-create bodies *use* those
helpers (`slugifyTitle`, the write/validate/index helpers), so deleting them from
`forge` breaks `forge`; re-exporting them as wrappers makes a `forge→construct→forge`
import **cycle**. Therefore forge's still-live construction + parse logic must all
land in `construct` in the *same commit* `forge` is removed from the build — there
is **no intermediate compiling state**. `archive/` is OUTSIDE the `go/` module, so
`git mv go/internal/forge → archive/` *removes forge from compilation* (it does not
merely relabel). Plan accordingly: the commit is large and atomic; the gate is run
once on the assembled result; rollback is a single revert.

`forge` is ~19.1K lines production (33 files) + ~10.8K lines test (33 char-net
files). But NOT all of it MOVES — much is SUPERSEDED (construct already builds the
covered schemas' events natively in `event_sourced.go`) and RETIRES. The net new
construct code is the parse front + the markdown/edit/index/guard bodies construct
currently reaches via seams — roughly 6–9K lines, not 19K.

### 16.1 Resolved decisions

- **(a) Parse-front home = `construct`.** `forge.PrepareForge` / `PrepareForgeEdit`
  / `PrepareForgeDelete` (parse params → schema lookup → project gate → field
  extract → `Validate`/`ValidatePartial` → slug-derive → chain-tasks peel) move into
  construct as `construct.PrepareCreate` / `PrepareEdit` / `PrepareDelete`, returning
  a construct-owned `Prep` type (replacing `forge.ForgePrep`). `from_forge.go`'s
  `InputFromForge(forge.ForgePrep)` becomes `InputFromPrep(construct.Prep)`. The
  agent envelope shapes + every rejection message stay byte-identical (the
  char-net pins them). The `record`-sugar dispatch in `cmd/toolkit-server`
  (`handleAgentForge`/`Edit`/`Delete` → renamed `handleRecordCreate`/…) calls the
  construct prep + the shared finalize tail, which also moves into construct
  (`construct.FinalizeCreate` / `FinalizeEdit`: SSE publish + burst/deferral nudges
  + the create/edit result envelope).
- **(b) Char-net relocation = into the live suite, pointed at construct.** The 33
  `internal/forge/*_test.go` files MOVE to `internal/construct/` (or a dedicated
  `internal/construct/charnet/` test package) and are repointed from `forge.HandleForge`
  / `forge.HandleForgeEdit` / `forge.HandleForgeDelete` to the construct entry
  points. They must NOT go to `archive/` (not compiled there → gate would stop
  running). Their green is THE archive gate. Tests that exercise forge-internal
  helpers directly (e.g. `strategy_test.go`'s `GenericStrategy` exemplar,
  `helpers_internal_test.go`) either move with their subject into construct or are
  dropped if their subject retires (note each drop per the "no silent caps" rule).

### 16.2 Per-file disposition (forge production files)

MOVE = relocate body into construct; RETIRE = superseded by construct's native
path, delete; DELETE-shim = P2-B compat shim, delete + repoint callers to
`fieldvalue`; RESHIM = re-home onto a registry-backed shim.

| forge file | lines | disposition | target / notes |
|---|---|---|---|
| `values.go` | 80 | DELETE-shim | repoint the ~3 remaining `forge.*` value refs to `fieldvalue`; body already in `fieldvalue` (P2-B) |
| `validate.go` | 48 | DELETE-shim | `Validate` forwarder → move the real `Validate` call into construct prep; `fieldvalue.Validate` is the leaf |
| `placeholder_guard.go` | 70 | MOVE | → `construct/guards.go` (`FirstPlaceholderShapedField`, B-G1) |
| `delete.go` | 36 | MOVE/RETIRE | generic Delete → `construct` generic-delete arm (construct.Delete already exists; fold in `IndexDeleteForArtifact`) |
| `deferral_nudge.go` | 82 | MOVE | → construct finalize tail (the deferral-capture nudge) |
| `forge_burst_tracker.go` | 106 | MOVE | → construct finalize tail (burst nudge) |
| `retrospective_followon_capture.go` | 149 | RETIRE (documented delta) | construct does NOT run the capture gate (file_schemas.go negative-pin test); confirm + drop, or move if we decide to honor it |
| `bench.go` | 205 | MOVE | `ResolveBenchFields` + bench create body → construct/bench.go (construct already has buildBench calling the seam) |
| `types.go` | 209 | MOVE | result types (`CreateResult`/`EditResult`/`EditOpts`/`DeleteResult`) → construct (some already mirrored) |
| `introspect.go` | 239 | RESHIM | `forge_schema` / `forge_schemas` → registry-backed shim in `work` (introspection is registry-level, not forge-construction) |
| `render.go` | 271 | MOVE | markdown render (`renderMarkdown`) → construct (file_schemas write path needs it) |
| `chain_tasks.go` | 324 | MOVE | chain-tasks peel + `ChainTaskEntry`/`ChainTaskModeFull` → construct prep (from_forge already consumes the mode const) |
| `hooks.go` | 395 | MOVE/RETIRE | chain skeleton fan-out hook → construct (the fan-out is now `ChainWithTasksInput`); most hook machinery RETIRES (construct dispatches natively) — verify nothing else rides hooks |
| `edit.go` | 443 | MOVE | `EditDBInTx` / `ValidatePartial` / set-by gate → construct update path |
| `migration.go` | 619 | MOVE | `WriteMigrationArtifact` + migration create body + `substrateMaxMigrationNumber` + `CheckDoubleDatedSlug` → construct (file_schemas + guards) |
| `edit_markdown.go` | 756 | MOVE | `EditMarkdownArtifact` / `EditMemoryArtifact` markdown-edit bodies → construct update path |
| `create.go` | 835 | MOVE/RETIRE | the 8 seams + `WriteMemoryArtifact`/`WriteChainAnchoredDoc`/`ChainAnchor`/`RepoRelativePath`/`ResolveMarkdownRoot`/skeleton-field helpers → construct; the `Create` orchestrator RETIRES (construct.Create is the orchestrator) |
| `strategy.go` | 862 | MOSTLY RETIRE | event-emitting per-shape strategies are SUPERSEDED by construct's `event_sourced.go`; KEEP→MOVE only `vaultNoteStrategy` (no-event file+pointer arm) + `GenericStrategy.Edit/Delete` (generic UPDATE/DELETE → construct generic arm, for bench/trained_model edit). The Strategy interface + registry + completeness gate RETIRE |
| `indexsync.go` | 889 | MOVE | `IndexSyncFromProjection` + `IndexUpsertOnCreateInTx`/`OnEditInTx` + notifiers + pointer builders → construct/index.go |
| `handler.go` | 1674 | MOVE/RETIRE | `Prepare*` + `Finalize*` + `extractFields`/envelope/`Validate` call + the rejection-envelope builders → construct prep+finalize; the `HandleForge*` standalone action entrypoints + `ExecutePrepared*` RETIRE (record-sugar is the surface; vault-note's delta persistence folds into construct.Create's no-event arm) |
| `doc.go` | 29 | RETIRE | package doc |

### 16.3 Importer repoint (the 12, incl. work + main)

- **`work/table.go`**: REMOVE the `forge` / `forge_edit` / `forge_delete` action
  registrations (record-sugar is the surface). RESHIM `forge_schema` / `forge_schemas`
  onto the registry. Drop `forge.WithCoreHooks`/`WithCoreStrategies`/
  `ValidateStrategyRegistry`/`NewForgeBurstTracker`/notifier wiring (moved/retired).
- **`work/batch.go`**: `forge.Deps`/`HandleForgeInTx`/`HandleForgeEditInTx`/
  `IndexUpsertOnCreateInTx` → the batch path already routes covered schemas through
  `construct.CreateInTx`/`UpdateInTx`; repoint the index seam + drop the forge fallback
  (batch's allowlist is all-covered).
- **`cmd/toolkit-server/main.go`**: the big one — `handleAgentForge`/`Edit`/`Delete`
  rebuilt on `construct.PrepareCreate`/`Edit`/`Delete` + `construct.Finalize*`; drop
  `forge.*` (Create/Execute*/Finalize*/Prepare*/notifiers/GenericStrategy/etc.);
  the 2 `map[string]forge.FieldValue` finalize-deps signatures → `fieldvalue.FieldValue`;
  `arcFallbackForge` (arcreview auto-filer `ForgeFn`) repoints to `construct.Create`.
- **construct ×9**: drop the `forge` import once the bodies are local; `forge.EditResult`/
  `EditOpts`/`ForgePrep`/`ChainTaskModeFull` → construct-local types; the seam calls →
  local functions.

### 16.4 Ordered execution sequence (within the one commit)

1. **Land the parse front + finalize tail in construct** (`construct/prepare.go`,
   `construct/finalize.go`): port `PrepareForge`/`Edit`/`Delete` + `Finalize*` +
   `extractFields`/envelope/rejection builders. Add `construct.Prep` type.
2. **Move the construction bodies** per 16.2 into their construct target files;
   repoint construct's internal `forge.X` → local. construct now imports NO forge.
3. **Repoint `from_forge.go`** → `InputFromPrep(construct.Prep)`.
4. **Repoint `main.go` + `work/batch.go` + `work/table.go`** off forge (16.3).
5. **Move + repoint the 33 char-net test files** into construct, pointed at the
   construct entry points. **RUN THEM — green is THE GATE.** (gate right here.)
6. **RESHIM `forge_schema(s)`** onto the registry.
7. **`git mv go/internal/forge archive/forge`** (out of the build). Confirm
   `make -C go build` + full `precommit.sh` green (suite + replay + govulncheck +
   dashboard + the relocated char-net).
8. **Docs sweep**: CONVENTIONS.md, CLAUDE.md, this doc (§15/§16 → "DONE"),
   TELEMETRY_CONSOLIDATION; add precommit forbidden-pattern guards banning new
   `toolkit/internal/forge` imports + the retired forge-action names.

### 16.5 Gate + rollback

GATE = the relocated char-net green (16.4 step 5) + full `precommit.sh` on the
assembled result (16.4 step 7): Go suite + `chain-replay-verify` byte-identical +
govulncheck + dashboard. HEADS-UP THE USER before the commit lands. Rollback =
single revert of the archive commit; forge code restorable from `archive/forge/`.
Post-merge: `make -C go build` in the MAIN checkout + `/mcp reconnect` + live smoke
(forge(bug)/record-sugar create + a vault-note + a trained_model create through the
deployed binary).

### 16.6 Open items to verify AT execution (don't trust this doc blindly)

- `hooks.go`: confirm the ONLY live hook is the chain skeleton fan-out (now
  `ChainWithTasksInput`); if other hooks ride the registry, they move too.
- `retrospective_followon_capture.go`: confirm it's a documented delta construct
  deliberately skips (negative-pin test) before dropping; if it must be honored,
  it MOVES into construct.Create's retrospective arm.
- `strategy.go`: re-confirm every event-emitting strategy is byte-parity-superseded
  by construct's `event_sourced.go` builder before retiring it (the char-net is the
  proof — if a char-net test fails after the repoint, that strategy's logic wasn't
  fully ported).
- The `forge.*` types crossing the work boundary (`CreateResult`/`EditResult`):
  decide construct-owned vs a tiny shared types package, to avoid a work→construct
  type dependency if one doesn't already exist.
