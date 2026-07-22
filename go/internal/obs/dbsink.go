package obs

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// DBSpanSink persists every span event to the span_events table so the
// /events/spans SSE handler can fan it out across processes. Installed
// unconditionally at startup (see toolkit-server/main.go) so stdio MCP
// processes — which don't run the HTTP daemon — still contribute to the
// live feed via the shared SQLite database.
//
// Implements [SpanSink].
type DBSpanSink struct {
	db *sql.DB
}

// NewDBSpanSink wraps the pool's *sql.DB. The caller keeps ownership; the
// sink doesn't close db.
func NewDBSpanSink(db *sql.DB) *DBSpanSink {
	return &DBSpanSink{db: db}
}

// Publish inserts the event synchronously. Telemetry must never crash
// the request path: on insert error, log at warn and drop. The dispatcher
// has already recorded the span via slog; the only loss is the dashboard
// live feed for that one event.
//
// Uses a short timeout so a wedged DB write can't block the dispatch
// goroutine indefinitely.
func (s *DBSpanSink) Publish(ev SpanEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var (
		parent     any = ev.ParentSpanID
		durationMS any = ev.DurationMS
		status     any = ev.Status
		errMsg     any = ev.ErrorMsg
	)
	if ev.ParentSpanID == "" {
		parent = nil
	}
	if ev.Type == "span_open" {
		durationMS = nil
		status = nil
	}
	if ev.ErrorMsg == "" {
		errMsg = nil
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO span_events
		    (type, span_id, parent_span_id, trace_id, name,
		     started_at, duration_ms, status, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Type, ev.SpanID, parent, ev.TraceID, ev.Name,
		ev.StartedAt, durationMS, status, errMsg,
	)
	if err != nil {
		L().Warn("span_event_persist_failed",
			slog.String("span_id", ev.SpanID),
			slog.String("type", ev.Type),
			slog.String("err", err.Error()),
		)
	}
}
