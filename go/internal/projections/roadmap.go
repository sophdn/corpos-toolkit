package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// roadmapView folds RoadmapUpdated, Chain*, and Task* events into
// proj_roadmap_view. Post-T5-roadmap (2026-05-21) the layout (positions,
// ref membership) is reconstructed from RoadmapUpdated.items payloads;
// the target_status / target_updated_at denormalized columns are
// refreshed by Chain* and Task* events. The previous "refresh from
// roadmap_items CRUD" path is deleted.
type roadmapView struct{}

func init() { Register(roadmapView{}) }

func (roadmapView) Name() string      { return "roadmap_view" }
func (roadmapView) TableName() string { return "proj_roadmap_view" }

// DependsOn declares the in-fold reads roadmap_view performs against
// proj_chain_status and proj_current_tasks (target_status /
// target_updated_at resolution for chain- and task-shaped roadmap
// items; resolveTargetStateOnTransitionForSlug consults both). Pre-
// DependentProjection this ordering held only because 'r' sorts after
// 'c' — a one-letter-alphabetical accident. Declared explicitly so a
// future projection added between c* and r* can't silently break it.
func (roadmapView) DependsOn() []string { return []string{"chain_status", "current_tasks"} }

// Fold dispatches:
//   - RoadmapUpdated.set → DELETE all proj_roadmap_view rows for the
//     project; INSERT new layout from payload.items
//   - RoadmapUpdated.insert → position-shift then INSERT one item
//   - RoadmapUpdated.update → UPDATE one item (PATCH semantics from
//     payload.items[0])
//   - RoadmapUpdated.mark_reassessed → no projection write (audit event)
//   - ChainClosed → DELETE proj_roadmap_view rows ref_kind='chain' AND
//     ref_slug=closed (cascade)
//   - TaskCompleted / TaskCancelled → DELETE proj_roadmap_view rows
//     ref_kind='task' AND ref_slug=closed (cascade)
//   - Other Chain* / Task* (non-terminal) → refresh target_status +
//     target_updated_at on matching proj_roadmap_view rows
func (roadmapView) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	switch evt.Type {
	case "RoadmapUpdated":
		return foldRoadmapUpdated(ctx, tx, evt)
	case "ChainClosed":
		return cascadeDeleteRoadmapRows(ctx, tx, "chain", evt.EntitySlug, evt.EventID, evt.Ts)
	case "TaskCompleted", "TaskCancelled":
		return cascadeDeleteRoadmapRows(ctx, tx, "task", evt.EntitySlug, evt.EventID, evt.Ts)
	}
	// Non-terminal Chain* / Task* events refresh denormalized columns.
	switch evt.EntityKind {
	case "chain":
		return refreshRoadmapRowsForRef(ctx, tx, "chain", evt.EntitySlug, evt.EventID, evt.Ts)
	case "task":
		return refreshRoadmapRowsForRef(ctx, tx, "task", evt.EntitySlug, evt.EventID, evt.Ts)
	}
	return nil
}

// RebuildFromEmpty replays events to repopulate proj_roadmap_view.
// Post-T5-roadmap (2026-05-21) the CRUD `roadmap_items` table is no
// longer the snapshot source.
func (roadmapView) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE type = 'RoadmapUpdated' OR type = 'ChainClosed' OR type = 'TaskCompleted' OR type = 'TaskCancelled'
		   OR (entity_kind IN ('chain', 'task'))
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
		if err := (roadmapView{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild roadmap fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}

func foldRoadmapUpdated(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.RoadmapUpdatedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("roadmap %s payload: %w", evt.EventID, err)
	}
	if evt.EntityProjectID == nil {
		// "mark_reassessed" can emit cross-cutting (no project); other
		// actions land project-scoped. No projection write needed for
		// the cross-cutting case.
		if p.ActionKind == "mark_reassessed" {
			return nil
		}
		return fmt.Errorf("roadmap event %s missing entity_project_id for action_kind=%s", evt.EventID, p.ActionKind)
	}
	projectID := *evt.EntityProjectID
	switch p.ActionKind {
	case "set":
		// Wipe layout for project and re-INSERT from payload.items.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM proj_roadmap_view WHERE project_id = ?`, projectID); err != nil {
			return fmt.Errorf("roadmap set delete: %w", err)
		}
		for _, item := range p.Items {
			if err := insertRoadmapRow(ctx, tx, projectID, item, evt.EventID, evt.Ts); err != nil {
				return err
			}
		}
		return nil
	case "insert":
		if len(p.Items) == 0 {
			return fmt.Errorf("roadmap insert event %s lacks items[]", evt.EventID)
		}
		item := p.Items[0]
		// Position shift: items at >= target shift +1 to make room.
		if _, err := tx.ExecContext(ctx,
			`UPDATE proj_roadmap_view SET position = position + 1
			 WHERE project_id = ? AND position >= ?`, projectID, item.Position); err != nil {
			return fmt.Errorf("roadmap insert position shift: %w", err)
		}
		return insertRoadmapRow(ctx, tx, projectID, item, evt.EventID, evt.Ts)
	case "update":
		if len(p.Items) == 0 {
			return fmt.Errorf("roadmap update event %s lacks items[]", evt.EventID)
		}
		item := p.Items[0]
		// PATCH: build SET clause from non-nil fields. ref_kind / ref_slug
		// always set (the payload's per-item shape requires them).
		if _, err := tx.ExecContext(ctx,
			`UPDATE proj_roadmap_view SET ref_kind = ?, ref_slug = ?,
			   last_event_id = ?, last_event_ts = ?
			 WHERE project_id = ? AND position = ?`,
			item.RefKind, item.RefSlug, evt.EventID, evt.Ts,
			projectID, item.Position); err != nil {
			return fmt.Errorf("roadmap update ref: %w", err)
		}
		if item.Note != nil {
			n := *item.Note
			// "" → NULL in proj_roadmap_view (matches the CRUD-era
			// AddNullableString semantics in HandleRoadmapUpdate).
			var noteVal interface{}
			if n == "" {
				noteVal = nil
			} else {
				noteVal = n
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE proj_roadmap_view SET note = ?
				 WHERE project_id = ? AND position = ?`,
				noteVal, projectID, item.Position); err != nil {
				return fmt.Errorf("roadmap update note: %w", err)
			}
		}
		if item.ChainSlug != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE proj_roadmap_view SET chain_slug = ?
				 WHERE project_id = ? AND position = ?`,
				*item.ChainSlug, projectID, item.Position); err != nil {
				return fmt.Errorf("roadmap update chain_slug: %w", err)
			}
		}
		// Refresh target_status / target_updated_at after a ref change.
		return refreshRoadmapRowsForRef(ctx, tx, item.RefKind, item.RefSlug, evt.EventID, evt.Ts)
	case "mark_reassessed":
		return nil
	}
	return nil
}

// insertRoadmapRow INSERTs one proj_roadmap_view row from an event item.
// Resolves target_status / target_updated_at from proj_chain_status /
// proj_current_tasks (those projections are kept current by their own
// fold paths; for entity kinds not yet flipped, the test-time mirror
// triggers keep them in sync with the CRUD source).
func insertRoadmapRow(ctx context.Context, tx *sql.Tx, projectID string, item events.RoadmapItemPayload, eventID, eventTs string) error {
	var targetStatus, targetUpdatedAt sql.NullString
	switch item.RefKind {
	case "chain":
		_ = tx.QueryRowContext(ctx,
			`SELECT status, updated_at FROM proj_chain_status WHERE slug = ?`, item.RefSlug).
			Scan(&targetStatus, &targetUpdatedAt)
	case "task":
		_ = tx.QueryRowContext(ctx,
			`SELECT status, updated_at FROM proj_current_tasks WHERE slug = ?`, item.RefSlug).
			Scan(&targetStatus, &targetUpdatedAt)
	}
	var chainSlug, note interface{}
	if item.ChainSlug != nil {
		chainSlug = *item.ChainSlug
	}
	if item.Note != nil {
		note = *item.Note
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO proj_roadmap_view (
			project_id, position, ref_kind, ref_slug, chain_slug, note,
			target_status, target_updated_at, last_event_id, last_event_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, item.Position, item.RefKind, item.RefSlug, chainSlug, note,
		targetStatus, targetUpdatedAt, eventID, eventTs)
	return err
}

// cascadeDeleteRoadmapRows removes proj_roadmap_view rows referencing
// an entity that's been closed. Mirrors the cascade DELETE the writers
// used to do in work/task.go and work/chain.go pre-T5-roadmap.
func cascadeDeleteRoadmapRows(ctx context.Context, tx *sql.Tx, refKind, refSlug, eventID, eventTs string) error {
	_ = eventID
	_ = eventTs
	_, err := tx.ExecContext(ctx,
		`DELETE FROM proj_roadmap_view WHERE ref_kind = ? AND ref_slug = ?`,
		refKind, refSlug)
	return err
}

// refreshRoadmapRowsForRef updates target_status + target_updated_at on
// every proj_roadmap_view row referencing the supplied (ref_kind,
// ref_slug). For an event on a chain not currently on any roadmap,
// the UPDATE touches zero rows — a no-op. Layout columns (position,
// ref_kind, ref_slug, chain_slug, note) are NOT touched here.
func refreshRoadmapRowsForRef(ctx context.Context, tx *sql.Tx, refKind, refSlug, eventID, eventTs string) error {
	const updateSQL = `
		UPDATE proj_roadmap_view
		SET target_status = (
		        SELECT CASE WHEN ? = 'chain' THEN (SELECT status FROM proj_chain_status WHERE slug = ?)
		                    WHEN ? = 'task'  THEN (SELECT status FROM proj_current_tasks WHERE slug = ?) END),
		    target_updated_at = (
		        SELECT CASE WHEN ? = 'chain' THEN (SELECT updated_at FROM proj_chain_status WHERE slug = ?)
		                    WHEN ? = 'task'  THEN (SELECT updated_at FROM proj_current_tasks WHERE slug = ?) END),
		    last_event_id = ?,
		    last_event_ts = ?
		WHERE ref_kind = ? AND ref_slug = ?`
	_, err := tx.ExecContext(ctx, updateSQL,
		refKind, refSlug, refKind, refSlug,
		refKind, refSlug, refKind, refSlug,
		eventID, eventTs,
		refKind, refSlug)
	return err
}

// RefreshRoadmapLayoutForProject is the pre-T5-roadmap helper that
// re-snapshots proj_roadmap_view from roadmap_items CRUD. Post-flip
// the CRUD table is no longer the source of truth; this function is
// preserved as a no-op stub so callers (T5 split's not-yet-flipped
// surfaces) keep compiling. Will be deleted entirely in T6 alongside
// the CRUD table drop.
func RefreshRoadmapLayoutForProject(ctx context.Context, tx *sql.Tx, projectID string) error {
	_ = ctx
	_ = tx
	_ = projectID
	return nil
}
