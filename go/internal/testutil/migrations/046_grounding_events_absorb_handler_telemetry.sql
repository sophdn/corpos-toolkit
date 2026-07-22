-- Migration 046 — absorb per-handler telemetry into grounding_events.
--
-- Chain `telemetry-substrate-cleanup` T2 (consolidate-per-handler-telemetry-tables).
-- Sibling drop migration: 047_drop_per_handler_telemetry_tables.sql.skeleton
-- (held .skeleton — out of the runner's glob — until a one-week post-deploy
-- soak passes; rename to .sql in a separate commit to activate stage 2).
--
-- ── Before / after shape ────────────────────────────────────────────────────
--
-- BEFORE: three handler-specific telemetry tables predate the unified
-- substrate:
--   vault_search_invocations (migrations 009 + 011) — pass1/pass2 latency split
--   kiwix_offload_invocations (migration 014) — qwen_fell_back, hits_in, hits_out
--   qwen_invocations (migration 029) — universal per-Qwen-call telemetry
--
-- AFTER: grounding_events carries the per-search handler specifics inline.
-- vault_search_invocations + kiwix_offload_invocations stop receiving writes
-- after migration 046 ships (the Go handlers switch in the same commit) and
-- drop in migration 047. qwen_invocations is NOT touched — it operates at
-- per-Qwen-call granularity (one Qwen call may serve many grounding events,
-- and many Qwen calls happen outside grounding contexts — classify, rubric
-- scoring) and feeds the /inference v2 page (this chain's T3) which cares
-- about per-call cost / latency rather than per-search lifecycle.
--
-- ── Column additions ────────────────────────────────────────────────────────
--
-- All five are NULLABLE INTEGER. Nullability reflects that only a subset of
-- grounding actions populate them:
--   pass1_latency_ms / pass2_latency_ms — vault_search only
--   qwen_fell_back / kiwix_hits_in / kiwix_hits_out — kiwix_search only
--   knowledge_search uses none of them.

ALTER TABLE grounding_events ADD COLUMN pass1_latency_ms INTEGER;
ALTER TABLE grounding_events ADD COLUMN pass2_latency_ms INTEGER;
ALTER TABLE grounding_events ADD COLUMN qwen_fell_back   INTEGER;
ALTER TABLE grounding_events ADD COLUMN kiwix_hits_in    INTEGER;
ALTER TABLE grounding_events ADD COLUMN kiwix_hits_out   INTEGER;

-- ── Backfill ───────────────────────────────────────────────────────────────
--
-- For every existing grounding_events row whose `action` matches one of the
-- handlers, copy the handler-specific columns from the nearest per-handler
-- row by created_at proximity (±5 seconds). The two writers fire from the
-- same handler exit, so timestamps are typically within milliseconds; the
-- ±5s window absorbs clock skew and tx-commit timing without false matches.
--
-- Unmappable rows (no temporal match within the window) stay NULL. That's
-- acceptable — the consolidated shape is what training pipelines depend on
-- going forward, and historical rows missing the handler-specifics still
-- carry the load-bearing substrate fields (span_id, source_refs,
-- query_source, etc.).
--
-- Implementation note: uses a CTE with ROW_NUMBER() to pick the
-- closest-by-timestamp per-handler row deterministically. SQLite's parser
-- forbids correlated references inside an inner subquery's ORDER BY, so
-- the simpler `(SELECT ... ORDER BY ... LIMIT 1)` form doesn't parse —
-- the window-function CTE is the supported pattern. Window functions
-- land in SQLite 3.25 (Sep 2018); the toolkit-server's bundled SQLite is
-- well past that. The CTE also tie-breaks on `vsi.id`/`koi.id` so
-- migration replay on the same DB is reproducible.

WITH vault_matched AS (
    SELECT gid, pass1_latency_ms, pass2_latency_ms
    FROM (
        SELECT
            g.id AS gid,
            vsi.pass1_latency_ms,
            vsi.pass2_latency_ms,
            ROW_NUMBER() OVER (
                PARTITION BY g.id
                ORDER BY
                    ABS(strftime('%s', vsi.created_at) - strftime('%s', g.created_at)) ASC,
                    vsi.id ASC
            ) AS rn
        FROM grounding_events AS g
        JOIN vault_search_invocations AS vsi
          ON ABS(strftime('%s', vsi.created_at) - strftime('%s', g.created_at)) <= 5
        WHERE g.action = 'vault_search'
    )
    WHERE rn = 1
)
UPDATE grounding_events
SET
    pass1_latency_ms = (SELECT pass1_latency_ms FROM vault_matched WHERE gid = grounding_events.id),
    pass2_latency_ms = (SELECT pass2_latency_ms FROM vault_matched WHERE gid = grounding_events.id)
WHERE id IN (SELECT gid FROM vault_matched);

WITH kiwix_matched AS (
    SELECT gid, qwen_fell_back, hits_in, hits_out
    FROM (
        SELECT
            g.id AS gid,
            koi.qwen_fell_back,
            koi.hits_in,
            koi.hits_out,
            ROW_NUMBER() OVER (
                PARTITION BY g.id
                ORDER BY
                    ABS(strftime('%s', koi.created_at) - strftime('%s', g.created_at)) ASC,
                    koi.id ASC
            ) AS rn
        FROM grounding_events AS g
        JOIN kiwix_offload_invocations AS koi
          ON ABS(strftime('%s', koi.created_at) - strftime('%s', g.created_at)) <= 5
        WHERE g.action = 'kiwix_search'
    )
    WHERE rn = 1
)
UPDATE grounding_events
SET
    qwen_fell_back = (SELECT qwen_fell_back FROM kiwix_matched WHERE gid = grounding_events.id),
    kiwix_hits_in  = (SELECT hits_in        FROM kiwix_matched WHERE gid = grounding_events.id),
    kiwix_hits_out = (SELECT hits_out       FROM kiwix_matched WHERE gid = grounding_events.id)
WHERE id IN (SELECT gid FROM kiwix_matched);
