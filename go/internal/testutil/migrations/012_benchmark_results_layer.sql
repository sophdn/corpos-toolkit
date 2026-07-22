-- Add `layer` discriminator to benchmark_results so the dashboard can
-- group rows by framework tier (l3/l4/l5/l6 — original tool-dispatch
-- layers; e1-e6 — utility-shape layers added by chain
-- benchmarks-framework-reshape).
--
-- Backfill mirrors the rule used by the benchmarks crate's own DB
-- migration (mcp-servers/benchmarks/src/db.rs::backfill_layer):
-- most-specific predicate first, default to l3.
--
-- Validation is enforced Rust-side at insert time (matches the
-- benchmarks/db.rs is_known_layer helper) rather than via a SQL CHECK
-- constraint — SQLite's ALTER TABLE ADD COLUMN historically didn't
-- support CHECK reliably across versions.
--
-- Note: as of this migration the canonical write path for
-- benchmark_results into toolkit.db isn't wired (the benchmarks
-- runner writes to a separate /mnt/data1/benchmarks.db). Bug filed
-- separately. The column lands now so the observe-http endpoint and
-- the dashboard can be updated independently of the runner write-path
-- fix.

ALTER TABLE benchmark_results ADD COLUMN layer TEXT;

-- Backfill order matters: most-specific predicate first.
UPDATE benchmark_results SET layer = 'l6'
  WHERE layer IS NULL AND notes LIKE 'l6:%';
UPDATE benchmark_results SET layer = 'l5'
  WHERE layer IS NULL AND interpretation_ok IS NOT NULL;
UPDATE benchmark_results SET layer = 'l4'
  WHERE layer IS NULL AND args_match IS NOT NULL;
UPDATE benchmark_results SET layer = 'l3'
  WHERE layer IS NULL;

CREATE INDEX IF NOT EXISTS idx_bench_layer ON benchmark_results (layer);
