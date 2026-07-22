package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// currentMemories folds MemoryWritten events into proj_memories. Memories
// were the only entity kind without a projection (chain substrate-health-
// audit-projections T7); the arc-close dedup loader read them straight off
// the events ledger via json_extract while every sibling kind reads a
// projection. This fold gives memories the same queryable surface.
//
// Keyed by `name` (the memory slug). The auto-memory dir is a single global
// namespace keyed by filename, so the same name re-filed from a different
// project context is ONE memory — the fold is last-write-wins by event ts.
// project_id records the most-recent write's project; filed_at preserves the
// FIRST write's ts (the ON CONFLICT clause deliberately omits filed_at).
//
// No DependsOn: the fold reads no sibling projection table, so the
// alphabetical tie-break in [All] suffices.
type currentMemories struct{}

func init() { Register(currentMemories{}) }

func (currentMemories) Name() string      { return "memories" }
func (currentMemories) TableName() string { return "proj_memories" }

// Fold dispatches on event Type. Only MemoryWritten exists today; the
// switch is tolerant of future MemoryEdited / MemoryDeleted event kinds —
// they fall through to a no-op until a handler is added, rather than
// erroring (per the T7 constraint: accommodate without requiring them).
func (currentMemories) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityKind != "memory" {
		return nil
	}
	switch evt.Type {
	case "MemoryWritten":
		return foldMemoryWritten(ctx, tx, evt)
	}
	// Unknown memory event type (e.g. a future MemoryEdited/MemoryDeleted
	// before its handler lands): no-op. The current row state is whatever
	// the most-recent MemoryWritten produced.
	return nil
}

// RebuildFromEmpty replays MemoryWritten events from the events table to
// repopulate proj_memories. Events ARE the source — there is no CRUD table
// for memories (they live on disk in the vault), so this projection is
// purely event-derived from the start.
func (currentMemories) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'memory'
		ORDER BY ts ASC, event_id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var evt RawEvent
		var entityProjectID, rationale, causedBy sql.NullString
		var payloadStr, relatedStr string
		if err := rows.Scan(&evt.EventID, &evt.Ts, &evt.ActorKind, &evt.ActorID,
			&evt.Type, &evt.EntityKind, &evt.EntitySlug, &entityProjectID,
			&payloadStr, &rationale, &causedBy, &relatedStr,
			&evt.SpanID, &evt.SchemaVersion); err != nil {
			return err
		}
		evt.Payload = json.RawMessage(payloadStr)
		evt.RelatedEntities = json.RawMessage(relatedStr)
		if entityProjectID.Valid {
			s := entityProjectID.String
			evt.EntityProjectID = &s
		}
		if rationale.Valid {
			s := rationale.String
			evt.Rationale = &s
		}
		if causedBy.Valid {
			s := causedBy.String
			evt.CausedByEventID = &s
		}
		if err := (currentMemories{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild memory fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}

// foldMemoryWritten upserts the proj_memories row for the memory named by
// the event. The entity slug is the memory name (== payload.name); the row
// is keyed on it. project_id comes from the envelope — memories may be
// cross-cutting (empty project), so unlike bugs/suggestions the fold does
// NOT require a non-nil entity_project_id; a nil pointer stores the empty
// string (the column default).
func foldMemoryWritten(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.MemoryWrittenPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("memory %s payload: %w", evt.EventID, err)
	}
	projectID := ""
	if evt.EntityProjectID != nil {
		projectID = *evt.EntityProjectID
	}
	// ON CONFLICT updates every column EXCEPT filed_at: filed_at preserves
	// the FIRST write's ts (memory "first filed" time), while last_event_ts
	// tracks the most-recent write — mirroring the bugs projection's
	// filed_at-fixed / last_event_ts-moving split. Replay is ts-ascending,
	// so the first MemoryWritten for a name sets filed_at and later writes
	// leave it untouched.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO proj_memories (
			name, kind, description, body_length_bytes, vault_path,
			project_id, filed_at, last_event_id, last_event_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			kind = excluded.kind,
			description = excluded.description,
			body_length_bytes = excluded.body_length_bytes,
			vault_path = excluded.vault_path,
			project_id = excluded.project_id,
			last_event_id = excluded.last_event_id,
			last_event_ts = excluded.last_event_ts`,
		evt.EntitySlug, p.Kind, p.Description, p.BodyLengthBytes, p.VaultPath,
		projectID, evt.Ts, evt.EventID, evt.Ts,
	)
	return err
}
