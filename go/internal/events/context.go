package events

import "context"

// Context keys are private so callers can't construct them directly —
// every read goes through ActorFromContext / SpanIDFromContext, every
// write through WithActor / WithSpanID. Default returns from the
// FromContext getters keep this package usable before T3 wires real
// actor inference and span minting at the dispatch boundary.
type ctxKey int

const (
	actorKey ctxKey = iota
	spanIDKey
	rationaleKey
	mcpSessionIDKey
)

// WithActor attaches an Actor to ctx for downstream Emit calls. T3's
// dispatch middleware calls this after inferring the actor from the
// transport (stdio→agent, portal HTTP→human, CLI→system); test code
// calls it to inject a known actor before driving a handler.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey, a)
}

// ActorFromContext returns the Actor attached via [WithActor], or a
// system-kind sentinel ({kind: "system", id: "unattributed"}) if none
// is present. The sentinel is deliberately not "agent" or "human" — an
// emit that lacks an actor must not silently masquerade as either; it
// is recorded as a system emit so the gap is queryable.
//
// This default is the seam between this chain's T2 (which ships the
// foundation) and T3 (which wires real actor inference). Until T3
// lands, every handler emit shows up as actor=system/unattributed in
// the ledger; T3's rollout sweep replaces that with real values.
func ActorFromContext(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey).(Actor); ok {
		return a
	}
	return Actor{Kind: "system", ID: "unattributed"}
}

// WithSpanID attaches a span_id to ctx. T3's dispatch middleware mints
// one UUIDv4 per MCP tools/call request; tests inject a known span to
// assert on it.
func WithSpanID(ctx context.Context, spanID string) context.Context {
	return context.WithValue(ctx, spanIDKey, spanID)
}

// SpanIDFromContext returns the span_id attached via [WithSpanID]. If
// none is present, generates a fresh UUIDv4 — Emit is then attributing
// that one event to its own one-event span. This is safe: span_id is
// observability metadata, not a correctness invariant; an orphan-span
// emit is queryable as such (no sibling events share the id) and the
// T3 dispatch wiring eliminates the case in production.
//
// Returns the empty string and an error only if crypto/rand fails,
// which on Linux means /dev/urandom is unreadable — a kernel-level
// failure that should propagate, not be silently swallowed.
func SpanIDFromContext(ctx context.Context) (string, error) {
	if id, ok := ctx.Value(spanIDKey).(string); ok && id != "" {
		return id, nil
	}
	return newUUIDv4()
}

// WithRationale attaches a validated rationale to ctx for downstream
// [Emit] calls. T3's dispatch middleware calls this once per request
// after the policy gate passes; handlers do not call it themselves.
// Storing the rationale on ctx (rather than threading it as a parameter
// through every handler) keeps the dual-write call sites unchanged
// across the 12+ mutating handlers landed in T2 — they emit without
// having to re-thread a string they never touched.
func WithRationale(ctx context.Context, rationale string) context.Context {
	return context.WithValue(ctx, rationaleKey, rationale)
}

// RationaleFromContext returns the rationale attached via
// [WithRationale]. Returns the empty string when none is present; a
// caller-supplied EmitArgs.Rationale takes precedence over the
// ctx value (see [Emit]).
func RationaleFromContext(ctx context.Context) string {
	if r, ok := ctx.Value(rationaleKey).(string); ok {
		return r
	}
	return ""
}

// WithMCPSessionID attaches a stable per-MCP-session identifier to ctx.
// The toolkit-server's MCP transport wrapper stamps this once per
// tools/call from the underlying ServerSession — stable across every
// call within one stdio connection (the session pointer's lifetime).
// Downstream callers that want session-scoped caching (parse_context's
// filter cache being the load-bearing example) read this value rather
// than deriving session identity from the per-call span TraceID — that
// trace is freshly minted on every dispatch and never matches across
// calls. Bug fix: cache_hits=0 in production on identical re-resolutions
// (chain parse-context-lean-orienting T1).
func WithMCPSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, mcpSessionIDKey, id)
}

// MCPSessionIDFromContext returns the MCP session id attached via
// [WithMCPSessionID]. Returns the empty string when none is present —
// callers that fall back to other identifiers (parse_context's handler
// drops to span TraceID as a last resort) must handle the empty case
// themselves.
func MCPSessionIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(mcpSessionIDKey).(string); ok {
		return id
	}
	return ""
}
