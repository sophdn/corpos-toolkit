package refresolve

import (
	"context"
	"database/sql"
	"log/slog"

	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// emitDisciplineSurfacedEvent fires a ParseContextDisciplineSurfaced
// event per parse_context call that ran the intent → discipline
// surfacing pass (chain parse-context-lean-orienting T7). The
// no-surfacings case (the resolver short-circuited on a docs intent
// or IntentNone) does NOT emit — distinct from the work-state event
// shape, where the no-fire case still emits because the fire-rate
// denominator is meaningful per-intent. Here the per-intent fire-rate
// is determined by whether the intent had any mapping entries at
// all, which is a constant per intent shape; the denominator is
// trivially derivable from the ParseContextIntentResolved event.
//
// Same async-emit pattern as the work-state / drift / intent events:
// goroutined with its own write tx so the request goroutine doesn't
// pay the cost.
func emitDisciplineSurfacedEvent(ctx context.Context, deps HandlerDeps, sessionID string, tel DisciplineSurfacingTelemetry) {
	if deps.Pool == nil || sessionID == "" {
		return
	}
	if tel.IntentShape == "" {
		// Resolver short-circuited (intent not in mapping or message
		// empty). Nothing to report.
		return
	}
	payload := events.ParseContextDisciplineSurfacedPayload{
		IntentShape:                       tel.IntentShape,
		DisciplinesSurfaced:               tel.Surfaced,
		DisciplinesSuppressedByDedup:      tel.SuppressedByDedup,
		DisciplinesSuppressedByOptout:     tel.SuppressedByOptOut,
		DisciplinesSuppressedByRecentFire: tel.SuppressedByRecentFire,
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
			obs.L().Warn("refresolve: discipline surfaced event emit failed",
				slog.String("err", txErr.Error()))
		}
	}()
}
