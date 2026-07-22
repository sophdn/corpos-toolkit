-- Rename `rubric_name` to `task_id` on benchmark_results, reindex, and
-- backfill task_id on the legacy untagged scenarios.
--
-- Per chain `mcp-servers/benchmarks-page-per-task-redesign` T1: the
-- discrete-task identifier needs to be broader than rubric. Legacy
-- benchmark scenarios (vault-rerank-retrieve, severity-classification-
-- bug-filing, forge-call-extraction, etc.) are also discrete tasks
-- but were never rubric-tagged. Renaming the column unifies semantics:
-- every row's `task_id` names the offload target it exercises.
--
-- Backfill rules (idempotent — UPDATEs gate on task_id IS NULL):
--   classify-severity-* / classify-honesty-severity-*  → severity-classification-bug-filing
--   classify-surface-*                                  → surface-tag-multi-class
--   classify-route-* / classify-honesty-route-*         → route-decision-bug-filing
--   retirement-* (untagged from buggy first run)        → retirement-signal
--   tier-* (untagged from digit-suffix first run)       → tiered-context
--   extract-bug-* / extract-honesty-*                   → forge-call-extraction
--   retrieve-decision-* / retrieve-bug-list-* /         →
--     retrieve-pass2-*                                  → vault-rerank-retrieve
--   retrieve-kiwix-*                                    → kiwix-rerank-retrieve
--   summarize-*                                         → summarize-tool-output
--
-- On a fresh DB these UPDATEs touch 0 rows; on the live deployment
-- they tag the ~90 legacy untagged rows. Idempotent on re-run.

ALTER TABLE benchmark_results RENAME COLUMN rubric_name TO task_id;

-- Reindex so the index name reflects the new column.
DROP INDEX IF EXISTS idx_bench_rubric_name;
CREATE INDEX IF NOT EXISTS idx_bench_task_id ON benchmark_results (task_id);

-- Backfill: tag legacy untagged scenarios with their discrete task_id.
UPDATE benchmark_results SET task_id = 'severity-classification-bug-filing'
  WHERE task_id IS NULL AND task_shape = 'Classify'
    AND (scenario_id LIKE 'classify-severity-%' OR scenario_id LIKE 'classify-honesty-severity-%');

UPDATE benchmark_results SET task_id = 'surface-tag-multi-class'
  WHERE task_id IS NULL AND task_shape = 'Classify'
    AND scenario_id LIKE 'classify-surface-%';

UPDATE benchmark_results SET task_id = 'route-decision-bug-filing'
  WHERE task_id IS NULL AND task_shape = 'Classify'
    AND (scenario_id LIKE 'classify-route-%' OR scenario_id LIKE 'classify-honesty-route-%');

UPDATE benchmark_results SET task_id = 'retirement-signal'
  WHERE task_id IS NULL AND task_shape = 'Classify'
    AND scenario_id LIKE 'retirement-%';

UPDATE benchmark_results SET task_id = 'tiered-context'
  WHERE task_id IS NULL AND task_shape = 'Classify'
    AND scenario_id LIKE 'tier-%';

UPDATE benchmark_results SET task_id = 'forge-call-extraction'
  WHERE task_id IS NULL AND task_shape = 'Extract'
    AND (scenario_id LIKE 'extract-bug-%' OR scenario_id LIKE 'extract-honesty-%');

UPDATE benchmark_results SET task_id = 'vault-rerank-retrieve'
  WHERE task_id IS NULL AND task_shape = 'Retrieve'
    AND (scenario_id LIKE 'retrieve-decision-%'
         OR scenario_id LIKE 'retrieve-bug-list-%'
         OR scenario_id LIKE 'retrieve-pass2-%'
         OR scenario_id LIKE 'retrieve-vault-%'
         OR scenario_id LIKE 'retrieve-suggest-%');

UPDATE benchmark_results SET task_id = 'kiwix-rerank-retrieve'
  WHERE task_id IS NULL AND task_shape = 'Retrieve'
    AND scenario_id LIKE 'retrieve-kiwix-%';

UPDATE benchmark_results SET task_id = 'summarize-tool-output'
  WHERE task_id IS NULL AND task_shape = 'Summarize'
    AND scenario_id LIKE 'summarize-%';
