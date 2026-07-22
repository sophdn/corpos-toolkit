-- Chain quiet-and-instrument-operator-surface T1 — the thin work-surface
-- call-telemetry spine. There is no per-tool-call telemetry today, so the
-- agent-operator friction (action_describe round-trips, param-error retries,
-- per-action latency) is unmeasurable. This table records ONE row per
-- meta-tool dispatch so the rest of the chain (and future passes) can be
-- validated against a baseline instead of asserted.
--
-- Captured at the single dispatch seam (dispatch.DispatchWithOptions via a
-- decoupled CallObserver hook), so every action across every surface (work /
-- admin / knowledge / measure / ml) is covered uniformly with no per-handler
-- opt-in. Recording action='action_describe' (admin surface) alongside the
-- work actions is what makes the describe-to-action ratio computable.
--
-- error_class semantics: '' = success. Non-empty = a short classifier:
--   unknown_action / rationale_required / wrong_nesting / bad_params  (dispatch-layer)
--   handler_error                                                     (handler returned a Go error)
--   <Violation* kind> / rejected                                      (handler returned a result envelope with a non-empty Error field)
-- Param rejections (forge missing-required / unknown-param) arrive as a
-- result envelope with err==nil, so they only surface here via the result's
-- Kind/Error reflection — exactly the fumble signal we want to measure.
--
-- THIN by design: one row per dispatch, fail-open at the writer (a
-- telemetry insert failure never aborts the underlying call).
--
-- HISTORICAL CORRECTION (chain per-tool-per-model-observability T12): an
-- earlier version of this comment framed work_tool_calls as the
-- "forward-compatible PRECURSOR to roadmap #8 (per-tool-per-model
-- observability)" that #8 would ALTER-ADD model/token/success columns onto.
-- That was WRONG. This table records per-ACTION surface dispatch telemetry,
-- a different grain from the per-(tool, model) INFERENCE telemetry roadmap #8
-- needs. #8 was instead built on the read-side substrate (inference_invocations
-- + proj_inference_tool_model_performance), this table acquired zero readers,
-- and it is DROPPED in migration 079. Treat work_tool_calls as retired.

CREATE TABLE work_tool_calls (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          TEXT    NOT NULL DEFAULT (datetime('now')),
    surface     TEXT    NOT NULL,                    -- work / admin / knowledge / measure / ml
    action      TEXT    NOT NULL,
    project_id  TEXT    NOT NULL DEFAULT '',
    latency_ms  INTEGER NOT NULL DEFAULT 0,
    error_class TEXT    NOT NULL DEFAULT '',          -- '' = success
    session_id  TEXT    NOT NULL DEFAULT '',          -- MCP session (from ctx) when available
    span_id     TEXT    NOT NULL DEFAULT ''           -- request span (from ctx) for cross-join to obs spans
);

CREATE INDEX work_tool_calls_action_idx  ON work_tool_calls (action);
CREATE INDEX work_tool_calls_ts_idx      ON work_tool_calls (ts DESC);
CREATE INDEX work_tool_calls_surface_idx ON work_tool_calls (surface, action);
-- Partial index over failures only — error-rate queries skip the success bulk.
CREATE INDEX work_tool_calls_error_idx   ON work_tool_calls (error_class) WHERE error_class <> '';
