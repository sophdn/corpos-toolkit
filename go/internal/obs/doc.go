// Package obs is the structured-observability substrate — spans plus
// slog, threaded through every meta-tool dispatch.
//
// ## Intended use
//
// **Workflow served:** an MCP request enters the dispatcher; the
// dispatcher opens a root span via SpanStart and threads the resulting
// ctx through every handler. Handlers emit structured log entries via
// Logger; they open child spans via SpanStart for distinct
// sub-operations (in-transaction hooks, FTS5 sync calls, downstream
// RPCs). The dashboard live-stream renders the span tree.
//
// **Invocation pattern:**
//
//	ctx, end := obs.SpanStart(ctx, "work.task_complete")
//	defer end(nil)
//	obs.Logger(ctx).Info("found task", slog.String("slug", slug))
//
// **Success shape:** every log line and every event emitted under the
// span ctx carries the same span_id. A subscriber on `/events/spans`
// receives one JSON object per span open and one per close — the
// dashboard folds these into a tree keyed by span id.
//
// **Non-goals:** this package does not own actor inference (see
// internal/events `WithActor`), the events ledger (internal/events
// `Emit`), or the write-point event bus (internal/eventbus). It is
// the substrate-primitive for spans and structured logging; the four
// meta-tool dispatchers wire it.
package obs
