-- Per-call telemetry for the kiwix_search Qwen-rerank offload (chain
-- `deploy-qwen-kiwix-rerank` T2). One row per kiwix_search invocation
-- — captures whether Qwen successfully reranked or the dispatch arm
-- fell back to the original kiwix hit list, plus latency and token
-- usage so the rerank's cost-savings hypothesis stays auditable.
--
-- Privacy: query column is truncated to 256 bytes at the dispatch
-- layer (matching vault_search_invocations) rather than stored
-- verbatim. No HTML snippet bodies persist here — only the query.
--
-- Append-only. Only writer: the `kiwix_search` arm in
-- toolkit-server::dispatch. Reader is whatever future admin action
-- surfaces fallback rate / p95 latency for the kiwix offload (out of
-- scope for T2; see the deploy plan).

CREATE TABLE IF NOT EXISTS kiwix_offload_invocations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    query           TEXT    NOT NULL,
    top_k           INTEGER NOT NULL,
    hits_in         INTEGER NOT NULL,            -- kiwix raw hit count before rerank
    hits_out        INTEGER NOT NULL,            -- count returned to caller (after rerank or fallback)
    latency_ms      INTEGER NOT NULL,            -- total wall-clock incl. kiwix HTTP + Qwen rerank + DB write
    input_tokens    INTEGER,                     -- Qwen prompt tokens (NULL if Qwen never called)
    output_tokens   INTEGER,                     -- Qwen completion tokens (NULL on fallback)
    qwen_fell_back  INTEGER NOT NULL,            -- 0 = rerank succeeded, 1 = fell back to original hit list
    created_at      TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_kiwix_offload_invocations_created_at
    ON kiwix_offload_invocations (created_at);
