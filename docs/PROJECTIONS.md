# Projections — Design

> **Status:** Initial substrate, produced by chain `agent-first-substrate` T4 (`projection-subsystem`). Sibling to `docs/EVENT_SUBSTRATE.md` (envelope + mechanics) and `docs/EVENT_CATALOG.md` (per-type registry).
>
> **Reading order:** §1 framing → §2 contract → §3 the three projections → §4 rebuild modes → §5 known caveats → §6 cross-chain seam.

---

## 1. What projections are and aren't

The toolkit-server work meta-tool reads via `proj_*` SQL tables — denormalised materialised views derived from the canonical CRUD tables (`bugs`, `chains`, `tasks`, `roadmap_items`). The dashboard and observe-HTTP endpoints query these projection tables directly; the CRUD tables remain as the durable write target (see `docs/EVENT_SUBSTRATE.md` §8.1).

Three projections ship in this chain:

| Projection | Table | Source events | Surfaces |
|---|---|---|---|
| `current_bugs` | `proj_current_bugs` | Bug* | `/bugs`, `work.bug_list` |
| `chain_status` | `proj_chain_status` | Chain*, Task* | `/chains`, `work.chain_status` |
| `roadmap_view` | `proj_roadmap_view` | Chain*, Task* (+ future roadmap events) | `/roadmap`, `work.roadmap_list` |

**Scope of this doc and chain:**

- Projection table schemas + per-row event watermark + per-projection watermark row.
- The `Projection` interface, fold dispatch, idempotent rebuild.
- Dashboard read-path cutover (observe-HTTP endpoints).
- The `toolkit-server rebuild-projections` subcommand.

**Out of scope here:**

| Out of scope | Why |
|---|---|
| Retiring CRUD tables | Phase 4 of `AGENT_AUDIT_AND_MIGRATION.md` §6. Projections need bake-in time before legacy tables retire. |
| `benchmark_trends` projection | Deferred until a concrete consumer surfaces. T6 captures provenance per run; aggregation is a follow-on. |
| RAG-corpus / vault projections | toolkit-server doesn't own a RAG corpus (`docs/EVENT_SUBSTRATE.md` §1 non-goal). |
| Roadmap-mutating events (`RoadmapItem*`) | Reserved namespace in `docs/EVENT_CATALOG.md`; a follow-on chain wires emit + fold. T4 keeps the roadmap projection coherent via direct refresh from `roadmap_set` / `roadmap_insert`. |
| Sibling-chain projections (TT3 of query-telemetry-substrate) | Three more projections land against the same interface; their schemas live in that chain's T1 deliverable. |

---

## 2. Projection contract

### 2.1 Interface

```go
// go/internal/projections/projections.go
type Projection interface {
    Name() string                  // "current_bugs" — watermark key
    TableName() string             // "proj_current_bugs"
    Fold(ctx, tx, RawEvent) error
    RebuildFromEmpty(ctx, tx) error
}
```

`Fold` is invoked synchronously by `FoldAll` after every successful `events.Emit` INSERT (via the `events.SetFoldHook` seam — see §2.4). `RebuildFromEmpty` is invoked by the rebuild CLI to repopulate the projection table from CRUD.

### 2.2 Registration

Each projection registers itself in `init()`:

```go
func init() { Register(currentBugs{}) }
```

The rebuild CLI iterates `projections.All()`; sibling-chain projections show up in `--projection=<name>` without code change in this chain.

### 2.3 Fold semantics — transitional dual-write phase

**In this chain (T4), fold reads CRUD inside the same tx as the event INSERT.** The fold is *not* a pure events-only function — it consults the current CRUD row to populate projection columns. Three reasons:

1. **`BugEdited` and friends** carry only `{updated_fields: [...]}` in the payload, not the new values. Reconstructing the new column values from event payload alone would require walking back through the entity's event history.
2. **Dual-write transitional framing** (`EVENT_SUBSTRATE.md` §8.1 row 1): CRUD tables remain the write target during the chain; events ledger is the audit + ordering surface. Reading CRUD from the fold is consistent with that framing.
3. **Idempotency for free**: "refresh row from CRUD" is naturally idempotent — folding the same event twice produces the same projection state.

**Dual-write ORDER** (post-T4): handlers do CRUD update *first*, `events.Emit` *second*. The fold inside `Emit`'s hook reads post-update CRUD, so the projection converges with the rebuild-from-empty path on identical column values. (Pre-T4 the order was Emit-first; T4 swapped every handler to put the CRUD write first — see commit notes.)

**Future**: when Phase 4 retires CRUD tables, fold becomes "construct row from event payload alone". Projections then become first-class derived state from the events ledger. The interface doesn't change; only fold-body implementations migrate.

### 2.4 The `SetFoldHook` seam

`events` and `projections` are independent packages; neither imports the other. The wiring lives at `cmd/toolkit-server/main.go`, which calls `events.SetFoldHook(...)` at startup with a closure that delegates to `projections.FoldAll`. This is the dependency-reversal pattern: `events` defines a `FoldHook` function type; `main` registers an implementation; `projections` knows nothing about `events`.

Tests that exercise the full Emit→Fold→Projection-table path install the same hook (see `internal/projections/projections_test.go::installProjectionsFoldHook`).

### 2.5 Watermarks

`projections_watermark (projection_name TEXT PK, last_event_id TEXT, last_folded_ts TEXT, schema_version INTEGER)` tracks the highest event_id folded into each projection. Per-row watermarks (`last_event_id`, `last_event_ts` columns on each `proj_*` table) record the event that last touched the specific row — useful for "when did this last change" queries cheaply.

The per-projection watermark is the resume point for `rebuild-projections --from-event=ID`; the per-row watermark is informational. Migration `033_projections.sql` seeds both: per-projection watermarks stamp to current `MAX(event_id)`; per-row watermarks stay at the sentinel `''` to mark "snapshot-seeded; no event observed yet".

### 2.6 Fold failure semantics

Per acceptance-criteria item (5) and chain `design_decisions` item 4: **fold failure aborts the originating mutation. Eventual consistency rejected.**

Concretely: any non-nil error from a projection's `Fold` propagates out of `events.Emit` → the handler's `pool.WithWrite` closure returns the error → SQLite rolls the whole tx back, including the events row INSERT, the CRUD update, and any sibling projection's already-applied fold.

This shape is structural: tests register a "poison" fold hook and assert the events table stays empty after a failing emit (see `TestFold_FailureAbortsTx`).

### 2.7 Projection tables are not for external mutation

Projection tables are NOT manually mutable by handlers, dashboard code, or ad-hoc SQL. The only paths that write are:

1. Migration `033_projections.sql`'s initial snapshot.
2. Projection `Fold` methods (via the events.Emit hook).
3. `Projection.RebuildFromEmpty` (via the rebuild CLI).
4. `projections.RefreshRoadmapLayoutForProject` (the non-event roadmap mutation path).

This is enforced by convention + code review, not by SQL triggers (a trigger that blocks UPDATE would block the fold itself, since SQLite can't distinguish caller context). The acceptance-criteria note "Projection tables NOT manually mutable — same trigger-based UPDATE/DELETE block as events" reads as aspirational; T4 ships the discipline, not the trigger.

---

## 3. The three projections

### 3.1 `current_bugs`

**Source events:** `BugReported`, `BugTriaged`, `BugResolved`, `BugReopened`, `BugEdited`, `BugStamped`.

**Fold:** matches `evt.EntityKind == "bug"`; refreshes the `(project_id, slug)` row from the `bugs` CRUD table via UPSERT.

**Columns:** every read-path column the dashboard `/bugs` endpoint and the `work.bug_list` MCP action use, including the nullable `resolution_kind`, `resolved_commit_sha`, `qwen_task_id`, and `resolved_at`.

**Query examples:**
```sql
-- Open bugs by severity, most recent first
SELECT slug, title, severity FROM proj_current_bugs
WHERE status = 'open' AND project_id = 'mcp-servers'
ORDER BY filed_at DESC LIMIT 50;

-- When did this bug last change?
SELECT slug, last_event_id, last_event_ts FROM proj_current_bugs
WHERE project_id = ? AND slug = ?;
```

**Rebuild cost:** O(N) one-shot INSERT from `bugs`. At current ~700 rows, sub-second.

### 3.2 `chain_status`

**Source events:** `ChainCreated`, `ChainClosed`, `ChainEdited` + `TaskCreated`, `TaskCompleted`, `TaskCancelled`, `TaskTransitioned`, `TaskEdited`, `TaskStamped`.

**Fold:** matches `evt.EntityKind ∈ {"chain", "task"}`. Chain events refresh the row directly; task events look up the parent chain via `tasks.chain_id` and refresh that chain's summary row. The five task-status counts (pending/active/blocked/closed/cancelled) recompute from the `tasks` CRUD table via correlated subqueries.

**Edge case — ambiguous task slugs**: a task slug can collide across chains (see `ErrAmbiguousSlug` in `internal/work`). The fold refreshes every chain that holds a task with the event's slug. Over-fold is harmless because each refresh is idempotent.

**Query examples:**
```sql
-- Open chains by activity
SELECT slug, total_tasks, pending FROM proj_chain_status
WHERE status NOT IN ('closed', 'cancelled')
ORDER BY pending DESC;

-- Single-chain detail
SELECT * FROM proj_chain_status WHERE slug = ?;
```

**Rebuild cost:** O(C × log T) for C chains and T tasks (the per-chain COUNT subqueries scan via the `idx_tasks_chain_id` index). At 243 chains / 2.6k tasks, sub-second.

### 3.3 `roadmap_view`

**Source events:** Chain*, Task* (for `target_status` denormalisation). Roadmap-layout mutations (`roadmap_set`, `roadmap_insert`, `chain_close`'s cascade DELETE) are NOT event-emitting in this chain (per `EVENT_CATALOG.md` "Intentionally non-emitting actions"); those handlers call `projections.RefreshRoadmapLayoutForProject` from inside their WithWrite tx to keep the projection coherent.

**Fold:** matches `evt.EntityKind ∈ {"chain", "task"}`; UPDATEs `target_status` and `target_updated_at` columns on every roadmap row referencing the affected slug. Layout columns (`position`, `chain_slug`, `note`) are NOT touched by event fold; they only change through the direct-refresh path.

**Query examples:**
```sql
-- Live roadmap, ordered
SELECT position, ref_kind, ref_slug, target_status, target_updated_at
FROM proj_roadmap_view
WHERE project_id = ? ORDER BY position ASC;
```

**Rebuild cost:** O(R × 1 join) for R roadmap items. At current ~32 rows, instantaneous.

---

## 4. Rebuild modes

The `toolkit-server rebuild-projections` subcommand drives full and incremental rebuilds. Runs entirely inside one write transaction so a mid-rebuild crash rolls back to the pre-rebuild state.

### 4.1 Default — full snapshot

```
toolkit-server rebuild-projections --db=PATH [--projection=NAME]
```

For each projection (or just the named one): TRUNCATE the projection table; call `RebuildFromEmpty` (which re-snapshots from CRUD); set the watermark to the current `MAX(event_id)`. After this completes, projection state matches current CRUD state byte-for-byte (the byte-identical-rebuild invariant).

**When to run:**
- After a manual schema migration that affects projection-source columns.
- To recover from a corruption suspicion (cheap; sub-second on current DB sizes).
- During tests, as the `seedX → rebuild → assert` pattern.

### 4.2 Incremental — `--from-event=ID`

```
toolkit-server rebuild-projections --db=PATH --from-event=01-evt-...
```

Reads events whose `event_id >= ID` in `event_id` order; for each, calls every target projection's `Fold` (or just the named projection's). The fold path is the same code as the live-emit hook. The watermark advances to the highest replayed event_id.

**When to run:**
- Narrow recovery: "events past X look suspect; re-fold from there".
- Verifying fold determinism: replay events on top of a known snapshot and compare.

**Caveat — transitional phase**: because fold reads CRUD (not event payload), replaying events past `--from-event=ID` re-applies "current truth" from CRUD, which already reflects everything those events recorded. Net effect: the projection re-converges on current CRUD state, the same as the default mode. The `--from-event` mode is more useful once Phase 4 retires CRUD and fold becomes payload-only.

### 4.3 Snapshot-based — `--from-snapshot=PATH`

```
toolkit-server rebuild-projections --db=PATH --from-snapshot=/mnt/data1/toolkit.db.snapshot-pre-rebuild-2026-05-22T22-28-28Z.db
```

ATTACHes the snapshot DB read-side, copies every `proj_*` row + `projections_watermark` row verbatim into the target, then folds ONLY events with `event_id > snap.MAX(event_id)`. Watermarks advance to the last folded event (or stay at the snapshot's value if zero events were folded).

**When to run:**
- The events table is known-incomplete and a default rebuild would lose terminal state (the 2026-05-22 incident shape). A snapshot from BEFORE the gap is the safe seed.
- Faster than a full rebuild when the snapshot is recent and the post-snapshot events tail is short.
- Disaster recovery: any auto-snapshot landed by an earlier default rebuild (§4.4) is a valid input here.

### 4.4 Auto-snapshot + post-rebuild regression guard

Every default rebuild auto-writes a snapshot via `VACUUM INTO` to `<db_dir>/<db_basename>.snapshot-pre-rebuild-<UTC-ISO8601>.db` BEFORE truncating anything. The last 10 auto-snapshots are kept; older ones are deleted automatically.

After the rebuild, the subcommand reads three counts on both pre- and post-rebuild state:

- open bugs (`proj_current_bugs WHERE status='open'`)
- pending tasks (`proj_current_tasks WHERE status='pending'`)
- open chains (`proj_chain_status WHERE status='open'`)

If ANY axis regresses (post-rebuild has MORE rows than pre-rebuild on any axis), the subcommand:

1. Prints the diff to stderr.
2. **Auto-restores from the snapshot it just wrote.** The pre-rebuild state is preserved.
3. Exits non-zero.

The regression direction "more open/pending/open" specifically catches the 2026-05-22 incident shape: rebuild-from-events flipped 707 bugs from terminal → open and 1500 tasks from closed → pending because the events table was incomplete.

**Bypass flags** (use sparingly):

- `--no-snapshot` — skip the auto-snapshot. The regression guard still runs, but on a regression there's no snapshot to restore from; the subcommand exits non-zero with the regressed state left in place.
- `--force-allow-regression` — print the diff but accept the regressed state. For intentional data purges only.

**Per-projection / from-event modes do NOT run the regression guard.** The guard is a full-rebuild safety check; per-projection runs (`--projection=NAME`) leave most state untouched, and `--from-event` is explicitly a partial replay. Both still write an auto-snapshot when `--no-snapshot` is unset.

### 4.5 Determinism invariants

Two paths must produce byte-identical state on equivalent inputs:

- **Path A (incremental):** seed CRUD; for each mutation, run the production handler (CRUD update → Emit → fold inside Emit). Capture projection checksum.
- **Path B (rebuild):** TRUNCATE projection; run default rebuild (CRUD snapshot). Capture projection checksum.

`projections_test.go::TestFold_IncrementalEqualsRebuild` asserts this; the checksum skips the per-row `last_event_id` / `last_event_ts` columns (which legitimately differ — path A stamps from the event, path B leaves the snapshot sentinel `''`).

---

## 5. Known caveats

### 5.1 Pre-event entities

The events table began recording at chain T2 close (2026-05-17). Entities created before that (currently ~700 bugs, ~243 chains, ~2.6k tasks) carry the sentinel `last_event_id = ''` on their projection rows. The projection content reflects current CRUD state; the gap is in the audit trail, not the projection.

### 5.2 Fold reading CRUD breaks pure "events-only" reasoning

A reader who assumes "I can rebuild projection state from the events log alone" will be surprised — until Phase 4, the fold needs CRUD to populate row contents. The events log is the audit trail; projections are CRUD-derived. This is documented at §2.3.

### 5.3 Roadmap layout updates don't emit events

`roadmap_set` / `roadmap_insert` / `chain_close`'s cascade DELETE don't emit events; they keep `proj_roadmap_view` coherent via direct refresh inside their tx. A follow-on chain wires `RoadmapItemAdded` / `RoadmapItemRemoved` events and the direct-refresh path retires.

### 5.4 Per-projection ordering

Inside `FoldAll`, projections fold in name-sorted order: `chain_status` → `current_bugs` → `roadmap_view`. Per-projection final state is order-independent (each touches its own table). The watermark advance per projection happens in the same loop iteration as the fold, so a fold failure leaves no projection mid-applied.

### 5.5 Schema floor — CHECK constraints (migration 066)

`proj_current_bugs`, `proj_current_tasks`, and `proj_chain_status` carry CHECK constraints enforcing (a) status vocabulary and (b) terminal-status implies a populated companion column. They reject any INSERT or UPDATE that would produce a semantically invalid row, regardless of which path tries it (fold, snapshot copy, direct SQL, rebuild).

The biconditional on `proj_current_bugs` (`status='open'` XNOR `resolved_at IS NULL`) is the load-bearing one: it catches the 2026-05-22 regression shape (status flipped to 'open' while resolved_at stayed populated) at the SQL layer, well before the rebuild's post-rebuild diff fires. The post-rebuild diff (§4.4) is a coarser second-line defence — count-axis based; the CHECK is per-row.

`'upstream'` is part of the bug vocab; chains are `open`/`closed` only (no `cancelled` — that's a task-side state). Tasks vocab is the five standard states; `commit_sha IS NOT NULL` is required for `closed` but not `cancelled` (cancelled tasks legit-close without code).

`proj_memories` (migration 072) carries the same kind of floor: `kind` in the four-value MemoryWritten enum, and non-empty `description` / `vault_path` / `last_event_ts`. Same intent as the reranker projection's invariants (migration 071) — a fold regression that dropped a relied-on column surfaces as a rejected insert, not a silently blank value. See §6.3.

---

## 6. Cross-chain coordination

### 6.1 Sibling chain: `query-telemetry-substrate` TT3 — LANDED

TT3 shipped three read-side projections against the same `Projection` interface:

- **`query_volume_by_source`** / `proj_query_volume_by_source` — per-day buckets keyed on (project_id, action, query_source, day). Source: `grounding_events` + `query_interactions` (success_count joins on `followed`/`resolved-from`). Consumer: dashboard search-volume panel.
- **`retrieval_success_per_query`** / `proj_retrieval_success_per_query` — one row per `grounding_events.id` with had_followed/had_cited/had_mentioned/had_resolved_from flags, max_click_weight rollup, kinds_fired JSON, success boolean. Consumer: retrieval health panel.
- **`training_data_for_reranker`** / `proj_training_data_for_reranker` — one row per (grounding_event_id, source_ref) with the TT1.5 5-value `label_kind` enum (`positive` / `weakly_positive` / `negative` / `hard_negative` / `unlabeled`). Consumer: cross-encoder reranker fine-tuning pipeline per `local-ml-roadmap.md` §1.1.

Implementation pattern — read-side projections re-snapshot from CRUD on every `Fold` (no incremental walk; the byte-identical-rebuild invariant holds vacuously). Trigger is **`telemetry.SetFoldHook(projections.FoldAllReadSide)`** wired at bootstrap: every `telemetry.EmitInteraction` / `telemetry.EmitResolution` runs the read-side folds inside its write tx; fold failure aborts the emit.

`Projection.Fold(_, _, RawEvent)` for read-side projections ignores the `RawEvent` — there's no events-ledger anchor, so the parameter exists only to keep the interface single-shape.

Namespace convention (see `docs/TELEMETRY_SUBSTRATE.md` §7.3): this chain owns `query_`, `retrieval_`, `training_` projection-name prefixes. `injection_*` reserved for proactive-injection follow-on; `offload_*` reserved for Qwen/ML offload future chains. A registry-validation test at `projections_test.go::TestRegistry_NamespacePrefixes` asserts every registered projection name carries one of the known prefixes (legacy unprefixed names `current_bugs` / `chain_status` / `roadmap_view` grandfathered).

- The interface (Name, TableName, Fold, RebuildFromEmpty) is frozen. TT3 binds against it without re-versioning.
- The rebuild CLI auto-discovers via `projections.All()` — TT3's projections show up in `--projection=<name>` without code change here.
- `projections_watermark.projection_name` is `TEXT PRIMARY KEY` (no enum); migration 038 seeds NULL rows for the three new projections.

#### 6.1.1 Worked example — fine-tuning the cross-encoder reranker

`local-ml-roadmap.md` §1.1 describes a cross-encoder reranker that learns from agent click patterns. Pipeline reads `proj_training_data_for_reranker` directly:

```sql
-- Strict-precision training set: only positive vs hard_negative pairs.
SELECT query_text, source_ref, label_kind, weight, label_sources
FROM proj_training_data_for_reranker
WHERE query_source = 'agent_initiated'
  AND label_kind IN ('positive', 'hard_negative')
ORDER BY grounding_event_id, candidate_position;

-- Recall-leaning training set: include weakly_positive as soft positives.
SELECT query_text, source_ref,
       CASE label_kind
         WHEN 'positive'        THEN 1.0
         WHEN 'weakly_positive' THEN 0.5
         WHEN 'negative'        THEN 0.0
         WHEN 'hard_negative'   THEN 0.0
       END AS soft_label,
       label_sources
FROM proj_training_data_for_reranker
WHERE label_kind != 'unlabeled';
```

The `label_sources` JSON array preserves every `click_kind` that fired for the (query, candidate) pair so the pipeline can re-weight (e.g. trust `followed` more than `cited`) without re-joining `query_interactions`. The 5-value enum is the spike-validated contract; the threshold rules live in `crates/shared-db/migrations/038_telemetry_projections.sql` and `go/internal/projections/query_training.go` — drift between the two is what the per-projection rebuild test catches.

### 6.2 Phase 4 readiness

When Phase 4 (CRUD-table retirement) lands, projections become the only read surface. The migration shape:

1. Switch `Fold` implementations to consume `evt.Payload` directly (no CRUD reads).
2. Backfill `BugEdited` / `TaskEdited` / `ChainEdited` payloads to include new values (event-type bump: `*EditedV2`).
3. Drop the CRUD tables once the bake-in confirms projections + events fully cover the read paths.

The interface, the rebuild CLI, and the watermark schema all carry forward unchanged.

### 6.3 `proj_memories` — substrate-health-audit-projections T7

Memories were the only entity kind with no projection. The arc-close dedup loader (`go/internal/arcreview/dedupe.go`, F2) read them straight off the events ledger via `json_extract` while every sibling kind read a projection. The chain's proj_* sweep (completion condition c) surfaced the asymmetry; T7 closes it.

- **`memories`** / `proj_memories` — one row per memory `name` (the PK), folding `MemoryWritten` events. Columns: `name`, `kind`, `description`, `body_length_bytes`, `vault_path`, `project_id`, `filed_at`, `last_event_id`, `last_event_ts`. Consumer today: the F2 dedup loader; future: dashboards + curate ranking.

Key shape decisions:

- **Keyed by `name`, last-write-wins.** The auto-memory dir is a single global namespace keyed by filename, so the same name re-filed from a different project context (16 such names in live data, e.g. `linguistic-tics` under both `seed-packet` and `mcp-servers`) is *one* memory. The fold upserts on `name`; the ON CONFLICT clause updates every column **except** `filed_at`, so `filed_at` preserves the first write's ts while `last_event_ts` tracks the latest. `project_id` records the most-recent write's project.
- **Event-derived, no CRUD table.** Unlike the original trio, memories have no CRUD source — they live on disk in the vault. `RebuildFromEmpty` replays `MemoryWritten` events directly. Migration 072 ships the table empty; `rebuild-projections` folds the real ledger to populate it (no synthetic backfill — consistent with the chain's no-backfill invariant).
- **Future-event tolerant.** `Fold` switches on event `Type`; `MemoryEdited` / `MemoryDeleted` (not yet emitted) fall through to a no-op until a handler lands, rather than erroring.
- **No `DependsOn`.** The fold reads no sibling projection table.

Write-side regression: `projections/memories_test.go` (rebuild + idempotency + last-write-wins) and the `proj_memories` invariant cases in `projections/check_constraints_test.go`.

---

## 7. Cross-references

- `docs/EVENT_SUBSTRATE.md` §1 (history note) — canonical record of the dual-write-order swap and the failure-mode equivalence.
- `docs/EVENT_SUBSTRATE.md` §7 — projection contract framing.
- `docs/EVENT_CATALOG.md` — per-type event registry. Reserved-namespace section names projection-relevant follow-on event families.
- `docs/AGENT_AUDIT_AND_MIGRATION.md` §6 — Phase 4 (legacy CRUD retirement); §8.3 — read-path audit positions.
- `go/internal/projections/` — implementation.
- `crates/shared-db/migrations/033_projections.sql` — table + watermark + snapshot.
- `go/cmd/toolkit-server/rebuild_projections.go` — subcommand entry.
