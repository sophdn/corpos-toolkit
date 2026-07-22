# Telemetry Substrate — Design

> **Status:** Draft for review. Produced by chain `query-telemetry-substrate` TT1 (`design-telemetry-substrate`). Decisions here are durable; downstream tasks TT1.5–TT4 bind to them. Two enum sets (`click_kind`, `label_kind`) are explicitly flagged PROVISIONAL pending the TT1.5 hand-label spike; every other decision in this doc is durable from TT1 forward.
>
> **Reading order:** §1 framing → §2 span hierarchy (resolves the cross-substrate join key) → §3–§4 the two new tables → §5 click definitions → §6 projections → §7 registry plug-in → §8 proactive-injection prerequisites → §9 consolidation plan → §10 privacy → §11 worked example → §12 cross-substrate seam → §13 roadmap coverage.
>
> **Companion docs:** `docs/EVENT_SUBSTRATE.md` (write-side ledger this chain FKs into); `docs/PROJECTIONS.md` (the projection contract this chain's three projections implement); `docs/EVENT_CATALOG.md` (write-side type catalog this chain's resolutions reference); `~/Documents/files/Already Processed Idea Files/local-ml-roadmap.md` (§Phase 0 + §1.1–§2.5 — the training-data consumers this substrate feeds). The chain `agent-first-substrate` closed 2026-05-17; its retrospective lives at `docs/SUBSTRATE_RETROSPECTIVE_2026-05-17.md`.

---

## 1. What this substrate is and isn't

The toolkit-server unified knowledge surface today emits **one row per search call** into `grounding_events` (migration `019_grounding_events.sql`) with a coarse `used` bit derived by a Stop hook that scans the next assistant turn for source-ref string presence. That table is the read-side analogue of the CRUD-vs-events shape `agent-first-substrate` named: a single mutable row per call, with reasoning ("did the agent actually use this?") flattened to a boolean.

This substrate adds **two tables alongside `grounding_events`**: `query_interactions` (one row per detected click signal — multiple rows per grounding event, identified by `(span_id, source_ref, click_kind)`) and `query_resolutions` (one row per resolved bug/task/chain that links back to the searches that fed it, plus the write-side events that closed the work). Together with a small set of forward-compat columns added to `grounding_events`, they turn each search call into a **full lifecycle** — query → results → tiered click signals → resolution — that **read out as training pairs** for the cross-encoder reranker (`local-ml-roadmap.md` §1.1), source router (§1.2), and chunk-quality scorer (§2.5).

### 1.1 Write-side / read-side distinction

The split with `agent-first-substrate` is structural, not stylistic:

| Substrate | Shape | Purpose | Mutability |
|---|---|---|---|
| `events` table (write-side) | append-only, immutable, transactional. One row per agent mutation, validated against `blueprints/events/<type>.json` at write time. | Audit ledger; rationale + actor + span attached to every state change. | UPDATE/DELETE blocked by SQLite trigger (`docs/EVENT_SUBSTRATE.md` §2.2). |
| `grounding_events` + `query_interactions` + `query_resolutions` (read-side) | mutable for `query_interactions` (the Stop hook may revise a `mentioned` row to `cited` once the assistant turn finishes); immutable for `query_resolutions` (a terminal resolution is the closing record). Analytical: joined by span hierarchy across search calls, ticket events, and the work meta-tool. | Training-data shaped. Folded into three projections (§6) that ML pipelines consume directly. | `query_resolutions` UPDATE/DELETE blocked by trigger; `query_interactions` allows UPDATE for click-kind refinement, DELETE blocked. |

The two substrates share `span_id` (per-MCP-request) and `event_id` (UUIDv7 from the write-side events table). The cross-substrate join is documented in §12.

### 1.2 Scope of this chain (and therefore this doc)

- `query_interactions` table: one row per (span, source_ref, click_kind) triple.
- `query_resolutions` table: one row per terminal resolution event, with JSON-array FKs to both substrates.
- Forward-compat columns on `grounding_events`: `query_source`, `prompt_id`, `user_message_id`, `parent_span_id`.
- Three projections (`query_volume_by_source`, `retrieval_success_per_query`, `training_data_for_reranker`) registered against the existing `projections.Projection` interface (TT3 LANDED via migration 038 — see `docs/PROJECTIONS.md` §6.1).
- The Stop-hook extension that detects the four `click_kind` tiers from transcripts.
- The cross-substrate FK contract (resolutions → `events.event_id`).

### 1.3 Out of scope (deferred to follow-ons or excluded entirely)

| Out of scope | Why |
|---|---|
| Retroactively reconstructing pre-2026-05-17 query history | Forward-only capture, like `agent-first-substrate`. The events ledger began at chain T2 close (2026-05-17); telemetry capture begins at TT2 close. |
| Workbench-log ingestion (roadmap §2.4 anomaly detection) | Different substrate. The Stop hook reads `~/.claude/projects/*.jsonl` transcripts; workbench logs are a separate ingestion. Tracked as a follow-on. |
| Per-handler latency tables (vault_search pass1/pass2, kiwix offload fallback) | These are absorbed into `grounding_events` as nullable columns in the follow-on chain `telemetry-substrate-cleanup` T2. This chain documents the consolidation plan (§9) but does not retire the legacy tables. |
| `qwen_invocations` (migration `029`) | STAYS. Different granularity — one Qwen call may serve many grounding events; the universal per-call shape (bug-1328) is the right home for Qwen cost/latency aggregation. Not consolidated. |
| Live-streaming telemetry to the dashboard | Admin/analytical access only via the three projections in this chain. A live-stream view is a follow-on chain. |
| PII redaction | Substrate is local-only (homelab scale); query text is stored as-is. A redaction projection would be required for any future cloud replication — out of scope here, named in §10. |
| Trained ML models | This is substrate work only. The roadmap's §Phase 1 / §Phase 2 models become buildable AFTER this chain closes. |

---

## 2. Span hierarchy

The span identity contract is the single most load-bearing decision in this chain. `agent-first-substrate` §6 (`docs/EVENT_SUBSTRATE.md`) declared `events.span_id` at MCP-request scope — **one span per `tools/call`** — and flagged in §6.3 that the sibling chain may need a session-scoped identifier above the span. The note ended:

> The sibling chain's design doc (T1 deliverable) should specify whether it joins by span or session, and the schema should support both.

This doc resolves that flag by introducing **three identifier layers**, each scoped to a distinct lifecycle, with the schema carrying all three on every read-side row.

### 2.1 The three layers

| Identifier | Scope | Source | Granularity | Used as join key for |
|---|---|---|---|---|
| `session_id` | One `claude` CLI launch | MCP `initialize` handshake; `sessionId` field in transcript JSONL header | Hours; spans many unrelated user prompts | Session-rollup analytics ("which corpora did session X consult"); already on `grounding_events.session_id` since migration 019. |
| `prompt_id` | One user-typed input + every subsequent assistant turn + tool call + tool result until the next user input | `promptId` field per transcript JSONL record | The "arc" of one user request — minutes to tens of minutes; can include many `tools/call` requests | **The trajectory join key for query_resolutions.** "User asks → searches happen → resolution lands" is one `prompt_id`. New column on `grounding_events`, `query_interactions`, `query_resolutions`. |
| `span_id` | One MCP `tools/call` request | Dispatcher-minted UUIDv4, per `docs/EVENT_SUBSTRATE.md` §6.1 | Milliseconds-to-seconds; one model inference's tool dispatch | Per-request join across events ledger and structured logs. Matches `events.span_id` directly; the column on `grounding_events` (migration 034) carries this. |

`session_id` ⊇ `prompt_id` ⊇ `span_id`. A single `tools/call` always has all three; a single user prompt may produce many `span_id`s; a single session may produce many `prompt_id`s.

### 2.2 Why three, not one

The original TT1 acceptance criterion proposed `span_id = promptId` as a single identifier. That conflicts with the already-shipped `events.span_id`, which is per-`tools/call`. Reconciling by overloading the column would either rename `events.span_id` (breaks the write-side substrate's invariant) or collapse the per-request granularity (breaks the structured-log join contract from `agent-first-substrate` T5).

Three layers is the simpler resolution and lets each consumer pick the join key it actually needs:

- **The cross-substrate trajectory join** (`query_resolutions` → events that closed the bug/task → grounding events that fed the agent) uses `prompt_id`. The user asked one question; the search happened, the resolution landed — all within one `prompt_id`, possibly spanning many `tools/call`s.
- **The per-request join** (a `vault_search` row in `grounding_events` joined to the `BugReported` event in `events` that emitted within the same handler invocation) uses `span_id`. This is the join `events.span_id` was designed for.
- **The session-rollup** ("over CLI session X, which corpora got consulted") uses `session_id`. This was already in `grounding_events` since 019; nothing changes.

### 2.3 `parent_span_id` and sidechain subagents

When a record in the transcript JSONL has `isSidechain=true`, it represents a subagent (spawned via the `Agent` tool). Subagents get their own `span_id` for each `tools/call` they make, but the JSONL records the parent UUID. A new nullable column `parent_span_id` on `grounding_events`, `query_interactions`, `query_resolutions` captures this:

- For top-level (non-sidechain) calls: `parent_span_id IS NULL`.
- For sidechain calls: `parent_span_id` carries the span_id of the parent agent's call that spawned the subagent.

This lets a trajectory query "find every search the subagent did to resolve bug X" recursively walk the parent chain. Without it, subagent searches are orphaned from the parent's trajectory and the training data loses subagent context.

### 2.4 `prompt_id` source-of-truth

`prompt_id` comes from the transcript JSONL at `~/.claude/projects/<project-slug>/<sessionId>.jsonl`. Every record carries a `promptId` field. The Stop hook (existing path: `~/.claude/hooks/grounding-events-processor.sh`) already opens the JSONL post-session; TT2 extends it to stamp `prompt_id` on each `grounding_events` row using the JSONL's `promptId` value for the record whose `requestId` matches the search call's MCP request.

### 2.5 Known trade-offs

- **Mid-prompt user redirects.** A user typing "search for X — actually wait, search for Y instead" produces one `prompt_id` covering both searches plus the final resolution. The training-data shape conflates the two queries under one trajectory. The alternative (splitting on assistant-final-text → next-user-input) is harder to implement reliably from transcripts and produces false splits when the assistant pauses mid-response. Accept the conflation; revisit only if TT1.5's spike reveals it's hurting label quality.
- **`prompt_id` not minted by the server.** Unlike `span_id` (dispatcher-minted) and `event_id` (handler-minted), `prompt_id` is post-hoc-stamped by the Stop hook from the transcript JSONL. Live emits during a `tools/call` don't know the `prompt_id` yet. Consequence: `prompt_id` is NULLABLE at write time, populated by the Stop hook at session end. Queries that need `prompt_id` (the trajectory projection) tolerate the lag; queries that need only `span_id` see it immediately.

---

## 3. `query_interactions` table

One row per detected click signal. Multiple signal kinds may fire per `(span_id, source_ref)` — each is its own row, identified by the `(span_id, source_ref, click_kind)` triple.

### 3.1 Columns

| Column | Type | Required | Notes |
|---|---|---|---|
| `interaction_id` | INTEGER PRIMARY KEY | yes | Autoincrement. Internal use; not stable across rebuilds. |
| `grounding_event_id` | INTEGER NOT NULL | yes | FK → `grounding_events.id`. Real SQL FK; CASCADE DELETE is rejected at the trigger layer (DELETE on `grounding_events` is itself blocked by the substrate's append-only posture for completed sessions). |
| `source_ref` | TEXT NOT NULL | yes | The pointer or path the click signal targets. Matches the `source_refs` JSON array entry from the parent `grounding_events` row. |
| `candidate_position` | INTEGER | no | Position of this source_ref in the original result list (1-indexed). NULL when the click signal references a candidate that wasn't in the top-N (e.g. `resolved-from` may cite a source the search didn't surface). |
| `click_kind` | TEXT NOT NULL | yes | One of: `followed`, `cited`, `mentioned`, `resolved-from`. PROVISIONAL pending TT1.5 spike — column type stays TEXT with a CHECK constraint defined by TT1.5's recommendation, not here. See §5. |
| `click_weight` | REAL NOT NULL | yes | Default weight per click_kind (`followed`=1.0, `cited`=0.8, `mentioned`=0.4, `resolved-from`=1.0). Mutable per-installation override; a TT2 config knob can rewrite the column without a schema change. |
| `citation_quote_chars` | INTEGER | no | For `click_kind='cited'`: length of the longest contiguous quoted span from the result body in the assistant text. NULL for other kinds. |
| `dwell_ms_estimate` | INTEGER | no | For `click_kind='followed'`: milliseconds between the parent grounding event and the follow-up read/fetch. NULL for other kinds. Estimate-only — derived from transcript timestamps which are wall-clock at message boundaries, not user-attention-time. |
| `was_injected` | INTEGER NOT NULL DEFAULT 0 | yes | Proactive-injection forward-compat. `1` when this candidate was injected via the future proactive-hook surface (per `~/.claude/vault/learnings/general/2026-05-15_proactive-injection-feature-design.md`); `0` for agent-initiated queries. Default `0` is backward-compatible with TT2's first emissions, since the proactive-injection chain hasn't shipped. |
| `injection_position` | INTEGER | no | When `was_injected=1`: the position of this candidate in the injected block. NULL otherwise. |
| `injection_was_user_visible` | INTEGER | no | When `was_injected=1`: `1` if the injection landed in a user-visible block, `0` if in a silent `<system-reminder>`. NULL otherwise. Different visibilities produce different agent-behavior signatures; the schema must not conflate them (per the proactive-injection vault note). |
| `span_id` | TEXT NOT NULL | yes | Per-`tools/call` ID; matches `grounding_events.span_id` and `events.span_id` for the parent search call. |
| `prompt_id` | TEXT | no | Per-user-input arc ID; NULLABLE because Stop-hook-stamped post-session. Joined to other rows in the same trajectory. |
| `session_id` | TEXT NOT NULL | yes | Denormalised from `grounding_events.session_id` for cheap session-rollups without a join. |
| `parent_span_id` | TEXT | no | For sidechain subagent interactions; NULL for top-level. |
| `detected_at` | TEXT NOT NULL | yes | When the hook detected this interaction. Lags `created_at` of the parent `grounding_events` row by however long the session was active. |
| `created_at` | TEXT NOT NULL DEFAULT (datetime('now')) | yes | Wall-clock at INSERT. |

### 3.2 Uniqueness

```sql
CREATE UNIQUE INDEX idx_query_interactions_triple
  ON query_interactions (span_id, source_ref, click_kind);
```

The triple is the natural key. INSERT-OR-REPLACE semantics let the Stop hook re-emit safely if it walks a session twice.

### 3.3 Indexes

```sql
CREATE INDEX idx_qi_grounding ON query_interactions (grounding_event_id);
CREATE INDEX idx_qi_prompt ON query_interactions (prompt_id);
CREATE INDEX idx_qi_session ON query_interactions (session_id);
CREATE INDEX idx_qi_source_ref ON query_interactions (source_ref);
```

`source_ref` is indexed because the cross-pointer projections (`training_data_for_reranker`) aggregate per-candidate across queries.

### 3.4 Mutation posture

- **INSERT**: from the Stop hook. TT2's implementation locus is the Go binary `go/cmd/grounding-events-processor/` — the bash shim `~/.claude/hooks/grounding-events-processor.sh` invokes it on session-end. The binary walks the transcript JSONL, runs the four click_kind detectors (`detect.go`), and emits per-row via `telemetry.EmitInteraction`. The previous Rust prototype at `benchmarks/src/bin/knowledge_grounding_processor.rs` was archived 2026-05-17 — see bug `knowledge-grounding-processor-misplaced-rust-binary-in-benchmarks`.
- **UPDATE**: allowed for click-kind refinement (e.g. a row first detected as `mentioned` is upgraded to `cited` once the assistant's full turn is in the transcript). The Stop hook walks records in order; mid-turn detections may be re-classified at turn boundary.
- **DELETE**: blocked by trigger. Misclassifications get a compensating row (e.g. `click_kind='mentioned-retracted'` — but only if TT1.5 confirms retraction as a kind; otherwise the row stays and the projection filters it).

```sql
CREATE TRIGGER query_interactions_no_delete BEFORE DELETE ON query_interactions
BEGIN SELECT RAISE(ABORT, 'query_interactions deletion is not supported; compensating rows instead'); END;
```

---

## 4. `query_resolutions` table

One row per terminal resolution that links back to the searches that fed it and the write-side events that closed it.

### 4.1 Columns

| Column | Type | Required | Notes |
|---|---|---|---|
| `resolution_id` | INTEGER PRIMARY KEY | yes | Autoincrement. |
| `prompt_id` | TEXT NOT NULL | yes | The trajectory join key. Matches the `prompt_id` on at least one `query_interactions` row and at least one `events` row by way of `events.span_id` ∈ {span_ids stamped with this prompt_id}. |
| `session_id` | TEXT NOT NULL | yes | Denormalised. |
| `entity_kind` | TEXT NOT NULL | yes | One of: `bug`, `task`, `chain`. Matches `events.entity_kind`. |
| `entity_slug` | TEXT NOT NULL | yes | Slug of the resolved entity. |
| `entity_project_id` | TEXT NOT NULL | yes | Project scope of the resolved entity. |
| `outcome_kind` | TEXT NOT NULL | yes | One of: `resolved` (bug fixed), `completed` (task completed), `cancelled` (task cancelled), `closed` (chain closed), `discarded` (entity deleted before resolution). The set is closed; CHECK constraint enforces. |
| `write_event_ids` | TEXT NOT NULL DEFAULT '[]' | yes | JSON array of `events.event_id` UUIDv7 strings. The terminal events on the write-side substrate that closed the work (e.g. `BugResolved`, `TaskCompleted`). **Integrity-checked FK**: an application-level check on INSERT verifies every event_id exists in `events` (SQLite doesn't enforce FKs on JSON arrays). |
| `grounding_event_ids` | TEXT NOT NULL DEFAULT '[]' | yes | JSON array of `grounding_events.id` integers. The search calls within the same `prompt_id` that preceded the resolution. |
| `query_interaction_ids` | TEXT NOT NULL DEFAULT '[]' | yes | JSON array of `query_interactions.interaction_id` integers. The detected clicks/citations within the trajectory. |
| `detected_at` | TEXT NOT NULL | yes | Stop-hook detection time. |
| `created_at` | TEXT NOT NULL DEFAULT (datetime('now')) | yes | Wall-clock at INSERT. |

### 4.2 Uniqueness

```sql
CREATE UNIQUE INDEX idx_query_resolutions_entity_prompt
  ON query_resolutions (entity_kind, entity_slug, entity_project_id, prompt_id);
```

One resolution per (entity, prompt). If the same prompt re-resolves an already-resolved-then-reopened entity, the second resolution gets its own row (different `prompt_id`-or-not, but the JSON `write_event_ids` array distinguishes the new BugReopened+BugResolved cycle).

### 4.3 Indexes

```sql
CREATE INDEX idx_qr_prompt ON query_resolutions (prompt_id);
CREATE INDEX idx_qr_entity ON query_resolutions (entity_kind, entity_slug, entity_project_id);
CREATE INDEX idx_qr_outcome ON query_resolutions (outcome_kind);
```

### 4.4 Mutation posture

UPDATE and DELETE on `query_resolutions` are both blocked by triggers (per chain `completion_condition` item (a)). A terminal resolution is the closing record; subsequent reopens get their own row.

```sql
CREATE TRIGGER query_resolutions_no_update BEFORE UPDATE ON query_resolutions
BEGIN SELECT RAISE(ABORT, 'query_resolutions is append-only; reopen+resolve cycles get new rows'); END;

CREATE TRIGGER query_resolutions_no_delete BEFORE DELETE ON query_resolutions
BEGIN SELECT RAISE(ABORT, 'query_resolutions deletion is not supported'); END;
```

### 4.5 Write path

TT2 ships a `processResolutions` step in the Stop hook (or a sibling hook fired on event-emit cascade — TT1.5 picks the architecture). The detection logic:

1. Read every `events` row of type `BugResolved` / `TaskCompleted` / `TaskCancelled` / `ChainClosed` since the last hook run.
2. For each, find the `prompt_id` of the `grounding_events` rows whose `span_id` precedes the event's `span_id` within the same `session_id` (the events ledger doesn't carry `prompt_id` directly; the join is via `session_id` + a time window or via the transcript walk that stamped grounding events).
3. Materialize the JSON arrays.
4. INSERT with the integrity check on `write_event_ids`.

The integrity check runs in SQL: `SELECT count(*) FROM events WHERE event_id IN (json_each.value)` against the JSON array; reject the INSERT if the count doesn't match the array length. Reference shape:

```sql
-- TT2 will land the full check as a constraint trigger.
INSERT INTO query_resolutions (..., write_event_ids, ...)
SELECT ?, ..., ?
WHERE (SELECT COUNT(*) FROM events
       WHERE event_id IN (SELECT value FROM json_each(?2)))
    = json_array_length(?2);
```

---

## 5. Click definitions and the four-tier `click_kind` enum

The single most important consumer-facing decision in this substrate: **what counts as a "click"**. The old `grounding_events.used` heuristic collapses all signal strengths into one bit — too coarse for training a reranker. This chain replaces it with a **tiered enum**, each tier independently observable from the transcript without client-side telemetry.

The canonical write-up of the design pattern is `~/.claude/vault/learnings/general/2026-05-17_tiered-implicit-feedback-for-rag-telemetry.md`. The four tiers:

| `click_kind` | Default weight | Definition (observable from transcript) |
|---|---|---|
| `followed` | 1.0 | Subsequent `vault_read` / `kiwix_fetch` / `Read` on the *exact* `source_ref` within the same `span_id`. The agent deliberately took the next action against this candidate. |
| `cited` | 0.8 | Assistant text in the same `prompt_id` either (a) quotes ≥40 contiguous characters from the result body or (b) includes `source_ref` as a markdown link `[...](source_ref)` or as a `file:line` reference. Post-processed by the Stop hook from the transcript. |
| `mentioned` | 0.4 | The `source_ref` string itself appears in subsequent assistant text within the same `prompt_id`. This is today's `grounding_events.used` heuristic, demoted to the weakest tier — it catches paraphrase + path-mention but doesn't distinguish "agent quoted the result" from "agent mentioned the filename in passing." |
| `resolved-from` | 1.0 | A terminal event on the write-side substrate (`BugResolved.rationale`, `TaskCompleted.closure_summary`, `ChainClosed.closure_summary`) within the same `prompt_id` references the `source_ref` (via path-substring match or `events.related_entities` linkage). The strongest signal: the source contributed to a resolution. |

### 5.1 Multiple kinds may fire per `(span_id, source_ref)`

Each detected tier is its own `query_interactions` row. A `vault_read` on the search result (`followed`) followed by a quoted citation in the next assistant turn (`cited`) followed by mention in the resulting `BugResolved.rationale` (`resolved-from`) produces three rows. Aggregation happens in the projection layer (see §6.3); the table preserves the trail.

### 5.2 Default weights, per-installation override

Default weights (1.0 / 0.8 / 0.4 / 1.0) are written to `query_interactions.click_weight` at INSERT. A per-installation config knob (TT2 ships the location — likely `~/.config/toolkit-server/click-weights.toml`) lets a deployment rewrite the column without a schema change. Different corpora have different signal-to-noise ratios; the doc-only default is meant to be tuned.

### 5.3 Citation definition (the `cited` tier in detail)

Two observable patterns count as a citation:

1. **Substring quote.** ≥40 contiguous characters from the result body (the chunk text the search returned) appear verbatim in assistant text within the same `prompt_id`. Why 40: shorter substrings catch generic phrases ("the user said"); 40 is past most boilerplate. Tunable in the same per-installation config knob as click_weight.

2. **Reference form.** The `source_ref` itself appears as a markdown link (`[anchor](source_ref)`), as a `file:line` reference (`/path/to/file.go:42`), or as a code fence with the `source_ref` in the language tag or path. These are the conventions Claude Code already uses for code citations.

Both patterns are detected by post-processing transcript JSONL after the session ends. The Stop hook walks records in `prompt_id` order; for each result returned by a search call, it tests both patterns against every subsequent assistant message in the same prompt.

### 5.4 Enum-closure hedge — RESOLVED by TT1.5

TT1.5 (`docs/TELEMETRY_LABEL_SPIKE.md`) closed 2026-05-17 with a CONFIRM on the four `click_kind` tiers (no new tier needed; 40-span sample showed 100% coverage) and a REVISE on `label_kind` (5 values, added `weakly_positive` — see §6.3). The CHECK constraint TT2 paste-targets:

```sql
CHECK (click_kind IN ('followed', 'cited', 'mentioned', 'resolved-from'))
```

Default weights (followed=1.0 / cited=0.8 / mentioned=0.4 / resolved-from=1.0) confirmed as-is, with the per-installation override mechanism unchanged.

### 5.5 Slug-form normalization for `mentioned` (TT2 implementation note)

TT1.5 surfaced that 80%+ of `mentioned` matches in real transcripts use the **slug form** of the source_ref (`<date>_<slug>`) rather than the full path (`learnings/<corpus>/<date>_<slug>.md`). The Stop hook's `mentioned`-detection logic must normalize source_ref by stripping the corpus prefix and `.md` suffix before string-matching against assistant text. Strict full-path matching would produce false-negatives on the dominant convention. See `docs/TELEMETRY_LABEL_SPIKE.md` §7.1 for the worked-example evidence.

---

## 6. Three read-side projections

The three projections in this chain register against the existing `projections.Projection` interface (`go/internal/projections/projections.go`). They produce **training-data-shaped views**, not just observability: each is what an ML pipeline consumes directly, without further joins.

The interface (from `projections.go`):

```go
type Projection interface {
    Name() string                                       // watermark key
    TableName() string                                  // proj_<name>
    Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error
    RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error
}
```

Watermark is package-level, not per-projection (`projections.ReadWatermark` / `WriteWatermark` / `ResetWatermark`). Registration is via `projections.Register(p)` from each projection's `init()`. See §7 for the registry plug-in.

### 6.1 `query_volume_by_source`

**Purpose:** "How often is each corpus queried, broken down by query_source? Where are the proactive-injection vs agent-initiated splits?"

**Source events:** `Bug*`, `Task*`, `Chain*` (write-side events, only as proxy for resolution-bound work); the projection mostly folds new rows on `grounding_events` writes — which today are not event-emitting. The fold reads `grounding_events` directly inside the same tx as the parent Emit when a relevant cascade fires, or via the periodic rebuild path. (This is the same dual-write transitional shape `docs/PROJECTIONS.md` §2.3 documents for the existing projections — fold reads CRUD because the events ledger doesn't carry the full row content.)

**Fold logic summary:** Per-day buckets keyed on `(project_id, action, query_source)`. Fold reads new `grounding_events` rows whose `created_at` >= the last bucketed timestamp, increments counts, updates row.

**Output columns:**

| Column | Type | Notes |
|---|---|---|
| `project_id` | TEXT | Project scope (from `grounding_events.project_id`). |
| `action` | TEXT | One of `vault_search`, `kiwix_search`, `knowledge_search`. |
| `query_source` | TEXT | `agent_initiated` / `proactive_hook` / `dashboard_user` / `other`. |
| `day` | TEXT | UTC date (`YYYY-MM-DD`). |
| `query_count` | INTEGER | Total queries in the bucket. |
| `zero_result_count` | INTEGER | Subset with `results_count = 0`. |
| `success_count` | INTEGER | Subset with at least one `followed` or `resolved-from` interaction within the trajectory. |
| `avg_results_count` | REAL | Mean `results_count` across the bucket. |
| `last_event_id` | TEXT | Watermark per projection convention. |
| `last_event_ts` | TEXT | Same. |

**Sample consumer SELECT:**

```sql
-- "Show me the daily search volume split by who initiated the search."
SELECT day, action, query_source, query_count, success_count
FROM proj_query_volume_by_source
WHERE project_id = 'mcp-servers' AND day >= date('now', '-30 days')
ORDER BY day DESC, query_count DESC;
```

### 6.2 `retrieval_success_per_query`

**Purpose:** "Per individual search call: was the retrieval successful? Which signal kinds fired? This is the row-shape used to compute aggregate success rates and to diagnose specific bad queries."

**Source:** `grounding_events` + `query_interactions`. Fold runs on (a) new `grounding_events` rows and (b) updates to `query_interactions` for that grounding_event_id.

**Fold logic summary:** Per `grounding_events.id`, derive a success-shape: max click_weight observed across all interactions, set of kinds that fired (as JSON array), boolean `resolved` if a `query_resolutions` row references this grounding_event_id in its `grounding_event_ids` array.

**Output columns:**

| Column | Type | Notes |
|---|---|---|
| `grounding_event_id` | INTEGER | PK; FK → `grounding_events.id`. |
| `project_id` | TEXT | Denormalised. |
| `action` | TEXT | Search action name. |
| `query_text` | TEXT | The query string (carried from `grounding_events`; TT2 adds the column if not present). |
| `prompt_id` | TEXT | Trajectory key; may be NULL for in-flight rows. |
| `results_count` | INTEGER | From parent grounding_events row. |
| `had_followed` | INTEGER | 1 if any `query_interactions` row with `click_kind='followed'`. |
| `had_cited` | INTEGER | 1 if any `cited`. |
| `had_mentioned` | INTEGER | 1 if any `mentioned`. |
| `had_resolved_from` | INTEGER | 1 if any `resolved-from`. |
| `max_click_weight` | REAL | The highest `click_weight` observed across this query's interactions; 0.0 if none. |
| `kinds_fired` | TEXT | JSON array of distinct `click_kind` values. |
| `success` | INTEGER | Convenience boolean: `max_click_weight >= 0.8 OR had_resolved_from = 1`. |
| `was_proactive` | INTEGER | 1 if `grounding_events.query_source = 'proactive_hook'`. |
| `last_event_id` | TEXT | Watermark. |
| `last_event_ts` | TEXT | Same. |

**Sample consumer SELECT:**

```sql
-- "What's the success rate of vault_search vs kiwix_search over the last week?"
SELECT action, COUNT(*) AS queries, SUM(success) AS successes,
       ROUND(100.0 * SUM(success) / COUNT(*), 1) AS success_pct
FROM proj_retrieval_success_per_query
WHERE last_event_ts >= datetime('now', '-7 days')
GROUP BY action;
```

### 6.3 `training_data_for_reranker`

**Purpose:** The substrate-to-ML bridge. Materializes `(query, candidate, label)` triples that a fine-tuning pipeline for the cross-encoder reranker (`local-ml-roadmap.md` §1.1) consumes directly, with no further joins.

**Source:** `grounding_events` + `query_interactions` + `knowledge_pointers` (the unified result-pointer index from migration 020). Fold runs on new `query_interactions` rows and on `query_resolutions` writes.

**Fold logic summary:** Per `(grounding_event_id, source_ref)` pair — i.e. per (query, candidate) — collapse all matching `query_interactions` rows into one training row. Take the max click_weight across kinds (consumer can re-aggregate; max is the conservative default). Resolve `source_ref` to a `pointer_id` via `knowledge_pointers.source_ref`. Set `label_kind` per TT3's recommendation (PROVISIONAL; the proposed set is `positive` / `negative` / `hard_negative` / `unlabeled`).

**Output columns:**

| Column | Type | Notes |
|---|---|---|
| `training_id` | INTEGER PRIMARY KEY | Internal. |
| `grounding_event_id` | INTEGER | FK → `grounding_events.id`. |
| `query_text` | TEXT | The query. |
| `candidate_pointer_id` | INTEGER | FK → `knowledge_pointers.id`. NULL when the source_ref didn't resolve (rare; means the candidate left the index since the search). |
| `source_ref` | TEXT | Denormalised for human inspection. |
| `candidate_position` | INTEGER | Position in the original result list, 1-indexed. |
| `label_kind` | TEXT | Five-value enum, **REVISED by TT1.5** (`docs/TELEMETRY_LABEL_SPIKE.md` §5): `positive` (`max_click_weight ≥ 0.8`) / `weakly_positive` (`max_click_weight > 0 AND < 0.8`, i.e. mentioned-only) / `negative` (shown, no tier fired, position ≤10) / `hard_negative` (shown, no tier fired, position ≤3 AND `results_count ≥ 5`) / `unlabeled` (in-flight, no resolution yet). The 5th value (`weakly_positive`) was added because mentioned-only pairs (`click_weight=0.4`) fell through both the `positive` (≥0.8) and `negative` (no tier fired) thresholds in the original 4-value enum and got no label. CHECK constraint defined in TT1.5; TT3 pastes it verbatim. |
| `weight` | REAL | The max click_weight across matching `query_interactions` rows; or 0.0 for negative/hard_negative. |
| `label_sources` | TEXT | JSON array of every `click_kind` that fired for this (query, candidate). Preserves the trail. |
| `query_source` | TEXT | From `grounding_events`. Models trained on agent-initiated data are kept distinct from models trained on proactive-injection data — see §8. |
| `was_injected` | INTEGER | 1 if any matching interaction has `was_injected=1`. |
| `prompt_id` | TEXT | Trajectory key. |
| `span_id` | TEXT | Per-request key. |
| `last_event_id` | TEXT | Watermark. |
| `last_event_ts` | TEXT | Same. |

**Sample consumer SELECT:**

```sql
-- "Hand me the (query, candidate, label) triples for fine-tuning the cross-encoder,
--  filtered to agent-initiated queries only, last 90 days."
SELECT query_text, candidate_pointer_id, source_ref, label_kind, weight
FROM proj_training_data_for_reranker
WHERE query_source = 'agent_initiated'
  AND label_kind IN ('positive', 'negative', 'hard_negative')
  AND last_event_ts >= datetime('now', '-90 days')
ORDER BY last_event_ts DESC;
```

The pipeline reads this table and produces a training file directly. **No further SQL knowledge required of the ML pipeline author** — the projection is the contract.

### 6.4 Rebuild semantics

Each projection's `RebuildFromEmpty` re-snapshots from the source tables (`grounding_events`, `query_interactions`, `knowledge_pointers`). The CLI invocation:

```
toolkit-server rebuild-projections --projection=query_volume_by_source
```

works unchanged from the existing `RebuildAll` logic (`projections.go`'s `RebuildAll` iterates the registered set; query_* projections show up automatically by registering with `init()`). The byte-identical-rebuild invariant (`docs/PROJECTIONS.md` §4.3) applies here: incremental fold + rebuild must converge.

---

## 7. Projection registry plug-in

The original TT1 acceptance criterion described designing "a `go/internal/projections/registry.go` module modeled on the existing `rubricRegistry` pattern in `go/internal/observehttp/benchmarks.go`." The projection registry has since shipped in `agent-first-substrate` T4 and supersedes that framing. This chain's projections plug into the live interface.

### 7.1 The live shape

```go
// go/internal/projections/projections.go (already shipped)
type Projection interface {
    Name() string
    TableName() string
    Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error
    RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error
}

func Register(p Projection)         // call from init()
func All() []Projection             // sorted by Name(); used by rebuild CLI
func Get(name string) (Projection, bool)
func FoldAll(ctx, tx, evt RawEvent) error
func RebuildAll(ctx, tx, names []string) ([]RebuildResult, error)

// Watermark is package-level, not per-projection:
func ReadWatermark(ctx, tx, name) (eventID, ts string, err error)
func WriteWatermark(ctx, tx, name, eventID, ts string) error
func ResetWatermark(ctx, tx, name string) error
```

Differences from the original TT1 framing worth flagging for chain-amendment purposes:

- The interface is `RebuildFromEmpty` (not `Rebuild`).
- Watermark is package-level (`Read/Write/ResetWatermark`); there is no per-projection `Watermark()` method.
- `Register` is the package-level function; no per-projection `Register()` method.

A canonical example lives at `go/internal/projections/bugs.go` (`type currentBugs struct{}` + `func init() { Register(currentBugs{}) }`). TT3 follows that shape:

```go
// go/internal/projections/query_volume_by_source.go (TT3 lands)
type queryVolumeBySource struct{}

func init() { Register(queryVolumeBySource{}) }

func (queryVolumeBySource) Name() string      { return "query_volume_by_source" }
func (queryVolumeBySource) TableName() string { return "proj_query_volume_by_source" }

func (queryVolumeBySource) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
    // ... per the fold logic in §6.1
}

func (queryVolumeBySource) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
    // ... INSERT ... SELECT FROM grounding_events GROUP BY ...
}
```

### 7.2 Per-package `doc.go`

`agent-first-substrate` T7 made the four-field `## Intended use` block convention lint-enforced for every Go package under `go/internal/`. Since the three projections in §6 ship as additional files in the existing `projections` package (not a new package), no new `doc.go` is required. If a future chain creates a `go/internal/telemetry/` package for the Stop-hook integration, that package's `doc.go` opens with the four-field block (matching `projections/doc.go`).

### 7.3 Namespace conventions

To prevent collisions when parallel chains ship projections without prior coordination:

| Prefix | Owner | Status |
|---|---|---|
| (unprefixed) | `agent-first-substrate` T4 | Existing: `current_bugs`, `chain_status`, `roadmap_view`. Not renamed; the prefix convention applies to NEW projections going forward. |
| `query_*` | `query-telemetry-substrate` TT3 (this chain) | The three projections in §6. |
| `injection_*` | future proactive-injection chain | Reserved. TT3 may document a stub schema for the proactive-injection chain to plug into; implementation deferred. |
| `offload_*` | future Qwen/ML-offload analytics chain | Reserved. The user has more offload ideas queued; the namespace prefix anchors them. |
| `bench_*` | future benchmark-trends chain | Reserved. `docs/PROJECTIONS.md` §1 deferred `benchmark_trends` until a concrete consumer surfaces. |

The CLI surfaces projections by name; no central enumeration to amend. A new projection lands as one file + one `init()` call.

---

## 8. Proactive-injection feature prerequisites

A future feature chain (not this one) will ship a post-message hook that scans the unified knowledge surface and injects useful context into the next agent turn. The design framing for that chain lives at `~/.claude/vault/learnings/general/2026-05-15_proactive-injection-feature-design.md`. **This substrate is its load-bearing telemetry prerequisite.** Four fields MUST be designed in now because forward-only data capture means retrofitting them later loses all the telemetry collected in the interim.

### 8.1 The four required fields

| Field | Location | Why |
|---|---|---|
| `query_source` | `grounding_events` (new column) | Distinguishes hook-initiated queries from agent-initiated queries. Without it, the should-fire classifier (proactive-injection chain's first model) cannot separate "agent decided to search" from "system fired the hook" — the training data conflates the two cases. CHECK constraint: `query_source IN ('agent_initiated', 'proactive_hook', 'dashboard_user', 'other')`. Default `'agent_initiated'` for backward compat with pre-TT2 rows. |
| `was_injected` + `injection_position` | `query_interactions` (new columns) | A candidate may be *retrieved* without being *injected* (only the top-N injected). "Quality of proactive injection" = "of the things injected, how often were they cited" — unanswerable without the distinction. |
| `user_message_id` | `grounding_events` (new column; content-hash or transcript UUID) | The trigger message that caused the query must be recoverable for the should-fire classifier to train on `(message, was-injection-cited)` pairs. Without recovery, the classifier is non-trainable. |
| `injection_was_user_visible` | `query_interactions` (new column) | Silent `<system-reminder>` injections vs visible blocks produce different agent-behavior signatures. Different visibilities are different cases and the schema must not conflate them. |

### 8.2 Reservation, not implementation

This chain does NOT implement the proactive-injection feature. It reserves the schema slots that the future chain will populate. Until then:

- `grounding_events.query_source` defaults to `'agent_initiated'` (all today's traffic).
- `query_interactions.was_injected` defaults to `0`; `injection_position` and `injection_was_user_visible` are NULL.
- `grounding_events.user_message_id` is stamped by TT2's Stop hook from the transcript JSONL's record-UUID of the user message that opened the prompt.

The future proactive-injection chain ships:
- The hook itself (a sibling to `grounding-events-processor.sh`).
- The `WriteWithSource(action, query_source='proactive_hook', ...)` emit path.
- The `injection_*` projection (§7.3 namespace) for tracking injection-specific success rates.

### 8.3 Open-enum CHECK constraint

`query_source` uses an open-enum CHECK with a fallback value:

```sql
CHECK (query_source IN ('agent_initiated', 'proactive_hook', 'dashboard_user', 'other'))
```

The `'other'` fallback means future query sources (e.g. `'session-start-warmup'`, `'roadmap-context-scan'`) can flow through as `'other'` without a schema migration during exploration; closing them into the enum is a follow-on once the source surfaces. This matches the proactive-injection vault note's "values kept open" guidance.

---

## 9. Per-handler telemetry consolidation plan

> **Update (chain `legacy-telemetry-sink-retirement` / Chain 5, 2026-05-27) — this plan is now COMPLETE; the §9.1/§9.2 tables below are the historical plan-of-record, not current state.**
> - `vault_search_invocations` + `kiwix_offload_invocations` (§9.1) were absorbed into `grounding_events` by `telemetry-substrate-cleanup` (migration 046) and **dropped** by migration 047.
> - `qwen_invocations` (§9.2, then marked "STAYS") was superseded by **`inference_invocations`** — the §9.2 decision ("don't merge it into `grounding_events`; the per-call grain is right") was *honored*: `inference_invocations` keeps the same per-call grain (so the bug-1328 failure mode stays closed) and only the mechanism (off-pattern raw sink → read-side source table feeding `proj_inference_tool_model_performance`) and coverage (local-only → local+remote) changed. The renamed table replaced it, and `qwen_invocations` was **dropped** by migration 083. See `docs/TELEMETRY_CONSOLIDATION.md` (the 5-chain program) and `docs/CHAIN5_LEGACY_SINK_INVENTORY.md`.
> - Canonical column-naming conventions for the unified substrate: **§9.4 below**.

This chain documents the consolidation; the follow-on chain `telemetry-substrate-cleanup` T2 implements it. The plan is input to that task.

### 9.1 Tables being consolidated INTO `grounding_events`

| Old table | Migration | What it tracks | Consolidation column on grounding_events |
|---|---|---|---|
| `vault_search_invocations` (deprecated) | 009 | Per-vault_search call envelope | Covered by `grounding_events.action='vault_search'`; row absorbed. |
| `vault_search_pass_latencies` | 011 | `pass1_latency_ms`, `pass2_latency_ms` per vault_search | New nullable columns on `grounding_events`: `pass1_latency_ms INTEGER`, `pass2_latency_ms INTEGER`. NULL for non-vault_search rows. |
| `kiwix_offload_invocations` | 014 | Per-kiwix call envelope + `qwen_fell_back` flag | New nullable column on `grounding_events`: `qwen_fell_back INTEGER`. NULL for non-kiwix or where Qwen wasn't invoked. |

The follow-on chain stops writing to the old tables, dashboard migrates its reads, then DROPs the old tables. This chain's TT2 already lays the column foundation (it adds the three columns to `grounding_events` alongside `query_source` / `prompt_id` / `user_message_id`) so the cleanup chain only has to flip the write path.

### 9.2 Tables that STAY

| Table | Migration | Why it doesn't consolidate |
|---|---|---|
| `qwen_invocations` | 029 | Different granularity. One Qwen call may serve many grounding events (e.g. a single rerank Qwen call processes 50 candidates from one search). Per-call cost/latency aggregation is the right shape for the universal-per-call table bug-1328 introduced. Consolidating would re-introduce the bug-1328 failure mode. |
| `knowledge_pointers` | 020 | The unified result-pointer index — this is what `training_data_for_reranker.candidate_pointer_id` FKs into. Not telemetry; it's the candidate-side substrate. |
| `pointer_links` (021), `curation_candidates` (022, 025) | — | Sibling read-side substrates for the knowledge surface, not telemetry. |

### 9.3 Migration ordering

The cleanup chain runs AFTER this one closes. TT2's migration adds the three latency/fallback columns + the four proactive-injection / span fields to `grounding_events` in one atomic migration. The cleanup chain backfills the new columns from the old tables, flips writers, then drops the old tables in a third migration. No data loss; the new substrate is strictly larger.

### 9.4 Canonical data-format & column-naming conventions

*(Added by chain `legacy-telemetry-sink-retirement` / Chain 5 — the "data-format / column-naming unification" deliverable of `TELEMETRY_CONSOLIDATION.md` §1.6/§2.4.)*

The read-side telemetry substrate uses one name per concept. New telemetry tables and projections **must** follow these; they are not aspirational — a live-schema sweep at Chain 5 close found **no drift** (the only historical exception, `tokens_input`/`tokens_output`, lived only in `portal_chats`, dropped at migration 031, and survives solely in immutable migration history).

| Concept | Canonical column(s) | Notes |
|---|---|---|
| Token usage (per-call) | `input_tokens`, `output_tokens` | Nullable INTEGER — NULL when the upstream model omits usage. **Not** `tokens_in`/`prompt_tokens`/`tokens_input`. (Go-side `GenerateResponse.PromptTokens`/`CompletionTokens` is the SDK-shaped struct field; it maps to `input_tokens`/`output_tokens` at the storage boundary.) |
| Token usage (projection rollup) | `total_input_tokens`, `total_output_tokens`, `calls_with_tokens` | Running totals + the denominator for "avg tokens where usage known." |
| Latency | `latency_ms` (per-call); `total_latency_ms`, `max_latency_ms`, `avg_latency_ms`, `pass1_latency_ms`, `pass2_latency_ms` | Always millisecond unit, always the `_latency_ms` suffix. Percentiles stay on the per-call table (not foldable from totals). |
| Model identity | `model_name` | The model string (`'qwen2.5-32b'`, `'claude-sonnet-4-6'`). (`model_id`/`model_version` belong to the *trained_model registry* domain, a different surface.) |
| Tool / inference purpose | `task_id` | The `qwenctx.TaskID` routing label (`classify_<rubric>`, `vault-rerank-retrieve`, …); `'unattributed'` sentinel when unstamped. The "tool" dimension of `(tool, model)` ranking. |
| Call-level success (Layer 1) | `success` (per-call), `success_count` (rollup) | `success = (no error AND non-empty output)`; closed `error_class` enum alongside. Emit-time liveness layer. |
| Outcome-level success (Layer 2) | `outcome_success` (per-call), `outcome_success_count` (rollup), `outcome_kind` | The materialized predicate layer (Chain 2). Distinct concept from Layer 1 — *not* drift. |
| Timestamps | `created_at` (row birth); `last_event_ts` / `last_invoked_at` (projection watermarks) | `datetime('now')` default; RFC3339 text. |

**This unification was substantively achieved by Chains 1–2** (which established `inference_invocations` / the both-layers success model on these names); Chain 5 verified no live drift remained and recorded the conventions here. A column-*rename* refactor was explicitly **rejected** in Chain 5 T2 — there is no live drift to fix, so renaming would be behavior-risking churn against already-uniform code.

---

## 10. Privacy and retention

The substrate is **local-only (homelab scale)**. Query text is stored as-is on `grounding_events.query_text` (TT2 adds the column if missing). User messages are referenced by UUID, not stored — `grounding_events.user_message_id` is the transcript JSONL record's UUID (already on disk in `~/.claude/projects/`).

### 10.1 No PII redaction rule

There is no enforced redaction rule on query text. The threat model assumes the local DB is no more sensitive than the transcript JSONL files that already sit unencrypted in `~/.claude/projects/`. If queries contain credentials or secrets, that's a higher-priority bug at the upstream user-input layer, not a substrate concern.

### 10.2 Cloud-replication caveat

Any future cloud replication would require a redaction projection — a view that scrubs query text, message IDs, and any source_ref paths that include `$HOME`. **Out of scope here.** The substrate is designed to make a future redaction projection cheap to add: every PII-bearing column lives on `grounding_events` (one table to filter), and the projection pipeline already supports new projections without schema migration.

### 10.3 Retention

No automatic retention or aging-out. The substrate is forward-only by design; old rows are reference data for retraining. A future cleanup pass could partition `grounding_events` by `created_at` month; not needed at current homelab scale (~hundreds of queries/day).

---

## 11. Worked example — full trajectory

A complete trajectory for one resolved bug, showing every row written across both substrates.

**Scenario:** The agent is asked to fix a bug filed as `forge-bug-title-omitted`. It does a `vault_search` for past forge schema patterns, reads the top result, cites it in a `BugResolved.rationale`, and the commit lands.

### 11.1 The transcript shape

The session's JSONL records (excerpted):

```jsonl
{"role":"user","promptId":"prompt-abc-001","sessionId":"sess-xyz",
 "content":"please fix forge-bug-title-omitted","uuid":"u1-msg-..."}

{"role":"assistant","promptId":"prompt-abc-001","sessionId":"sess-xyz",
 "uuid":"u2-asst-...","requestId":"req-101",
 "content":[{"type":"tool_use","name":"work","input":{"action":"bug_read",...}}]}

{"role":"assistant","promptId":"prompt-abc-001","sessionId":"sess-xyz",
 "uuid":"u3-asst-...","requestId":"req-102",
 "content":[{"type":"tool_use","name":"knowledge","input":{"action":"vault_search",
   "params":{"query":"forge schema title field default"}}}]}

{"role":"user","promptId":"prompt-abc-001","sessionId":"sess-xyz",
 "uuid":"u4-tool-result-...",
 "content":[{"type":"tool_result","content":"[{\"source_ref\":\"vault/learnings/general/2026-05-12_forge-schema-title-fallback.md\",...}]"}]}

{"role":"assistant","promptId":"prompt-abc-001","sessionId":"sess-xyz",
 "uuid":"u5-asst-...","requestId":"req-103",
 "content":[{"type":"tool_use","name":"knowledge","input":{"action":"vault_read",
   "params":{"path":"vault/learnings/general/2026-05-12_forge-schema-title-fallback.md"}}}]}

{"role":"assistant","promptId":"prompt-abc-001","sessionId":"sess-xyz",
 "uuid":"u8-asst-...","requestId":"req-105",
 "content":[{"type":"tool_use","name":"work","input":{"action":"bug_resolve",
   "params":{"slug":"forge-bug-title-omitted","kind":"fixed","commit_sha":"abc1234",
             "resolution_note":"Fixed per vault/learnings/general/2026-05-12_forge-schema-title-fallback.md — the schema default was treating title as required-but-empty..."}}}]}
```

### 11.2 Rows written

**A. `grounding_events` (the `vault_search` call at req-102, written by the existing path + new TT2 stamping):**

```
id=42, action=vault_search, results_count=3, source_refs='[".../forge-schema-title-fallback.md",...]',
session_id=sess-xyz, call_id=req-102, span_id=span-vs-102,
prompt_id=prompt-abc-001 (Stop-hook stamped),
user_message_id=u1-msg-..., query_source=agent_initiated,
parent_span_id=NULL,
pass1_latency_ms=85, pass2_latency_ms=120 (consolidated columns; NULL until TT2 ships them).
```

**B. `events` (write-side, three events — already shipped by `agent-first-substrate`):**

```
event_id=01-evt-bug-read-..., type=BugReported is NOT here (the bug existed pre-substrate);
event_id=01-evt-bug-resolve-..., type=BugResolved,
   entity={kind:bug, slug:forge-bug-title-omitted, project_id:mcp-servers},
   payload={kind:"fixed", commit_sha:"abc1234"},
   rationale="Fixed per vault/learnings/general/2026-05-12_forge-schema-title-fallback.md — ...",
   span_id=span-br-105,
   actor={kind:agent, id:claude-opus-4-7}.
```

**C. `query_interactions` (three rows, written by Stop hook post-session):**

```
interaction_id=101, grounding_event_id=42,
   source_ref=".../forge-schema-title-fallback.md",
   candidate_position=1, click_kind='followed', click_weight=1.0,
   dwell_ms_estimate=8500 (between u4-tool-result and u5-asst vault_read),
   span_id=span-vs-102, prompt_id=prompt-abc-001, session_id=sess-xyz,
   was_injected=0, injection_position=NULL, injection_was_user_visible=NULL.

interaction_id=102, grounding_event_id=42,
   source_ref=".../forge-schema-title-fallback.md",
   click_kind='cited', click_weight=0.8,
   citation_quote_chars=42 (the matching substring in the resolution_note rationale),
   span_id=span-vs-102, prompt_id=prompt-abc-001.

interaction_id=103, grounding_event_id=42,
   source_ref=".../forge-schema-title-fallback.md",
   click_kind='resolved-from', click_weight=1.0,
   span_id=span-vs-102, prompt_id=prompt-abc-001.
```

Three rows because three independent signals fired for the same (span_id, source_ref). The triple `(span_id, source_ref, click_kind)` is unique.

**D. `query_resolutions` (one row, written by the resolution-detection step of the hook):**

```
resolution_id=12, prompt_id=prompt-abc-001, session_id=sess-xyz,
   entity_kind=bug, entity_slug=forge-bug-title-omitted, entity_project_id=mcp-servers,
   outcome_kind=resolved,
   write_event_ids='["01-evt-bug-resolve-..."]',
   grounding_event_ids='[42]',
   query_interaction_ids='[101, 102, 103]',
   detected_at=2026-05-18T03:00:00Z (post-session run).
```

### 11.3 Cross-substrate join

The full trajectory is recoverable by a single JOIN:

```sql
SELECT ge.action, ge.query_text, ge.results_count,
       qi.source_ref, qi.click_kind, qi.click_weight,
       qr.entity_kind, qr.entity_slug, qr.outcome_kind,
       e.event_id, e.rationale
FROM query_resolutions qr
JOIN events e ON e.event_id IN (SELECT value FROM json_each(qr.write_event_ids))
JOIN grounding_events ge ON ge.id IN (SELECT value FROM json_each(qr.grounding_event_ids))
JOIN query_interactions qi ON qi.interaction_id IN (SELECT value FROM json_each(qr.query_interaction_ids))
WHERE qr.entity_slug = 'forge-bug-title-omitted';
```

This is the trajectory the cross-encoder reranker fine-tuner reads from `proj_training_data_for_reranker`, materialized once per `(grounding_event_id, source_ref)` pair.

---

## 12. Cross-substrate seam (the FK contract)

This is the explicit, load-bearing contract between this chain and `agent-first-substrate`. Migration 032's comment named it; this section formalizes it.

### 12.1 Outgoing FK: `query_resolutions.write_event_ids` → `events.event_id`

- Target column: `events.event_id` (UUIDv7, TEXT PRIMARY KEY per migration 032).
- Source column: JSON array on `query_resolutions.write_event_ids`.
- Cardinality: one resolution may reference 1..N events (typically 1 — a single `BugResolved` — but compound resolutions like `BugResolved` + a follow-on `TaskCompleted` may reference both).
- Enforcement: integrity-checked at INSERT (SQLite doesn't enforce FKs on JSON arrays). The check uses `json_each` to expand the array and counts matches against `events.event_id`. INSERT rejects if the count differs from `json_array_length`.
- **Invariant from migration 032:** `event_id` is the stable FK target forever. Future `schema_version` bumps on the events envelope MUST NOT rename or retype this column. Adding a column is fine; removing or renaming requires a chain-level decision involving both substrates.

### 12.2 Outgoing FK: `query_resolutions.grounding_event_ids` → `grounding_events.id`

- Same shape, internal to the read-side substrate.

### 12.3 Span sharing (`span_id`)

- `grounding_events.span_id` (migration 034) === `events.span_id` whenever both fired under the same MCP `tools/call`. The structured-log substrate (`agent-first-substrate` T5) carries the same value.
- Cross-substrate queries can JOIN by span_id directly for per-request analyses.

### 12.4 Prompt sharing (`prompt_id`)

- New column on `grounding_events`, `query_interactions`, `query_resolutions`. NOT on `events` (the write-side ledger is `span_id`-scoped per its design). Resolutions infer the prompt_id from the parent search calls in the same prompt arc.
- The Stop hook is the single populator of `prompt_id` across the read-side tables.

### 12.5 Audit events from this chain

TT4 emits two write-side events:

| Event | Schema location | Purpose |
|---|---|---|
| `TelemetryAuditCompleted` | `blueprints/events/TelemetryAuditCompleted.json` (TT4 lands) | Closing-retrospective event for this chain; matches the `ArchitectureAuditCompleted` shape from `agent-first-substrate` T8. Self-hosting proof: the read-side substrate uses the write-side substrate to record its own audit. |
| `TelemetryProjectionRebuilt` | `blueprints/events/TelemetryProjectionRebuilt.json` (TT3/TT4 lands) | Optional; fires when `rebuild-projections --projection=query_*` runs. Lets the dashboard show "projections fresh as of …" without scraping watermarks. |

Both events follow the `agent-first-substrate` envelope, schema validation, and rationale enforcement. The reserved `Telemetry*` prefix in `docs/EVENT_CATALOG.md` "Reserved namespace" is the slot they occupy.

---

## 13. Roadmap §Phase-0 line-item coverage

Every line item from `~/Documents/files/Already Processed Idea Files/local-ml-roadmap.md` §Phase 0 gets a stated position. Positions:

- **covered**: this substrate addresses the item directly.
- **partially-covered**: the substrate addresses part; the remainder is in a named follow-on chain.
- **will-not-address-this-chain**: the gap is real but excluded from this chain's scope.
- **out-of-scope-this-codebase**: the item assumes a domain toolkit-server doesn't own.

| Roadmap item | Position | Reason |
|---|---|---|
| `queries`: query text, timestamp, source(s) hit, results returned, session ID | **covered** | `grounding_events` already carries action (source), `created_at`, `results_count`, `source_refs`, `session_id`. TT2 adds `query_text` if not already present + `query_source` + `prompt_id` + `user_message_id` + `span_id` (the last is already on the column from migration 034). |
| `interactions`: which results were clicked/cited/used, dwell time, follow-up queries, explicit feedback | **covered** | `query_interactions` with the four-tier `click_kind` (§5) gives the click/cite/use distinction; `dwell_ms_estimate` is the dwell. Follow-up queries are recoverable via `prompt_id` join across `grounding_events`. Explicit feedback is out of scope (no UI to capture it; the implicit-feedback approach is the substitute and is canonical per the 2026-05-17 vault note). |
| `tickets`: full history of state transitions, comments, linked artifacts | **covered by sibling chain** | This is the write-side substrate's responsibility — the `events` ledger captures every state transition with rationale (`agent-first-substrate` T2 closed 2026-05-17). The cross-substrate join (§12) lets a ticket's full lifecycle be reassembled across both substrates. |
| `resolutions`: which retrievals preceded a successful fix; which artifacts ended up cited | **covered** | `query_resolutions` is exactly this shape. `write_event_ids` + `grounding_event_ids` + `query_interaction_ids` JSON arrays make the join 1-hop. |
| "Implicit feedback (click, cite, dwell, follow-up) is good enough at homelab scale if it's captured cleanly. Retrofitting this later is painful." | **covered** | This is the chain's thesis. The four-tier enum + forward-only capture + the projection-shaped output for ML pipelines is the operationalization. |
| "Lives in the Rust data layer. Probably a separate SQLite/Postgres schema from app data so analytics queries don't contend with operational ones." | **partially-covered** | The tables ship in the canonical `crates/shared-db/migrations/` alongside app data, not a separate schema. Reason: the cross-substrate FK to `events.event_id` is cheaper in the same DB; SQLite ATTACH adds operational complexity. Operational contention is mitigated by the projection layer (the dashboard reads `proj_*`, not the raw tables). If contention surfaces, a future migration can split. |
| "Effort: one weekend. Do this first." | n/a | The roadmap line predates the substrate framing this chain inherited from `agent-first-substrate`. This chain ships 5 tasks (TT1, TT1.5, TT2, TT3, TT4) over a more deliberate cadence because the cross-substrate seam needs to land cleanly. The "weekend" estimate matches the work scope only; the framing scope (feature-not-patch, per the chain `design_decisions`) is larger. |

The roadmap's downstream consumers (§1.1 cross-encoder reranker, §1.2 source router, §2.5 chunk-quality scorer) all read `proj_training_data_for_reranker` — they don't need to know the table schema.

---

## 14. Glossary

| Term | Meaning |
|---|---|
| **Click** | Any of the four `click_kind` tiers — `followed`, `cited`, `mentioned`, `resolved-from`. Not a bit; a tier with a default weight. |
| **Click kind** | One of the four enum values in `query_interactions.click_kind`. PROVISIONAL pending TT1.5. |
| **Click weight** | The numeric weight on `query_interactions.click_weight`, defaulting per kind. Mutable per installation. |
| **Citation** | A `click_kind='cited'` interaction. Substring quote ≥40 chars OR markdown link / `file:line` reference of the `source_ref`. |
| **Grounding event** | A row on `grounding_events`. One row per search call (vault/kiwix/knowledge_search). The substrate's primary read-side record. |
| **Interaction** | A row on `query_interactions`. One detected click signal. |
| **Resolution** | A row on `query_resolutions`. One terminal event that closed work, with the trajectory linking back to the searches that fed it. |
| **Span_id** | Per-`tools/call` UUIDv4, dispatcher-minted. Matches `events.span_id`. |
| **Prompt_id** | Per-user-input arc identifier. Sourced from transcript JSONL `promptId`. Stop-hook-stamped. |
| **Session_id** | Per-CLI-launch identifier. Sourced from MCP `initialize` handshake. |
| **Parent_span_id** | The span_id of the parent agent's call that spawned a subagent (sidechain). NULL for top-level calls. |
| **Trajectory** | The set of grounding_events + query_interactions + write-side events sharing one `prompt_id`. The unit of training data for the cross-encoder reranker. |
| **Trajectory join key** | `prompt_id`. The cross-substrate join key for resolutions ↔ searches ↔ events. |
| **Projection (here)** | A `proj_query_*` table folded from the read-side substrate. Implements the `projections.Projection` interface from `go/internal/projections/`. |
| **Reserved namespace** | A projection-name prefix (`query_*`, `injection_*`, `offload_*`, `bench_*`) anchored to a chain. Prevents collision when parallel chains ship projections. |

---

## 15. Cross-references

- `docs/EVENT_SUBSTRATE.md` — the write-side ledger; §6.3 was the seam this doc's §2 resolves. §7 is the projection contract this chain follows.
- `docs/EVENT_CATALOG.md` — write-side type catalog. The `Telemetry*` reserved prefix is this chain's slot.
- `docs/PROJECTIONS.md` — projection contract details. §6.1 specifically calls out the sibling-chain seam.
- `crates/shared-db/migrations/019_grounding_events.sql` — the pre-existing partial logging. Forward-compat columns added in TT2.
- `crates/shared-db/migrations/020_knowledge_pointers.sql` — `training_data_for_reranker.candidate_pointer_id` FKs into this.
- `crates/shared-db/migrations/029_qwen_invocations.sql` — STAYS (different granularity per §9.2). The bug-1328 universal-per-call shape is preserved.
- `crates/shared-db/migrations/032_events.sql` — write-side ledger. `event_id` is this chain's FK target.
- `crates/shared-db/migrations/033_projections.sql` — projection table foundation and the `projections_watermark` schema.
- `crates/shared-db/migrations/034_grounding_events_span_id.sql` — `span_id` column on `grounding_events` already shipped. This chain binds the semantic (per-`tools/call`) and adds the higher-level `prompt_id` layer.
- `crates/shared-db/migrations/<next>_telemetry_substrate.sql.skeleton` — this chain's design-artifact skeleton; TT2 lands the real migration.
- `go/internal/projections/` — the registry this chain plugs into. `bugs.go` is the canonical projection example.
- `~/.claude/hooks/grounding-events-processor.sh` — the existing Stop hook; TT2 extends it for click_kind detection.
- `~/.claude/vault/learnings/general/2026-05-17_tiered-implicit-feedback-for-rag-telemetry.md` — canonical write-up of the four-tier `click_kind` design.
- `~/.claude/vault/learnings/general/2026-05-15_proactive-injection-feature-design.md` — the four telemetry fields this substrate is the prerequisite for.
- `~/Documents/files/Already Processed Idea Files/local-ml-roadmap.md` — the downstream consumer roadmap. §Phase 0 coverage in §13 above.
- Chain `agent-first-substrate` (closed 2026-05-17) — `docs/SUBSTRATE_RETROSPECTIVE_2026-05-17.md` for the cross-substrate seam framing.

---

## 16. Open questions for review

These are decisions I am proposing but the user may want to override before the doc lands. None block downstream tasks if accepted as-stated.

1. **`prompt_id` is NULLABLE at write time** (populated by the Stop hook at session end). Live queries that need `prompt_id` for trajectory analytics see a lag. Alternative: dispatcher mints `prompt_id` from the MCP `initialize` handshake or a per-request header. Trade-off: dispatcher minting requires the MCP transport to surface `promptId`, which isn't currently propagated from `claude` → MCP server. Stop-hook-stamping is the simpler path and matches the existing hook architecture. Confirm.

2. **`query_resolutions.write_event_ids` is JSON-array, not a separate join table.** Alternative: a junction table `query_resolution_events(resolution_id, event_id)`. JSON-array is simpler (fewer rows, fewer joins) and matches `events.related_entities` shape (also JSON-array). Trade-off: SQL FK enforcement is application-level, not engine-level (handled in §12.1). Confirm.

3. ~~**The four `click_kind` tiers and default weights (1.0 / 0.8 / 0.4 / 1.0)** are TT1's proposal. PROVISIONAL pending TT1.5; the spike validates against 30–50 hand-labeled spans. Confirm the proposal as the spike's starting point.~~ **RESOLVED by TT1.5:** CONFIRMED — four tiers cover 100% of patterns in the 40-span sample; default weights validated. See `docs/TELEMETRY_LABEL_SPIKE.md` §4 + §6.

4. **`prompt_id` and `session_id` are redundant on `query_interactions` and `query_resolutions`** (derivable from `grounding_event_id` join). Denormalized for cheap session/prompt-rollup queries without a join. Trade-off: storage bloat (~80 bytes/row × N rows). At homelab scale this is negligible. Confirm.

5. **`query_interactions` allows UPDATE but blocks DELETE** while `query_resolutions` blocks both. Reason: interactions get refined as the Stop hook walks the transcript (a `mentioned` row may be upgraded to `cited` once the full turn lands); resolutions are terminal records. Confirm.

6. ~~**`label_kind` thresholds** in `training_data_for_reranker.label_kind`:~~ **RESOLVED by TT1.5 (REVISE):** added `weakly_positive` as a 5th value to handle mentioned-only pairs (max_click_weight ∈ (0, 0.8) that fell through both `positive` and `negative` thresholds in the original 4-value enum). Final set: `positive` / `weakly_positive` / `negative` / `hard_negative` / `unlabeled`. See `docs/TELEMETRY_LABEL_SPIKE.md` §5.

7. **No retention policy.** Forward-only capture; old rows are reference data. Future cleanup is a follow-on if the table grows past comfort. Confirm "no automatic retention" is acceptable.

---
