# Phase 4 — Legacy Free-Form Field Deprecation

Chain: `phase-4-legacy-field-deprecation` (id 584). F1 design doc.
Companion to `docs/AGENT_AUDIT_AND_MIGRATION.md` §6 Phase 4 ("Cut the frontend over") and `docs/SUBSTRATE_CRUD_RETIREMENT.md` (the cousin chain that retired the CRUD tables themselves).

## 1. Context

The cousin chain `agent-substrate-crud-retirement` (closed 2026-05-21, migration 060) dropped the artifact CRUD tables. The legacy free-form fields — `resolution_note`, `design_decisions`, `output`, `completion_condition`, `handoff_output`, `constraints`, `acceptance_criteria` — now live only on the projection tables (`proj_current_bugs`, `proj_current_suggestions`, `proj_current_chains`, `proj_current_tasks`), populated by the projection fold modules from event payloads.

Phase 4's scope: decide which of these projection columns + their dashboard render surfaces are duplicates of the typed events ledger (and therefore retire) and which are genuinely load-bearing read-path state (and therefore keep). For each retire candidate, verify the EventTimeline already surfaces the equivalent typed signal.

**Bake-in gate.** The chain's original creation-time `design_decisions` mandated a 30-day calendar gate on agent-substrate-frontend bake-in plus operator-adoption signals. Gates #1/#2/#3 were waived by the user on 2026-05-21 on the basis that the cousin chain's byte-identical rebuild-from-events validation (`scripts/equivalence-harness.sh`) provides stronger substrate proof than the calendar gate was designed for. Gate #4 (no dual-write fallback) is met automatically as a side effect of the cousin chain. Recorded risk: dashboard operator-level adoption signal hasn't accumulated; if a retired prose field carried signal the EventTimeline doesn't yet surface, regression discovery is post-retirement.

## 2. Audit surface set

For each field, F1 inspected:

- the source event payload(s) that carry it (`blueprints/events/*.json`),
- the projection fold module that writes it (`go/internal/projections/*.go`),
- the observe-http handler that surfaces it (`go/internal/observehttp/*.go`),
- the dashboard render site (`apps/dashboard/src/pages/**`, `apps/dashboard/src/components/shared/EventTimeline/per-type-renderers.tsx`),
- the forge schema input acceptance (`blueprints/forge-schemas/*.toml`).

## 3. Per-field disposition matrix

| Field | Source events | Projection col | Observe-http | Dashboard render | EventTimeline render | **Disposition** |
|---|---|---|---|---|---|---|
| `bug.resolution_note` | `BugResolved.payload.resolution_note` | `proj_current_bugs.resolution_note` | bugs detail JSON | `BugDetailPanel.tsx:91` (prose block, resolved-only) | `bugResolved` renderer shows it implicitly via the structured payload view | **RETIRE** |
| `suggestion.resolution_note` | `SuggestionResolved.payload.resolution_note` | `proj_current_suggestions.resolution_note` | suggestions detail JSON | `SuggestionDetailPanel.tsx:95` (prose block, resolved-only) | `bugResolved`-shaped renderer (suggestions reuse the bug-resolved shape per `agent-suggestion-box`) | **RETIRE** |
| `chain.design_decisions` | `ChainCreated.payload.design_decisions`, `ChainEdited.updated_values` | `proj_current_chains.design_decisions` | `chains.go:89,109` chain detail JSON | `ChainIndex/index.tsx:610` (prose block) **and** `per-type-renderers.tsx:236` (timeline "Decisions" row, truncated 200) | Yes, truncated 200 chars in `chainCreated` renderer | **RETIRE** (with caveat) |
| `chain.output` | `ChainCreated.payload.output`, `ChainEdited.updated_values` | `proj_current_chains.output` | `chains.go:88,109` | `ChainIndex/index.tsx` `ProseSection` | Yes, truncated 200 in `chainCreated` renderer | **KEEP** |
| `chain.completion_condition` | `ChainCreated.payload.completion_condition`, `ChainEdited.updated_values` | `proj_current_chains.completion_condition` | `chains.go:90,109` | `ChainIndex/index.tsx:609` `ProseSection` | **NO** — `chainCreated` renderer skips it (gap, see §5) | **KEEP** |
| `task.handoff_output` | `TaskCreated.payload.handoff_output`, `TaskEdited.updated_values`, **`TaskCompleted.payload.closure_summary`** (overwrites — see §5) | `proj_current_tasks.handoff_output` | `tasks.go:143,196` search content | Not rendered as a prose block (no `TaskIndex` page) | Not surfaced as a row | **MIGRATE-TO-EVENT** (column-shape fix first) |
| `task.constraints` | `TaskCreated.payload.constraints`, `TaskEdited.updated_values` (also `BugReported.payload.constraints`, `SuggestionReported.payload.constraints` for sibling kinds) | `proj_current_tasks.constraints` (mirrored on bugs/suggestions) | `tasks.go:143,147` search content | Not rendered as a prose block | Not surfaced as a row | **KEEP** |
| `task.acceptance_criteria` | `TaskCreated.payload.acceptance_criteria`, `TaskEdited.updated_values` (also `BugReported`, `SuggestionReported`) | `proj_current_tasks.acceptance_criteria` (mirrored on bugs/suggestions) | `tasks.go:142,146` search content | Not rendered as a prose block | Not surfaced as a row | **KEEP** |

### Retire-disposition rationale

- **`bug.resolution_note`, `suggestion.resolution_note`** are pure caches of the resolving event's payload. The EventTimeline's `BugResolved` / `SuggestionResolved` per-type-renderer already shows the relevant context (kind, commit_sha, routed targets); the structured-payload drawer surfaces the full `resolution_note` value on click. The standalone prose block at the top of the detail panel is the duplicate.
- **`chain.design_decisions`** is the multi-paragraph rationale written at chain creation (and amended via `ChainEdited.updated_values`). The `chainCreated` per-type-renderer truncates it to 200 chars for the timeline row; the structured drawer shows the full payload. The top-of-page `ProseSection` is the third surface and the one F3 deletes.

  **Caveat:** this is the largest UX shift in Phase 4. `chain.design_decisions` is often multiple kilobytes (this chain's own value is ~5KB). Today operators see it above the fold on the chain detail page; post-retirement they will need to open the `ChainCreated` event row in the timeline and click to drawer. F3 must confirm the drawer renders multi-paragraph rationale readably (line-wrapping, scrolling) before merging. Truncation in the timeline row itself stays.

### Keep-disposition rationale

- **`chain.output`**: one-sentence chain identity (per `forge-schemas/chain.toml`: "what exists when this chain is complete"). Functions as the chain's tagline, not as rationale. Belongs visible above the timeline, not behind a drawer click.
- **`chain.completion_condition`**: observable acceptance test for chain closure; F4 of every chain reads this to verify chain done-ness. Must be scannable above the fold.
- **`task.constraints`, `task.acceptance_criteria`**: pre-emission plan content (chain framing explicitly cites `acceptance_criteria` as the canonical "keep" example). Used as observe-http search content. No prose-block dashboard render exists to retire.

### Migrate-to-event-disposition rationale

- **`task.handoff_output`** is structurally confused (see §5 finding 1). The column is *not* rendered as a prose block in the dashboard — no `TaskIndex` page exists; the only observable read site is the observe-http search response snippet. The column's purpose has effectively drifted into "free-text search index for task closing context", which the cousin chain's own retro flagged via the dual-write story. F2 picks one of three resolutions:
  - **(M1)** Rename the column to `closure_summary` to match what `foldTaskCompleted` actually writes; preserve the pre-completion handoff_output value as a separate event-payload addition (`TaskCompleted.payload.handoff_output`, additive bump);
  - **(M2)** Drop the column entirely and migrate observe-http task search to FTS5 over the events table (consistent with the cousin chain's bugs_fts/suggestions_fts pattern);
  - **(M3)** Keep the column name and fix `foldTaskCompleted` to stop overwriting it with `closure_summary`; preserve the pre-set handoff_output across the TaskCompleted fold (status flips to closed, but the column carrying the pre-set handoff context survives).

  F2 makes the choice. Recommendation: **(M3)** — least disruptive, fixes the shape collision, preserves observe-http search behavior. (M1)/(M2) are larger lifts that don't match Phase 4's "tactical cleanup" scope.

## 4. Surfaces to update in F2 and F3

### F2 (backend cutover)

For each **RETIRE** field:
- Projection fold module: delete the `excluded.<field>` clause from the upsert ON CONFLICT and remove the field from the SQL column list. Add a migration to drop the column from the projection table (consistent with the cousin chain's approach to dropped state).
- Observe-http handler: remove the column from the SELECT list and from the response JSON struct.
- Forge schema: mark the field deprecated in the schema validator so `forge_edit` rejects new writes. (Decision: hard-reject vs. silently ignore. Recommend hard-reject — agents need to learn the new contract.)
- Forge handler in `go/internal/forge/indexsync.go`: drop the case branches that route the field to the projection write.

For the **MIGRATE** field (`task.handoff_output`), apply F2's chosen resolution (M3 recommended) — F2 documents which.

### F3 (dashboard cutover)

For each **RETIRE** field:
- Delete the JSX block from the corresponding detail panel:
  - `bug.resolution_note` → `apps/dashboard/src/pages/BugIndex/BugDetailPanel.tsx:91-98` (the `{detail.resolution_note && …}` block).
  - `suggestion.resolution_note` → `apps/dashboard/src/pages/SuggestionIndex/SuggestionDetailPanel.tsx:95-102`.
  - `chain.design_decisions` → `apps/dashboard/src/pages/ChainIndex/index.tsx:610` `ProseSection` + the `design_decisions.trim()` check at line 578.
- Delete the duplicating per-type-renderer block in `apps/dashboard/src/components/shared/EventTimeline/per-type-renderers.tsx`:
  - The `payload['design_decisions']` row at line 236-241 (the "Decisions" `PayloadRow`).
- Delete the dormant duplicates:
  - `apps/dashboard/src/pages/_dormant/WorkSearch/index.tsx:405-407` (dormant page; clean as a sweep).
- Update Vitest snapshots; delete the resolved-bug-renders-resolution-note tests in `BugDetailPanel.test.tsx` / `SuggestionDetailPanel.test.tsx` since the rendering site is gone (the timeline-side rendering is covered by EventTimeline tests).
- Manual visual sign-off per the global feedback-frontend-visual-verify memory: the chain detail page in particular needs an operator look (largest UX shift).

For each **KEEP** field, no dashboard work.

### Forge-schema / dispatch policy

- The forge schemas (`blueprints/forge-schemas/{bug,chain,task,suggestion}.toml`) currently accept the retired fields as `forge_edit` inputs. F2 marks them deprecated; downstream `forge_edit` calls that supply them get rejected with a clear migration hint.
- The dispatch policies don't gate any retired field individually — no action required there.

## 5. Adjacent findings surfaced during the audit

These are not blocking Phase 4 but were uncovered during the field inventory. Each gets filed independently rather than absorbed into F2/F3.

### Finding 1 (file as bug): `foldTaskCompleted` overwrites `handoff_output` with `closure_summary`

`go/internal/projections/tasks.go:189-194` writes `closure_summary` into the `handoff_output` projection column at TaskCompleted fold time. This means the column's content changes meaning across the lifecycle (pre-completion: handoff plan; post-completion: closure summary). Search results route the post-completion value to the `handoff_output` field label, which mislabels it for any operator reading the search snippet. This shape collision pre-dates Phase 4 and is the root motivation for the **MIGRATE-TO-EVENT** disposition on `task.handoff_output`. Resolution choice (M3 above) belongs in F2 or as a standalone bug — F1 surfaces it; F2 picks the home.

### Finding 2 (file as suggestion): `per-type-renderers.tsx` `chainCreated` skips `completion_condition`

`per-type-renderers.tsx:224-244` renders `tasks` count, `output`, and `design_decisions` for `ChainCreated`, but omits `completion_condition` despite the payload carrying it. Once `chain.design_decisions` retires from the prose-panel side, the timeline becomes the operator's primary read surface for chain-creation rationale; `completion_condition` should show there too. Suggested addition: a third truncated `PayloadRow` for `completion_condition`.

### Finding 3 (file as suggestion): per-type-renderer truncation may be too aggressive post-retirement

`per-type-renderers.tsx` truncates `design_decisions` to 200 chars for the timeline row. Pre-Phase-4, the prose-panel above the timeline carried the full text; the truncation was fine because clicking into the drawer was a secondary path. Post-retirement, the drawer becomes primary. The truncation stays for the timeline row (compact layout requires it) but F3 must confirm the structured-payload drawer renders untruncated and readably.

### Finding 4 (file as suggestion): observe-http chain detail surfaces always-empty retired fields

After F2 drops the retired columns from the projection, `chains.go:88-119`'s `Output`/`DesignDecisions`/`CompletionCondition` struct fields become "load from a column that doesn't exist post-migration." F2 must decide whether to (a) keep the JSON shape with always-empty strings for back-compat (no caller exists that depends on this; the dashboard is the only caller), or (b) drop the keys from the response struct. Recommend (b) — clean break.

## 6. F2 / F3 / F4 scope handoff

F2 input: this matrix. F2 mechanically executes RETIRE rows for the backend (fold module, observe-http, forge schema, migration) and picks the M-resolution for `task.handoff_output`.

F3 input: this matrix + F2's commit. F3 mechanically executes RETIRE rows for the dashboard (panel JSX deletion, per-type-renderer prose-duplicate deletion, snapshot updates, dormant-page cleanup) and does the manual visual sign-off on the chain detail page.

F4 input: F2 + F3 closed. F4 emits the closing `Phase4CutoverCompleted` event (new event type, mirroring `SubstrateFrontendAuditCompleted` / `ArchitectureAuditCompleted`), updates `docs/AGENT_AUDIT_AND_MIGRATION.md` §6 Phase 4 to "complete", and writes the retrospective.

## 7. Open questions for the user

None blocking F2. The four adjacent findings (§5) are deferrable to follow-ons. The biggest judgment call inside Phase 4 is the **caveat** under `chain.design_decisions` retire — operators losing the above-fold view of multi-paragraph chain rationale. F3's visual sign-off is the safety valve; if the drawer-rendering of the payload turns out to feel like a regression, F3 can recommend reverting that field's disposition to KEEP and emitting a partial-retirement closing event.

---

## 8. F4 retrospective (2026-05-21)

### Commit ledger

| Task | Commit | Summary |
|---|---|---|
| F1 | `44474aa` | Design doc + per-field disposition matrix landed. Bake-in gates #1/#2/#3 waived per user decision; suggestion.resolution_note + EventTimeline per-type-renderer added to audit surface; chain `design_decisions` addendum recorded the supersession. |
| F2 | `356ea48` | Migration 065 drops the three retired projection columns. Fold modules (bugs.go, suggestions.go, chains.go) stop writing them; `foldChainEdited` silently tolerates ChainEdited targeting `design_decisions` via `isRetiredChainColumn`. observe-http detail endpoints drop the JSON keys. Work-surface `Bug` / `Suggestion` / `ChainDetail` structs drop the fields. `fetchBug/SuggestionResolutionSnapshot` stop sourcing `resolution_note` for the BugReopened/SuggestionReopened previous_resolution payload. forge/indexsync's chain-pointer falls back to `completion_condition` only. **Equivalence harness re-verified**: every projection's row count matches live exactly post-rebuild; Vitest passes against rebuilt-DB-backed daemon. |
| F3 | `99b4e92` | Dashboard JSX deletions: `BugDetailPanel.tsx` + `SuggestionDetailPanel.tsx` + `ChainIndex/index.tsx` + `EventTimeline/per-type-renderers.tsx` (the duplicating `chainCreated` "Decisions" PayloadRow) + the dormant `WorkSearch` page. Type / adapter cleanup across `api/chains.ts`, `api/bugs.ts`, `api/suggestions.ts`, `lib/chainIndex.ts`, `lib/bugIndex.ts`, `lib/suggestionIndex.ts`. Vitest tests updated; 421/421 pass. F3 also addressed adjacent finding §5.2 by adding a "Completion" PayloadRow to `chainCreated` so the timeline now surfaces `completion_condition` (previously skipped). |
| F4 | _(this commit)_ | Closing retrospective + `ArchitectureAuditCompleted` event landed via `go/cmd/phase-4-legacy-field-deprecation-audit-emit`. `docs/AGENT_AUDIT_AND_MIGRATION.md` §6 Phase 4 marked complete with links to both this chain + the cousin chain. |

### Dispositions, executed

The matrix's three RETIRE rows landed exactly as designed. The four KEEP rows ship unchanged. The one **MIGRATE-TO-EVENT** row (`task.handoff_output`) was revised at F2 start to **KEEP** when the F2 audit confirmed no dashboard prose render exists for the field — the M3 column-rename / shape-fix recommendation in F1 §3 is now an orthogonal pre-existing issue tracked separately, not a Phase 4 deliverable.

### Surprises

- **Workflow gap blocked task-status closure.** When the chain was created with inline tasks declaring `status=blocked` (prose-blockers, not structural edges), the work-surface had no action that transitioned them to `pending` once F1 closed. `task_unblock` requires `blocker_slug`; `task_start` errors on `blocked→active`; `task_complete` errors on `blocked→closed`; `forge_edit(status=pending)` silently no-ops for non-`{open,closed}` values. Filed as bug `no-path-from-status-only-blocked-task-to-pending` during F2 startup. The engineering work proceeded regardless; the tracker hygiene gap is downstream of the commits. Chain close also blocked by this bug — F4 closes the chain via either a structural-edge workaround at chain-close time or after the bug fix lands.
- **Dashboard had no TaskIndex page.** F1's matrix assumed task fields (`handoff_output`, `constraints`, `acceptance_criteria`) would have prose-block renders to retire. The audit revealed none exist — tasks are visible inside the ChainIndex tree but their content fields surface only via `observe-http` search responses, not as standalone detail panels. This is why F1's MIGRATE-TO-EVENT disposition for `handoff_output` flipped to KEEP at F2 start.
- **Bake-in gate waiver was free.** The original 30-day calendar gate was designed to protect against substrate flakiness. The cousin chain's byte-identical rebuild-from-events validation provided stronger protection than the gate envisioned. Waiving gates #1/#2/#3 carried negligible risk because the substrate validation was already gold-standard. The recorded risk (operator-level EventTimeline adoption signal hadn't accumulated) is real but small in expected impact.
- **`foldChainEdited` needed a tolerance escape hatch.** Removing `design_decisions` from `isAllowedChainColumn` would have broken rebuild-from-events for every chain that's ever been edited (the ChainEdited payload still carries `updated_fields=["design_decisions"]` for historical events). Solution: an explicit `isRetiredChainColumn` check that silently drops retired-column edits before the allowed-column gate. Pattern likely reusable for future field retirements; F1's doc didn't anticipate it.

### Adjacent findings: dispositions

The four adjacent findings F1's §5 surfaced were dispatched as follows:

- **Finding 1** (foldTaskCompleted overwrites handoff_output with closure_summary) — confirmed standalone, deferred to a future fix. Out of Phase 4 scope.
- **Finding 2** (per-type-renderer skips completion_condition) — **fixed in F3** as a side-improvement (added the "Completion" PayloadRow alongside the existing "Output" + the now-deleted "Decisions" duplicate).
- **Finding 3** (drawer-render readability post-design_decisions retirement) — visual sign-off pending on user side. The truncated 200-char timeline-row preview stays; the full payload is surfaced via the structured-payload drawer.
- **Finding 4** (observe-http chain-detail struct cleanup) — **landed in F2** as a clean break (the JSON key dropped, no always-empty-string fallback).

### Token / surface impact

- `observe-http /chains/<slug>` detail JSON: 1 key dropped (`design_decisions`).
- `observe-http /bugs/<slug>` detail JSON: 1 key dropped (`resolution_note`).
- `observe-http /suggestions/<slug>` detail JSON: 1 key dropped (`resolution_note`).
- Dashboard chain detail page: ~5KB (typical) of above-fold prose disappears; same content remains reachable via EventTimeline drawer click.
- Dashboard bug + suggestion detail panels: small ~100–500 byte prose blocks disappear from resolved-state views.
- `work.bug_read` / `work.suggestion_read` / `work.chain_state` response shape: 1 key dropped from each.

### Substrate effect

Phase 4 is the second of two cleanup chains that took the agent-first substrate from "events-as-supplement" to "events-as-source-of-truth" with no projection-side duplication of post-emission content. The combined effect:

- Every state change is traceable to an event with actor, timestamp, payload, and rationale. (Migration 9 success criterion #1)
- Frontend reads exclusively from projections. (#2)
- Projections rebuild from events with no data loss. (#3) — equivalence harness verified after both cleanup chains.
- Agents interact via the closed verb set; the legacy free-form prose duplicates are gone. (#4)

Two of `docs/AGENT_AUDIT_AND_MIGRATION.md` §9's six success criteria (#5 benchmark interpretability over time, #6 "Why?" as queryable) are out of scope for these two chains and remain the domain of the benchmark-substrate + the audit-ledger UI respectively.

### Recommended next phase

Per `docs/AGENT_AUDIT_AND_MIGRATION.md` §6, the natural follow-on is **Phase 5 — Retrospective** (verify the migration's success criteria at corpus level). The user can decide whether to fork a new chain for that or treat this F4 + cousin chain T7 as the §6 closing record.

No new chain proposed by this retrospective.
