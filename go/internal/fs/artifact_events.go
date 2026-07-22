package fs

import (
	"context"
	"database/sql"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// artifact_events.go is the OPT-IN provenance-stamping path shared by fs.write
// and fs.edit: when a caller asks to record, the committed mutation is emitted
// to the owned event log as an ArtifactWritten / ArtifactEdited event, so
// fs.read provenance mode can later fold per-write intent into a file's history
// (the write half of the write->read provenance loop). The DEFAULT write/edit
// emits nothing — record mode is strictly opt-in and never changes the parity
// output. Emission is fail-open: a pool error or a nil pool yields a nil receipt
// and never fails the underlying mutation, which has already committed.

// ArtifactEvent is the receipt attached to a write/edit result in record mode:
// the event-log id of the emitted artifact event and its type. Omitted entirely
// on a default (non-record) mutation, so the parity output is unchanged.
type ArtifactEvent struct {
	EventID string `json:"event_id"`
	Type    string `json:"type"`
}

// emitArtifact records payload as an artifact event keyed by the file's absolute
// path, carrying intent as the event rationale. Returns nil (not an error) when
// no pool is configured so callers can stay fail-open at the call site.
func emitArtifact(ctx context.Context, pool *db.Pool, slug string, payload events.Payload, intent string) (*ArtifactEvent, error) {
	if pool == nil {
		return nil, nil
	}
	var rationale *string
	if intent != "" {
		rationale = &intent
	}
	var eventID string
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, e := events.Emit(ctx, tx, events.EmitArgs{
			Entity:    events.NewCrossCuttingEntityRef("artifact", slug),
			Payload:   payload,
			Rationale: rationale,
		})
		if e != nil {
			return e
		}
		eventID = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ArtifactEvent{EventID: eventID, Type: payload.EventType()}, nil
}

// maybeEmitWriteArtifact emits an ArtifactWritten event for a just-completed
// write when record mode is requested. Fail-open: any emit error yields a nil
// receipt (the write already committed) so the action still succeeds.
func maybeEmitWriteArtifact(ctx context.Context, pool *db.Pool, p WriteParams, res WriteResult) *ArtifactEvent {
	if !p.Record {
		return nil
	}
	ev, err := emitArtifact(ctx, pool, absPath(res.FilePath), events.ArtifactWrittenPayload{
		FilePath:     absPath(res.FilePath),
		BytesWritten: res.BytesWritten,
		LineCount:    res.LineCount,
		Created:      res.Created,
	}, p.Intent)
	if err != nil {
		return nil
	}
	return ev
}

// maybeEmitEditArtifact emits an ArtifactEdited event for a just-completed edit
// when record mode is requested. Fail-open, exactly like the write path.
func maybeEmitEditArtifact(ctx context.Context, pool *db.Pool, p EditParams, res EditResult) *ArtifactEvent {
	if !p.Record {
		return nil
	}
	ev, err := emitArtifact(ctx, pool, absPath(res.FilePath), events.ArtifactEditedPayload{
		FilePath:     absPath(res.FilePath),
		Replacements: res.Replacements,
		Created:      res.Created,
	}, p.Intent)
	if err != nil {
		return nil
	}
	return ev
}

// maybeEmitMoveArtifact emits an ArtifactMoved event for a just-completed move
// when record mode is requested. Keyed by the destination abs path (where the
// bytes now live). Fail-open, exactly like the write/edit path.
func maybeEmitMoveArtifact(ctx context.Context, pool *db.Pool, p MoveParams, res MoveResult) *ArtifactEvent {
	if !p.Record {
		return nil
	}
	ev, err := emitArtifact(ctx, pool, absPath(res.Dest), events.ArtifactMovedPayload{
		Source:      absPath(res.Source),
		Dest:        absPath(res.Dest),
		IsDir:       res.IsDir,
		CrossDevice: res.CrossDevice,
	}, p.Intent)
	if err != nil {
		return nil
	}
	return ev
}

// maybeEmitRemoveArtifact emits an ArtifactRemoved event for a just-completed
// remove when record mode is requested. Keyed by the removed abs path.
// Fail-open, exactly like the write/edit path.
func maybeEmitRemoveArtifact(ctx context.Context, pool *db.Pool, p RemoveParams, res RemoveResult) *ArtifactEvent {
	if !p.Record {
		return nil
	}
	ev, err := emitArtifact(ctx, pool, absPath(res.FilePath), events.ArtifactRemovedPayload{
		FilePath: absPath(res.FilePath),
		WasDir:   res.WasDir,
	}, p.Intent)
	if err != nil {
		return nil
	}
	return ev
}
