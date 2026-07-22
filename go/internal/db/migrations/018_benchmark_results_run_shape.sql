-- Add `run_shape` to `benchmark_results` so every write is self-describing.
--
-- Values: 'production' (live MCP dispatch via record_benchmark_dispatch),
-- 'regression' (regression runner binaries), 'smoke' (A1 smoke runner via
-- run_classify_scenario). NULL is kept for legacy rows — no backfill needed.
--
-- Per bug benchmark-results-no-run-shape-column-mixes-smoke-and-regression-rows.

ALTER TABLE benchmark_results ADD COLUMN run_shape TEXT;

CREATE INDEX IF NOT EXISTS idx_bench_run_shape ON benchmark_results (run_shape);
