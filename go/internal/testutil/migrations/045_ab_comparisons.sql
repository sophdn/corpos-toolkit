-- ml-capability-substrate T6: A/B hot-swap harness — per-call comparison
-- rows between a baseline implementation and a trained_model.
--
-- See docs/ML_CAPABILITY_SUBSTRATE.md §8 for the design.
--
-- Pattern: when a trained_model row is in status='ab_testing', any
-- code path that goes through go/internal/ml/abtest.Dispatch fires
-- BOTH the baseline implementation and the trained model on every
-- call, records the outputs as JSON blobs in this table, and returns
-- the output selected by the configured Policy (PreferBaseline /
-- PreferTrained / Alternate).
--
-- used_path records which output the CALLER actually consumed —
-- distinct from "trained model also fired" — so downstream
-- query_interactions joins can answer "did the user click the
-- trained-path's pick, or the baseline-path's pick?"
--
-- features_hash is the same SHA-256 content key the model_predictions
-- table uses (T5 / migration 044) so a Dispatch call's comparison row
-- joins cleanly to BOTH the baseline-output's notional row AND the
-- trained-output's prediction row via (model_id, features_hash).
--
-- The proj_ab_promotion_gate evaluator (T6) reads from this table +
-- query_interactions to score baseline-vs-trained on actual outcomes.

CREATE TABLE ab_comparisons (
    id                  INTEGER PRIMARY KEY,
    model_id            INTEGER NOT NULL REFERENCES trained_models(id),
    features_hash       TEXT NOT NULL,
    baseline_output     TEXT NOT NULL,
    trained_output      TEXT NOT NULL,
    used_path           TEXT NOT NULL CHECK (used_path IN ('baseline','trained')),
    policy              TEXT NOT NULL CHECK (policy IN ('prefer_baseline','prefer_trained','alternate')),
    baseline_latency_ms REAL NOT NULL DEFAULT 0,
    trained_latency_ms  REAL NOT NULL DEFAULT 0,
    span_id             TEXT NOT NULL,
    grounding_event_id  INTEGER,
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_ab_comparisons_model_time ON ab_comparisons (model_id, created_at);
CREATE INDEX idx_ab_comparisons_span       ON ab_comparisons (span_id);
CREATE INDEX idx_ab_comparisons_grounding  ON ab_comparisons (grounding_event_id);
