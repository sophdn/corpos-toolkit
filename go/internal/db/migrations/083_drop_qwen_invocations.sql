-- Chain 5 (legacy-telemetry-sink-retirement, T3): drop qwen_invocations.
--
-- qwen_invocations (migration 029, bug 1328) was the universal per-Qwen-call
-- telemetry sink. Chain 1 (per-tool-per-model-observability) superseded it with
-- inference_invocations (model-agnostic, +success/error_class, +remote coverage)
-- and the proj_inference_tool_model_performance read-side projection; T12
-- repointed every /inference reader onto the projection and commit f97cd465
-- removed the transitional dual-write. The table has been empty-and-unread since
-- — no production reader, and no projection folds from it (pinned by the
-- characterization net in chain T1). This DROP is the deferral named in
-- migration 077's header finally coming due.
--
-- The dead writer (db/qwen_telemetry.go: QwenInvocation / RecordQwenInvocation)
-- and its round-trip test are deleted in the same commit.

DROP TABLE IF EXISTS qwen_invocations;
