-- Add `task_shape` discriminator + per-criterion subscore columns to
-- benchmark_results so the dashboard can render the shape × criterion
-- card grid (one card per shape, each card a radar of criterion
-- subscores with multi-model overlay).
--
-- Reframes the `layer` axis (E1-E6) introduced in migration 012:
-- E1-E6 conflated task shape (E1=schema-extraction, E5=summarize-with-
-- budget) with predicates over outputs (E2/E3 = ranking-quality,
-- E6 = honesty). The shape × criteria model collapses E2+E3 into
-- Retrieve and absorbs E6 as a per-row honesty subscore measurable
-- across all four shapes (Extract, Classify, Retrieve, Summarize).
-- Chain `benchmarks-shape-criteria-reshape` T2 (scope doc:
-- seed-packet/process-docs/adhoc/benchmarks-shape-criteria-reshape/scope.md).
--
-- The `layer` column STAYS. Subsequent tasks (runners, endpoint,
-- dashboard) shift to reading task_shape; the layer column gets
-- dropped in lock-in once nothing reads it.
--
-- Validation is enforced Rust-side at insert time (matches the
-- benchmarks/db.rs is_known_task_shape helper) rather than via a SQL
-- CHECK constraint — SQLite's ALTER TABLE ADD COLUMN historically
-- didn't support CHECK reliably across versions.

ALTER TABLE benchmark_results ADD COLUMN task_shape TEXT;
ALTER TABLE benchmark_results ADD COLUMN accuracy_score REAL;
ALTER TABLE benchmark_results ADD COLUMN honesty_score REAL;
ALTER TABLE benchmark_results ADD COLUMN ranking_quality_score REAL;
ALTER TABLE benchmark_results ADD COLUMN within_budget_score REAL;

-- Backfill from the existing `layer` column. Order doesn't matter here
-- since each clause's WHERE is mutually exclusive on task_shape IS NULL.
--
-- E-tier rows: each E-tier maps to exactly one shape. E6 rows split by
-- scenario_id (each E6 scenario already names its underlying shape).
-- For E6 rows, honesty_score = invocation_ok (1.0 if the model refused,
-- 0.0 if it hallucinated).
UPDATE benchmark_results SET task_shape = 'Extract'
  WHERE task_shape IS NULL AND layer = 'e1';
UPDATE benchmark_results SET task_shape = 'Classify'
  WHERE task_shape IS NULL AND layer = 'e4';
UPDATE benchmark_results SET task_shape = 'Retrieve'
  WHERE task_shape IS NULL AND layer IN ('e2', 'e3');
UPDATE benchmark_results SET task_shape = 'Summarize'
  WHERE task_shape IS NULL AND layer = 'e5';

UPDATE benchmark_results
   SET task_shape = 'Retrieve', honesty_score = invocation_ok * 1.0
 WHERE task_shape IS NULL AND layer = 'e6'
   AND scenario_id LIKE 'e6-vault-rerank-%';
UPDATE benchmark_results
   SET task_shape = 'Extract', honesty_score = invocation_ok * 1.0
 WHERE task_shape IS NULL AND layer = 'e6'
   AND scenario_id LIKE 'e6-extract-%';
UPDATE benchmark_results
   SET task_shape = 'Classify', honesty_score = invocation_ok * 1.0
 WHERE task_shape IS NULL AND layer = 'e6'
   AND scenario_id IN ('e6-classify-with-no-fitting-label', 'e6-tool-call-when-no-tool-fits');
UPDATE benchmark_results
   SET task_shape = 'Summarize', honesty_score = invocation_ok * 1.0
 WHERE task_shape IS NULL AND layer = 'e6'
   AND scenario_id LIKE 'e6-summarize-%';

-- L-tier rows: best-effort mapping. L3 (tool-name selection) → Classify,
-- L4 + L5 (argument extraction + interpretation — both extraction-
-- shaped) → Extract, L6 (NoTool refusal) → Classify with honesty
-- subscore. Ambiguous rows that don't match any predicate stay NULL
-- (queryable but won't appear on the new card grid).
UPDATE benchmark_results SET task_shape = 'Classify'
  WHERE task_shape IS NULL AND layer = 'l3';
UPDATE benchmark_results SET task_shape = 'Extract'
  WHERE task_shape IS NULL AND layer IN ('l4', 'l5');
UPDATE benchmark_results
   SET task_shape = 'Classify', honesty_score = invocation_ok * 1.0
 WHERE task_shape IS NULL AND layer = 'l6';

CREATE INDEX IF NOT EXISTS idx_bench_task_shape ON benchmark_results (task_shape);
