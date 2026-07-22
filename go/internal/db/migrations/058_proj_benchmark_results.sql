-- agent-substrate-crud-retirement chain T2 — proj_benchmark_results
-- projection table + Option-A synthetic-event backfill for post-035
-- (provenance-tracked) benchmark_results rows.
--
-- See docs/SUBSTRATE_CRUD_RETIREMENT.md §4.5 for the column matrix.
--
-- BACKFILL SCOPE: only rows with provenance_id IS NOT NULL get a
-- synthetic BenchmarkRunCompleted event. Pre-035 rows are accepted as
-- a "frozen state" exception per docs/BENCHMARKS_DB_RETIREMENT_2026-05-19.md
-- and §6 of the design doc — those rows snapshot into the projection
-- but receive no synthetic event. They retain the per-row sentinel
-- last_event_id = '' so the byte-identical rebuild test treats them as
-- canonical-from-CRUD-only.

CREATE TABLE proj_benchmark_results (
    id                      TEXT    NOT NULL,
    project_id              TEXT    NOT NULL,
    scenario_id             TEXT    NOT NULL,
    tool_name               TEXT    NOT NULL,
    model_name              TEXT    NOT NULL,
    run_id                  TEXT,
    run_at                  INTEGER NOT NULL,
    wall_clock_ms           INTEGER NOT NULL,
    input_tokens            INTEGER,
    output_tokens           INTEGER,
    invoked_contextually    INTEGER NOT NULL DEFAULT 1,
    invocation_ok           INTEGER NOT NULL,
    args_match              INTEGER,
    extracted_args          TEXT,
    interpretation_ok       INTEGER,
    detected_tool           TEXT,
    notes                   TEXT,
    layer                   TEXT,
    task_shape              TEXT,
    accuracy_score          REAL,
    honesty_score           REAL,
    ranking_quality_score   REAL,
    within_budget_score     REAL,
    task_id                 TEXT,
    run_shape               TEXT,
    provenance_id           TEXT,
    last_event_id           TEXT    NOT NULL DEFAULT '',
    last_event_ts           TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (id)
);

CREATE INDEX proj_benchmark_results_project_idx    ON proj_benchmark_results (project_id);
CREATE INDEX proj_benchmark_results_tool_idx       ON proj_benchmark_results (tool_name);
CREATE INDEX proj_benchmark_results_model_idx      ON proj_benchmark_results (model_name);
CREATE INDEX proj_benchmark_results_run_id_idx     ON proj_benchmark_results (run_id);
CREATE INDEX proj_benchmark_results_layer_idx      ON proj_benchmark_results (layer);
CREATE INDEX proj_benchmark_results_task_id_idx    ON proj_benchmark_results (task_id);
CREATE INDEX proj_benchmark_results_run_at_idx     ON proj_benchmark_results (run_at DESC);
CREATE INDEX proj_benchmark_results_provenance_idx ON proj_benchmark_results (provenance_id);

-- Snapshot-seed from benchmark_results CRUD (every row, pre-035 included).
INSERT INTO proj_benchmark_results (
    id, project_id, scenario_id, tool_name, model_name, run_id, run_at,
    wall_clock_ms, input_tokens, output_tokens, invoked_contextually,
    invocation_ok, args_match, extracted_args, interpretation_ok,
    detected_tool, notes, layer, task_shape, accuracy_score, honesty_score,
    ranking_quality_score, within_budget_score, task_id, run_shape,
    provenance_id, last_event_id, last_event_ts
)
SELECT id, project_id, scenario_id, tool_name, model_name, run_id, run_at,
       wall_clock_ms, input_tokens, output_tokens, invoked_contextually,
       invocation_ok, args_match, extracted_args, interpretation_ok,
       detected_tool, notes, layer, task_shape, accuracy_score, honesty_score,
       ranking_quality_score, within_budget_score, task_id, run_shape,
       provenance_id, '', ''
FROM benchmark_results;

INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'benchmark_results', max_id, max_ts FROM (
    SELECT (SELECT event_id FROM events ORDER BY event_id DESC LIMIT 1) AS max_id,
           (SELECT ts       FROM events ORDER BY event_id DESC LIMIT 1) AS max_ts
);

-- ────────────────────────────────────────────────────────────────────
-- Option-A synthetic-event backfill — only post-035 rows.
-- ────────────────────────────────────────────────────────────────────
--
-- One BenchmarkRunCompleted per provenance-tracked row. The payload
-- carries the BenchmarkRunCompleted blueprint's required fields
-- (run_id, wall_clock_ms) plus optional input_tokens / output_tokens.
-- Rubric-side columns (accuracy_score etc.) land in the projection via
-- the snapshot above; the synthetic event captures only the blueprint-
-- shaped subset because the result_columns / rubric_outcome extension
-- is T3's bump (see §5.5 of the design doc — payload-only fold needs
-- it). For T2 this is sufficient: the fold reads CRUD until T3+T5 ship
-- the payload extension and the post-T5 payload-only fold.
--
-- ts is set to the row's run_at (unix-seconds epoch) converted to
-- RFC3339; the pre-substrate boundary uses the same MIN(events.ts)
-- gate. NULL run_id rows are skipped — the blueprint's `run_id`
-- minLength=1 rule rejects them, and they predate the run_id
-- requirement anyway. NULL provenance_id rows are skipped per the
-- §6 frozen-state exception.

INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (br.run_at * 1000) / 65536,
        (br.run_at * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', br.run_at, 'unixepoch') AS ts,
    'system' AS actor_kind,
    'pre-substrate-backfill' AS actor_id,
    'BenchmarkRunCompleted' AS type,
    'benchmark_run' AS entity_kind,
    br.id AS entity_slug,
    br.project_id AS entity_project_id,
    json_object(
        'run_id', br.run_id,
        'wall_clock_ms', br.wall_clock_ms,
        'input_tokens', br.input_tokens,
        'output_tokens', br.output_tokens
    ) AS payload,
    NULL AS rationale,
    NULL AS caused_by_event_id,
    '[]' AS related_entities,
    lower(printf(
        '%s-%s-4%s-%s%s-%s',
        lower(hex(randomblob(4))),
        lower(hex(randomblob(2))),
        substr(lower(hex(randomblob(2))), 2),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        substr(lower(hex(randomblob(2))), 2),
        lower(hex(randomblob(6)))
    )) AS span_id,
    1 AS schema_version
FROM benchmark_results br
WHERE br.provenance_id IS NOT NULL
  AND br.run_id IS NOT NULL
  AND strftime('%Y-%m-%dT%H:%M:%fZ', br.run_at, 'unixepoch') < COALESCE(
        (SELECT MIN(ts) FROM events),
        '9999-12-31'
      );
