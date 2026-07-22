-- Grounding events: one row per vault_search/kiwix_search/knowledge_search call,
-- recording whether the returned sources appeared in the subsequent assistant turn.
--
-- Primary use: T6 (curation-pipeline) reads zero-result-but-answered rows as
-- gap candidates. The `used` column is an approximation — slug/path presence
-- in assistant text is imperfect. Document the approximation; do not treat
-- used=true as ground truth.

CREATE TABLE grounding_events (
    id                   INTEGER PRIMARY KEY,
    project_id           TEXT NOT NULL,
    session_id           TEXT NOT NULL,
    call_id              TEXT NOT NULL,
    action               TEXT NOT NULL,
    results_count        INTEGER NOT NULL DEFAULT 0,
    source_refs          TEXT NOT NULL DEFAULT '[]',
    next_turn_has_output INTEGER NOT NULL DEFAULT 0,
    used                 INTEGER,
    created_at           TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_grounding_project ON grounding_events (project_id);
CREATE INDEX idx_grounding_gaps ON grounding_events (results_count, next_turn_has_output);
CREATE UNIQUE INDEX idx_grounding_call ON grounding_events (session_id, call_id);
