-- Chain substrate-health-audit-projections T3: lock the projection-
-- population guarantees for proj_training_data_for_reranker as DB-level
-- invariants, so the correctness fixed at the writer (bugs
-- reranker-projection-drops-query-text-on-positive-labels +
-- reranker-projection-last-event-ts-never-populated, commits 4199aaf8 +
-- 5c8fd43b) cannot silently regress.
--
-- query_text NOT NULL CHECK (<> ''): every row in this projection is a
-- (query, candidate, label) training triple — a cross-encoder reranker
-- scores (query, candidate), so a row with no query is untrainable. The
-- writer (query_training.go) now filters NULL/empty-query rows out, so the
-- projection contains only query-bearing rows; this invariant enforces
-- that at the DB. It drops the 458 legacy NULL-query rows (53 positive +
-- 15 weakly_positive + 244 negative + 146 hard_negative, all from
-- un-backfilled processor-created grounding_events). Per the no-backfill
-- substrate-health principle they stay known-bad and excluded from the
-- corpus; new post-fix traffic is the trustworthy data.
--
-- last_event_ts NOT NULL CHECK (<> ''): populated from the source
-- grounding_event's real created_at (migration 068 dropped the masking
-- NOT NULL DEFAULT ''). Enforce non-empty so a future writer regression
-- surfaces as a REJECTED insert, not a silently-blank time column — which
-- would break chain 272's most-recent-~15% time-based held-out split. The
-- source grounding_events.created_at is itself NOT NULL DEFAULT
-- (datetime('now')), so the writer can always satisfy this.
--
-- Safe to DROP + CREATE without copying rows: proj_training_data_for_reranker
-- is a full-rebuild projection (rebuildProjection TRUNCATEs + re-INSERTs on
-- every event via FoldAll), fully derived from grounding_events +
-- query_interactions, and its sole consumer (chain 272, the cross-encoder
-- reranker training pipeline) has not started — no read in the repopulation
-- window. The next event repopulates it through the now-filtered writer, so
-- every rebuilt row satisfies the new invariants. Indexes recreated to
-- match migration 068 / 038.

DROP TABLE proj_training_data_for_reranker;

CREATE TABLE proj_training_data_for_reranker (
    training_id          INTEGER PRIMARY KEY AUTOINCREMENT,
    grounding_event_id   INTEGER NOT NULL REFERENCES grounding_events(id),
    query_text           TEXT NOT NULL CHECK (query_text <> ''),
    candidate_pointer_id INTEGER,
    source_ref           TEXT NOT NULL,
    candidate_position   INTEGER NOT NULL,
    label_kind           TEXT NOT NULL CHECK (label_kind IN
                            ('positive', 'weakly_positive', 'negative', 'hard_negative', 'unlabeled')),
    weight               REAL NOT NULL DEFAULT 0.0,
    label_sources        TEXT NOT NULL DEFAULT '[]',
    query_source         TEXT NOT NULL DEFAULT 'agent_initiated',
    was_injected         INTEGER NOT NULL DEFAULT 0,
    prompt_id            TEXT,
    span_id              TEXT,
    last_event_id        TEXT,
    last_event_ts        TEXT NOT NULL CHECK (last_event_ts <> ''),
    UNIQUE (grounding_event_id, source_ref)
);

CREATE INDEX proj_tdfr_label_kind_idx    ON proj_training_data_for_reranker (label_kind);
CREATE INDEX proj_tdfr_query_source_idx  ON proj_training_data_for_reranker (query_source);
CREATE INDEX proj_tdfr_pointer_idx       ON proj_training_data_for_reranker (candidate_pointer_id);
CREATE INDEX proj_tdfr_prompt_idx        ON proj_training_data_for_reranker (prompt_id);
