-- agent-first-substrate chain T6 — benchmark provenance + cutover trigger.
--
-- Promotes benchmark-run provenance to a typed first-class table joined 1:1
-- to benchmark_results via run_id. Every run from the cutover line forward
-- carries the full (model_id, model_version, prompt_template_hash,
-- corpus_hash, retriever_version, retriever_config_hash, seed, env_hash)
-- bundle, joined to the BenchmarkRunStarted event that emitted at run-start
-- via started_event_id. See docs/EVENT_SUBSTRATE.md §… and docs/BENCHMARKS.md
-- "Provenance & replay" for the contract; blueprints/events/BenchmarkRunStarted.json
-- is the schema source-of-truth for the eight provenance fields.
--
-- ONE-WAY CUTOVER LINE: existing benchmark_results rows predate provenance
-- and remain provenance_id IS NULL forever. The BEFORE INSERT trigger below
-- raises ABORT on any NEW row missing provenance_id, so every write from
-- this migration's apply-time forward is replayable. Pre-cutover rows are
-- legacy archaeology; replay is undefined for them and the Go
-- HandleBenchmarkReplay handler surfaces a clear error when called against
-- a NULL-provenance row.

CREATE TABLE benchmark_provenance (
    id                     TEXT    PRIMARY KEY,
    -- run_id tags this provenance row with its batch — the per-invocation
    -- UUID a runner allocates and re-uses across every scenario in the
    -- same `benchmarks --tool X --model Y` call. NOT UNIQUE: a batch
    -- emits one provenance row per scenario (prompt + corpus inputs
    -- differ), all sharing the same run_id. The PK that the
    -- benchmark_results.provenance_id FK targets is `id`, so the
    -- per-result lookup is direct and unambiguous regardless of how many
    -- scenarios share a batch.
    run_id                 TEXT    NOT NULL,
    model_id               TEXT    NOT NULL,
    model_version          TEXT    NOT NULL,
    prompt_template_hash   TEXT    NOT NULL,
    corpus_hash            TEXT    NOT NULL,
    retriever_version      TEXT    NOT NULL,
    retriever_config_hash  TEXT    NOT NULL,
    seed                   INTEGER NOT NULL,
    env_hash               TEXT    NOT NULL,
    -- started_event_id points at the BenchmarkRunStarted that carried this
    -- provenance payload. Joins the relational shape back to the event log
    -- without a hard FK (events table is append-only; FK-constraint cascade
    -- semantics don't apply). Stored as plain TEXT so the join is opt-in.
    started_event_id       TEXT    NOT NULL,
    created_at             INTEGER NOT NULL DEFAULT (unixepoch())
);

-- Replay lookup paths the dashboard + Go HandleBenchmarkReplay will use.
-- model_id/model_version pairs let "show every run on this exact model
-- weights" stay an index scan. corpus_hash catches "every run on this
-- gold-corpus snapshot". env_hash supports detecting cross-machine drift.
CREATE INDEX idx_bench_prov_model    ON benchmark_provenance (model_id, model_version);
CREATE INDEX idx_bench_prov_corpus   ON benchmark_provenance (corpus_hash);
CREATE INDEX idx_bench_prov_env      ON benchmark_provenance (env_hash);
-- Batch-lookup: "list every provenance row for this run_id" supports the
-- dashboard's per-batch drill-down and the Go replay handler's lookup.
CREATE INDEX idx_bench_prov_run_id   ON benchmark_provenance (run_id);

-- Add the join column to benchmark_results. ALTER TABLE ADD COLUMN with no
-- NOT NULL is the SQLite-friendly migration shape: existing rows stay valid
-- with the column populated as NULL.
ALTER TABLE benchmark_results ADD COLUMN provenance_id TEXT REFERENCES benchmark_provenance(id);

-- Cutover trigger: every NEW row must carry a non-NULL provenance_id from
-- this migration's apply-time forward. RAISE(ABORT, ...) rolls back the
-- enclosing transaction with the message below — the Rust harness, Go
-- HandleBenchmarkRecord, and any future writer all see the same failure
-- shape if they forget to thread provenance through.
CREATE TRIGGER benchmark_results_require_provenance
BEFORE INSERT ON benchmark_results
FOR EACH ROW
WHEN NEW.provenance_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'benchmark_results.provenance_id NOT NULL required after migration 035 (T6 cutover): emit a benchmark_provenance row + BenchmarkRunStarted event first, then INSERT the result with provenance_id set');
END;
