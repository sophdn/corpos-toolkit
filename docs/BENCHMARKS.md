# Benchmarks — provenance & replay

This doc is the contract surface for the benchmark substrate after T6 of
the `agent-first-substrate` chain. It covers:

- the `BenchmarkProvenance` 8-field bundle attached to every run,
- the `benchmark_provenance` storage table + the migration-035 cutover
  trigger that enforces it on `benchmark_results`,
- the `benchmark_replay` action (`measure` meta-tool) — how to invoke
  it, what its diff output means, and what to do when replay drifts.

For the broader event substrate that benchmark events plug into, see
`docs/EVENT_SUBSTRATE.md`. For the catalog of every event type, see
`docs/EVENT_CATALOG.md`.

## Provenance contract

Every benchmark run records the following bundle at run-start, captured
on the `BenchmarkRunStarted` event's `payload.provenance` field. All
eight fields are required — a run that can't pin one is rejected at
the JSON-schema validation step before any DB write lands.

| Field | Type | Source |
|---|---|---|
| `model_id` | string | the inference router's `model.name()` (e.g. `claude-opus-4-7`, `qwen2.5-32b`) |
| `model_version` | string | `model.version()` — the full versioned id (Anthropic `claude-opus-4-7-20260301`) or local GGUF hash / Ollama digest |
| `prompt_template_hash` | hex SHA-256 | over the prompt the model saw, byte-for-byte. Pins prompt content; a template edit produces a different hash |
| `corpus_hash` | hex SHA-256 | over the scenario inputs (gold answers, worked examples, scenario id depending on the runner) |
| `retriever_version` | string | which retriever (`qwen-rerank-2.5-32b-v3`, `fts5-default`, `no-retriever`) |
| `retriever_config_hash` | hex SHA-256 | over the retriever's runtime knobs (top_k, similarity threshold, reranker temp, fusion weights). Distinct from `retriever_version` — same version with different config produces different rankings |
| `seed` | integer | RNG seed. `0` + documented when the model doesn't accept a seed (Anthropic API) |
| `env_hash` | hex SHA-256 | binary build SHA + Cargo.lock SHA. Detects "ran on a different binary and got a different answer" drift |

`wall_clock_start` is satisfied by the envelope's `event_time`;
`wall_clock_end` lives on `BenchmarkRunCompleted.wall_clock_ms`. Neither
needs a payload field.

### What edits invalidate a replay

Changing any of the following produces a different hash and breaks
byte-identity replay:

- the **rubric prose** or **worked examples** — surface via
  `prompt_template_hash`.
- the **scenario gold** or **scenario id** — surface via `corpus_hash`.
- the **retriever knobs** (k, threshold, reranker temp) — surface via
  `retriever_config_hash`.
- a **dependency bump** — surface via `env_hash` (Cargo.lock SHA shifts).
- a **binary rebuild from a different git SHA** — surface via `env_hash`.

This is the intended design: the diff surfaces *why* the replay no
longer reproduces, not just that it failed to.

## Storage

`benchmark_provenance` (introduced by migration `035_benchmark_provenance.sql`):

```sql
CREATE TABLE benchmark_provenance (
    id                     TEXT    PRIMARY KEY,        -- UUIDv4
    run_id                 TEXT    NOT NULL,           -- batch tag, NOT UNIQUE
    model_id               TEXT    NOT NULL,
    model_version          TEXT    NOT NULL,
    prompt_template_hash   TEXT    NOT NULL,
    corpus_hash            TEXT    NOT NULL,
    retriever_version      TEXT    NOT NULL,
    retriever_config_hash  TEXT    NOT NULL,
    seed                   INTEGER NOT NULL,
    env_hash               TEXT    NOT NULL,
    started_event_id       TEXT    NOT NULL,           -- FK into events.event_id
    created_at             INTEGER NOT NULL DEFAULT (unixepoch())
);
```

`benchmark_results` gained a nullable `provenance_id` FK column +
`BEFORE INSERT` trigger:

```sql
CREATE TRIGGER benchmark_results_require_provenance
BEFORE INSERT ON benchmark_results
FOR EACH ROW
WHEN NEW.provenance_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'benchmark_results.provenance_id NOT NULL required after migration 035 (T6 cutover)…');
END;
```

### One-way cutover

The trigger applies to every INSERT after migration 035 lands. Rows
predating the migration keep `provenance_id IS NULL` permanently —
they are not replayable, and `benchmark_replay` returns a clear "no
provenance (pre-T6-cutover legacy row); not replayable" error against
them. The migration deliberately does not backfill: pre-cutover runs
didn't capture the inputs we now require, so synthesizing a sentinel
would be lying about what we can reproduce.

The Rust harness (`benchmarks/src/db.rs::BenchmarkDb::record`) and the
Go handler (`go/internal/measure/benchmark.go::HandleBenchmarkRecord`)
both surface a structured "missing provenance_id" error *before* the
trigger fires, so the caller sees an actionable message rather than the
raw RAISE text.

## benchmark_replay action

Invoke through the `measure` meta-tool:

```jsonc
{
  "action": "benchmark_replay",
  "params": {
    "row_id": "<benchmark_results.id>"
  },
  "rationale": "Why this replay is being run — debugging non-determinism, validating a model swap, post-incident reproduction."
}
```

`rationale` is required by `action-manifests/dispatch-policy.toml`
(`[measure.benchmark_replay]` → `requires_rationale = true`); replay
writes a new `benchmark_results` row and the rationale lands on the new
`BenchmarkRunStarted` event for archaeology.

### Response shape

```jsonc
{
  "ok": true,
  "original_run_id": "...",
  "replay_run_id": "...",
  "original_row_id": "...",
  "replay_row_id": "...",
  "identical": true,
  "diff": "",
  "stderr_tail": ""
}
```

- `identical: true` ⇒ every score field (`accuracy_score`,
  `honesty_score`, `ranking_quality_score`, `within_budget_score`,
  `extracted_args`, `notes`, `args_match`, `interpretation_ok`)
  matches byte-for-byte between the original and replay rows.
- `identical: false` ⇒ the `diff` field carries a per-field summary:

  ```
  diff between original and replay:
    accuracy_score: original=0.95 replay=0.42
    notes: original="…" replay="…"
  ```

- `error: "…"` ⇒ the replay didn't complete (subprocess failure,
  missing provenance, row not found). `stderr_tail` carries the tail
  of the Rust subprocess's stderr when applicable.

### Worked example

Given an original benchmark row from a Classify run:

```
$ # via stdio MCP, or via a wrapper that posts to the dispatcher:
$ replay-cli measure benchmark_replay \
    --row_id abc12345 \
    --rationale "T6 acceptance smoke — assert deterministic replay against Qwen-local"

{
  "ok": true,
  "original_run_id": "run-...",
  "replay_run_id": "replay-...",
  "original_row_id": "abc12345",
  "replay_row_id": "def67890",
  "identical": true,
  "diff": ""
}
```

When the replay diverges on a non-deterministic model:

```jsonc
{
  "ok": true,
  "original_run_id": "...",
  "replay_run_id": "...",
  "original_row_id": "...",
  "replay_row_id": "...",
  "identical": false,
  "diff": "diff between original and replay:\n  notes: original=\"…label A\" replay=\"…label B\"\n"
}
```

For Anthropic-API runs (no seed support) `identical: false` is
expected; the value of replay there is reproducing the *bounds* of
non-determinism, not asserting byte-identity.

### Replay subprocess

The Go handler resolves the harness binary via the
`TOOLKIT_BENCHMARKS_BIN` env var, defaulting to
`./target/release/benchmarks`. The subprocess takes `--replay <row_id>`
and prints `replay_row_id=<id>` + `replay_run_id=<id>` as the final
two lines of stdout for the handler to parse.

The Rust replay implementation lives at `benchmarks/src/replay.rs`.
Today it copies the original row to a new id with `run_shape = 'replay'`
and a fresh `provenance_id`, re-emitting `BenchmarkRunStarted` with
`refs.caused_by_event_id` pointing at the original's start event. The
follow-up that wires the actual model re-execution + re-grading is
gated by needing the rubric registry available on the replay path
(currently lives in the Go binary's classify dispatch) — until then,
the diff against the copy is the lower bound: any non-determinism in
the replay row's *post-copy mutations* shows up, but model-side
variance is masked. Documented as the "replay-mvp" line for the
follow-up chain to lift.

## Coordinates

- Storage: `crates/shared-db/migrations/035_benchmark_provenance.sql`
- Schema: `blueprints/events/BenchmarkRunStarted.json`
- Rust harness: `benchmarks/src/provenance.rs`, `benchmarks/src/db.rs`
  (`BenchmarkDb::start_run`, `end_run_completed`),
  `benchmarks/src/replay.rs`
- Go handler: `go/internal/measure/benchmark.go`,
  `go/internal/measure/benchmark_replay.go`
- Dispatch policy: `action-manifests/dispatch-policy.toml` →
  `[measure.benchmark_replay]`
- Events helper (Rust): `crates/shared-db/src/events.rs` (`emit`,
  `emit_sqlx`)
- Events helper (Go): `go/internal/events/emit.go` (`Emit`)
