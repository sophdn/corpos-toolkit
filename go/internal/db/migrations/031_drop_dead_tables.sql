-- janitorial-pass-2026-05 T6364: drop tables / views whose last live
-- consumer was retired in this chain.
--
-- portal_chats + portal_chat_messages were the storage for the
-- Layer-A portal chat surface. The only writers were shared-db's
-- portal_chats.rs module (deleted in T6363) and the Rust toolkit-
-- server's portal_chat handler (archived at T69). Go does not
-- carry a portal-chat surface.
--
-- roadmap_items_v_with_status was a read-only convenience view
-- introduced at migration 028 to surface chain/task status alongside
-- a roadmap row. The dashboard and the Go observe-http handlers
-- query roadmap_items directly (status is JOINed in application
-- code, not in the view). The view's only live consumer was the
-- existence assertion in go/internal/testutil/testutil_test.go,
-- which is updated in the same commit.
--
-- Note: the task spec also listed `vault_search_pass_latencies` as
-- a fourth drop target, but no such table exists — migration 011
-- adds two columns (pass1_latency_ms, pass2_latency_ms) to the live
-- vault_search_invocations table. Those columns are written on
-- every vault_search by go/internal/db/vault_telemetry.go, so
-- nothing is dead there. The mistake is captured in the chain's
-- bug log; this migration drops the three real surfaces.

-- SQLite drops a table's indexes automatically; no explicit DROP INDEX needed.
DROP VIEW  IF EXISTS roadmap_items_v_with_status;
DROP TABLE IF EXISTS portal_chat_messages;
DROP TABLE IF EXISTS portal_chats;
