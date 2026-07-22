package obs_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/obs"
	"toolkit/internal/testutil"
)

// TestSpanTail_ReplayOnConnect: rows present BEFORE the client connects
// must be streamed during the initial replay (capped at ReplayN). The
// old SpanBus had no replay; this is a UX improvement that the bug's
// acceptance criteria call out, so guard it explicitly.
func TestSpanTail_ReplayOnConnect(t *testing.T) {
	pool := testutil.NewTestDB(t)
	sink := obs.NewDBSpanSink(pool.DB())

	for i, name := range []string{"alpha", "beta", "gamma"} {
		sink.Publish(obs.SpanEvent{
			Type:      "span_open",
			SpanID:    name,
			TraceID:   "trace-r",
			Name:      name,
			StartedAt: "2026-05-17T19:00:0" + itoa(i) + ".000Z",
		})
	}

	tail := obs.NewSpanTailWithOptions(pool.DB(), obs.SpanTailOptions{
		PollInterval: 20 * time.Millisecond,
		KeepAlive:    time.Hour,
		ReplayN:      10,
	})
	srv := httptest.NewServer(tail.Handler())
	t.Cleanup(srv.Close)

	events := readSSEUntil(t, srv.URL, 3, 2*time.Second)
	names := spanNames(events)
	if got, want := names, []string{"alpha", "beta", "gamma"}; !sliceEq(got, want) {
		t.Fatalf("replay order: got %v want %v", got, want)
	}
}

// TestSpanTail_LiveTailAfterConnect: new rows inserted AFTER the client
// connects must arrive within a poll cycle. This is the core acceptance
// criterion of the bug fix.
func TestSpanTail_LiveTailAfterConnect(t *testing.T) {
	pool := testutil.NewTestDB(t)
	sink := obs.NewDBSpanSink(pool.DB())

	tail := obs.NewSpanTailWithOptions(pool.DB(), obs.SpanTailOptions{
		PollInterval: 20 * time.Millisecond,
		KeepAlive:    time.Hour,
		ReplayN:      10,
	})
	srv := httptest.NewServer(tail.Handler())
	t.Cleanup(srv.Close)

	// Start the read in parallel; give it a moment to drain the (empty)
	// replay before publishing.
	type result struct {
		events []obs.SpanEvent
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		resultCh <- result{events: readSSEUntil(t, srv.URL, 2, 3*time.Second)}
	}()
	time.Sleep(80 * time.Millisecond)

	sink.Publish(obs.SpanEvent{
		Type: "span_open", SpanID: "live1", TraceID: "trace-l",
		Name: "live1", StartedAt: "2026-05-17T19:01:00.000Z",
	})
	sink.Publish(obs.SpanEvent{
		Type: "span_close", SpanID: "live1", TraceID: "trace-l",
		Name: "live1", StartedAt: "2026-05-17T19:01:00.000Z",
		DurationMS: 5, Status: "ok",
	})

	r := <-resultCh
	if r.err != nil {
		t.Fatalf("read: %v", r.err)
	}
	names := spanTypes(r.events)
	if got, want := names, []string{"span_open", "span_close"}; !sliceEq(got, want) {
		t.Fatalf("live tail types: got %v want %v", got, want)
	}
}

// TestSpanTail_CrossProcessVisibility is the bug's regression guard.
// Two distinct *sql.DB pools opened on the same file simulate the two
// toolkit-server processes (stdio MCP + HTTP daemon). Spans published
// via pool A must reach a tail handler attached to pool B.
func TestSpanTail_CrossProcessVisibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cross.db")
	stdioPool, err := db.Open(path)
	if err != nil {
		t.Fatalf("open stdio pool: %v", err)
	}
	t.Cleanup(func() { stdioPool.Close() })
	httpPool, err := db.Open(path)
	if err != nil {
		t.Fatalf("open http pool: %v", err)
	}
	t.Cleanup(func() { httpPool.Close() })

	// Sink lives in the "stdio MCP" pool (the one that doesn't run HTTP).
	sink := obs.NewDBSpanSink(stdioPool.DB())
	// Tail lives in the "HTTP daemon" pool — different *sql.DB handle.
	tail := obs.NewSpanTailWithOptions(httpPool.DB(), obs.SpanTailOptions{
		PollInterval: 20 * time.Millisecond,
		KeepAlive:    time.Hour,
		ReplayN:      10,
	})
	srv := httptest.NewServer(tail.Handler())
	t.Cleanup(srv.Close)

	resultCh := make(chan []obs.SpanEvent, 1)
	go func() {
		resultCh <- readSSEUntil(t, srv.URL, 1, 3*time.Second)
	}()
	time.Sleep(80 * time.Millisecond)

	sink.Publish(obs.SpanEvent{
		Type: "span_open", SpanID: "cross-span", TraceID: "cross-trace",
		Name: "stdio.dispatch", StartedAt: "2026-05-17T19:02:00.000Z",
	})

	events := <-resultCh
	if len(events) != 1 {
		t.Fatalf("want 1 event from cross-process publish, got %d", len(events))
	}
	if events[0].SpanID != "cross-span" {
		t.Fatalf("wrong span id: %q", events[0].SpanID)
	}
}

// --- helpers ---

func readSSEUntil(t *testing.T, url string, want int, timeout time.Duration) []obs.SpanEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	var events []obs.SpanEvent
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev obs.SpanEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("unmarshal %q: %v", payload, err)
		}
		events = append(events, ev)
		if len(events) >= want {
			return events
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		t.Fatalf("scanner: %v", err)
	}
	return events
}

func spanNames(evs []obs.SpanEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Name
	}
	return out
}

func spanTypes(evs []obs.SpanEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return ""
}
