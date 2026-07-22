-- Add span_id to grounding_events so the read-side knowledge handlers'
-- rows can be joined to the events ledger (and to the structured-log
-- stream) by the same per-MCP-request span_id the dispatcher mints.
--
-- Background: 019_grounding_events.sql declared session_id+call_id as
-- the natural key for an offline-processed event (a benchmark tool
-- scans MCP session logs and reconstructs rows post-hoc). T5 of chain
-- agent-first-substrate wires online emission directly from the Go
-- knowledge handlers — vault_search, kiwix_search, knowledge_search.
-- The new column is nullable so pre-T5 offline-emitted rows continue
-- to load; the online path always populates it.
--
-- The cross-substrate join the sibling chain query-telemetry-substrate
-- relies on uses this column: read-side grounding_events.span_id ==
-- write-side events.span_id whenever both fired under the same MCP
-- request context.

ALTER TABLE grounding_events ADD COLUMN span_id TEXT;

CREATE INDEX idx_grounding_span ON grounding_events (span_id);
