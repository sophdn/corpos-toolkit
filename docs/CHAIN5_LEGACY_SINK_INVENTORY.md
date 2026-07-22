# Chain 5 — Legacy-sink retirement & data-format unification — Inventory + Net (refactor steps 1–2)

> Chain `legacy-telemetry-sink-retirement` (309), T1 (`scope-inventory-and-characterization-net`).
> Fifth and final chain of the telemetry-consolidation program (`docs/TELEMETRY_CONSOLIDATION.md` §3).
> This is the refactor-discipline step-1 (scope & inventory) + step-2 (characterization net) artifact.
> The retirement itself (steps 3–7) is forged as the downstream tasks T2–T5 from this inventory.

---

## 0. Boundary

**In scope:** the three remaining legacy telemetry **sink tables** and the work to retire them losslessly:

| Sink table | Added | Superseded by | Supersession landed |
|---|---|---|---|
| `qwen_invocations` | migration 029 (bug 1328) | `inference_invocations` + `proj_inference_tool_model_performance` | Chain 1 (`per-tool-per-model-observability`) T11/T12; dual-write removed at commit `f97cd465` |
| `vault_search_invocations` | migration 009 (+011 cols) | `grounding_events` (absorbed cols) | `telemetry-substrate-cleanup` T2, migration 046 |
| `kiwix_offload_invocations` | migration 014 | `grounding_events` (absorbed cols) | `telemetry-substrate-cleanup` T2, migration 046 |

Plus the program's named seam **data-format / column-naming unification** (`TELEMETRY_CONSOLIDATION.md` §1.6, §2.4).

**Out of scope:** the read-side substrate itself (untouched — it already supersedes these sinks); `grounding_events` schema; the inference projections; curation-scorer coverage (Chain 1 follow-on). No behavior change to any dashboard endpoint — this chain is a pure removal of dead/superseded surfaces with a parity proof.

---

## 1. Per-sink current state (writers / readers / mechanism)

### 1.1 `qwen_invocations` — superseded sink, write path already severed

- **Schema:** `(id, task_id, model_name, latency_ms, input_tokens, output_tokens, created_at)` + index `idx_qwen_invocations_task_created`. Migration 029. No views/triggers/projections reference it (grep-confirmed).
- **Writer:** `db.RecordQwenInvocation` (`go/internal/db/qwen_telemetry.go`) — the only `INSERT INTO qwen_invocations`. **Unwired:** commit `f97cd465` dropped the router dual-write once Chain-1 T12 repointed readers; `RecordQwenInvocation` + the `QwenInvocation` struct **remain compilable but have no live caller** (confirmed: only references are its own def, a doc-comment in `inference_telemetry.go`, and an audit-evidence string in `cmd/observability-audit-emit/main.go`). The table is empty-and-unread, soaking until this chain's DROP.
- **Readers:** **none in production.** The only `FROM qwen_invocations` / `SELECT … qwen_invocations` are in tests: `qwen_telemetry_test.go` (writer round-trip) and `inference_telemetry_test.go` (a comment). The `/inference/*` endpoints read `proj_inference_tool_model_performance` / `inference_invocations` (Chain 1) — confirmed via reader grep.
- **Doc-comment / evidence-string references (not readers):** `qwenctx/{doc,qwenctx}.go`, `inference/doc.go`, `inference/router/{router,invocation_stamp}.go`, `measure/classify.go`, `refresolve/domain_term_classifier.go`, `arcreview/dispatch.go`, `db/grounding_events.go:88`, `cmd/grounding-events-processor/{main,session}.go`, `cmd/observability-audit-emit/main.go`. These describe the historical sink narrative or the `±5s` success-predicate join window; none execute SQL against the table. They are the "no orphaned readers remain" cleanup surface (completion_condition), to be reviewed/repointed-in-prose during retirement.
- **Retirement mechanism:** new DROP migration (`DROP TABLE IF EXISTS qwen_invocations`) in both migration locations + delete the dead writer (`qwen_telemetry.go`, `QwenInvocation`, `RecordQwenInvocation`) and its test (`qwen_telemetry_test.go`) + scrub stale doc-comments/evidence strings.

### 1.2 `vault_search_invocations` — write path already severed (migration 046), drop pre-staged

- **Schema:** `(id, query, top_k, results_count, latency_ms, input_tokens, output_tokens, created_at)` + `pass1_latency_ms`, `pass2_latency_ms` (migration 011) + index. Migrations 009/011.
- **Writer:** **none live.** Migration 046 (`telemetry-substrate-cleanup` T2) switched the `vault_search` handler to write `grounding_events` (`pass1_latency_ms`/`pass2_latency_ms` columns) in the same commit; the old writer was removed then. No `INSERT INTO vault_search_invocations` exists in Go.
- **Readers:** **none live.** Only reference in Go is the table-existence spot-check list in `db/migrate_test.go:59` (a "must exist" assertion) and an audit-evidence string in `cmd/telemetry-audit-emit/main.go`.
- **Retirement mechanism:** **already pre-staged.** `migrations/047_drop_per_handler_telemetry_tables.sql.skeleton` (held out of the runner's `*.sql` glob) drops both this and `kiwix_offload_invocations`. Activate by renaming `.skeleton → .sql` in both migration locations (the skeleton's step-3 names a third `crates/shared-db/migrations/` location — **stale**; that tree was removed with the Rust workspace, see CLAUDE.md §Migrations).

### 1.3 `kiwix_offload_invocations` — write path already severed (migration 046), drop pre-staged

- **Schema:** `(id, query, top_k, hits_in, hits_out, latency_ms, input_tokens, output_tokens, qwen_fell_back, created_at)` + index. Migration 014.
- **Writer:** **none live.** Same migration-046 switch; specifics now in `grounding_events` (`qwen_fell_back`, `kiwix_hits_in`, `kiwix_hits_out`).
- **Readers:** **none live.** Same `migrate_test.go:59` existence assertion + `cmd/telemetry-audit-emit/main.go` evidence string; `kiwix_handler.go`/`grounding.go` references are comments.
- **Retirement mechanism:** same migration 047 as §1.2.

### 1.4 Soak gate (047's precondition)

047's activation note requires migration 046 to have been in production **≥ 1 week**. Current migration head is **082**; 046 predates it by 36 migrations and is referenced as long-landed across the program docs. **Soak satisfied.** 047's verification query (recent `grounding_events` `vault_search` rows carry `pass1_latency_ms`) should be run against the live DB at execution time as the empirical confirmation.

---

## 2. Data-format / column-naming unification (§1.6, §2.4)

The program doc flagged "the same concept under different column names" with two examples:

- **`input_tokens` vs `tokens_in`:** the live read-side substrate uniformly uses **`input_tokens` / `output_tokens`** (per-call) and **`total_input_tokens` / `total_output_tokens`** (projection). The only `tokens_in*`/`tokens_out*`-shaped columns are `tokens_input`/`tokens_output` in `portal_chats` (migration 008) — a table **already dropped** in migration 031, surviving only in immutable migration history (correctly never edited). **No live drift.**
- **`error_class` vs a success predicate:** resolved by Chains 1–2 — `inference_invocations.error_class` (closed enum) is the call-level layer; outcome success is materialized in `proj_inference_call_success` (migration 082). **Unified.**

**Finding:** the column-naming/data-format unification this chain nominally owns was **substantively achieved by Chains 1–2** establishing `input_tokens`/`output_tokens` + the both-layers success model as canonical. The residual is a **verify-and-document** task, not a refactor — confirm no live drift remains across the read-side substrate and record the canonical naming in `TELEMETRY_SUBSTRATE.md`. Per the refactor triage gate, manufacturing a rename refactor here would be churn against already-unified code (recorded as a rejection, see T4).

---

## 3. Characterization net (step 2 — the gate)

The chain's net is **rebuild parity — no data loss**. Current state pinned by:

1. **(NEW, this task)** `TestLegacySinksFeedNoReadSideProjection` (`internal/projections/legacy_sink_independence_test.go`) — seeds **only** the three doomed sinks, rebuilds every read-side projection, asserts all are empty. Directly pins the load-bearing invariant: **the sinks feed no projection, so the DROP loses no projection data.** GREEN.
2. **(existing, Chain 1)** `internal/observehttp/inference_characterization_test.go` — 9 golden tests pinning `/inference/*` endpoint JSON. These already read the projection/`inference_invocations`, not `qwen_invocations`, so they prove the endpoints are independent of the doomed sink. GREEN.
3. **(existing)** `TestInferenceToolModelPerformance_RebuildIsByteIdentical` / `_FoldEqualsRebuild` — read-side `Fold == RebuildFromEmpty` invariant. GREEN.
4. **(existing)** `db.TestOpen_AppliesAllMigrations` — table-existence spot-checks. **Currently asserts `vault_search_invocations` + `kiwix_offload_invocations` EXIST** (`migrate_test.go:59`). This is the assertion the retirement must **flip** (move both to the inverse "must-NOT-exist" list, alongside `qwen_invocations`) — the pin that proves the drop took effect.

**Structural proof (grep):** no projection source in `internal/projections/` references any of the three doomed tables; no migration view/trigger references them. Independence is both structurally and (now) test-pinned.

Baseline suite GREEN at branch base `dd476062`: `go test -tags sqlite_fts5 ./internal/db/... ./internal/projections/... ./internal/observehttp/...` → all `ok`.

---

## 4. Retirement order + soak (what T-exec must respect)

1. **`qwen_invocations`** and **`vault_search_invocations`/`kiwix_offload_invocations`** are independent drops — orderable in either order. Group them in one new migration + the 047 activation for a single rebuild-parity proof.
2. Run 047's empirical verification query against the live DB before activating (047 step 2).
3. Flip `migrate_test.go` existence assertions in the **same commit** as the drop migration (the gate runs them).
4. Delete dead `qwen_invocations` writer code + test in the same change; scrub stale doc-comments/evidence strings (completion_condition "no orphaned readers remain").
5. Full gate + rebuild-parity green after; densify (pin the "tables absent" state).

---

## 5. Downstream tasks forged from this inventory (steps 3–7)

- **T2** — Audit + findings ledger + first-principles target + triage (steps 3–5): classify every reference (live-reader vs dead-writer vs doc-comment), lock the retirement target shape, record the column-naming "already unified → verify-only" rejection.
- **T3** — Behavior-preserving execution + parity (step 6): the DROP migration (qwen_invocations) + 047 activation (vault/kiwix), dead-code deletion, `migrate_test` assertion flip, doc-comment scrub; full net + rebuild-parity green.
- **T4** — Data-format/column-naming unification verify-and-document (the §2 residual): confirm no live drift, record canonical naming in `TELEMETRY_SUBSTRATE.md`, record the rejection of a rename refactor.
- **T5** — Post-retirement densification + retrospective (step 7): pin the new "tables absent" state, re-run rebuild-parity over the new structure, verify completion_condition, chain retrospective.

---

## 6. T3 execution notes & corrections (added during execution)

- **Correction to §1 "empty-and-unread":** imprecise. The sinks are **unread** (no live reader/projection — that part holds, grep + net proven) and receive **no new writes**, but they are **not empty**: on the live DB they hold pre-cutover historical rows (`qwen_invocations` 990, `vault_search_invocations` 151, `kiwix_offload_invocations` 56). The retirement **intentionally discards** these raw rows — that is what sink retirement is. "No data loss" in this chain's net means *no loss of data any live reader/projection depends on*, which holds: migration 046 explicitly accepts that historical handler-specifics not backfilled into `grounding_events` are dropped ("the consolidated shape is what training pipelines depend on going forward"), and Chain 1's dual-write was forward-only by design. The `cmd/observability-audit-emit` evidence string's "now-empty" was likewise about *no new writes*, not row count.
- **Runner-numbering question (T2's open item) — RESOLVED:** `db/migrate.go` `RunMigrations` applies any migration whose slug is absent from `_migrations`, by a per-slug presence check, regardless of its number relative to head. So `047` (below head 082) applies on next deploy without renumbering. Confirmed empirically (below).
- **Empirical deploy parity proof:** ran the real runner (`db.Open`) against a **copy** of the live `data/toolkit.db`. Result: 047 + 083 applied and recorded in `_migrations`; all three sinks dropped; survivor tables unchanged (`grounding_events` 984, `events` 15692, `knowledge_pointers` 2640) and the dashboard's source projection `proj_inference_tool_model_performance` intact (4 rows). No-data-loss at deploy level proven; the live DB itself was untouched (tested on the copy).
- **Independent finding filed:** bug `vault-search-grounding-rows-missing-pass1-latency` — ~132 of 215 live-emit post-046 `vault_search` `grounding_events` rows have `results_count` but no `pass1_latency_ms`. This is a **write-path** gap (the doomed sink holds no post-05-19 rows), independent of and non-blocking for the drop; routed to the vault_search write-path owner.
- **Irreversibility note:** the DROP is committed to the chain branch but is **inert until merge** — the post-merge advisor runs the migration on the live DB at merge time. The user vets before merge; this chain stops at a verified, ready-to-merge branch.
