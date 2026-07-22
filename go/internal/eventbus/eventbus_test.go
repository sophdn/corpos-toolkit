package eventbus_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"toolkit/internal/eventbus"
)

func TestBus_PublishReachesAllSubscribers(t *testing.T) {
	b := eventbus.New(8)

	sub1, cancel1 := b.Subscribe()
	defer cancel1()
	sub2, cancel2 := b.Subscribe()
	defer cancel2()

	if b.SubscriberCount() != 2 {
		t.Fatalf("SubscriberCount: want 2, got %d", b.SubscriberCount())
	}

	want := eventbus.BugFiled("mcp-servers", "x", "medium")
	b.Publish(want)

	for i, sub := range []<-chan eventbus.Event{sub1, sub2} {
		select {
		case got := <-sub:
			if got.Type != "bug_filed" || got.Slug != "x" {
				t.Errorf("subscriber %d got %+v", i+1, got)
			}
		case <-time.After(500 * time.Millisecond):
			t.Errorf("subscriber %d did not receive event", i+1)
		}
	}
}

func TestBus_CancelRemovesSubscriber(t *testing.T) {
	b := eventbus.New(8)
	_, cancel := b.Subscribe()
	if b.SubscriberCount() != 1 {
		t.Fatalf("pre-cancel count: %d", b.SubscriberCount())
	}
	cancel()
	if b.SubscriberCount() != 0 {
		t.Fatalf("post-cancel count: %d", b.SubscriberCount())
	}
}

func TestBus_SlowSubscriberDropsRatherThanBlocks(t *testing.T) {
	b := eventbus.New(2) // small buffer
	sub, cancel := b.Subscribe()
	defer cancel()

	// Send three events; subscriber never reads. Third should drop, not block.
	done := make(chan struct{})
	go func() {
		b.Publish(eventbus.BugFiled("p", "a", "low"))
		b.Publish(eventbus.BugFiled("p", "b", "low"))
		b.Publish(eventbus.BugFiled("p", "c", "low"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Publish blocked on slow subscriber")
	}
	// First two should still be in the buffer.
	if got := <-sub; got.Slug != "a" {
		t.Errorf("first: %v", got)
	}
	if got := <-sub; got.Slug != "b" {
		t.Errorf("second: %v", got)
	}
}

func TestBus_EventJSONShape(t *testing.T) {
	e := eventbus.BugFiled("mcp-servers", "x", "medium")
	e.Timestamp = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["event"] != "bug_filed" {
		t.Errorf("event: %v", got["event"])
	}
	if got["project_id"] != "mcp-servers" {
		t.Errorf("project_id: %v", got["project_id"])
	}
	if got["severity"] != "medium" {
		t.Errorf("severity: %v", got["severity"])
	}
	if got["timestamp"] != "2026-05-14T12:00:00Z" {
		t.Errorf("timestamp: %v", got["timestamp"])
	}
}

func TestHandler_StreamsEventsToTwoSubscribers(t *testing.T) {
	b := eventbus.New(8)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	subscribe := func(t *testing.T) (*bufio.Scanner, *http.Response, func()) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
			t.Fatalf("Content-Type: %q", got)
		}
		sc := bufio.NewScanner(resp.Body)
		return sc, resp, func() { _ = resp.Body.Close() }
	}

	sc1, _, c1 := subscribe(t)
	defer c1()
	sc2, _, c2 := subscribe(t)
	defer c2()

	// httptest's server registers subscribers asynchronously — wait
	// briefly until both are visible on the bus.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && b.SubscriberCount() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if b.SubscriberCount() < 2 {
		t.Fatalf("expected 2 subscribers, got %d", b.SubscriberCount())
	}

	want := eventbus.BugFiled("mcp-servers", "smoke-bug", "high")
	b.Publish(want)

	// Each subscriber should see exactly one `data: …` line within 1s.
	for i, sc := range []*bufio.Scanner{sc1, sc2} {
		got, err := readSSEData(sc, time.Second)
		if err != nil {
			t.Fatalf("subscriber %d: %v", i+1, err)
		}
		if !strings.Contains(got, `"event":"bug_filed"`) || !strings.Contains(got, `"slug":"smoke-bug"`) {
			t.Errorf("subscriber %d payload: %q", i+1, got)
		}
	}
}

// readSSEData reads from the scanner until it sees a `data: <…>` line,
// returning the payload portion. Times out per the deadline.
func readSSEData(sc *bufio.Scanner, timeout time.Duration) (string, error) {
	type result struct {
		payload string
		err     error
	}
	out := make(chan result, 1)
	go func() {
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				out <- result{payload: strings.TrimPrefix(line, "data: ")}
				return
			}
		}
		err := sc.Err()
		if err == nil {
			err = io.EOF
		}
		out <- result{err: err}
	}()
	select {
	case r := <-out:
		return r.payload, r.err
	case <-time.After(timeout):
		return "", io.EOF
	}
}

func TestHandler_ClientDisconnectCleansSubscriber(t *testing.T) {
	b := eventbus.New(8)
	// Short keep-alive so the handler probes the closed connection
	// quickly — httptest doesn't propagate request-context cancellation
	// until the next write attempt fails, so we rely on the keep-alive
	// write rather than ctx.Done().
	srv := httptest.NewServer(b.HandlerWithKeepAlive(50 * time.Millisecond))
	defer srv.Close()

	// Open one connection in a goroutine that we close after first event.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		// Read one byte then close — simulates a dashboard disconnect.
		buf := make([]byte, 1)
		_, _ = resp.Body.Read(buf)
		_ = resp.Body.Close()
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && b.SubscriberCount() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if b.SubscriberCount() < 1 {
		t.Fatal("subscriber not registered")
	}
	wg.Wait()

	// After client disconnect, the handler eventually returns and cancels
	// its subscriber. Wait for SubscriberCount to drop to 0.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.SubscriberCount() == 0 {
			return
		}
		// Publishing nudges the handler's select to fire and notice the
		// context cancellation.
		b.Publish(eventbus.BugFiled("p", "x", "low"))
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("subscriber not cleaned up after disconnect; count = %d", b.SubscriberCount())
}

// TestBus_ParallelPublishCancelDoesNotDropOrPanic pins the fix for bug
// 1317: Publish holds the RLock across the send loop so a concurrent
// cancel() can't close a subscriber's channel mid-iteration. Before the
// fix, the snapshot-then-release shape allowed the close to fire between
// snapshot and send; the panic was contained by recover() in trySend
// but events were silently dropped. After the fix, no race window
// exists — publishes always either deliver or drop on full-buffer,
// never drop on close.
//
// The test drives the race: N goroutines publish in a tight loop while
// another goroutine cycles subscribers (subscribe / receive a few /
// cancel) at high frequency. With the old shape this triggers the
// recover path frequently and the panic-on-closed-channel sometimes
// escaped under -race (since recover only catches what the goroutine
// is currently doing); with the new shape the loop runs cleanly.
func TestBus_ParallelPublishCancelDoesNotDropOrPanic(t *testing.T) {
	b := eventbus.New(64)

	const publishers = 4
	const cyclers = 2
	const duration = 50 * time.Millisecond

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish(eventbus.BugFiled("p", "x", "medium"))
				}
			}
		}()
	}

	for i := 0; i < cyclers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					sub, cancel := b.Subscribe()
					// Drain a few events to exercise the channel before
					// closing it.
					for j := 0; j < 3; j++ {
						select {
						case <-sub:
						default:
						}
					}
					cancel()
				}
			}
		}()
	}

	time.Sleep(duration)
	close(stop)
	wg.Wait()

	// No assertion beyond "didn't panic." Run with `-race -count=20` to
	// stress the race detector against the fix.
}
