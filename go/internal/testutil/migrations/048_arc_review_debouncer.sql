-- arc-close-filing-review T4 Stage 2: per-session debouncer state for
-- the work.review_arc_for_filing MCP action.
--
-- One row per session_id. Both firing paths (Stop hook on the harness
-- side; substrate event listener on the toolkit-server side) call the
-- same action; the action's first step is the debouncer check below.
-- Both paths share this single source of truth.
--
-- Suppression model: every incoming trigger checks last_fire_at. If
-- the difference (now - last_fire_at) < backoff window (default 60s,
-- per docs/ARC_CLOSE_FILING_REVIEW.md §Thresholds), the trigger is
-- skipped. After a successful fire the row is upserted with the new
-- last_fire_at. The 30s coalesce property is naturally satisfied by
-- the 60s backoff: multiple triggers arriving within 30s of the first
-- one all fall inside the 60s window and are coalesced into the
-- original fire.
--
-- last_fire_attempt_at distinguishes "skipped here too" from "never
-- saw this session" — useful for telemetry (how often is the
-- debouncer suppressing fires?) without bloating the row.

CREATE TABLE arc_review_debouncer (
    session_id            TEXT PRIMARY KEY,
    last_fire_at          TEXT NOT NULL,
    last_fire_attempt_at  TEXT NOT NULL,
    updated_at            TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Index on last_fire_at supports periodic cleanup sweeps (rows older
-- than the retention window can be purged; sessions rotate as the
-- harness creates fresh transcript paths).
CREATE INDEX arc_review_debouncer_last_fire_at_idx
    ON arc_review_debouncer (last_fire_at);
