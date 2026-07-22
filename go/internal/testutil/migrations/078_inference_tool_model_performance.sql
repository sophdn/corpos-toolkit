-- Chain per-tool-per-model-observability (T12, execution-projection-and-repoint):
-- proj_inference_tool_model_performance is the read-side projection over
-- inference_invocations, keyed (task_id, model_name) — the per-tool-per-model
-- ranking surface this chain exists to provide, and the table the Chain-3
-- data-driven router will read.
--
-- Stored as running TOTALS so (a) rates/averages compute on read and (b) the
-- read-side invariant Fold == RebuildFromEmpty holds vacuously: the
-- projection re-snapshots from inference_invocations on every fold (the
-- proj_query_volume_by_source pattern), so a rebuild-from-empty is
-- byte-identical to the incremental state by construction.
--
-- success_count is the CALL-LEVEL success sum (inference_invocations.success);
-- it is NOT the read-time predicate-registry success the /inference health
-- cards emit (that stays on the per-call table for parity). Chain 2 unifies
-- the two success notions. See docs/CHAIN1_INFERENCE_TELEMETRY_DESIGN.md §2.2.
--
-- Latency percentiles are intentionally absent: they are not foldable from
-- totals, so percentile reads stay on the per-call table (design §3). This
-- projection carries avg (= total/count) and max latency, which is what
-- per-model ranking needs.

CREATE TABLE proj_inference_tool_model_performance (
    task_id             TEXT    NOT NULL,                 -- the tool / inference purpose
    model_name          TEXT    NOT NULL,
    call_count          INTEGER NOT NULL DEFAULT 0,
    success_count       INTEGER NOT NULL DEFAULT 0,       -- SUM(inference_invocations.success) — call-level
    total_latency_ms    INTEGER NOT NULL DEFAULT 0,       -- → avg = total / call_count
    max_latency_ms      INTEGER NOT NULL DEFAULT 0,
    total_input_tokens  INTEGER NOT NULL DEFAULT 0,
    total_output_tokens INTEGER NOT NULL DEFAULT 0,
    calls_with_tokens   INTEGER NOT NULL DEFAULT 0,       -- denom for "avg tokens where usage known"
    last_invoked_at     TEXT    NOT NULL DEFAULT '',
    last_event_id       TEXT    NOT NULL DEFAULT '',       -- watermark convention (carries MAX(id); '' baseline)
    last_event_ts       TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (task_id, model_name)
);

CREATE INDEX proj_itmp_task_idx ON proj_inference_tool_model_performance (task_id);
