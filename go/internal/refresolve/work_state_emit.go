package refresolve

import (
	"context"
	"database/sql"
	"log/slog"

	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// emitWorkStateEvent fires a ParseContextWorkStateSurfaced event per
// parse_context call that ran the work-state resolver (chain
// parse-context-lean-orienting T6). The no-surfacings case (zero
// counts) still emits so T10's measurement gets a fire-rate
// denominator. Same async-emit pattern as the drift / intent events
// — goroutined with its own write tx so the request goroutine
// doesn't pay the cost.
//
// No-op when the pool is absent (test driver, smoke boot), the
// session id is empty, or the resolver short-circuited (telemetry
// IntentShape unset — happens on docs/none intents).
func emitWorkStateEvent(ctx context.Context, deps HandlerDeps, sessionID string, intent IntentShape, tel WorkStateTelemetry) {
	if deps.Pool == nil || sessionID == "" {
		return
	}
	if tel.IntentShape == "" {
		// Resolver short-circuited (intent not in firing set OR no
		// project resolved). Nothing to report — the absence is
		// itself queryable via ParseContextIntentResolved.
		return
	}
	payload := events.ParseContextWorkStateSurfacedPayload{
		IntentShape: tel.IntentShape,
		BugsCount:   tel.BugsCount,
		TasksCount:  tel.TasksCount,
		ChainsCount: tel.ChainsCount,
		CacheHit:    tel.CacheHit,
		ProjectID:   tel.ProjectID,
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
			obs.L().Warn("refresolve: work-state event emit failed",
				slog.String("err", txErr.Error()))
		}
	}()
}
