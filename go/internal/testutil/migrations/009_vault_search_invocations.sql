-- Per-call telemetry for vault_search. Lets `admin.vault_search_metrics`
-- answer "how often is vault being pulled?" over a configurable window,
-- which is what the cancellation criterion in the agent-vault-read-
-- discipline chain checks against (< 30% session pull-rate after 4
-- weeks → close wontfix).
--
-- Privacy: query column is truncated to 256 chars at the dispatch
-- layer rather than stored verbatim. Top-queries surfacing in the
-- metrics action returns only the truncated form.

CREATE TABLE IF NOT EXISTS vault_search_invocations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    query           TEXT    NOT NULL,
    top_k           INTEGER NOT NULL,
    results_count   INTEGER NOT NULL,
    latency_ms      INTEGER NOT NULL,
    input_tokens    INTEGER,
    output_tokens   INTEGER,
    created_at      TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_vault_search_invocations_created_at
    ON vault_search_invocations (created_at);
