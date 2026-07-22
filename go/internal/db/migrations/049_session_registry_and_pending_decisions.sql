-- arc-close-filing-review-substrate-listener-wiring T2: two tables that
-- close the seam between harness-derived session state and substrate-
-- derived event triggers.
--
-- (1) session_registry
--     Bridges the Stop hook (harness-side, knows session_id +
--     transcript_path from the Stop event JSON) to the substrate event
--     listener (substrate-side, knows project_id + event payload but
--     no transcript). The hook UPSERTs on every fire; the listener
--     SELECTs (project_id = ?, ORDER BY last_active_at DESC LIMIT 1)
--     when a SubstrateTriggerEvent lands to resolve "which transcript
--     do I review?" Eviction sweep removes rows older than 7 days
--     (mirrors hooks/arc-close-detector.sh::cleanup_old_counters
--     retention). See docs/ARC_CLOSE_FILING_REVIEW_SUBSTRATE_LISTENER.md
--     §Q1.
--
-- (2) pending_decisions
--     Dispatch queue between the substrate-side listener fire (no live
--     agent at fire time) and the next Stop event (live agent reads +
--     dispatches via system-reminder). The substrate goroutine INSERTs
--     when ReviewArcForFiling returns status="fired"; the Stop hook
--     calls work.pending_decisions_claim(project, limit) which atomically
--     SELECTs undispatched rows and UPDATEs dispatched_at + dispatch_session_id
--     in one tx (BEGIN IMMEDIATE serializes concurrent claims under
--     SQLite's single-writer lock). Dispatched rows linger for 30 days
--     to support the T8 audit join; undispatched rows older than 7 days
--     get evicted as garbage. The partial index over (dispatched_at IS
--     NULL) keeps the hot claim query small as dispatched rows
--     accumulate. See docs/ARC_CLOSE_FILING_REVIEW_SUBSTRATE_LISTENER.md
--     §Q2.
--
-- event_id is NOT declared as a FOREIGN KEY to events.event_id even
-- though every row's event_id refers to a row there. Two reasons:
-- (a) the events table is append-only by design (per EVENT_SUBSTRATE.md
-- §3.4) so deletes never fire the FK check; (b) explicit absence beats
-- implicit safety for cross-substrate seams in this codebase.

CREATE TABLE session_registry (
    session_id      TEXT PRIMARY KEY,
    project_id      TEXT,
    transcript_path TEXT NOT NULL,
    last_active_at  TEXT NOT NULL,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX session_registry_project_active_idx
    ON session_registry (project_id, last_active_at DESC);

CREATE TABLE pending_decisions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id            TEXT NOT NULL,
    project_id          TEXT,
    target_session_id   TEXT NOT NULL,
    decisions_json      TEXT NOT NULL,
    triggers_json       TEXT NOT NULL,
    arc_summary         TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    dispatched_at       TEXT,
    dispatch_session_id TEXT,
    dispatch_error      TEXT
);

CREATE INDEX pending_decisions_undispatched_idx
    ON pending_decisions (project_id, created_at)
    WHERE dispatched_at IS NULL;

CREATE INDEX pending_decisions_created_at_idx
    ON pending_decisions (created_at);
