package obs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// SpanTail serves the /events/spans SSE stream by tailing the
// span_events table. Every toolkit-server process — stdio MCPs without
// --http-port included — persists via [DBSpanSink]; the HTTP daemon
// streams the union here.
//
// On connect: replay the last [defaultSpanTailReplayN] rows so a fresh
// page load is never empty. Then poll the table on a fixed tick,
// advance the cursor by id, and push each new row as a
// `data: <json>\n\n` SSE frame. JSON shape matches [SpanEvent]
// byte-for-byte so dashboard consumers see no wire change.
type SpanTail struct {
	db           *sql.DB
	pollInterval time.Duration
	keepAlive    time.Duration
	replayN      int
	pageLimit    int
}

const (
	defaultSpanTailPollInterval = 250 * time.Millisecond
	defaultSpanTailReplayN      = 200
	defaultSpanTailPageLimit    = 500
)

// DefaultSpanKeepAlive is the SSE keep-alive interval for /events/spans.
// Matches eventbus.DefaultKeepAliveInterval (30s) so reverse-proxy
// behaviour is uniform across the two streams.
const DefaultSpanKeepAlive = 30 * time.Second

// NewSpanTail wires a tail handler against db with default cadence.
// Tests use [NewSpanTailWithOptions] for sub-second polling so
// assertions don't have to wait the full 250 ms tick.
func NewSpanTail(db *sql.DB) *SpanTail {
	return &SpanTail{
		db:           db,
		pollInterval: defaultSpanTailPollInterval,
		keepAlive:    DefaultSpanKeepAlive,
		replayN:      defaultSpanTailReplayN,
		pageLimit:    defaultSpanTailPageLimit,
	}
}

// SpanTailOptions tunes cadence for tests. All zero values fall back to
// the package defaults.
type SpanTailOptions struct {
	PollInterval time.Duration
	KeepAlive    time.Duration
	ReplayN      int
	PageLimit    int
}

// NewSpanTailWithOptions is NewSpanTail with explicit knobs.
func NewSpanTailWithOptions(db *sql.DB, opts SpanTailOptions) *SpanTail {
	t := NewSpanTail(db)
	if opts.PollInterval > 0 {
		t.pollInterval = opts.PollInterval
	}
	if opts.KeepAlive > 0 {
		t.keepAlive = opts.KeepAlive
	}
	if opts.ReplayN > 0 {
		t.replayN = opts.ReplayN
	}
	if opts.PageLimit > 0 {
		t.pageLimit = opts.PageLimit
	}
	return t
}

// Handler returns an http.Handler that streams span_events as SSE.
func (t *SpanTail) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ctx := r.Context()

		// Initial replay: fetch the last replayN rows in id order (so the
		// client receives them oldest → newest, matching the live tail),
		// then continue tailing from the max id we sent.
		cursor, err := t.replayInitial(ctx, w, flusher)
		if err != nil {
			// Client disconnected mid-replay, or DB error. Either way, stop.
			return
		}

		ticker := time.NewTicker(t.pollInterval)
		defer ticker.Stop()
		keepAlive := time.NewTicker(t.keepAlive)
		defer keepAlive.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next, err := t.pushSince(ctx, w, flusher, cursor)
				if err != nil {
					return
				}
				cursor = next
			case <-keepAlive.C:
				if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}

// replayInitial sends the most recent replayN rows in chronological
// (id-ascending) order and returns the max id seen — the starting
// cursor for the tail loop.
func (t *SpanTail) replayInitial(ctx context.Context, w http.ResponseWriter, flusher http.Flusher) (int64, error) {
	// Find the cursor first so we can stream by id ASC without an
	// in-memory reversal. The page is bounded by replayN; if the table is
	// empty we start from 0.
	var maxID sql.NullInt64
	if err := t.db.QueryRowContext(ctx, `SELECT MAX(id) FROM span_events`).Scan(&maxID); err != nil {
		L().Warn("span_tail_max_id_failed", slog.String("err", err.Error()))
		return 0, nil
	}
	if !maxID.Valid {
		return 0, nil
	}
	startAfter := maxID.Int64 - int64(t.replayN)
	if startAfter < 0 {
		startAfter = 0
	}
	cursor, err := t.pushSince(ctx, w, flusher, startAfter)
	if err != nil {
		return 0, err
	}
	return cursor, nil
}

// pushSince fetches up to pageLimit rows with id > cursor in id-ascending
// order and writes them as SSE frames. Returns the new cursor (max id
// pushed, or the input cursor if no rows). On client-write error, returns
// the error so the caller closes the connection.
func (t *SpanTail) pushSince(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, cursor int64) (int64, error) {
	rows, err := t.db.QueryContext(ctx,
		`SELECT id, type, span_id, parent_span_id, trace_id, name,
		        started_at, duration_ms, status, error
		   FROM span_events
		  WHERE id > ?
		  ORDER BY id ASC
		  LIMIT ?`,
		cursor, t.pageLimit,
	)
	if err != nil {
		L().Warn("span_tail_query_failed", slog.String("err", err.Error()))
		return cursor, nil
	}
	defer rows.Close()

	newCursor := cursor
	for rows.Next() {
		var (
			id         int64
			ev         SpanEvent
			parent     sql.NullString
			durationMS sql.NullInt64
			status     sql.NullString
			errMsg     sql.NullString
		)
		if err := rows.Scan(
			&id, &ev.Type, &ev.SpanID, &parent, &ev.TraceID, &ev.Name,
			&ev.StartedAt, &durationMS, &status, &errMsg,
		); err != nil {
			L().Warn("span_tail_scan_failed", slog.String("err", err.Error()))
			continue
		}
		if parent.Valid {
			ev.ParentSpanID = parent.String
		}
		if durationMS.Valid {
			ev.DurationMS = durationMS.Int64
		}
		if status.Valid {
			ev.Status = status.String
		}
		if errMsg.Valid {
			ev.ErrorMsg = errMsg.String
		}

		payload, jerr := json.Marshal(ev)
		var werr error
		if jerr != nil {
			_, werr = fmt.Fprintf(w, "data: {\"error\":\"serialize: %s\"}\n\n", jerr.Error())
		} else {
			_, werr = fmt.Fprintf(w, "data: %s\n\n", payload)
		}
		if werr != nil {
			return newCursor, werr
		}
		flusher.Flush()
		newCursor = id
	}
	if err := rows.Err(); err != nil {
		L().Warn("span_tail_rows_err", slog.String("err", err.Error()))
	}
	return newCursor, nil
}

// PruneOlderThan deletes span_events rows older than cutoff. Returns the
// number of rows deleted. Intended for periodic invocation from the HTTP
// daemon's retention janitor; runs synchronously so callers can log the
// result.
func (t *SpanTail) PruneOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := t.db.ExecContext(ctx,
		`DELETE FROM span_events WHERE inserted_at < ?`,
		cutoff.UTC().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
