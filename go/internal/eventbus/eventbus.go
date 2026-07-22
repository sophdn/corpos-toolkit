// Package eventbus implements an in-process SSE event bus for the
// toolkit-server.
//
// The bus broadcasts write-point events (artifact created, task
// transitioned, bug filed) to all attached subscribers. The dashboard
// connects via HTTP /events and receives Server-Sent Events as long as
// the connection lives. Ports the Rust crates/observe-http surface — the
// event variants match byte-for-byte so the dashboard can read either
// implementation during the migration window.
//
// Design:
//
//   - One sync.RWMutex-guarded slice of subscriber channels.
//   - Publish() snapshots the slice under read lock and sends to each
//     subscriber non-blocking; backed-up subscribers drop the event
//     rather than stalling the publisher. The Rust implementation uses
//     a tokio broadcast::Sender with the same effective semantics.
//   - Subscribe() returns a receive-only channel plus an unsubscribe
//     callback. Subscribers MUST call the callback when done so the
//     slice doesn't grow unbounded.
//
// The HTTP layer is in eventbus_http.go.
package eventbus

import (
	"context"
	"sync"
	"time"
)

// Event is one broadcast unit. Type carries the variant name
// ("task_completed", "bug_filed", ...); Kind carries the artifact kind
// for filtering by dashboard panel. The variant-specific fields
// (ChainSlug, TaskSlug, ToStatus, Severity) are typed onto Event
// directly with omitempty — surveyed across the four canonical event
// variants (TaskCompleted, TaskTransitioned, BugFiled, BugResolved,
// ArtifactCreated), the payload vocabulary is closed at these four
// strings. Default struct marshaling emits the same wire shape Rust
// serde produces for the flat-tagged enum, with empty fields elided.
//
// If a future event variant needs payload fields not covered here, add
// a typed field with omitempty rather than reintroducing an untyped
// map — the bus's payload schema is part of the public dashboard
// contract and is worth keeping explicit.
type Event struct {
	Type      string    `json:"event"`
	ProjectID string    `json:"project_id,omitempty"`
	Slug      string    `json:"slug,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	ChainSlug string    `json:"chain_slug,omitempty"`
	TaskSlug  string    `json:"task_slug,omitempty"`
	ToStatus  string    `json:"to_status,omitempty"`
	Severity  string    `json:"severity,omitempty"`
	Priority  string    `json:"priority,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// Constructor helpers for the canonical event variants. Mirror the Rust
// Event enum names.

// TaskCompleted constructs a task_completed event.
func TaskCompleted(projectID, chainSlug, taskSlug string) Event {
	return Event{
		Type:      "task_completed",
		ProjectID: projectID,
		Slug:      taskSlug,
		ChainSlug: chainSlug,
		TaskSlug:  taskSlug,
	}
}

// TaskTransitioned constructs a task_transitioned event.
func TaskTransitioned(projectID, taskSlug, toStatus string) Event {
	return Event{
		Type:      "task_transitioned",
		ProjectID: projectID,
		Slug:      taskSlug,
		TaskSlug:  taskSlug,
		ToStatus:  toStatus,
	}
}

// BugFiled constructs a bug_filed event.
func BugFiled(projectID, slug, severity string) Event {
	return Event{
		Type:      "bug_filed",
		ProjectID: projectID,
		Slug:      slug,
		Severity:  severity,
	}
}

// BugResolved constructs a bug_resolved event.
func BugResolved(projectID, slug, kind string) Event {
	return Event{
		Type:      "bug_resolved",
		ProjectID: projectID,
		Slug:      slug,
		Kind:      kind,
	}
}

// SuggestionFiled constructs a suggestion_filed event. Sibling to
// BugFiled per chain `agent-suggestion-box`; the SSE Priority field is
// the suggestion-side analogue of bug.Severity.
func SuggestionFiled(projectID, slug, priority string) Event {
	return Event{
		Type:      "suggestion_filed",
		ProjectID: projectID,
		Slug:      slug,
		Priority:  priority,
	}
}

// SuggestionResolved constructs a suggestion_resolved event. Kind is
// one of adopted / deferred / rejected (suggestion vocabulary).
func SuggestionResolved(projectID, slug, kind string) Event {
	return Event{
		Type:      "suggestion_resolved",
		ProjectID: projectID,
		Slug:      slug,
		Kind:      kind,
	}
}

// ArtifactCreated is the generic create-event used by forge for any
// schema that does not have a dedicated variant.
func ArtifactCreated(projectID, schemaName, slug string) Event {
	return Event{
		Type:      "artifact_created",
		ProjectID: projectID,
		Slug:      slug,
		Kind:      schemaName,
	}
}

// Bus is the in-process broadcast surface.
type Bus struct {
	mu          sync.RWMutex
	subscribers []chan Event
	// bufferSize caps how many in-flight events each subscriber can buffer
	// before Publish starts dropping for that subscriber. Zero falls back
	// to a sensible default at construction time.
	bufferSize int
}

// New returns a fresh bus. bufferSize is per-subscriber; a small value
// (default 64) keeps memory bounded while absorbing brief publisher bursts.
func New(bufferSize int) *Bus {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	return &Bus{bufferSize: bufferSize}
}

// Subscribe registers a new subscriber. Returns a receive-only channel
// and an unsubscribe callback. Callers MUST invoke the callback when
// they're done — the SSE handler uses defer for this.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, b.bufferSize)
	b.mu.Lock()
	b.subscribers = append(b.subscribers, ch)
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, sub := range b.subscribers {
			if sub == ch {
				b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
				close(ch)
				return
			}
		}
	}
	return ch, cancel
}

// Publish sends event to every subscriber. Non-blocking: a slow subscriber
// whose buffer is full silently drops the event. The dashboard interprets
// a missed event as a hint to refetch from the read endpoints — same
// semantics as Rust's broadcast::Sender Lagged variant, minus the explicit
// lagged-marker event.
func (b *Bus) Publish(event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	// Hold the RLock across the entire send loop so concurrent
	// Subscribe cancellation (which takes the write Lock to remove +
	// close the channel) cannot fire between snapshot and send. Each
	// send is non-blocking via the select default, so total hold time
	// is O(num_subs * one-channel-op) microseconds — fine even with
	// hundreds of subscribers, since RLock allows multiple publishers
	// to fan out in parallel. The only thing this blocks is the rare
	// unsubscribe path, which is happy to wait out a publish.
	//
	// Bug 1307's earlier fix used a snapshot-then-release shape with a
	// trySend recover() to contain the close-during-publish panic. That
	// prevented crashes but silently dropped events to closing
	// subscribers. Holding the lock removes the race window entirely:
	// no concurrent close can happen mid-loop, so the send is safe.
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subscribers {
		select {
		case sub <- event:
		default:
			// dropped; subscriber is responsible for refetching state
		}
	}
}

// SubscriberCount returns the live subscriber count — useful for health
// probes and tests.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Drain closes every subscriber channel. Used at shutdown so any HTTP
// SSE handlers waiting on a Receive unblock and return cleanly.
func (b *Bus) Drain(ctx context.Context) {
	b.mu.Lock()
	subs := b.subscribers
	b.subscribers = nil
	b.mu.Unlock()
	for _, sub := range subs {
		// Use a non-blocking send-context pattern so a slow listener can't
		// stall shutdown.
		select {
		case <-ctx.Done():
			return
		default:
		}
		close(sub)
	}
}
