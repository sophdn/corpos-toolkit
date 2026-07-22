// Package qwenctx carries the task_id stamp through context for the
// inference router's universal telemetry table (bug 1328).
//
// The router's Generate path reads the stamp and persists one row per
// call into inference_invocations, so the /inference dashboard can
// attribute every Qwen call back to the handler that issued it
// (rubric name for classify, "vault-rerank-retrieve" for vault search
// retrieval, etc.). Calls without a stamp record as "unattributed" —
// the row still lands so the call-count figure is accurate, but the
// dashboard surfaces the gap so the caller can be wired up.
package qwenctx

import "context"

type taskIDKey struct{}

// Unattributed is the stand-in stamp recorded when a caller invokes
// the router without first calling WithTaskID. Exported so handlers
// and tests can branch on the sentinel without re-declaring it.
const Unattributed = "unattributed"

// WithTaskID returns a context that carries the given task_id stamp.
// Handlers call this once before invoking router.Generate; the value
// flows through any number of nested calls.
func WithTaskID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, taskIDKey{}, id)
}

// TaskID returns the task_id stamp on ctx, or [Unattributed] when no
// caller has stamped one. The router uses this to populate the
// inference_invocations row.
func TaskID(ctx context.Context) string {
	if v, ok := ctx.Value(taskIDKey{}).(string); ok && v != "" {
		return v
	}
	return Unattributed
}
