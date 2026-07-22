# Telemetry Label Spike — TT1.5

> **Status:** Closing recommendation. Produced by chain `query-telemetry-substrate` TT1.5 (`spike-training-data-label-coverage`). Validates the PROVISIONAL `click_kind` and `label_kind` enums proposed in TT1 (`docs/TELEMETRY_SUBSTRATE.md` §5 and §6.3) against 40 hand-labeled spans drawn from `~/.claude/projects/*/`. The output is a CHECK constraint SQL fragment for TT2 and a label_kind enum for TT3.
>
> **TL;DR:** **CONFIRM the four `click_kind` tiers and their default weights as TT1 proposes.** **REVISE `label_kind` to add a fifth value, `weakly_positive`, for the (mentioned-only, no stronger tier fired) case** — without this, mentioned-only rows fall through both the `positive` (max_click_weight ≥ 0.8) and `negative` (no tier fired) thresholds and become silent gaps. TT2 lands the `click_kind` CHECK constraint verbatim from §4; TT3 lands the `label_kind` enum verbatim from §5.
>
> **Reading order:** §1 method → §2 sample composition → §3 labeling walkthrough on 5 representative spans → §4 click_kind recommendation (CONFIRM) → §5 label_kind recommendation (REVISE) → §6 default weights → §7 implementation notes (not enum revisions but real cautions for TT2).

---

## 1. Method

`scripts/spike_label_coverage.py` walks every JSONL transcript under `~/.claude/projects/*/`, groups records by `promptId` (the user-input-arc identifier per TT1 §2.1), and emits a per-span digest containing:

- The `promptId`, `sessionId`, and `project_slug` of the span.
- Every search-tool call (`mcp__toolkit-server__knowledge` with `action ∈ {vault_search, kiwix_search, knowledge_search}`, plus legacy `mcp__work-server__*_search` forms found in older seed-packet sessions) and the `source_refs` extracted from each tool_result body.
- Every follow-up call (`vault_read`, `kiwix_fetch`, native `Read`) within the same span.
- Every terminal-event call (`bug_resolve`, `task_complete`, `task_cancel`, `chain_close`), including the rationale text.
- Every assistant text snippet in the span (truncated at 1.2 KB per snippet).

The script's `inventory` subcommand produced the cross-project span counts (§2); the `sample` subcommand produced a stratified 40-span sample using quotas designed to hit the chain's acceptance-criteria requirements (≥3 project slugs; ≥5 spans ending in a resolve; ≥5 spans where the search produced zero useful refs; spans biased toward variety in search count).

The script implementation lives at `scripts/spike_label_coverage.py`; it's a one-shot helper, NOT a production component. TT2's grounding-events processor is the production locus of click_kind detection and writes to `query_interactions`. This script's output is `tt15_spike/spans_sample.json`, used as the labeling input here.

**Important note on `promptId` threading.** Initial implementation grouped only records that carry `promptId` directly. The transcript reality: `promptId` lives only on the **user** record; subsequent assistant records, tool_use blocks, and tool_results inherit it by sequence position. The script threads the most-recent-user `promptId` through every following record until the next user message. Without this thread, no assistant tool_use call is attributed to a span — discovered when an inventory pass showed 0 search calls across 2,990 spans.

**Scope and boundaries.** Hand-labeling for this spike: 40 spans across 3 project slugs (seed-packet, home-sophi, dm-toolkit). The labels are the spike author's best read of "what actually happened" from the transcript, not the agent's self-report. Reasonable disagreement is fine; the goal is enum-fit, not perfect labels.

---

## 2. Sample composition

```
total spans (with searches): 84 across 3 project slugs
  -home-sophi-dev-seed-packet  62  (older sessions, May 4–12)
  -home-sophi                  20  (recent sessions, May 14–17)
  -home-sophi-dev-dm-toolkit    2

sampled 40 (quota-driven mix):
  with_resolve            32 available, 7 quota-taken
  zero_result_no_answer    8 available, 5 quota-taken
  long_span (≥20 searches) 1 available, 1 taken
  short_span (<5 searches) 80 available, 5 quota-taken
  per-project fill        4 each for top 4 projects (3 distinct projects exist)
  random fill to N=40     remainder
```

**Sidechain (subagent) spans:** The script does not currently descend into `~/.claude/projects/*/subagents/agent-*.jsonl` files; those exist (I saw 4 in the May 14–17 home-sophi window) but were not included in the sample. This means the `parent_span_id` column proposed in TT1 §2.3 is not validated against subagent data in this spike. **Not enum-affecting**: the parent_span_id column captures sidechain → parent linkage which is orthogonal to the click_kind enum being validated.

**Cross-check against `data/toolkit.db` `grounding_events`:** Skipped in this spike — the existing `used` bit is a known coarse approximation and the four-tier decomposition's value is to replace it, not to align with it. A future TT2-close validation should sanity-check the new tier-distribution against the legacy `used` rate (expectation: `mentioned` weight × prevalence ≈ legacy `used` rate, because mentioned IS today's heuristic).

---

## 3. Labeling walkthrough — 5 representative spans

Each (span, source_ref) pair gets a freeform reading + a mapped `click_kind`. Pairs that don't map cleanly are the signal this spike exists to surface.

### 3.1 Span #14 — clean `followed`-only

| field | value |
|---|---|
| `promptId` | `38e61089-a6f9-4740-96f6-d6e99a5ffb6a` |
| user | "any friction to file or vault learnings" |
| searches | 1 (vault_search) |
| follows | 1 (vault_read on a returned ref) |
| resolves | 0 |

The agent did one vault_search returning 5 refs, then vault_read'd the second-position result (`decisions/2026-05-16_polymorphic-ref-table-naming-convention.md`). No quote in assistant text; no resolve.

| source_ref | freeform reading | click_kind |
|---|---|---|
| `learnings/mcp-servers/2026-05-06_work-lib-project-scoping-convention.md` | shown, not touched | (none — negative training pair) |
| `decisions/2026-05-16_polymorphic-ref-table-naming-convention.md` | vault_read on exact path | `followed` |
| `learnings/general/2026-05-09_promote-junction-to-entity-when-relationship-carries-metadata.md` | shown, not touched | (none) |
| `learnings/mcp-servers/2026-05-04_forge-context-project-id-fk.md` | shown, not touched | (none) |
| `learnings/mcp-servers/2026-05-09_silent-drop-pattern-recurring-by-shape.md` | shown, not touched | (none) |

**Enum coverage:** 100%. `followed` fires once; the other 4 pairs are absence-of-row (correctly captured as negative label_kind in the projection layer).

### 3.2 Span #24 — `followed` × 2, no resolve rationale

| field | value |
|---|---|
| `promptId` | `1f73b794-2a1b-4e59-a0d1-85477ed43b27` |
| user | "please continue working through go-typed-returns-rollout" |
| searches | 1 |
| follows | 20 (mix of Read on code paths + 2 vault_reads) |
| resolves | 1 (task_complete; **rationale is empty**) |

The agent did one vault_search, vault_read'd two of the five refs verbatim, then read .go source files. Assistant text mentions the typed-returns pattern but doesn't quote the vault notes. The task_complete call has empty `closure_summary` (the `complete-task` skill historically left this blank when the close was a follow-on).

| source_ref | freeform reading | click_kind |
|---|---|---|
| `reference/2026-05-15_go-mcp-dispatch-typed-returns-pattern.md` | vault_read; concept paraphrased in assistant text | `followed`; `mentioned` (slug substring "typed-returns-pattern" appears in assistant text) |
| `learnings/general/2026-05-15_survey-real-data-before-declaring-schemaless.md` | vault_read; influenced approach | `followed` |
| `learnings/mcp-servers/2026-05-13_go-mcp-sdk-json-rawmessage-schema-generation.md` | shown, not touched | (none) |
| `learnings/mcp-servers/2026-05-15_load-schema-before-applying-schema-aware-guards.md` | shown, not touched | (none) |
| `learnings/general/2026-05-10_minimal-envelope-save-strategy.md` | shown, not touched | (none) |

**Enum coverage:** 100%. Note one (span, ref) pair triggered two tiers (`followed` + `mentioned`) — exactly the case TT1 §5.1 calls out ("Multiple kinds may fire per (span_id, source_ref) — each is its own query_interactions row"). The aggregation rule (max_weight wins → 1.0) gives this pair the right label.

**Edge case surfaced:** task_complete with empty rationale. `resolved-from` cannot fire when there's no text to substring-match against. This is correct behavior: no rationale = no resolved-from signal. The bug is not in the enum; it's in the upstream practice of leaving `closure_summary` blank. Future work: a structure-lint that rejects empty closure_summary on terminal events would close this loop (out of scope for this chain).

### 3.3 Span #33 — `followed` + `mentioned` + slug-variant detection

| field | value |
|---|---|
| `promptId` | `fd400acf-bed6-428e-aaba-b302b92cbdd0` |
| project | seed-packet |
| searches | 4 (overlapping vault_searches) |
| follows | 7 |
| resolves | 0 |

Multi-search "catalog these recurring patterns" session. The agent read several vault notes from the result sets and wrote a catalog entry in assistant text that **explicitly quotes the slug-form** of a learning:

> "**B10 — Schema-API field synchronization.** Vault learning `2026-05-09_silent-drop-pattern-recurring-by-shape` documents 8 instances..."

The source_ref was `learnings/mcp-servers/2026-05-09_silent-drop-pattern-recurring-by-shape.md` — the assistant text uses the slug only (no path prefix, no `.md` suffix).

| source_ref | freeform reading | click_kind |
|---|---|---|
| `learnings/general/2026-05-08_chain-design-patterns-from-toolkit-server-projects.md` | vault_read; not quoted | `followed` |
| `decisions/2026-05-09_benchmarks-shape-criteria-reframe.md` | vault_read; **slug substring `benchmarks-shape-criteria-reframe` appears in assistant text** | `followed` + `mentioned` (slug-form) |
| `learnings/mcp-servers/2026-05-09_silent-drop-pattern-recurring-by-shape.md` | vault_read; slug quoted in code-style backticks in assistant text | `followed` + `mentioned` (slug-form via backticks) |
| `decisions/2026-05-09_eval-framework-framing-follows-consumer-posture.md` | shown, not touched | (none) |
| `learnings/general/2026-05-07_audit-task-descriptions-undercount.md` | shown, not touched | (none) |
| (multiple others) | shown, not touched | (none) |

**Edge case surfaced — slug-variant matching:** The `mentioned` tier defined in TT1 §5 says "the `source_ref` string itself appears in subsequent assistant text." Strict-string match would miss the `2026-05-09_silent-drop-pattern-recurring-by-shape` form because the assistant text doesn't include the `learnings/mcp-servers/` path prefix or the `.md` suffix.

**Implementation note for TT2 (not an enum revision):** The Stop hook's `mentioned`-detection logic must normalize source_ref to the slug form (the date-prefixed unique-tail) and match THAT against assistant text. Falling back to strict full-path match would produce false-negatives on the dominant assistant-text convention. This is a hook-implementation note, captured at §7.

### 3.4 Span #6 — multi-resolve span, `resolved-from` via rationale

| field | value |
|---|---|
| `promptId` | `7c0e38e1-36db-4708-9442-90d46b309097` |
| project | home-sophi |
| user | "please solve our open bugs" |
| searches | 1 |
| follows | 9 (mix of vault_read + code Read) |
| resolves | **12** (large bug-sweep) |

The agent resolved 12 bugs in one prompt. The first bug_resolve's rationale (truncated for clarity):

> "blueprints/forge-schemas/portal-chat.toml removed. forge_schemas no longer enumerates portal-chat; forge(portal-chat, create) now fails fast at the schema-lookup layer..."

The rationale mentions `blueprints/forge-schemas/portal-chat.toml` — a code path, not a vault source_ref. But the search returned `learnings/mcp-servers/2026-05-11_migration-partial-application-recovery.md` (among 5 refs), and **one resolve's rationale does reference this path**:

> "...consulted `2026-05-11_migration-partial-application-recovery` for the rollback approach..."

| source_ref | freeform reading | click_kind |
|---|---|---|
| `learnings/mcp-servers/2026-05-11_migration-partial-application-recovery.md` | vault_read; slug appears in one bug's rationale | `followed` + `resolved-from` |
| `learnings/mcp-servers/2026-05-11_split-sql-statements-begin-end-limitation.md` | shown, not touched | (none) |
| `learnings/mcp-servers/2026-05-04_sqlx-open-pool-memory-path.md` | shown, not touched | (none) |
| `learnings/mcp-servers/2026-05-06_work-lib-project-scoping-convention.md` | shown, not touched | (none) |

**Enum coverage:** 100%. `resolved-from` fires correctly on the slug-form match (per the same implementation note in §3.3).

**Edge case surfaced — multi-resolve trajectory.** One span produces 12 distinct `query_resolutions` rows (one per bug closed). Each cross-references the same single `grounding_events.id` via `grounding_event_ids` JSON array. TT1 §4 supports this — `query_resolutions` is one row per (entity, prompt) with arrays linking to upstream signals.

### 3.5 Span #5 — heavy search activity, search-results NOT cited in resolve

| field | value |
|---|---|
| `promptId` | `930e4c58-e783-4697-ab37-7568985cbe48` |
| project | seed-packet |
| searches | 10 (substantial overlap; some exact duplicate queries) |
| follows | 22 (almost all on .rs code paths) |
| resolves | 7 |

The agent did 10 searches over ~5 minutes, retrieved overlapping result sets, but performed almost all the actual work via Reads on `.rs` source code (NOT on the returned vault refs). The eventual bug_resolve rationale cites a `process-docs/adhoc/agent-vault-rag/precision-sharpening-design_2026-05-09.md` doc — **a path that was NOT returned by any of the 10 vault_searches in this span**.

| source_ref (representative sample) | freeform reading | click_kind |
|---|---|---|
| `learnings/general/2026-05-08_small-corpus-retrieval-skip-embeddings.md` | returned in 5 different searches; never read; shown | (none × 5 search calls → 5 negative training pairs) |
| `reference/rag-architecture.md` | returned 5×; never read | (none × 5) |
| `learnings/general/2026-05-08_smoke-battery-vocab-alignment-flatters-retrieval-eval.md` | returned 4×; never read | (none × 4) |
| `projects/local-llm-offload.md` | returned 2×; never read | (none × 2) |
| (others) | shown, not touched | (none) |

**Edge case surfaced — heavy-search-low-utilization:** All 40 source_refs returned by the searches produced ZERO click signals. The resolution drew from a completely different document. The span is rich training data: 10 grounding_events × ~4 refs each = ~40 high-quality `negative` / `hard_negative` examples. The four-tier enum captures this correctly via absence-of-row.

**Edge case surfaced — repeat-query convergence:** Two identical queries fire 2 minutes apart returning identical result sets. Each is a distinct `span_id` (per-`tools/call` per TT1 §2.1), so the (span_id, source_ref, click_kind) uniqueness on `query_interactions` holds without collision. **Validates the TT1 three-layer hierarchy** — without per-`tools/call` span_id, two identical queries in the same prompt would have collapsed into one row and lost signal.

---

## 4. Click_kind recommendation — CONFIRM

The four tiers proposed in TT1 §5 (`followed`, `cited`, `mentioned`, `resolved-from`) cover every pattern observed in the 40-span sample. **No new tier needs to be added.** **Pasteable CHECK constraint for TT2:**

```sql
-- TT2: paste into the query_interactions CREATE TABLE definition.
CHECK (click_kind IN ('followed', 'cited', 'mentioned', 'resolved-from'))
```

Rationale per tier:

- **`followed`** fires cleanly on `vault_read` / `kiwix_fetch` / `Read` against the exact `source_ref`. The 40-span sample contains roughly a dozen `followed` instances (most common in single-search-result-driven spans like §3.1, §3.2). Definition holds.

- **`cited`** (≥40 char quote OR `[text](source_ref)` markdown link OR `file:line` reference) was the rarest tier in the sample — I observed it ZERO times as a pure assistant-text quote of a search result body. The cases that look like citations (e.g., §3.3's "`2026-05-09_silent-drop-pattern-recurring-by-shape`") are slug-mentions, not body quotes. **Definition holds, but TT2's implementation should expect `cited` to be rare** until proactive injection lands (where the agent quotes injected blocks more often). Default weight 0.8 is conservative-safe.

- **`mentioned`** is common but produces false negatives at the slug-vs-full-path level — addressed as an implementation note in §7, NOT an enum revision. Definition holds with the path-normalization caveat.

- **`resolved-from`** fires correctly on rationale text within `bug_resolve` / `task_complete` / `task_cancel` / `chain_close` parameters (§3.4). Empty-rationale resolves don't fire it — correct behavior. Definition holds.

**Patterns that did NOT need a new tier:**

| Observed pattern | Captured by | Why no new tier |
|---|---|---|
| Code-file Read that wasn't a search result | (no row) | The `exact source_ref` requirement on `followed` correctly excludes these — they're outside the RAG-surface telemetry scope. |
| Slug-form vs full-path source_ref reference | `mentioned` with normalization | Single-tier; just an implementation detail of the matcher. |
| Rationale references a non-RAG-surfaced doc | (no row for the search candidates) | The `resolved-from` tier requires the source_ref BE A CANDIDATE — out-of-RAG references stay on the write-side `events.rationale` only. Correct boundary. |
| Repeat-query convergence | One row per (span_id, source_ref, click_kind) | The three-layer span hierarchy isolates per-`tools/call` so identical queries don't collide. |
| Bug ID references (numeric IDs) in assistant text | `mentioned` (if knowledge_pointers.source_ref includes the bug ID as a slug variant) | Knowledge-pointer convention concern, not an enum gap. |

---

## 5. Label_kind recommendation — REVISE (add `weakly_positive`)

The TT1 design proposes four label_kind values for `proj_training_data_for_reranker.label_kind`:

| label_kind (TT1 proposal) | Rule |
|---|---|
| `positive` | any tier ≥0.8 fired (i.e., `followed`, `cited`, or `resolved-from`) |
| `negative` | shown, no tier fired, position ≤ 10 |
| `hard_negative` | shown, no tier fired, position ≤ 3 AND results_count ≥ 5 |
| `unlabeled` | in-flight, no resolution yet |

**Gap surfaced by the sample:** A (span, source_ref) pair with ONLY `mentioned` fired (weight 0.4, no other tier) has `max_click_weight = 0.4` — too low for `positive` (which requires ≥0.8), but a tier DID fire so the pair is NOT `negative`. **The pair falls through both thresholds and gets no label_kind.** Across the 40-span sample, mentioned-only fires on roughly 5–8 (span, ref) pairs — small but non-zero, and structurally important because `mentioned` is the noisiest tier whose signal-to-noise ratio is exactly what a training pipeline needs to decide whether to include.

**Recommendation: add `weakly_positive` as a fifth label_kind value.** Definition:

| label_kind (REVISED) | Rule |
|---|---|
| `positive` | `max_click_weight ≥ 0.8` (`followed` / `cited` / `resolved-from` fired) |
| `weakly_positive` | `max_click_weight > 0 AND max_click_weight < 0.8` (mentioned-only) |
| `negative` | shown, no tier fired, position ≤ 10 |
| `hard_negative` | shown, no tier fired, position ≤ 3 AND `grounding_events.results_count ≥ 5` |
| `unlabeled` | in-flight, no resolution yet |

**Pasteable CHECK constraint for TT3:**

```sql
-- TT3: paste into proj_training_data_for_reranker CREATE TABLE definition.
CHECK (label_kind IN ('positive', 'weakly_positive', 'negative', 'hard_negative', 'unlabeled'))
```

The benefit: training pipelines explicitly decide whether to include `weakly_positive`. A cross-encoder reranker fine-tune that prioritizes precision can filter to `label_kind = 'positive'` only; one that prioritizes recall can include `weakly_positive` with a lower loss weight; the chunk-quality scorer can use `weakly_positive` as soft positives. Without the 5th value, every consumer reimplements the threshold logic and the projection's contract is incomplete.

**Why this didn't surface during TT1's design:** TT1 wrote the four-value enum reasoning from first principles ("strong/weak/none"). The 40-span sample makes the gap concrete — a non-trivial fraction of pairs fall in the gap, and they're structurally distinct from both buckets.

---

## 6. Default weights — CONFIRM (with one-line caveat)

| `click_kind` | Default weight (CONFIRMED) | Rationale from sample |
|---|---|---|
| `followed` | 1.0 | Strong signal in every observed case. Agent reads → agent acts. |
| `cited` | 0.8 | Rare in the sample (zero pure-text citations observed); weight is conservative-safe. Re-tune after proactive injection ships and citation frequency rises. |
| `mentioned` | 0.4 | Noisy: ~half the observed `mentioned` cases were slug-substring matches where the agent paraphrased the note; the other half were genuine references. Weight 0.4 puts these in `weakly_positive` (label_kind §5), distinct from strong signals. |
| `resolved-from` | 1.0 | Strongest signal — agent's terminal rationale explicitly cites the path. |

**Caveat:** Default weights are a per-installation override (TT1 §5.2). The values above ship as defaults in `~/.config/toolkit-server/click-weights.toml` (TT2 lands the file location); deployments may re-tune after observing their own corpus's signal-to-noise. **No re-tuning recommended at this spike's resolution** — the 40-span sample is too small to overfit weights to. After 30 days of TT2-live traffic, a re-run of this script can validate weights against actual distributions.

---

## 7. Implementation notes for TT2 (NOT enum revisions)

These are real cautions the sample surfaced. None require schema changes; all are detection-logic notes for TT2's `grounding-events-processor.sh` extension.

### 7.1 Slug-form normalization for `mentioned`

Source_refs returned by `vault_search` carry the full path: `learnings/general/<date>_<slug>.md`. Assistant text overwhelmingly references the same note by **just the slug** (`<date>_<slug>` or `<date>_<slug>.md`), often inside markdown backticks. The `mentioned` detector must normalize:

- Strip `learnings/<corpus>/` (and similar) prefixes.
- Strip `.md` suffix.
- Match the resulting `<date>_<slug>` against assistant text.

Strict full-path matching would produce false-negatives on the dominant convention. Pattern: 80%+ of slug-references in assistant text use the slug-only form, NOT the full path.

### 7.2 `resolved-from` looks at tool_use parameter text, not assistant text

Bug/task/chain rationales come through as `tool_use.input.params.resolution_note` (or `closure_summary`, `reason`, depending on action). The `resolved-from` detector reads from the **tool_use parameter content**, NOT from assistant `text` blocks. Confirmed correct in TT1 §5; flagged here because the implementation should not confuse rationale-text with assistant-narrative-text.

### 7.3 `tool_result.content` is sometimes a JSON-string, sometimes a list

`source_refs` extraction needs to handle both:
- `content: "[{\"source_ref\": ...}, ...]"` (JSON-string)
- `content: [{"type": "text", "text": "..."}, ...]` (list of blocks)

The spike script handles both via `extract_text()` + `parse_source_refs_from_tool_result()`. TT2's Stop-hook bash port needs the same defensive parsing.

### 7.4 Empty rationale is a real and common case

A non-trivial fraction of `task_complete` calls have empty `closure_summary` (in particular, when the close came via the `complete-task` skill leaving the field blank). `resolved-from` correctly doesn't fire for these. The right fix is upstream (lint or enforce non-empty rationale at the dispatch boundary, per `agent-first-substrate` T3's pattern), NOT a telemetry-layer workaround.

### 7.5 Subagent (sidechain) spans live in subdirectory JSONLs

`~/.claude/projects/<proj>/subagents/agent-*.jsonl` contains sidechain transcripts. TT2's hook must walk these too, stamping `parent_span_id` from the parent agent's span. The spike script does NOT currently descend into subagent subdirectories — captured here as a TT2 implementation gap to fix.

### 7.6 Cross-substrate join (`prompt_id` ↔ `events.span_id`) is per-resolve-event

Detecting `resolved-from` requires correlating `events` rows of types `BugResolved` / `TaskCompleted` / etc. against the `grounding_events` in the same prompt. TT1 §4.5 describes the detection flow; the practical loop in the hook:

1. For each `events` row of a terminal type emitted in the session:
2. Find all `grounding_events` with the same `session_id` where `created_at` precedes the event's `ts`.
3. For each, check whether the event's rationale text mentions the source_ref (using slug-form normalization from §7.1).
4. Emit a `query_interactions` row with `click_kind='resolved-from'` and a `query_resolutions` row linking everything.

The order matters: resolutions are computed AFTER click_kind detection for `followed`/`cited`/`mentioned` so the `query_interaction_ids` array can include them.

---

## 8. TT2 / TT3 unblock pointer

**TT2 (interactions-and-resolutions-tables) reads §4 verbatim:**

```sql
CHECK (click_kind IN ('followed', 'cited', 'mentioned', 'resolved-from'))
```

**TT3 (read-side-projections) reads §5 verbatim:**

```sql
CHECK (label_kind IN ('positive', 'weakly_positive', 'negative', 'hard_negative', 'unlabeled'))
```

**Both tasks read §6 for default click_weight values:**

```toml
# Default ~/.config/toolkit-server/click-weights.toml
followed       = 1.0
cited          = 0.8
mentioned      = 0.4
resolved-from  = 1.0
```

**Both tasks read §7 as cautions** (slug-form normalization, empty-rationale handling, sidechain JSONL traversal, tool_result content parsing). These do NOT require schema or interface changes — they're implementation guidance.

**docs/TELEMETRY_SUBSTRATE.md updates (out of this spike's scope, captured for the chain's TT4 retrospective):**

- §5 (click_kind definitions) — add a §5.5 note that `mentioned` detection uses slug-form normalization per §7.1 of THIS doc.
- §6.3 (training_data_for_reranker label_kind) — replace the four-value enum with the five-value set from §5 of THIS doc.
- §16 (open questions) — close question #3 (CONFIRM click_kind tiers + weights); close question #6 (REVISE label_kind to add `weakly_positive`).

---

## 9. Cross-references

- `docs/TELEMETRY_SUBSTRATE.md` — TT1 design doc this spike validates. §5 (click_kind), §6.3 (label_kind), §16 (open questions resolved by this doc).
- `scripts/spike_label_coverage.py` — the one-shot helper. Re-run after 30 days of TT2-live traffic to validate weights against real distributions.
- `tt15_spike/spans_sample.json` — the 40-span sampled dataset (not committed; regenerable from the script).
- `~/.claude/vault/learnings/general/2026-05-17_tiered-implicit-feedback-for-rag-telemetry.md` — canonical write-up of the four-tier pattern. Confirmed by this spike.
- Chain `agent-first-substrate` `docs/EVENT_SUBSTRATE.md` §6.3 — the cross-substrate-seam flag this spike's three-layer hierarchy resolves.
