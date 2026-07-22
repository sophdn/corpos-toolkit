-- chain 311 (emit-surface-forge-v2) T7 Stage 6 P2-A — bench event-sourcing
-- backfill. As of 2026-05-29 the bench payload was bumped to carry
-- flag_set_json + gate_metrics, and a bench_harnesses fold (projections/
-- bench_harnesses.go) now materializes the table from BenchmarkForged events
-- so forge's direct INSERT can be retired and forge archived.
--
-- The TWO historical BenchmarkForged events predate the payload bump (they
-- lack flag_set_json), so the fold skips them and a from-empty rebuild would
-- NOT reproduce the live bench_harnesses rows they describe. Those rows are
-- LIVE CONFIG (measure.bench_run depends on them), not forensic history — so,
-- exactly as migration 058 did for proj_benchmark_results' post-035 rows, this
-- migration emits ONE synthetic, full-payload BenchmarkForged per existing
-- bench_harnesses row (Option-A synthetic-event backfill). The synthetic event
-- carries the row's current canonical columns, so RebuildFromEmpty folds it and
-- reconstructs the row identically; the grandfathered originals self-skip.
--
-- Hermetic test DBs have zero bench_harnesses rows, so this backfill is a no-op
-- there (the SELECT yields nothing). On production it emits one event for the
-- single `parse-context` harness.
--
-- event_id is a synthesized UUIDv7-shaped id derived from the row's created_at
-- (same generator shape as migration 058); ts is the row's created_at so the
-- synthetic event sits chronologically with the original registration. The
-- NOT EXISTS guard makes the backfill safe to re-run (one synthetic event per
-- (project, slug), keyed on the actor_id sentinel).

INSERT INTO events (
    event_id, ts, actor_kind, actor_id, type,
    entity_kind, entity_slug, entity_project_id,
    payload, rationale, caused_by_event_id, related_entities,
    span_id, schema_version
)
SELECT
    lower(printf(
        '%08x-%04x-7%03x-%s%03x-%s',
        (strftime('%s', bh.created_at) * 1000) / 65536,
        (strftime('%s', bh.created_at) * 1000) % 65536,
        (abs(random()) % 4096),
        substr('89ab', 1 + (abs(random()) % 4), 1),
        abs(random()) % 4096,
        lower(hex(randomblob(6)))
    )) AS event_id,
    bh.created_at AS ts,
    'system' AS actor_kind,
    'p2a-bench-backfill' AS actor_id,
    'BenchmarkForged' AS type,
    'bench' AS entity_kind,
    bh.slug AS entity_slug,
    bh.project_id AS entity_project_id,
    json_object(
        'slug', bh.slug,
        'binary_path', bh.binary_path,
        'baseline_json_path', bh.baseline_json_path,
        'parse_output_as', bh.parse_output_as,
        'timeout_ms', bh.timeout_ms,
        'flag_set_json', bh.flag_set_json,
        'gate_metrics', bh.gate_metrics,
        'idempotent', json('false')
    ) AS payload,
    'P2-A synthetic backfill: makes the pre-bump bench_harnesses row reconstructable from events (forge archival prerequisite)' AS rationale,
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
FROM bench_harnesses bh
WHERE NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.type = 'BenchmarkForged'
      AND e.actor_id = 'p2a-bench-backfill'
      AND e.entity_slug = bh.slug
      AND e.entity_project_id = bh.project_id
);

-- Seed the bench_harnesses projection watermark to the current max event so
-- the live (already direct-written) row is treated as caught-up and the
-- incremental fold path doesn't re-walk history. A from-empty rebuild ignores
-- this watermark (it truncates + replays), folding the synthetic event above.
-- Mirrors migration 058's benchmark_results watermark seed.
INSERT OR REPLACE INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
SELECT 'bench_harnesses',
       (SELECT event_id FROM events ORDER BY ts DESC, event_id DESC LIMIT 1),
       (SELECT ts       FROM events ORDER BY ts DESC, event_id DESC LIMIT 1);
