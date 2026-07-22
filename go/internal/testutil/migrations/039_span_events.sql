-- Span events ledger. Persists every span_open / span_close so the
-- dashboard's /events/spans SSE stream can fan out across processes.
--
-- Why a table: SpanBus (go/internal/obs/spanbus.go) is in-memory and
-- per-process. Live claude sessions speak MCP to stdio toolkit-server
-- processes; the dashboard's HTTP daemon is a separate process. The
-- in-process bus never sees writes from sibling processes, so the live
-- spans page rendered empty under exactly the conditions where spans
-- were being emitted at high volume. See bug
-- live-spans-empty-spanbus-per-process-cross-process-gap.
--
-- The new path: every process installs a DBSpanSink that INSERTs here
-- on Publish; the HTTP daemon's /events/spans handler tails this table
-- (id > cursor poll, ~250 ms tick) and pushes rows out as SSE frames.
-- Wire shape on the SSE stream stays byte-for-byte identical to the
-- prior bus path.
--
-- These rows are transient telemetry, not source-of-truth business
-- history — no append-only trigger. A retention janitor in the HTTP
-- daemon trims rows older than 24 h.

CREATE TABLE span_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    type           TEXT NOT NULL CHECK (type IN ('span_open', 'span_close')),
    span_id        TEXT NOT NULL,
    parent_span_id TEXT,
    trace_id       TEXT NOT NULL,
    name           TEXT NOT NULL,
    started_at     TEXT NOT NULL,
    duration_ms    INTEGER,
    status         TEXT,
    error          TEXT,
    inserted_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- id is already the tail cursor (PRIMARY KEY AUTOINCREMENT). Extra
-- indexes for trace lookup (per-trace replays) and retention prune.
CREATE INDEX span_events_trace_idx    ON span_events (trace_id);
CREATE INDEX span_events_inserted_idx ON span_events (inserted_at);
