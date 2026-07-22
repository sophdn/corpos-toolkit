package eventbus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DefaultKeepAliveInterval matches the Rust observe-http handler's 30s
// ping. Dashboards behind aggressive reverse proxies need periodic data
// to keep the SSE channel alive; an empty SSE comment line works as
// keep-alive without polluting the event stream.
const DefaultKeepAliveInterval = 30 * time.Second

// Handler returns an http.Handler that streams events from the bus as
// Server-Sent Events using DefaultKeepAliveInterval.
func (b *Bus) Handler() http.Handler {
	return b.HandlerWithKeepAlive(DefaultKeepAliveInterval)
}

// HandlerWithKeepAlive is Handler with a configurable keep-alive interval.
// Tests use a sub-second interval so connection-close detection doesn't
// have to wait the full 30s for the next write attempt to surface the
// closed-connection error.
//
// Per-connection lifecycle:
//
//  1. Subscribe to the bus.
//  2. Set the SSE response headers and flush.
//  3. Loop: receive event → write "data: <json>\n\n" → flush.
//  4. On context cancellation (client disconnect), write failure, or
//     bus close, return.
//
// The http.ResponseWriter must implement http.Flusher; modern net/http
// satisfies this through the underlying conn (HTTP/1.1 and HTTP/2).
func (b *Bus) HandlerWithKeepAlive(interval time.Duration) http.Handler {
	if interval <= 0 {
		interval = DefaultKeepAliveInterval
	}
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

		ch, cancel := b.Subscribe()
		defer cancel()

		keepAlive := time.NewTicker(interval)
		defer keepAlive.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				payload, err := json.Marshal(event)
				var werr error
				if err != nil {
					_, werr = fmt.Fprintf(w, "data: {\"error\":\"serialize: %s\"}\n\n", err.Error())
				} else {
					_, werr = fmt.Fprintf(w, "data: %s\n\n", payload)
				}
				if werr != nil {
					return
				}
				flusher.Flush()
			case <-keepAlive.C:
				if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}
