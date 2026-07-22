# Chain 5 — Legacy-sink retirement & data-format unification — Retrospective

> Chain `legacy-telemetry-sink-retirement` (309), T5. Closes the **5-chain telemetry-consolidation program** (`docs/TELEMETRY_CONSOLIDATION.md`).
> Branch `worktree-legacy-telemetry-sink-retirement`. Commits: T1 `1457499c` · T2 `d6a48f5e` · T3 `8509fe13` · T4 `63ac2636` · T5 (this).

## What shipped

The three remaining legacy telemetry sinks are retired:

- **`qwen_invocations`** — DROPped (migration 083). Superseded by `inference_invocations` + `proj_inference_tool_model_performance` in Chain 1; dual-write removed at `f97cd465`; dead writer (`db/qwen_telemetry.go` + test) deleted.
- **`vault_search_invocations` + `kiwix_offload_invocations`** — DROPped by activating the pre-staged migration 047 (`.sql.skeleton → .sql`). Absorbed into `grounding_events` by migration 046 (`telemetry-substrate-cleanup`).
- **Data-format / column-naming** — verified no live drift; canonical conventions documented in `TELEMETRY_SUBSTRATE.md §9.4`.

## completion_condition — clause by clause

| Clause | Status | Evidence |
|---|---|---|
| `qwen_invocations` dropped; all readers on `inference_tool_model_performance` | ✅ | migration 083; no production reader of the sink (T1 grep + net); `/inference` reads `proj_inference_tool_model_performance`, intact (4 rows) in the empirical parity run |
| `vault_search_invocations`/`kiwix_offload_invocations` retirement completed/coordinated | ✅ | migration 047 activated; both dropped; coordinated with the already-planned `telemetry-substrate-cleanup` staging |
| data-format/column-naming unified | ✅ | T4 sweep found no live drift; documented in `TELEMETRY_SUBSTRATE.md §9.4` |
| rebuild-parity verified | ✅ | full suite green (55 pkgs); `RebuildIsByteIdentical` + Chain-1 inference goldens byte-identical; empirical deploy parity on a copy of the live DB |
| no orphaned readers remain | ✅ | zero live readers; stale current-behavior comments repointed to `inference_invocations`; only migration history, lineage notes, and frozen audit-evidence strings retain the name (intentionally) |

## Refactor-discipline scorecard

- **Net-first held.** T1 pinned current behavior (the now-superseded independence guard + the inherited Chain-1 goldens + rebuild-parity) before a line moved. The parity nets that *must not change* (inference goldens) stayed byte-identical end-to-end.
- **Triage said no.** The program named a "column-naming unification" — the audit found it already done by Chains 1–2 and **rejected** a rename refactor with a recorded reason rather than manufacturing churn.
- **One transformation per commit**; full gate green at each.

## What surprised us (lessons)

1. **"Unread" ≠ "empty".** The sinks were described as "empty/unread"; on the live DB they held pre-cutover historical rows (qwen 990, vault 151, kiwix 56). A superseded sink keeps its old rows after its *writer* is severed. The retirement *intentionally discards* them — "no data loss" means *no loss of data any live reader/projection depends on*, not *no rows exist*. The discard is design-sanctioned (migration 046/077 chose forward-only consolidation). **Verify row counts against a copy of the live DB before asserting a drop is lossless** — don't trust an audit-evidence string's "now-empty" (it meant "no new writes").
2. **The staged `.skeleton` mechanism works as designed.** The runner (`db/migrate.go`) applies any migration whose slug is absent from `_migrations` by a per-slug check, *regardless of its number vs head* — so a long-staged, low-numbered drop (047, below head 082) activates on rename with no renumber. Proven empirically against the live-DB copy.
3. **Drop-on-a-branch is reversible until merge.** The DROP is inert until the post-merge advisor migrates the live DB. Testing against a *copy* kept the live DB untouched while proving the transition.

## Filed during this chain

- bug `vault-search-grounding-rows-missing-pass1-latency` — ~132/215 live-emit post-046 `vault_search` `grounding_events` rows carry `results_count` but no `pass1_latency_ms`. A **write-path** gap independent of this chain (the dropped sink held no post-cutover rows); routed to the `vault_search` write-path owner.

## Program close

This is **Chain 5 of 5** — the telemetry-consolidation program is complete: inference telemetry moved onto the read-side substrate (Chain 1), success model unified (Chain 2), data-driven routing (Chain 3), page IA unified (Chain 4), legacy sinks retired + data-format unified (Chain 5). The substrate is now one architecture (emit → fold → projection → page) with no off-pattern raw sinks remaining.
