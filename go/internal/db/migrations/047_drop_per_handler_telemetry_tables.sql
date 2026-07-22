-- Migration 047 — drop per-handler telemetry tables.
--
-- STAGE 2 OF chain `telemetry-substrate-cleanup` T2. ACTIVATED by chain
-- `legacy-telemetry-sink-retirement` (Chain 5) T3 on 2026-05-27.
-- ============================================================================
--
-- This file shipped as `.sql.skeleton` (out of the runner's `*.sql` glob) until
-- the soak gate cleared. Activation per the original stage-2 procedure:
--
--   1. Soak (≥ 1 week since migration 046's writer-switch): SATISFIED — 046
--      predates the current migration head (082) by 36 migrations; the
--      writer-switch has been live far longer than a week, so the recoverable
--      rollback window has long since served its purpose.
--   2. Consolidated columns populated on recent rows — verified at activation:
--      `SELECT COUNT(*) FROM grounding_events
--         WHERE action = 'vault_search' AND pass1_latency_ms IS NOT NULL
--           AND created_at >= datetime('now', '-7 days')` tracks the recent
--      vault_search call count (the consolidated shape is the live source).
--   3. Renamed `.sql.skeleton → .sql` in go/internal/db/migrations/ and
--      go/internal/testutil/migrations/. (The original note's third location,
--      crates/shared-db/migrations/, no longer exists — the Rust workspace was
--      retired; see CLAUDE.md §Migrations.)
--
-- The runner applies any migration whose slug is absent from `_migrations`
-- regardless of its number relative to head (db/migrate.go RunMigrations), so
-- this lower-numbered file applies on the existing live DB on next deploy. The
-- DROPs are order-independent (`IF EXISTS`).

DROP TABLE IF EXISTS vault_search_invocations;
DROP TABLE IF EXISTS kiwix_offload_invocations;
