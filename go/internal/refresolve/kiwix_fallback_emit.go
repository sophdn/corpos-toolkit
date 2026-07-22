package refresolve

import (
	"context"
	"database/sql"
	"log/slog"

	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// emitKiwixFallbackEvent fires a ParseContextKiwixFallbackFired event
// per parse_context call that EVALUATED the low-confidence kiwix
// fallback gate (chain parse-context-lean-orienting T8). Fires on
// BOTH the fire branch and every suppress branch so T10's measurement
// has a fire-rate denominator and a suppress-mix breakdown.
//
// No-op when tel.IntentShape is empty — that's the sentinel for "the
// gate didn't evaluate" (degraded boot with no search injected,
// pool/session absent). Distinct from a recorded suppress, which
// always carries an intent shape and a SuppressedReason.
//
// Same async-emit pattern as the work-state / discipline / drift
// events: detached goroutine with its own write tx + parent-span
// carry-through so the request goroutine doesn't pay the tx cost.
func emitKiwixFallbackEvent(ctx context.Context, deps HandlerDeps, sessionID string, tel KiwixFallbackTelemetry) {
	if deps.Pool == nil || sessionID == "" {
		return
	}
	if tel.IntentShape == "" {
		return
	}
	payload := events.ParseContextKiwixFallbackFiredPayload{
		IntentShape:          tel.IntentShape,
		Fired:                tel.Fired,
		SuppressedReason:     tel.SuppressedReason,
		CandidatesReturned:   tel.CandidatesReturned,
		KiwixSearchLatencyMs: tel.KiwixSearchLatencyMs,
	}
	parentSpan := obs.SpanFromContext(ctx)
	asyncCtx := context.Background()
	if parentSpan != nil {
		asyncCtx = obs.WithSpan(asyncCtx, parentSpan)
	}
	go func() {
		txErr := deps.Pool.WithWrite(asyncCtx, func(tx *sql.Tx) error {
			_, emitErr := events.Emit(asyncCtx, tx, events.EmitArgs{
				Entity:  events.NewCrossCuttingEntityRef("parse_context_session", sessionID),
				Payload: payload,
			})
			return emitErr
		})
		if txErr != nil {
			obs.L().Warn("refresolve: kiwix fallback event emit failed",
				slog.String("err", txErr.Error()))
		}
	}()
}
