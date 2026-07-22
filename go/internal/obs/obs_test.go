package obs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// recordingSink captures every SpanEvent for assertions in tests. Safe
// for concurrent Publish since spans fire from multiple goroutines in
// the dispatch path.
type recordingSink struct {
	mu     sync.Mutex
	events []obs.SpanEvent
}

func (r *recordingSink) Publish(ev obs.SpanEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingSink) Snapshot() []obs.SpanEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]obs.SpanEvent, len(r.events))
	copy(out, r.events)
	return out
}

// TestSpanStart_RootMintsTraceIDFromSpan asserts the contract that a
// root span (no parent on ctx) has TraceID == ID and ParentID == "".
// This is the canonical "first frame in a request" shape.
func TestSpanStart_RootMintsTraceIDFromSpan(t *testing.T) {
	sink := &recordingSink{}
	obs.SetSpanSink(sink)
	t.Cleanup(func() { obs.SetSpanSink(nil) })

	ctx, end := obs.SpanStart(context.Background(), "test.root")
	defer end(nil)

	span := obs.SpanFromContext(ctx)
	if span == nil {
		t.Fatal("expected span on ctx after SpanStart")
	}
	if span.ID == "" {
		t.Fatal("span.ID empty")
	}
	if span.TraceID != span.ID {
		t.Errorf("root span TraceID %q != ID %q", span.TraceID, span.ID)
	}
	if span.ParentID != "" {
		t.Errorf("root span ParentID %q want empty", span.ParentID)
	}
	if span.Name != "test.root" {
		t.Errorf("span name = %q, want test.root", span.Name)
	}
}

// TestSpanStart_ChildInheritsTraceIDLinksParent verifies that opening a
// child span under a parent ctx yields a new span_id but the same trace
// id, with ParentID pointing at the outer span. This is the load-bearing
// shape for the cross-handler propagation: the events table's
// span_id == child.ID, and a tree-render groups on trace_id.
func TestSpanStart_ChildInheritsTraceIDLinksParent(t *testing.T) {
	sink := &recordingSink{}
	obs.SetSpanSink(sink)
	t.Cleanup(func() { obs.SetSpanSink(nil) })

	ctx, endRoot := obs.SpanStart(context.Background(), "test.root")
	defer endRoot(nil)
	root := obs.SpanFromContext(ctx)

	childCtx, endChild := obs.SpanStart(ctx, "test.child")
	defer endChild(nil)
	child := obs.SpanFromContext(childCtx)

	if child.ID == root.ID {
		t.Fatal("child.ID == root.ID — should be a fresh UUID")
	}
	if child.TraceID != root.TraceID {
		t.Errorf("child.TraceID %q != root.TraceID %q", child.TraceID, root.TraceID)
	}
	if child.ParentID != root.ID {
		t.Errorf("child.ParentID %q != root.ID %q", child.ParentID, root.ID)
	}
}

// TestSpanStart_StampsEventsSpanID covers the seam to internal/events:
// after obs.SpanStart, events.SpanIDFromContext returns the obs span_id,
// so an events.Emit fired inside the span lands in the events table with
// span_id == span.ID. This is the join key the audit's structured-log
// rationale (AGENT_AUDIT_CONVENTIONS.md §11) requires.
func TestSpanStart_StampsEventsSpanID(t *testing.T) {
	ctx, end := obs.SpanStart(context.Background(), "test.events-seam")
	defer end(nil)

	span := obs.SpanFromContext(ctx)
	eventsSpanID, err := events.SpanIDFromContext(ctx)
	if err != nil {
		t.Fatalf("events.SpanIDFromContext: %v", err)
	}
	if eventsSpanID != span.ID {
		t.Errorf("events.SpanIDFromContext = %q, want %q (obs span)", eventsSpanID, span.ID)
	}
}

// TestSpanStart_PublishesOpenAndCloseOnce verifies the SSE contract:
// exactly one span_open and one span_close per SpanStart/end pair. The
// dashboard's tree-fold counts on this 1:1 invariant — a missing close
// shows up as an unclosed branch.
func TestSpanStart_PublishesOpenAndCloseOnce(t *testing.T) {
	sink := &recordingSink{}
	obs.SetSpanSink(sink)
	t.Cleanup(func() { obs.SetSpanSink(nil) })

	_, end := obs.SpanStart(context.Background(), "test.lifecycle")
	end(nil)

	got := sink.Snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d span events, want 2 (open+close)", len(got))
	}
	if got[0].Type != "span_open" {
		t.Errorf("event[0].Type = %q, want span_open", got[0].Type)
	}
	if got[1].Type != "span_close" {
		t.Errorf("event[1].Type = %q, want span_close", got[1].Type)
	}
	if got[0].SpanID != got[1].SpanID {
		t.Errorf("open span_id %q != close span_id %q", got[0].SpanID, got[1].SpanID)
	}
	if got[1].Status != "ok" {
		t.Errorf("close status = %q, want ok", got[1].Status)
	}
}

// TestSpanStart_ErrorEndMarksStatus verifies that passing a non-nil err
// to the EndFn flips the span_close status to "error" and records the
// message. Dashboard can render failed spans distinctly.
func TestSpanStart_ErrorEndMarksStatus(t *testing.T) {
	sink := &recordingSink{}
	obs.SetSpanSink(sink)
	t.Cleanup(func() { obs.SetSpanSink(nil) })

	_, end := obs.SpanStart(context.Background(), "test.error-end")
	end(errors.New("boom"))

	got := sink.Snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d span events, want 2", len(got))
	}
	closeEv := got[1]
	if closeEv.Status != "error" {
		t.Errorf("close status = %q, want error", closeEv.Status)
	}
	if !strings.Contains(closeEv.ErrorMsg, "boom") {
		t.Errorf("close ErrorMsg = %q, want substring 'boom'", closeEv.ErrorMsg)
	}
}

// TestSpanFromContext_AbsentReturnsNil documents the safe default: a
// ctx without an attached span returns nil from SpanFromContext, and
// Logger(ctx) falls back to the bare logger with no span attrs.
func TestSpanFromContext_AbsentReturnsNil(t *testing.T) {
	if obs.SpanFromContext(context.Background()) != nil {
		t.Fatal("SpanFromContext on bare ctx should return nil")
	}
}
