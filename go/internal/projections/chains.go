package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// chainStatus folds Chain* and Task* events into proj_chain_status.
// Post-T5-chains (2026-05-21) the fold constructs rows from event
// payload alone; the previous refresh-from-CRUD path is deleted.
// Task-status counts (the denormalised pending/active/blocked/closed/
// cancelled columns) are recomputed by SELECT COUNT(*) FROM
// proj_current_tasks WHERE chain_id = ? — that projection is the
// post-T5 source. Until T5-tasks lands, proj_current_tasks is kept in
// sync via the testutil mirror triggers (tests) and the dual-write
// contract (production); after T5-tasks it becomes event-only.
type chainStatus struct{}

func init() { Register(chainStatus{}) }

func (chainStatus) Name() string      { return "chain_status" }
func (chainStatus) TableName() string { return "proj_chain_status" }

// DependsOn declares the in-fold cross-projection read: every Task*
// event's fold (refreshChainTaskCountsForTaskSlug below) recomputes
// chain_status's denormalised counters from proj_current_tasks. The
// current_tasks projection MUST have applied its fold to the same
// event before chain_status runs, otherwise the recompute captures
// pre-event state. Without this declaration the package's
// alphabetical sort ran chain_status FIRST (c < c, then 'h' < 'u'),
// producing the permanent off-by-one drift pinned by bug
// `proj-chain-status-counters-always-one-task-transition-behind`.
func (chainStatus) DependsOn() []string { return []string{"current_tasks"} }

// Fold dispatches on event Type. ChainCreated INSERTs; ChainEdited
// applies per-column updates from payload.updated_values; ChainClosed
// marks status='closed' + closure_summary. Task* events refresh the
// task-status counts on the parent chain row.
func (chainStatus) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	switch evt.Type {
	case "ChainCreated":
		return foldChainCreated(ctx, tx, evt)
	case "ChainEdited":
		return foldChainEdited(ctx, tx, evt)
	case "ChainClosed":
		return foldChainClosed(ctx, tx, evt)
	}
	if evt.EntityKind == "task" {
		// Task event triggers a task-counts refresh on the parent
		// chain(s). chain_id resolution prefers payload context
		// (TaskCreated.chain_slug / TaskAssignedToChain.{from,to}_chain_slug)
		// because task slugs are NOT globally unique — slug "1" exists
		// across 30+ chains in seed-packet today, so a slug-only lookup
		// arbitrarily picks one chain and refreshes the wrong row. For
		// events that don't carry chain context in payload
		// (TaskTransitioned/Completed/Cancelled/Edited/Stamped/Retired),
		// fall back to looking up every (project_id, slug) match and
		// refreshing each — same fan-out semantics the task fold itself
		// already uses for status updates, so the chain counts stay
		// consistent with the per-task projection state.
		return refreshChainTaskCountsForTaskEvent(ctx, tx, evt)
	}
	return nil
}

// RebuildFromEmpty replays Chain* + Task* events.
func (chainStatus) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'chain' OR entity_kind = 'task'
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
		if err := (chainStatus{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild chain fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}

func foldChainCreated(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityProjectID == nil {
		return fmt.Errorf("chain event %s missing entity_project_id", evt.EventID)
	}
	var p events.ChainCreatedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("chain %s payload: %w", evt.EventID, err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) + 1 FROM proj_chain_status`).Scan(&id); err != nil {
		return err
	}
	// design_decisions column retired in migration 065 per Phase 4 F2.
	// The value still rides on ChainCreated.payload.design_decisions —
	// it's the substrate's source of truth — but the projection no
	// longer caches it. The EventTimeline surfaces it from the event.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO proj_chain_status (
			slug, project_id, id, status, output,
			completion_condition, closure_summary,
			total_tasks, pending, active, blocked, closed, cancelled,
			created_at, updated_at, last_event_id, last_event_ts
		) VALUES (?, ?, ?, 'open', ?, ?, '', 0, 0, 0, 0, 0, 0, ?, ?, ?, ?)
		ON CONFLICT(project_id, slug) DO UPDATE SET
			-- id is INTENTIONALLY NOT updated: the chain's primary key is
			-- stable for its lifetime and child rows (proj_current_tasks.
			-- chain_id) FK-reference it. A re-fired ChainCreated for an
			-- existing (project_id, slug) — an erroneous duplicate forge(chain)
			-- create, or a re-fold — must refresh content only, never the PK.
			-- Reassigning id here to the freshly-computed MAX+1 orphaned every
			-- child task (bug chain-create-fold-reassigns-pk-on-conflict-
			-- orphaning-tasks, observed live 2026-05-25). The MAX+1 id bound in
			-- VALUES is used solely on a genuine insert.
			output = excluded.output,
			completion_condition = excluded.completion_condition,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at,
			last_event_id = excluded.last_event_id,
			last_event_ts = excluded.last_event_ts`,
		evt.EntitySlug, *evt.EntityProjectID, id,
		p.Output, p.CompletionCondition,
		evt.Ts, evt.Ts, evt.EventID, evt.Ts)
	return err
}

func foldChainEdited(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityProjectID == nil {
		return fmt.Errorf("chain event %s missing entity_project_id", evt.EventID)
	}
	var p events.ChainEditedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("chain %s payload: %w", evt.EventID, err)
	}
	for _, col := range p.UpdatedFields {
		val, ok := p.UpdatedValues[col]
		if !ok {
			continue
		}
		if isRetiredChainColumn(col) {
			// design_decisions retired in migration 065 (Phase 4 F2).
			// The payload still flows through ChainEdited events for
			// audit history; the fold just drops it on the floor.
			continue
		}
		if !isAllowedChainColumn(col) {
			return fmt.Errorf("chain edit event %s: unknown column %q", evt.EventID, col)
		}
		stmt := fmt.Sprintf(`UPDATE proj_chain_status SET %s = ?, updated_at = ?, last_event_id = ?, last_event_ts = ? WHERE project_id = ? AND slug = ?`, col)
		if _, err := tx.ExecContext(ctx, stmt,
			val, evt.Ts, evt.EventID, evt.Ts,
			*evt.EntityProjectID, evt.EntitySlug); err != nil {
			return fmt.Errorf("chain edit column %s: %w", col, err)
		}
	}
	return nil
}

// isRetiredChainColumn names columns that ChainEdited may target but
// the projection no longer caches (migration 065, Phase 4 F2). Fold
// drops these silently; the EventTimeline still surfaces the value
// from the event payload.
func isRetiredChainColumn(col string) bool {
	return col == "design_decisions"
}

func foldChainClosed(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityProjectID == nil {
		return fmt.Errorf("chain event %s missing entity_project_id", evt.EventID)
	}
	var p events.ChainClosedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("chain %s payload: %w", evt.EventID, err)
	}
	summary := ""
	if p.ClosureSummary != nil {
		summary = *p.ClosureSummary
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_chain_status SET
			status = 'closed', closure_summary = ?, updated_at = ?,
			last_event_id = ?, last_event_ts = ?
		WHERE project_id = ? AND slug = ?`,
		summary, evt.Ts, evt.EventID, evt.Ts,
		*evt.EntityProjectID, evt.EntitySlug)
	return err
}

// refreshChainTaskCountsForTaskEvent recomputes the denormalised
// task-status counts on every chain affected by a task event. Reads
// from proj_current_tasks (post-T5-tasks source-of-truth).
//
// Chain resolution: task slugs are NOT globally unique within a
// project — slug "1" lives on 30+ chains in seed-packet today. A
// slug-only lookup arbitrarily picks one chain and refreshes the
// wrong row, which is the root cause of the bulk of the surviving
// drift after the DependentProjection fold-ordering fix. We resolve
// the chain set as follows:
//
//  1. TaskCreated / TaskAssignedToChain payloads carry chain_slug
//     (and from_chain_slug for the source on reassignment). Resolve
//     each named chain through proj_chain_status by (project, slug)
//     to get its id. Direct, unambiguous.
//
//  2. Other task events (TaskTransitioned / TaskCompleted /
//     TaskCancelled / TaskEdited / TaskStamped / TaskRetired) carry
//     no chain context. Look up every (project_id, slug) match in
//     proj_current_tasks and refresh each — same fan-out semantics
//     the task fold itself already applies to these events
//     (foldTaskCompleted updates every matching task row), so the
//     chain counts stay consistent with the per-task state. Within
//     a project this typically resolves to one chain; for
//     legacy-data slug collisions it correctly fans out.
//
// Empty result is a no-op (sql.ErrNoRows isn't a fold failure — the
// event simply doesn't reference any chain whose counters are
// currently tracked).
func refreshChainTaskCountsForTaskEvent(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityProjectID == nil {
		// Defensive: task events carry project; without it we can't
		// scope the lookup safely. Skip rather than risk cross-project
		// refresh.
		return nil
	}
	project := *evt.EntityProjectID

	// Collect the chain_id set for this event.
	chainIDs, err := chainIDsForTaskEvent(ctx, tx, project, evt)
	if err != nil {
		return err
	}
	for _, chainID := range chainIDs {
		if err := refreshChainTaskCountsByID(ctx, tx, chainID, evt.EventID, evt.Ts); err != nil {
			return err
		}
	}
	return nil
}

// chainIDsForTaskEvent resolves the chain ids a task event should
// refresh counts on. Payload-context-bearing events use their
// declared chain_slug; everything else fans out by (project, slug).
func chainIDsForTaskEvent(ctx context.Context, tx *sql.Tx, project string, evt RawEvent) ([]int64, error) {
	switch evt.Type {
	case "TaskCreated":
		// payload.chain_slug names the destination chain.
		var p events.TaskCreatedPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			return nil, fmt.Errorf("task %s payload: %w", evt.EventID, err)
		}
		return chainIDBySlug(ctx, tx, project, p.ChainSlug)
	case "TaskAssignedToChain":
		// Reassignment touches both the source (if any) and target.
		var p events.TaskAssignedToChainPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			return nil, fmt.Errorf("task %s payload: %w", evt.EventID, err)
		}
		var out []int64
		if p.FromChainSlug != nil && *p.FromChainSlug != "" {
			ids, err := chainIDBySlug(ctx, tx, project, *p.FromChainSlug)
			if err != nil {
				return nil, err
			}
			out = append(out, ids...)
		}
		ids, err := chainIDBySlug(ctx, tx, project, p.ToChainSlug)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
		return out, nil
	}
	// Fallback: fan-out over every (project, slug) match in
	// proj_current_tasks. The same fan-out the task fold itself uses
	// to update status under the same slug-collision conditions.
	rows, err := tx.QueryContext(ctx,
		`SELECT t.chain_id FROM proj_current_tasks t
		 JOIN proj_chain_status c ON c.id = t.chain_id
		 WHERE t.slug = ? AND c.project_id = ?`,
		evt.EntitySlug, project)
	if err != nil {
		return nil, fmt.Errorf("resolve chain_ids for task event %s: %w", evt.EventID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// chainIDBySlug returns the chain id for (project, slug), or empty
// slice when no row exists (chain may not have folded yet, e.g.
// during a partial replay). Single-row by definition of
// proj_chain_status's (project_id, slug) unique constraint.
func chainIDBySlug(ctx context.Context, tx *sql.Tx, project, slug string) ([]int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
		project, slug).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return []int64{id}, nil
}

// refreshChainTaskCountsByID writes the denormalised
// pending/active/blocked/closed/cancelled/total_tasks counters for
// one chain row from proj_current_tasks live state. Atomic — every
// counter computed from the same snapshot view of proj_current_tasks
// within the surrounding tx.
func refreshChainTaskCountsByID(ctx context.Context, tx *sql.Tx, chainID int64, eventID, eventTs string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_chain_status SET
			total_tasks = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ?),
			pending     = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'pending'),
			active      = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'active'),
			blocked     = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'blocked'),
			closed      = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'closed'),
			cancelled   = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'cancelled'),
			updated_at  = ?,
			last_event_id = ?, last_event_ts = ?
		WHERE id = ?`,
		chainID, chainID, chainID, chainID, chainID, chainID,
		eventTs, eventID, eventTs, chainID)
	return err
}

// isAllowedChainColumn is the closed list of column names ChainEdited's
// updated_values map may target. design_decisions retired in migration
// 065 per Phase 4 F2; ChainEdited events targeting it now no-op (the
// payload still rides through events, just nothing folds into a column
// the projection doesn't have).
func isAllowedChainColumn(col string) bool {
	switch col {
	case "output", "completion_condition", "closure_summary":
		return true
	}
	return false
}
