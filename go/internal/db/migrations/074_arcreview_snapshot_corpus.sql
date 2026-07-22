-- Chain arc-close-snapshot-corpus-capture T1 — the snapshot training-corpus
-- surface. Forged from the snapshot-corpus-recovery spike (2026-05-24):
-- the arc-close audit (ArcCloseFilingReviewed event) persists snapshot
-- METADATA only (count / tokens / truncated), never the snapshot CONTENT —
-- the primary input feature the two arc-close ML chains (trained-arc-close-
-- filing-classifier-v1 #275, trained-smart-snapshot-filter-v1 #276) need. So
-- no faithful training corpus is accumulating. This table captures the exact
-- snapshot content fed to Qwen, keyed by the ArcCloseFilingReviewed event_id
-- so it joins to that event's decisions / arc_summary / triggers.
--
-- ## Why a dedicated table, NOT a proj_* projection (T1 decision)
--
-- The snapshot content is PRIMARY capture data (the raw model input), not a
-- derived/foldable view of other ledger state — so it is a capture log, not a
-- projection, and is intentionally NOT touched by rebuild-projections. Two
-- forces ruled out the event-sourced alternatives:
--   - Carrying ~3 KB of message text on every ArcCloseFilingReviewed payload
--     bloats the immutable events ledger for a field only the ML corpus reads.
--   - The "rebuildable from the ledger" property can't hold anyway: the ~261
--     HISTORICAL fires' events do not contain the content, so the recovered
--     half (T4) must be written directly regardless. A projection that is only
--     partly event-derived is a worse model than an honest primary table.
-- The capture is still durable + atomic: the writer (T2) INSERTs here inside
-- the SAME pool.WithWrite tx as the events.Emit of the ArcCloseFilingReviewed
-- event (handler.go::emitFilingReviewedEvent) — no fire lands without its
-- snapshot, no orphan snapshot lands.
--
-- ## Row-integrity invariants (the DB's job)
--
-- messages_json non-empty + non-'[]', message_count > 0, fire_ts non-empty,
-- source ∈ {live,recovered}, event_id → events(event_id). The cross-table
-- guarantee ("every ArcCloseFilingReviewed with a non-empty snapshot has a
-- corpus row") is a WRITER guarantee (atomic same-tx insert) locked by a
-- write-side regression test in T2, not a single-table CHECK.
--
-- ## source = live | recovered
--
-- live      = captured at fire time by the T2 writer (exact).
-- recovered = reconstructed by the T4 cmd from the real session transcript at
--             the fire's point-in-time (as-of fire_ts, original caps). Real
--             re-derivation, never synthetic — the flag keeps the fidelity
--             distinction explicit for the ML exporters.
--
-- Consumers MUST split train/holdout by session_id, not by event_id: the 265
-- live fires came from only 33 sessions, so a fire-level split leaks (same-
-- session fires share a transcript). Documented in docs/ARC_CLOSE_SNAPSHOT_CORPUS.md.

CREATE TABLE arcreview_snapshot_corpus (
    event_id         TEXT    PRIMARY KEY REFERENCES events(event_id),
    session_id       TEXT    NOT NULL,
    fire_ts          TEXT    NOT NULL CHECK (fire_ts <> ''),
    -- The kept messages fed to Qwen, as a JSON array of {role, content}
    -- (arcreview.Message shape), in the order presented. The load-bearing
    -- training feature; non-empty by construction (the review skips empty
    -- snapshots before emit).
    messages_json    TEXT    NOT NULL CHECK (messages_json <> '' AND messages_json <> '[]'),
    message_count    INTEGER NOT NULL CHECK (message_count > 0),
    estimated_tokens INTEGER NOT NULL DEFAULT 0,
    truncated        INTEGER NOT NULL DEFAULT 0,
    -- Caps in force at capture time (the last-N-turns / token budget). Stored
    -- so a recovered row can reproduce the exact point-in-time cut and so a
    -- future caps change is auditable per row.
    max_turns        INTEGER NOT NULL,
    max_tokens       INTEGER NOT NULL,
    source           TEXT    NOT NULL CHECK (source IN ('live', 'recovered')),
    schema_version   INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX arcreview_snapshot_corpus_session_idx ON arcreview_snapshot_corpus (session_id);
CREATE INDEX arcreview_snapshot_corpus_source_idx  ON arcreview_snapshot_corpus (source);
CREATE INDEX arcreview_snapshot_corpus_fire_ts_idx ON arcreview_snapshot_corpus (fire_ts DESC);
