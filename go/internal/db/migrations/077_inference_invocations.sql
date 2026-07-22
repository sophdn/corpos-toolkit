-- Chain per-tool-per-model-observability (T11, execution-relocate-write-path):
-- inference_invocations is the per-call inference telemetry table that
-- supersedes qwen_invocations. It is model-agnostic (local Qwen AND remote
-- Claude), and records call-level success + a closed error_class at the
-- router emit seam — the two signals qwen_invocations never carried.
--
-- Grain: one row per model call, identical to qwen_invocations. This honors
-- TELEMETRY_SUBSTRATE.md §9.2 / bug-1328 (one model call serves many
-- grounding events, so model telemetry must NOT be merged into
-- grounding_events). Only the mechanism (off-pattern raw sink → read-side
-- substrate source table) and the coverage (local-only → local+remote)
-- change; the grain does not.
--
-- This is the source table for the proj_inference_tool_model_performance
-- read-side projection added in T12. During the T11→T12 transition the
-- router recorder DUAL-WRITES this table and qwen_invocations so the still-
-- qwen_invocations-reading /inference endpoints stay byte-identical; T12
-- repoints the readers and removes the qwen_invocations write. The
-- qwen_invocations table DROP itself is deferred to Chain 5 (legacy-sink
-- retirement). See docs/CHAIN1_INFERENCE_TELEMETRY_DESIGN.md.
--
-- success/error_class are RECORDED here but not yet CONSUMED by the
-- success_rate the endpoints emit (that stays the read-time predicate
-- registry for parity); Chain 2 switches success_rate onto the call-level +
-- materialized-outcome model.

CREATE TABLE inference_invocations (
    id            INTEGER PRIMARY KEY,
    task_id       TEXT    NOT NULL,                       -- the "tool" / inference purpose (qwenctx.TaskID);
                                                          --   'unattributed' when the caller didn't stamp one
    model_name    TEXT    NOT NULL,                       -- local ('qwen2.5-32b') OR remote ('claude-sonnet-4-6')
    latency_ms    INTEGER NOT NULL,
    input_tokens  INTEGER,                                -- NULL when the upstream model omits usage
    output_tokens INTEGER,
    success       INTEGER NOT NULL DEFAULT 1 CHECK (success IN (0, 1)),     -- call-level: 1 = no error AND non-empty output
    error_class   TEXT    NOT NULL DEFAULT '' CHECK (error_class IN (
                      '', 'upstream_error', 'empty_response', 'not_configured', 'timeout')),
    created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_inference_invocations_task_created
    ON inference_invocations (task_id, created_at);
CREATE INDEX idx_inference_invocations_task_model
    ON inference_invocations (task_id, model_name);
