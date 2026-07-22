-- Chain 2 (telemetry-success-model-unification, T4 behavior-preserving-execution):
-- the both-layers success model. Two changes, both additive.
--
-- (1) proj_inference_call_success is the per-call Layer-2 (OUTCOME) success
-- materialization — one row per inference_invocations row, uniform with the
-- search cluster's per-row proj_retrieval_success_per_query.success. It moves
-- the outcome-success predicate evaluation (default / classify->benchmark /
-- vault->grounding) OUT of the read-time SQL interpolation that lived in
-- observehttp/inference_v2.go and INTO projection data: rebuildable, testable,
-- and consumed by the windowed /inference/health-cards + per-day
-- /inference/sparklines reads (which an all-time rollup cannot serve).
--
-- BOTH layers live on each row: call_success (= inference_invocations.success,
-- the emit-time liveness layer, Chain 1) and outcome_success (the materialized
-- predicate layer). outcome_kind names which predicate produced outcome_success.
--
-- Read-side projection: Fold == RebuildFromEmpty re-snapshots from
-- inference_invocations (+ proj_benchmark_results / grounding_events joins) on
-- every fold, so outcome reflects ground truth as of the last fold and the
-- byte-identical rebuild invariant holds vacuously. See
-- docs/CHAIN2_SUCCESS_MODEL_TARGET.md §1.
--
-- (2) proj_inference_tool_model_performance gains outcome_success_count — the
-- Layer-2 rollup over the same (task_id, model_name) group, alongside the
-- existing call-level success_count. This is the completion_condition's named
-- materialization home and the signal the Chain-3 data-driven router consumes.

CREATE TABLE proj_inference_call_success (
    id              INTEGER PRIMARY KEY,                                          -- = inference_invocations.id (per-call grain)
    task_id         TEXT    NOT NULL,
    model_name      TEXT    NOT NULL,
    created_at      TEXT    NOT NULL,
    call_success    INTEGER NOT NULL DEFAULT 0 CHECK (call_success IN (0, 1)),    -- Layer 1: inference_invocations.success
    outcome_success INTEGER NOT NULL DEFAULT 0 CHECK (outcome_success IN (0, 1)), -- Layer 2: dispatched predicate result
    outcome_kind    TEXT    NOT NULL DEFAULT 'default'                            -- which predicate fired (self-describing; cross-checked vs the Go basis registry)
                    CHECK (outcome_kind IN ('default', 'classify', 'vault-rerank-retrieve')),
    last_event_id   TEXT    NOT NULL DEFAULT '',                                  -- watermark convention (carries the row id; '' baseline)
    last_event_ts   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX proj_ics_task_created_idx ON proj_inference_call_success (task_id, created_at);

ALTER TABLE proj_inference_tool_model_performance
    ADD COLUMN outcome_success_count INTEGER NOT NULL DEFAULT 0;                  -- SUM(outcome_success) — Layer-2 rollup
