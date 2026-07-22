package obs

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// SpanEvent is one open/close span notification published to the SSE
// span stream. The wire shape is JSON; field tags mirror the dashboard's
// expected shape. StartedAt is RFC 3339 with millisecond precision to
// match the events table's `ts` formatting.
type SpanEvent struct {
	Type         string `json:"type"` // "span_open" | "span_close"
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	TraceID      string `json:"trace_id"`
	Name         string `json:"name"`
	StartedAt    string `json:"started_at"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	Status       string `json:"status,omitempty"`
	ErrorMsg     string `json:"error,omitempty"`
}

// SpanSink receives span events for downstream broadcast. The
// observehttp layer registers a sink at server startup that fans events
// out to every /events/spans subscriber; tests register a recording sink
// that asserts on shape and order.
//
// Implementations must be safe for concurrent Publish — spans fire from
// every meta-tool dispatch goroutine plus the in-transaction hooks they
// open underneath.
type SpanSink interface {
	Publish(SpanEvent)
}

// spanSink is the global sink, stored via atomic.Pointer so SetSpanSink
// is safe to call from tests without a per-publish lock. nil means no
// subscriber — Publish is a no-op.
var spanSink atomic.Pointer[SpanSink]

// SetSpanSink installs the global span sink. Calling twice replaces the
// prior sink — tests use this to swap in a recorder and restore the nil
// default in t.Cleanup.
func SetSpanSink(sink SpanSink) {
	if sink == nil {
		spanSink.Store(nil)
		return
	}
	spanSink.Store(&sink)
}

// publishSpanOpen emits the span_open event to the sink and writes an
// "span_open" log entry. The log entry carries the same span_id the
// event does, so a grep for one finds the other.
func publishSpanOpen(ctx context.Context, s *Span) {
	if p := spanSink.Load(); p != nil {
		(*p).Publish(SpanEvent{
			Type:         "span_open",
			SpanID:       s.ID,
			ParentSpanID: s.ParentID,
			TraceID:      s.TraceID,
			Name:         s.Name,
			StartedAt:    s.Start.Format("2006-01-02T15:04:05.000Z07:00"),
		})
	}
	Logger(ctx).Info("span_open", slog.String("name", s.Name))
}

// publishSpanClose emits the span_close event with timing + status.
// Mirror of publishSpanOpen — same span_id, additional duration_ms and
// status fields.
func publishSpanClose(ctx context.Context, s *Span, d time.Duration, status string, err error) {
	ev := SpanEvent{
		Type:         "span_close",
		SpanID:       s.ID,
		ParentSpanID: s.ParentID,
		TraceID:      s.TraceID,
		Name:         s.Name,
		StartedAt:    s.Start.Format("2006-01-02T15:04:05.000Z07:00"),
		DurationMS:   d.Milliseconds(),
		Status:       status,
	}
	if err != nil {
		ev.ErrorMsg = err.Error()
	}
	if p := spanSink.Load(); p != nil {
		(*p).Publish(ev)
	}
	attrs := []slog.Attr{
		slog.String("name", s.Name),
		slog.Int64("duration_ms", d.Milliseconds()),
		slog.String("status", status),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	Logger(ctx).LogAttrs(ctx, slog.LevelInfo, "span_close", attrs...)
}
