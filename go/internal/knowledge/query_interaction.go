package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"toolkit/internal/mcpparam"
	"toolkit/internal/telemetry"
)

// RecordQueryInteractionResult is the outcome of recording one click signal.
type RecordQueryInteractionResult struct {
	Recorded         bool   `json:"recorded"`
	InteractionID    int64  `json:"interaction_id,omitempty"`
	GroundingEventID int64  `json:"grounding_event_id,omitempty"`
	Error            string `json:"error,omitempty"`
}

// HandleRecordQueryInteraction records one click-signal (a query_interactions
// row) against the grounding_events row of a search call, identified by that
// call's span_id. It is the owned WRITE path for a client that detects click
// signals from its own transcript — corpos (chain toolkit-decomposition T5),
// porting what the toolkit's grounding-events-processor did for Claude Code from
// the session JSONL. The grounding event is resolved server-side from span_id, so
// the client never needs the DB row id (it only has the span_id off its tool
// result + the hit source_ref).
//
// Params (all required except position): span_id, source_ref, click_kind
// (followed|cited|mentioned|resolved-from), session_id; position (1-based rank of
// the clicked hit). A span with no grounding_event is a soft no-op, not an error.
func HandleRecordQueryInteraction(ctx context.Context, deps Deps, params json.RawMessage) (RecordQueryInteractionResult, error) {
	spanID := mcpparam.String(params, "span_id")
	sourceRef := mcpparam.String(params, "source_ref")
	clickKind := mcpparam.String(params, "click_kind")
	sessionID := mcpparam.String(params, "session_id")
	if spanID == "" || sourceRef == "" || clickKind == "" || sessionID == "" {
		return RecordQueryInteractionResult{Error: "params span_id, source_ref, click_kind, session_id are all required"}, nil
	}
	if deps.Pool == nil {
		return RecordQueryInteractionResult{Error: "no database configured"}, nil
	}

	// Resolve the grounding_event by the search call's span_id (most recent if
	// several share it). A missing event is a soft no-op — the span may not have
	// grounded (a non-search call) — rather than a hard error.
	var geID int64
	err := deps.Pool.DB().QueryRowContext(ctx,
		`SELECT id FROM grounding_events WHERE span_id = ? ORDER BY id DESC LIMIT 1`, spanID).Scan(&geID)
	if errors.Is(err, sql.ErrNoRows) {
		return RecordQueryInteractionResult{Recorded: false, Error: "no grounding_event for span_id"}, nil
	}
	if err != nil {
		return RecordQueryInteractionResult{Error: fmt.Sprintf("lookup grounding_event: %s", err.Error())}, nil
	}

	args := telemetry.InteractionArgs{
		GroundingEventID: geID,
		SourceRef:        sourceRef,
		ClickKind:        telemetry.ClickKind(clickKind),
		SpanID:           spanID,
		SessionID:        sessionID,
	}
	if pos := mcpparam.Int64Opt(params, "position"); pos != nil && *pos > 0 {
		p := int(*pos)
		args.Position = &p
	}

	var interactionID int64
	if werr := deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, e := telemetry.EmitInteraction(ctx, tx, args)
		interactionID = id
		return e
	}); werr != nil {
		return RecordQueryInteractionResult{Error: fmt.Sprintf("emit interaction: %s", werr.Error())}, nil
	}
	return RecordQueryInteractionResult{Recorded: true, InteractionID: interactionID, GroundingEventID: geID}, nil
}
