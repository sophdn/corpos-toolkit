// Package qwenctx carries a per-call task_id stamp through context for
// the inference router's universal telemetry table.
//
// ## Intended use
//
// **Workflow served:** the inference router's universal telemetry
// table (`inference_invocations`) needs to know which handler issued each
// Qwen call so the `/inference` dashboard can attribute every
// call back to the caller (the rubric name for classify, the literal
// "vault-rerank-retrieve" for vault search retrieval, and so on).
//
// **Invocation pattern:** handlers call
// `ctx = qwenctx.WithTaskID(ctx, "rubric:bug-severity")` once before
// invoking the router; the router reads the stamp via
// `qwenctx.TaskIDFromContext(ctx)` and persists it on every
// inference_invocations row.
//
// **Success shape:** each Qwen call lands a row in `inference_invocations`
// carrying the caller's task_id stamp; unstamped callers record as the
// sentinel `qwenctx.Unattributed` so the call-count figure remains
// accurate while the dashboard surfaces the attribution gap.
//
// **Non-goals:** not a tracing system (see internal/obs for spans),
// not an auth carrier, does not validate stamps (free-form strings —
// discipline lives at the caller); designed for single-stamp-per-call
// attribution, not nested or hierarchical attribution.
package qwenctx
