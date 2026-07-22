// Package observehttp serves the read-only HTTP observe API consumed
// by the dashboard frontend.
//
// ## Intended use
//
// **Workflow served:** the dashboard hits a stable HTTP API for
// benchmark cards, rubric cards, tasks, projects, hosts, bug lists,
// and projection reads; this package serves those endpoints and
// aggregates rows into the dashboard's card shapes. SSE for the live
// streams (/events, /events/spans) is owned by internal/eventbus and
// internal/obs, mounted alongside.
//
// **Invocation pattern:** `srv := observehttp.New(pool, eventBus,
// port)` at startup, then `srv.ListenAndServe()`; the dashboard makes
// `GET /api/...` requests against this server during normal operation.
//
// **Success shape:** JSON-encoded card / row arrays matching the
// dashboard's TypeScript types; benchmark-card aggregation produces
// `ModelMetrics` rows folded from `cardRow` inputs across the three
// card-shaped endpoints (`/cards`, `/rubric-cards`, `/tasks`).
//
// **Non-goals:** not a write API (writes go through the MCP meta-tools
// via internal/dispatch), no auth (localhost-only at this stage), does
// not own card shapes — those mirror the Rust observe-http surface
// during the migration window so the dashboard can read either
// implementation interchangeably.
package observehttp
