package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"toolkit/internal/events"
)

// EnvLogFormat is the env var read by [InitFromEnv] to pick the slog
// handler format. Set to "text" for human-readable development output;
// any other value (including unset) selects "json", the canonical daemon
// shape.
const EnvLogFormat = "TOOLKIT_LOG_FORMAT"

// logger is set once at [Init] and read concurrently from handlers.
// Stored via atomic.Pointer so a re-init from tests is safe without a
// per-call mutex.
var logger atomic.Pointer[slog.Logger]

// Init configures the package-level slog handler. Idempotent — re-calls
// replace the previous logger so tests can swap handlers cleanly between
// table cases.
//
// format ∈ {"json", "text"}. Anything other than "text" selects JSON,
// the canonical daemon shape. The handler writes to os.Stderr; stdout is
// reserved for MCP JSON-RPC frames over the stdio transport.
func Init(format string) {
	var h slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	logger.Store(slog.New(h))
}

// InitFromEnv reads [EnvLogFormat] and calls [Init] accordingly. Used by
// main() at startup; tests call [Init] directly with an explicit format.
func InitFromEnv() {
	Init(os.Getenv(EnvLogFormat))
}

// L returns the bare package logger without ctx-derived attrs. Use this
// at server init time, before the dispatcher mints a request span — once
// a request is in flight, prefer [Logger] so the span context is folded
// into every entry.
func L() *slog.Logger {
	if lg := logger.Load(); lg != nil {
		return lg
	}
	// Lazy default — Init not yet called (init-time race, test forgot to
	// call Init, etc.). Pick JSON so the daemon shape is preserved.
	Init("")
	return logger.Load()
}

// Logger returns the package logger with span_id, parent_span_id, and
// trace_id attrs pulled from ctx. Use this inside handlers — every log
// line auto-carries the span context, and a downstream join across the
// events table and the log stream is just `WHERE span_id = ?`.
//
// When ctx has no span (init-time emit, pre-T5 caller), the returned
// logger is the bare [L] result; the span attrs simply don't appear.
func Logger(ctx context.Context) *slog.Logger {
	base := L()
	span := SpanFromContext(ctx)
	if span == nil {
		return base
	}
	return base.With(
		slog.String("span_id", span.ID),
		slog.String("parent_span_id", span.ParentID),
		slog.String("trace_id", span.TraceID),
	)
}

// Fatalf writes a final error log entry through the structured handler
// and exits the process with code 1. Use at server init time when an
// unrecoverable startup condition is detected (DB open failure, rubrics
// load failure, etc.). Preserves log structure where stdlib log.Fatalf
// would emit a plain stderr line that breaks JSON parsers on the
// receiving side.
//
// Format mirrors fmt.Sprintf so the migration from log.Fatalf is
// mechanical.
func Fatalf(format string, args ...any) {
	L().Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Span is one frame in a span tree. The ID is what gets stamped onto
// every event emitted and every log entry written while the span is
// active. ParentID is the span this one was born under (empty for the
// root request span). TraceID matches the root span's ID — for in-
// process spans this is always the same as the root span_id.
//
// Stored on ctx as an immutable pointer; child spans construct a new
// Span and a new ctx via [WithSpan] rather than mutating the parent.
type Span struct {
	ID       string
	ParentID string
	TraceID  string
	Name     string
	Start    time.Time
}

type ctxKey int

const spanCtxKey ctxKey = 0

// SpanFromContext returns the active span on ctx, or nil if none.
func SpanFromContext(ctx context.Context) *Span {
	if s, ok := ctx.Value(spanCtxKey).(*Span); ok {
		return s
	}
	return nil
}

// WithSpan attaches s to ctx for downstream [Logger] / [SpanFromContext]
// reads. Also stamps [events.WithSpanID] so the events substrate emits
// rows under the same span — handlers therefore only need to thread
// the obs-attached ctx; the events package picks up the same id without
// a second ctx mutation.
//
// Passing nil clears the obs span but leaves the events span_id in place
// (it remains whatever the previous frame set it to). The clear-nil
// shape is reserved for tests; production callers always attach a real
// Span.
func WithSpan(ctx context.Context, s *Span) context.Context {
	ctx = context.WithValue(ctx, spanCtxKey, s)
	if s != nil {
		ctx = events.WithSpanID(ctx, s.ID)
	}
	return ctx
}

// SpanStart opens a new span named `name`. If a parent span exists on
// ctx, the new span links to it via ParentID and shares TraceID;
// otherwise the new span is a root and TraceID == ID.
//
// Standard usage:
//
//	ctx, end := obs.SpanStart(ctx, "knowledge.vault_search")
//	defer end(nil)
//
// EndFn must be called exactly once. Pass a non-nil error to mark the
// span as failed — the span_close event records the message and the
// status flips to "error". Returning the original ctx (not the new one
// the caller threaded internally) is fine for end-time bookkeeping;
// the span pointer is captured by the closure.
//
// Crypto/rand failures on UUIDv4 minting are degraded gracefully: the
// span is treated as absent (ctx returns unchanged, end is a no-op).
// The error path logs at error level so the operator sees the kernel
// /dev/urandom outage; the calling handler continues without spans
// rather than erroring out.
func SpanStart(ctx context.Context, name string) (context.Context, func(error)) {
	id, err := newUUIDv4()
	if err != nil {
		Logger(ctx).Error("obs: failed to mint span id", slog.String("err", err.Error()))
		return ctx, func(error) {}
	}
	parent := SpanFromContext(ctx)
	span := &Span{
		ID:    id,
		Name:  name,
		Start: time.Now().UTC(),
	}
	if parent != nil {
		span.ParentID = parent.ID
		span.TraceID = parent.TraceID
	} else {
		span.TraceID = span.ID
	}
	newCtx := WithSpan(ctx, span)
	publishSpanOpen(newCtx, span)
	end := func(endErr error) {
		d := time.Since(span.Start)
		status := "ok"
		if endErr != nil {
			status = "error"
		}
		publishSpanClose(newCtx, span, d, status, endErr)
	}
	return newCtx, end
}

// newUUIDv4 returns a fresh random UUIDv4. Mirrors events.newUUIDv4
// shape; reimplemented here so obs has no upward dependency on events
// internals.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	b[6] = (b[6] & 0x0F) | 0x40 // version 4
	b[8] = (b[8] & 0x3F) | 0x80 // RFC 4122 variant
	hexStr := hex.EncodeToString(b[:])
	return hexStr[0:8] + "-" + hexStr[8:12] + "-" + hexStr[12:16] + "-" + hexStr[16:20] + "-" + hexStr[20:], nil
}
