-- Universal per-call telemetry for the Go inference router (bug 1328).
--
-- Before this table, /inference/stats merged three disjoint sources
-- (vault_search_invocations, kiwix_offload_invocations, benchmark_results)
-- under one "Calls" column. Only the first two had continuous-write
-- paths in production; the third only populated when an agent
-- explicitly invoked classify_X, so ~17 task_ids on the dashboard
-- froze the moment benchmarks stopped running. The "Calls" affordance
-- looked live but was week-old benchmark data.
--
-- This table is written from router.GenerateWithOpts on every Qwen call,
-- with task_id stamped via qwenctx so the row is attributable to its
-- handler (rubric name for classify, "vault-rerank-retrieve" for vault
-- search retrieval, etc.). Unattributed callers land under task_id =
-- "unattributed" — the row still exists so the volume figure is
-- accurate even when the caller forgot to stamp ctx.
--
-- /inference/stats now reads call_count + avg_latency_ms from this
-- table. The vault/kiwix telemetry tables remain authoritative for
-- their per-table specifics (pass1/pass2 latency, truncation,
-- fell-back signal) — they just no longer feed the universal
-- aggregation.

CREATE TABLE qwen_invocations (
    id              INTEGER PRIMARY KEY,
    task_id         TEXT NOT NULL,
    model_name      TEXT NOT NULL,
    latency_ms      INTEGER NOT NULL,
    input_tokens    INTEGER,
    output_tokens   INTEGER,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_qwen_invocations_task_created
    ON qwen_invocations (task_id, created_at);
