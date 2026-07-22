// Package eventbus implements the in-process SSE event bus used by the
// dashboard live stream.
//
// ## Intended use
//
// **Workflow served:** write-point notifications (artifact created, task
// transitioned, bug filed) need to reach the dashboard without polling;
// the eventbus broadcasts these to all attached SSE subscribers and
// serves `/events` (write-point) and `/events/spans` (observability
// span tree) over Server-Sent Events.
//
// **Invocation pattern:** `bus := eventbus.New()` once at server boot;
// handlers call `bus.Publish(event)` after their write commits; the
// dashboard hits `GET /events` (or `/events/spans`) and stays connected.
// Subscribers receive a `(<-chan Event, cancel)` pair from
// `bus.Subscribe()` and MUST invoke `cancel` to detach.
//
// **Success shape:** subscribers receive one JSON object per event on
// their channel; `Publish` returns immediately even if a subscriber is
// backed up — backed-up subscribers drop events rather than stall the
// publisher (non-blocking with drop-on-overflow, mirroring the Rust
// crates/observe-http semantics).
//
// **Non-goals:** not the events ledger (see internal/events for the
// append-only typed-event substrate), not durable across restarts, does
// not retry on subscriber back-pressure (events are lost rather than
// queued), not a transport — only in-process pub/sub plus the SSE HTTP
// handler.
package eventbus
