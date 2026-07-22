-- Link a filed bug to the benchmark_results row that triggered it.
-- Nullable — only populated when a Qwen classify/extract/retrieve call
-- surfaces a bug directly (e.g. a forge-call-extraction run that spots
-- a dispatch regression). The value is the benchmark_results.task_id
-- string (e.g. "severity-classification-bug-filing") so the
-- /inference/stats endpoint can cross-reference call volume against
-- bug counts per task without a join.

ALTER TABLE bugs ADD COLUMN qwen_task_id TEXT;

CREATE INDEX IF NOT EXISTS idx_bugs_qwen_task_id
    ON bugs (qwen_task_id) WHERE qwen_task_id IS NOT NULL;
