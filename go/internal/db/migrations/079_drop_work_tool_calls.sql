-- Chain per-tool-per-model-observability (T12): drop the dead work_tool_calls
-- table. It was added by migration 075 (chain quiet-and-instrument-operator-
-- surface) as a per-ACTION dispatch-telemetry sink, but it acquired ZERO
-- readers (no Go or dashboard query ever read it) and its 075 comment
-- FALSELY framed it as the forward-compatible precursor to this chain's
-- per-TOOL-per-MODEL observability. It is not: per-action surface telemetry
-- is a different grain from per-(tool, model) inference telemetry, which this
-- chain delivers via inference_invocations + proj_inference_tool_model_
-- performance. The 075 comment has been corrected; the writer (a
-- dispatch.CallObserver closure in main.go) is removed; this drops the table.

DROP INDEX IF EXISTS work_tool_calls_action_idx;
DROP INDEX IF EXISTS work_tool_calls_ts_idx;
DROP INDEX IF EXISTS work_tool_calls_surface_idx;
DROP INDEX IF EXISTS work_tool_calls_error_idx;
DROP TABLE IF EXISTS work_tool_calls;
