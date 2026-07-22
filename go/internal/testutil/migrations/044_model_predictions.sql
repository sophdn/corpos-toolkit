-- ml-capability-substrate T5: model_predictions telemetry side-table.
--
-- One row per `ml.inference` call (T5) or per ml convenience-action
-- call (T5+, e.g. route_query / curation_score / forge_suggest_surfaces).
-- See docs/ML_CAPABILITY_SUBSTRATE.md §7 for the design.
--
-- Sits alongside grounding_events / query_interactions (chain
-- query-telemetry-substrate, closed 2026-05-17), joined via span_id
-- and the optional grounding_event_id FK. Future `proj_model_eval`
-- projections will join this table to query_interactions (did the
-- rerank top result get clicked?) and to events (was the curation
-- candidate ultimately promoted?). That projection feeds the A/B
-- promotion-gate body (T6).
--
-- features_hash content-addresses the input. Use cases:
--   - drift detection: distinct (model_id, features_hash) over time
--     charts input-distribution shift
--   - caching opportunities: identical hash → identical output
--   - debugging: re-run the same inference offline
--
-- output_summary is a bounded-size JSON representation; not the raw
-- bytes (those are derivable from feat_hash + model_id).
--
-- span_id is required and carried through from the agent-first-substrate
-- envelope (closed 2026-05-17, docs/EVENT_SUBSTRATE.md). The MCP
-- dispatch layer stamps span_id into ctx; the inference handler reads
-- it via events.SpanIDFromContext.
--
-- grounding_event_id is nullable — populated when the prediction was
-- triggered by a search (cross-encoder reranker, source router).
-- Classifier predictions (curation classifier, bug surface tagger)
-- leave it NULL.

CREATE TABLE model_predictions (
    id                  INTEGER PRIMARY KEY,
    model_id            INTEGER NOT NULL REFERENCES trained_models(id),
    features_hash       TEXT NOT NULL,
    output_summary      TEXT NOT NULL,
    latency_ms          REAL NOT NULL,
    span_id             TEXT NOT NULL,
    grounding_event_id  INTEGER,
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_model_predictions_model_time    ON model_predictions (model_id, created_at);
CREATE INDEX idx_model_predictions_span          ON model_predictions (span_id);
CREATE INDEX idx_model_predictions_grounding     ON model_predictions (grounding_event_id);
