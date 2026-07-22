-- arc-review per-arc-content dedupe cache. Closes bug
-- `arc-close-filing-review-fires-multiply-on-overlapping-arc-within-single-session`
-- (1482): the existing arc_review_debouncer (migration 048) is keyed
-- on session_id with a short time-based backoff (60s default). It
-- catches close-in-time cross-trigger fires but not the 2-5 minute
-- spaced fires the bug observed, where three triggers (CommitLanded
-- twice + TaskCompleted) each produced a separate Qwen call against
-- substantially the same underlying arc.
--
-- This table is the content-keyed second layer: a per-session+arc-hash
-- cache of recent fires so a duplicate hash within the TTL skips the
-- Qwen pipeline entirely (saves both the per-call latency AND the
-- attention cost of a redundant filing-proposal dispatch).
--
-- Hash shape: caller-computed sha256 over the snapshot's joined
-- message content. Hashing is in Go (handler-side) so the substrate
-- doesn't need to know the snapshot's structure.
--
-- TTL is enforced at read time (the SELECT filters last_fire_at by
-- a window; older rows are ignored). Periodic cleanup via the
-- last_fire_at index keeps the table bounded.
--
-- Prior_event_id points at the ArcCloseFilingReviewed row from the
-- original fire so the dedupe surface can name the prior fire when
-- it short-circuits, giving the operator a way to inspect the
-- canonical decision without re-running.

CREATE TABLE arc_review_hash_cache (
    session_id      TEXT NOT NULL,
    arc_hash        TEXT NOT NULL,
    prior_event_id  TEXT NOT NULL,
    fired_at        TEXT NOT NULL,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (session_id, arc_hash)
);

-- Time-window queries (cache lookup, cleanup sweep) and per-session
-- enumeration both benefit from a last-fire index. The PK already
-- covers session_id-only lookups.
CREATE INDEX arc_review_hash_cache_fired_at_idx
    ON arc_review_hash_cache (fired_at);
