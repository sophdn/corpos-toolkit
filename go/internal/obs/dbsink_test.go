package obs_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"toolkit/internal/obs"
	"toolkit/internal/testutil"
)

func TestDBSpanSink_PublishOpenAndClose(t *testing.T) {
	pool := testutil.NewTestDB(t)
	sink := obs.NewDBSpanSink(pool.DB())

	sink.Publish(obs.SpanEvent{
		Type:      "span_open",
		SpanID:    "span-a",
		TraceID:   "trace-1",
		Name:      "work.forge",
		StartedAt: "2026-05-17T19:00:00.000Z",
	})
	sink.Publish(obs.SpanEvent{
		Type:         "span_close",
		SpanID:       "span-b",
		ParentSpanID: "span-a",
		TraceID:      "trace-1",
		Name:         "forge.index_upsert",
		StartedAt:    "2026-05-17T19:00:00.123Z",
		DurationMS:   42,
		Status:       "ok",
	})
	sink.Publish(obs.SpanEvent{
		Type:       "span_close",
		SpanID:     "span-c",
		TraceID:    "trace-2",
		Name:       "work.measure",
		StartedAt:  "2026-05-17T19:00:01.000Z",
		DurationMS: 7,
		Status:     "error",
		ErrorMsg:   "boom",
	})

	rows, err := pool.DB().Query(
		`SELECT type, span_id, parent_span_id, trace_id, name,
		        started_at, duration_ms, status, error
		   FROM span_events ORDER BY id ASC`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type rec struct {
		typ, spanID, traceID, name, startedAt string
		parent, status, errMsg                sql.NullString
		duration                              sql.NullInt64
	}
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.typ, &r.spanID, &r.parent, &r.traceID, &r.name,
			&r.startedAt, &r.duration, &r.status, &r.errMsg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}

	if len(got) != 3 {
		t.Fatalf("want 3 rows, got %d", len(got))
	}

	// span_open: parent NULL, duration NULL, status NULL.
	if got[0].typ != "span_open" || got[0].spanID != "span-a" {
		t.Errorf("row 0 wrong: %+v", got[0])
	}
	if got[0].parent.Valid || got[0].duration.Valid || got[0].status.Valid {
		t.Errorf("row 0 should have null parent/duration/status, got %+v", got[0])
	}

	// span_close with parent + duration + ok status.
	if got[1].typ != "span_close" || !got[1].parent.Valid || got[1].parent.String != "span-a" {
		t.Errorf("row 1 wrong: %+v", got[1])
	}
	if !got[1].duration.Valid || got[1].duration.Int64 != 42 {
		t.Errorf("row 1 duration: %+v", got[1])
	}
	if !got[1].status.Valid || got[1].status.String != "ok" {
		t.Errorf("row 1 status: %+v", got[1])
	}
	if got[1].errMsg.Valid {
		t.Errorf("row 1 should have null error, got %v", got[1].errMsg.String)
	}

	// span_close with error + populated error message.
	if got[2].typ != "span_close" || got[2].spanID != "span-c" {
		t.Errorf("row 2 wrong: %+v", got[2])
	}
	if !got[2].errMsg.Valid || got[2].errMsg.String != "boom" {
		t.Errorf("row 2 error: %+v", got[2])
	}
	if !got[2].status.Valid || got[2].status.String != "error" {
		t.Errorf("row 2 status: %+v", got[2])
	}
}

func TestDBSpanSink_PublishDoesNotPanicOnClosedDB(t *testing.T) {
	pool := testutil.NewTestDB(t)
	sink := obs.NewDBSpanSink(pool.DB())
	pool.Close()
	// Closed pool should warn-and-drop, not panic. The cleanup t.Cleanup
	// from NewTestDB will also try to close — make sure the second close
	// is harmless.
	sink.Publish(obs.SpanEvent{
		Type:      "span_open",
		SpanID:    "span-x",
		TraceID:   "trace-z",
		Name:      "noop",
		StartedAt: "2026-05-17T19:00:00.000Z",
	})
}

func TestSpanTail_PruneOlderThan(t *testing.T) {
	pool := testutil.NewTestDB(t)
	sink := obs.NewDBSpanSink(pool.DB())
	tail := obs.NewSpanTail(pool.DB())

	// Insert one row, then backdate it.
	sink.Publish(obs.SpanEvent{
		Type: "span_open", SpanID: "old", TraceID: "t",
		Name: "n", StartedAt: "2026-05-17T19:00:00.000Z",
	})
	if _, err := pool.DB().Exec(
		`UPDATE span_events SET inserted_at = datetime('now','-2 days') WHERE span_id='old'`,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	// Insert a fresh row that should survive the prune.
	sink.Publish(obs.SpanEvent{
		Type: "span_open", SpanID: "fresh", TraceID: "t",
		Name: "n", StartedAt: "2026-05-17T19:00:00.000Z",
	})

	deleted, err := tail.PruneOlderThan(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("want 1 deleted, got %d", deleted)
	}

	var remaining int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM span_events`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("want 1 remaining, got %d", remaining)
	}
}
