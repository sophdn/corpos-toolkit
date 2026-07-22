-- agent-substrate-crud-retirement T6a unblock — synth missing
-- BenchmarkRunCompleted events for the 716 proj_benchmark_results
-- rows that were snapshot-seeded by migration 058 from CRUD without
-- a post-T5 event source. Closes bug
-- `proj-benchmark-results-rebuild-produces-zero-rows-pre-T5-events-
-- lack-identifying-cols`.
--
-- Pre-T5-benchmarks BenchmarkRunCompleted events (if any exist) lack
-- the identifying-columns + result_columns blocks that T5-benchmarks
-- (94618fe) added; the fold skips them per the
-- foldBenchmarkRunCompleted guard:
--
--   if p.BenchmarkResultID == nil || p.ProjectID == nil { return nil }
--
-- Result: rebuild-from-empty produces 0 rows in proj_benchmark_results
-- vs the live 716. Documented as Option-B-accepted in design §13.3,
-- but it violates the chain's completion_condition (b) for this
-- projection.
--
-- This migration walks proj_benchmark_results and emits one synthetic
-- BenchmarkRunCompleted per row carrying the full payload (result_
-- columns + identifying columns). The fold's INSERT/UPSERT path
-- (ON CONFLICT(id) DO UPDATE) means pre-T5 events that skip and the
-- new synthetic events coexist cleanly. Idempotent: NOT EXISTS guard
-- on the synthetic actor_id.
--
-- We also emit a paired BenchmarkRunStarted event so the
-- caused_by_event_id linkage on the Completed event resolves cleanly
-- (the events.caused_by_event_id has a self-referencing FK; NULL is
-- accepted but a populated linkage is better for audit). The Started
-- event carries minimal valid provenance derived from the projection
-- columns; this is best-effort reconstruction since the original
-- provenance row may or may not exist for these historical runs.

-- ───────────────────────────────────────────────────────────────────
-- 1. Synthetic BenchmarkRunStarted per orphan benchmark row.
--    Provides the caused_by_event_id anchor for the paired Completed
--    emit below.
-- ───────────────────────────────────────────────────────────────────
INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    'started-' || br.id AS event_id, -- deterministic, prefixed for idempotency
    strftime('%Y-%m-%dT%H:%M:%fZ', datetime(br.run_at, 'unixepoch')) AS ts,
    'system' AS actor_kind,
    'benchmark-backfill-063' AS actor_id,
    'BenchmarkRunStarted' AS type,
    'benchmark_run' AS entity_kind,
    COALESCE(br.run_id, br.id) AS entity_slug,
    NULL AS entity_project_id, -- benchmark_run is cross-cutting per the envelope contract
    json_object(
        'scenario_id', br.scenario_id,
        'provenance', json_object(
            'model_id',              br.model_name,
            'model_version',         br.model_name,
            'prompt_template_hash',  'pre-T5-backfill:' || COALESCE(br.task_id, br.tool_name),
            'corpus_hash',           'pre-T5-backfill:' || COALESCE(br.notes, ''),
            'retriever_version',     'no-retriever',
            'retriever_config_hash', 'pre-T5-backfill:no-retriever-config',
            'seed',                  0,
            'env_hash',              'pre-T5-backfill:unknown-env'
        )
    ) AS payload,
    NULL AS rationale,
    NULL AS caused_by_event_id,
    '[]' AS related_entities,
    'started-span-' || br.id AS span_id,
    1 AS schema_version
FROM proj_benchmark_results br
WHERE NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.actor_id = 'benchmark-backfill-063'
      AND e.type = 'BenchmarkRunStarted'
      AND e.entity_kind = 'benchmark_run'
      AND e.entity_slug = COALESCE(br.run_id, br.id)
);

-- ───────────────────────────────────────────────────────────────────
-- 2. Synthetic BenchmarkRunCompleted per orphan benchmark row, with
--    the post-T5 full payload (result_columns + identifying cols).
--    The fold uses these fields directly to UPSERT proj_benchmark_results.
-- ───────────────────────────────────────────────────────────────────
INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    'completed-' || br.id AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', datetime(br.run_at, 'unixepoch')) AS ts,
    'system' AS actor_kind,
    'benchmark-backfill-063' AS actor_id,
    'BenchmarkRunCompleted' AS type,
    'benchmark_run' AS entity_kind,
    COALESCE(br.run_id, br.id) AS entity_slug,
    NULL AS entity_project_id,
    json_object(
        'run_id',              COALESCE(br.run_id, br.id),
        'wall_clock_ms',       br.wall_clock_ms,
        'input_tokens',        br.input_tokens,
        'output_tokens',       br.output_tokens,
        'benchmark_result_id', br.id,
        'project_id',          br.project_id,
        'scenario_id',         br.scenario_id,
        'provenance_id',       br.provenance_id,
        'run_at',              br.run_at,
        'result_columns', json_object(
            'tool_name',             br.tool_name,
            'model_name',            br.model_name,
            'layer',                 br.layer,
            'task_shape',            br.task_shape,
            'task_id',               br.task_id,
            'run_shape',             br.run_shape,
            'accuracy_score',        br.accuracy_score,
            'honesty_score',         br.honesty_score,
            'ranking_quality_score', br.ranking_quality_score,
            'within_budget_score',   br.within_budget_score,
            'invocation_ok',         CASE WHEN br.invocation_ok = 1 THEN json('true') ELSE json('false') END,
            'args_match',            CASE WHEN br.args_match IS NULL THEN NULL WHEN br.args_match = 1 THEN json('true') ELSE json('false') END,
            'extracted_args',        br.extracted_args,
            'interpretation_ok',     CASE WHEN br.interpretation_ok IS NULL THEN NULL WHEN br.interpretation_ok = 1 THEN json('true') ELSE json('false') END,
            'detected_tool',         br.detected_tool,
            'notes',                 br.notes,
            'invoked_contextually',  CASE WHEN br.invoked_contextually = 1 THEN json('true') ELSE json('false') END
        )
    ) AS payload,
    NULL AS rationale,
    'started-' || br.id AS caused_by_event_id,
    '[]' AS related_entities,
    'completed-span-' || br.id AS span_id,
    1 AS schema_version
FROM proj_benchmark_results br
WHERE NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.actor_id = 'benchmark-backfill-063'
      AND e.type = 'BenchmarkRunCompleted'
      AND e.entity_kind = 'benchmark_run'
      AND e.entity_slug = COALESCE(br.run_id, br.id)
);
