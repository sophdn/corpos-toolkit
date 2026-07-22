package telemetry

import "context"

// querySourceKey carries the QuerySource discriminator through ctx so
// the agent-initiated path stays the default and the future proactive-
// hook caller can stamp 'proactive_hook' (or 'other') without each
// handler rewriting its parameter list.
//
// Modeled on the qwenctx pattern (internal/qwenctx) — one ctx key per
// orthogonal axis, no shared "telemetry context object" because
// per-handler relevance varies.
type querySourceKey struct{}

type userMessageIDKey struct{}

// WithQuerySource returns a context that carries the given query_source.
// Callers: the dispatcher when handling a tools/call (default
// SourceAgentInitiated), the future proactive hook (SourceProactiveHook),
// the dashboard portal-write paths (SourceDashboardUser).
func WithQuerySource(ctx context.Context, src QuerySource) context.Context {
	if src == "" {
		return ctx
	}
	return context.WithValue(ctx, querySourceKey{}, src)
}

// QuerySourceFromContext returns the QuerySource on ctx, or
// SourceAgentInitiated when no caller has stamped one. Default mirrors
// the column default in migration 037.
func QuerySourceFromContext(ctx context.Context) QuerySource {
	if v, ok := ctx.Value(querySourceKey{}).(QuerySource); ok && v != "" {
		return v
	}
	return SourceAgentInitiated
}

// WithUserMessageID stamps the user message UUID (or content hash) on
// ctx so subsequent grounding_events writes carry the trigger-message
// identifier. The proactive-injection chain needs this to train the
// should-fire classifier on (message, was-injection-cited) pairs.
// Agent-initiated calls typically don't stamp this — the column lands
// NULL.
func WithUserMessageID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, userMessageIDKey{}, id)
}

// UserMessageIDFromContext returns the stamped user_message_id or "".
func UserMessageIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userMessageIDKey{}).(string); ok {
		return v
	}
	return ""
}
