-- Add `rubric_name` discriminator to benchmark_results so the
-- dashboard can render per-(shape, model, rubric) cards. One card per
-- rubric on the Benchmarks page; row filter when the dashboard's
-- per-rubric drill-down fires.
--
-- Foundation chain `mcp-servers/extract-now-rubric-foundation` T5.
-- Subsequent task T6 backfills the 9 A1 sub-chain smoke run_ids with
-- their rubric_name so the dashboard has data on first render.
--
-- Validation: rubric_name is enforced Rust-side at insert time against
-- the rubric-lib::registry table (the same registry the dispatcher
-- uses), keeping the db column and the runtime registry in lockstep.
-- No SQL CHECK constraint for the same reason migration 013 cited
-- (SQLite ALTER TABLE ADD COLUMN historic CHECK support).

ALTER TABLE benchmark_results ADD COLUMN rubric_name TEXT;

-- Index for the dashboard's per-rubric card filter. Cardinality is
-- bounded (~9 rubrics) so this is a small ceiling — the index pays
-- for itself on the GROUP BY rubric_name aggregation that the cards
-- endpoint will run.
CREATE INDEX IF NOT EXISTS idx_bench_rubric_name ON benchmark_results (rubric_name);
