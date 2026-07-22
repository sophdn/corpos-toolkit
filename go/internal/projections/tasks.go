package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// currentTasks folds Task* events into proj_current_tasks. Post-T5-tasks
// (2026-05-21) the fold constructs rows from event payload alone.
// Task ids assigned via MAX+1 (event-emission order = id order). The
// chain_id is resolved from proj_chain_status by chain_slug at fold time.
type currentTasks struct{}

func init() { Register(currentTasks{}) }

func (currentTasks) Name() string      { return "current_tasks" }
func (currentTasks) TableName() string { return "proj_current_tasks" }

func (currentTasks) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityKind != "task" {
		return nil
	}
	if evt.EntityProjectID == nil {
		return fmt.Errorf("task event %s missing entity_project_id", evt.EventID)
	}
	switch evt.Type {
	case "TaskCreated":
		return foldTaskCreated(ctx, tx, evt)
	case "TaskEdited":
		return foldTaskEdited(ctx, tx, evt)
	case "TaskTransitioned":
		return foldTaskTransitioned(ctx, tx, evt)
	case "TaskCompleted":
		return foldTaskCompleted(ctx, tx, evt)
	case "TaskCancelled":
		return foldTaskCancelled(ctx, tx, evt)
	case "TaskRetired":
		return foldTaskRetired(ctx, tx, evt)
	case "TaskAssignedToChain":
		return foldTaskAssignedToChain(ctx, tx, evt)
	case "TaskStamped":
		return foldTaskStamped(ctx, tx, evt)
	}
	return nil
}

func (currentTasks) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	return replayTaskEvents(ctx, tx, currentTasks{})
}

func foldTaskCreated(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.TaskCreatedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("task %s payload: %w", evt.EventID, err)
	}
	// Resolve chain_id from proj_chain_status by chain_slug.
	var chainID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
		*evt.EntityProjectID, p.ChainSlug).Scan(&chainID); err != nil {
		if err == sql.ErrNoRows {
			return nil // chain not yet folded; skip
		}
		return fmt.Errorf("task %s chain lookup: %w", evt.EventID, err)
	}
	// MAX+1 id assignment.
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) + 1 FROM proj_current_tasks`).Scan(&id); err != nil {
		return err
	}
	position := 0
	if p.Position != nil {
		position = *p.Position
	} else {
		// Default: max position within chain + 1
		var maxPos sql.NullInt64
		_ = tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(position), 0) FROM proj_current_tasks WHERE chain_id = ?`,
			chainID).Scan(&maxPos)
		position = int(maxPos.Int64) + 1
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO proj_current_tasks (
			id, chain_id, slug, position, status, problem_statement,
			acceptance_criteria, context_required, constraints, handoff_output,
			originated_chain_id, moved_on, commit_sha, created_at, updated_at,
			last_event_id, last_event_ts
		) VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, NULL, NULL, NULL, ?, ?, ?, ?)
		ON CONFLICT(chain_id, slug) DO UPDATE SET
			-- id is INTENTIONALLY NOT updated (mirrors the chains fix for bug
			-- chain-create-fold-reassigns-pk-on-conflict-orphaning-tasks). A
			-- task's primary key is stable for its lifetime; task_blockers,
			-- commit stamps, and external references point at it. A re-fired
			-- TaskCreated for an existing (chain_id, slug) — duplicate forge or
			-- a re-fold — must refresh content only, never reassign the PK to
			-- the freshly-computed MAX+1 (which is used solely on a genuine
			-- insert). Reassigning it orphaned every referrer in the chains case.
			position = excluded.position,
			problem_statement = excluded.problem_statement,
			acceptance_criteria = excluded.acceptance_criteria,
			context_required = excluded.context_required,
			constraints = excluded.constraints,
			handoff_output = excluded.handoff_output,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at,
			last_event_id = excluded.last_event_id,
			last_event_ts = excluded.last_event_ts`,
		id, chainID, evt.EntitySlug, position,
		p.ProblemStatement,
		acceptanceCriteriaJoined(p.AcceptanceCriteria),
		nullableStringFromPtr(p.ContextRequired),
		nullableStringFromPtr(p.Constraints),
		nullableStringFromPtr(p.HandoffOutput),
		evt.Ts, evt.Ts,
		evt.EventID, evt.Ts)
	return err
}

func foldTaskEdited(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.TaskEditedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("task %s payload: %w", evt.EventID, err)
	}
	// Map input-key aliases (handoff_output_append) to actual column
	// names (handoff_output). The append op's pre-computed final value
	// is in the map under the input-key form; we apply it to the column.
	for _, col := range p.UpdatedFields {
		val, ok := p.UpdatedValues[col]
		if !ok {
			continue
		}
		column := resolveTaskEditColumn(col)
		if column == "" {
			continue // unknown input alias; skip silently
		}
		stmt := fmt.Sprintf(`UPDATE proj_current_tasks SET %s = ?, updated_at = ?, last_event_id = ?, last_event_ts = ?
			WHERE `+taskTargetWhere(p.ChainSlug), column)
		var err error
		if p.ChainSlug != "" {
			_, err = tx.ExecContext(ctx, stmt,
				val, evt.Ts, evt.EventID, evt.Ts,
				evt.EntitySlug, *evt.EntityProjectID, p.ChainSlug)
		} else {
			_, err = tx.ExecContext(ctx, stmt,
				val, evt.Ts, evt.EventID, evt.Ts,
				evt.EntitySlug, *evt.EntityProjectID)
		}
		if err != nil {
			return fmt.Errorf("task edit column %s: %w", column, err)
		}
	}
	return nil
}

func resolveTaskEditColumn(inputKey string) string {
	switch inputKey {
	case "handoff_output_append":
		return "handoff_output"
	case "problem_statement", "acceptance_criteria", "context_required",
		"constraints", "handoff_output", "position":
		return inputKey
	}
	return ""
}

// taskTargetWhere returns the WHERE fragment a task lifecycle fold uses to
// target the single task its event refers to. With a non-empty chainSlug it
// scopes to exactly that chain — preventing fanout across same-slug tasks in
// other chains (bug `task-lifecycle-event-folds-fan-out-across-duplicate-
// task-slugs`). With an empty chainSlug it falls back to the legacy
// slug+project match so pre-disambiguation events replay faithfully (the
// historical fanout is reproduced, then corrected by chain-scoped
// compensating events).
func taskTargetWhere(chainSlug string) string {
	if chainSlug != "" {
		return "slug = ? AND chain_id = (SELECT id FROM proj_chain_status WHERE project_id = ? AND slug = ?)"
	}
	return "slug = ? AND chain_id IN (SELECT id FROM proj_chain_status WHERE project_id = ?)"
}

func foldTaskTransitioned(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.TaskTransitionedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("task %s payload: %w", evt.EventID, err)
	}
	// TaskTransitioned only carries non-terminal states (pending/active/
	// blocked) — a non-closed task must not retain a commit_sha. Clearing it
	// here also undoes a stale SHA left by a fanned-out completion before a
	// reopen (bug `task-lifecycle-event-folds-fan-out-across-duplicate-task-
	// slugs`). from→to preserves the L1181 blocked→blocked self-transition.
	q := `UPDATE proj_current_tasks SET status = ?, commit_sha = NULL, updated_at = ?,
			last_event_id = ?, last_event_ts = ?
		 WHERE ` + taskTargetWhere(p.ChainSlug)
	if p.ChainSlug != "" {
		_, err := tx.ExecContext(ctx, q,
			p.ToStatus, evt.Ts, evt.EventID, evt.Ts,
			evt.EntitySlug, *evt.EntityProjectID, p.ChainSlug)
		return err
	}
	_, err := tx.ExecContext(ctx, q,
		p.ToStatus, evt.Ts, evt.EventID, evt.Ts,
		evt.EntitySlug, *evt.EntityProjectID)
	return err
}

func foldTaskCompleted(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.TaskCompletedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("task %s payload: %w", evt.EventID, err)
	}
	sha := ""
	if p.CommitSHA != nil {
		sha = *p.CommitSHA
	}
	closure := ""
	if p.ClosureSummary != nil {
		closure = *p.ClosureSummary
	}
	q := `UPDATE proj_current_tasks SET status = 'closed', commit_sha = ?, handoff_output = ?,
			updated_at = ?, last_event_id = ?, last_event_ts = ?
		 WHERE ` + taskTargetWhere(p.ChainSlug)
	if p.ChainSlug != "" {
		_, err := tx.ExecContext(ctx, q,
			sha, closure, evt.Ts, evt.EventID, evt.Ts,
			evt.EntitySlug, *evt.EntityProjectID, p.ChainSlug)
		return err
	}
	_, err := tx.ExecContext(ctx, q,
		sha, closure, evt.Ts, evt.EventID, evt.Ts,
		evt.EntitySlug, *evt.EntityProjectID)
	return err
}

func foldTaskCancelled(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.TaskCancelledPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("task %s payload: %w", evt.EventID, err)
	}
	q := `UPDATE proj_current_tasks SET status = 'cancelled',
			updated_at = ?, last_event_id = ?, last_event_ts = ?
		 WHERE ` + taskTargetWhere(p.ChainSlug)
	if p.ChainSlug != "" {
		_, err := tx.ExecContext(ctx, q,
			evt.Ts, evt.EventID, evt.Ts,
			evt.EntitySlug, *evt.EntityProjectID, p.ChainSlug)
		return err
	}
	_, err := tx.ExecContext(ctx, q,
		evt.Ts, evt.EventID, evt.Ts,
		evt.EntitySlug, *evt.EntityProjectID)
	return err
}

// foldTaskRetired removes the task row entirely from proj_current_tasks
// (and any proj_task_blockers edges referencing it), distinct from
// TaskCancelled which keeps the row at status='cancelled'. Used by the
// migration-062 synthetic-event backfill to retire the 6 phantom
// flip-write-contract-* task slugs that the pre-8f2cb87 buggy forge-
// task-delete path removed from CRUD without emitting an event.
// Refreshes proj_chain_status counters so the chain's task count stays
// consistent with the post-retire projection state.
func foldTaskRetired(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	// Resolve the task's id + chain_id before we delete the row so the
	// blocker-edge cleanup and counter refresh can target them.
	var taskID, chainID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT t.id, t.chain_id FROM proj_current_tasks t
		   JOIN proj_chain_status c ON c.id = t.chain_id
		  WHERE t.slug = ? AND c.project_id = ?`,
		evt.EntitySlug, *evt.EntityProjectID).Scan(&taskID, &chainID); err != nil {
		if err == sql.ErrNoRows {
			return nil // already retired or never folded; idempotent
		}
		return fmt.Errorf("task %s retire lookup: %w", evt.EventID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM proj_task_blockers
		  WHERE blocked_task_id = ? OR blocker_task_id = ?`,
		taskID, taskID); err != nil {
		return fmt.Errorf("task %s retire blocker cleanup: %w", evt.EventID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM proj_current_tasks WHERE id = ?`, taskID); err != nil {
		return fmt.Errorf("task %s retire delete: %w", evt.EventID, err)
	}
	// Refresh the parent chain's task-status counters since one row
	// just disappeared from the chain's set.
	if _, err := tx.ExecContext(ctx,
		`UPDATE proj_chain_status SET
		   total_tasks = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ?),
		   pending     = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'pending'),
		   active      = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'active'),
		   blocked     = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'blocked'),
		   closed      = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'closed'),
		   cancelled   = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'cancelled'),
		   last_event_id = ?, last_event_ts = ?
		 WHERE id = ?`,
		chainID, chainID, chainID, chainID, chainID, chainID,
		evt.EventID, evt.Ts, chainID); err != nil {
		return fmt.Errorf("task %s retire counter refresh: %w", evt.EventID, err)
	}
	return nil
}

func foldTaskAssignedToChain(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.TaskAssignedToChainPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("task %s payload: %w", evt.EventID, err)
	}
	var toChainID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
		*evt.EntityProjectID, p.ToChainSlug).Scan(&toChainID); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("task %s to_chain lookup: %w", evt.EventID, err)
	}
	pos := 0
	if p.ToPosition != nil {
		pos = *p.ToPosition
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_current_tasks SET chain_id = ?, position = ?,
			moved_on = ?, updated_at = ?,
			last_event_id = ?, last_event_ts = ?
		 WHERE slug = ?`,
		toChainID, pos, evt.Ts, evt.Ts, evt.EventID, evt.Ts, evt.EntitySlug)
	return err
}

func foldTaskStamped(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.TaskStampedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("task %s payload: %w", evt.EventID, err)
	}
	q := `UPDATE proj_current_tasks SET commit_sha = ?, updated_at = ?,
			last_event_id = ?, last_event_ts = ?
		 WHERE ` + taskTargetWhere(p.ChainSlug)
	if p.ChainSlug != "" {
		_, err := tx.ExecContext(ctx, q,
			p.CommitSHA, evt.Ts, evt.EventID, evt.Ts,
			evt.EntitySlug, *evt.EntityProjectID, p.ChainSlug)
		return err
	}
	_, err := tx.ExecContext(ctx, q,
		p.CommitSHA, evt.Ts, evt.EventID, evt.Ts,
		evt.EntitySlug, *evt.EntityProjectID)
	return err
}

func isAllowedTaskColumn(col string) bool {
	switch col {
	case "problem_statement", "acceptance_criteria", "context_required",
		"constraints", "handoff_output", "position":
		return true
	}
	return false
}

// taskBlockers folds task_blockers edges from TaskTransitioned events.
type taskBlockers struct{}

func init() { Register(taskBlockers{}) }

func (taskBlockers) Name() string      { return "task_blockers" }
func (taskBlockers) TableName() string { return "proj_task_blockers" }

// DependsOn declares the in-fold reads task_blockers performs against
// proj_chain_status (scoping the blocked-task lookup by project) and
// proj_current_tasks (joining edges to the task rows being folded).
// Before [DependentProjection] landed this ordering held only by
// alphabetical accident — 't' sorts after 'c'. Declaring it explicitly
// pins the contract so a future rename or a sibling projection added
// between c* and t* can't silently break it.
func (taskBlockers) DependsOn() []string { return []string{"chain_status", "current_tasks"} }

// Fold maintains the invariant that proj_task_blockers contains only
// edges where BOTH endpoints are open (status in pending/active/blocked).
// The contract:
//   - TaskTransitioned with blocker_slug → INSERT edge (T3's L1181 guard lift)
//   - TaskTransitioned with removed_blocker_slug → DELETE edge
//   - TaskCompleted / TaskCancelled / TaskRetired on any task → DELETE
//     every edge referencing that task on either side. This mirrors the
//     pre-T5 cleanupBlockersAfterClose CRUD-write that disappeared when
//     the production write path flipped to event-only at T5-tasks
//     (7128e48); without this cleanup, rebuild-from-empty leaves
//     orphan edges to closed/cancelled/retired tasks that don't exist
//     in the live system.
func (taskBlockers) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityKind != "task" {
		return nil
	}
	switch evt.Type {
	case "TaskTransitioned":
		return foldTaskBlockersTransitioned(ctx, tx, evt)
	case "TaskCompleted", "TaskCancelled", "TaskRetired":
		return foldTaskBlockersCleanupOnClose(ctx, tx, evt)
	}
	return nil
}

// resolveTaskIDForBlockerFold resolves a task id by slug, scoped to chainSlug
// when it is non-empty (the anti-fanout disambiguation — bug
// `task-blocker-edge-fold-resolves-by-project-slug-ignoring-chain-fans-out`).
// The bare (project_id, slug) query the blocker folds used previously returns
// an UNSPECIFIED row when a slug recurs across chains (generic chain-step slugs
// do), so the edge INSERT/DELETE could land on the wrong chain's same-slug task.
// Empty chainSlug falls back to the legacy (project_id, slug) match so
// pre-disambiguation events (which carried no chain_slug) replay faithfully —
// preserving rebuild parity.
func resolveTaskIDForBlockerFold(ctx context.Context, tx *sql.Tx, projectID, chainSlug, slug string) (int64, error) {
	var id int64
	var err error
	if chainSlug != "" {
		err = tx.QueryRowContext(ctx,
			`SELECT t.id FROM proj_current_tasks t JOIN proj_chain_status c ON c.id = t.chain_id
			 WHERE c.project_id = ? AND c.slug = ? AND t.slug = ?`,
			projectID, chainSlug, slug).Scan(&id)
	} else {
		err = tx.QueryRowContext(ctx,
			`SELECT t.id FROM proj_current_tasks t JOIN proj_chain_status c ON c.id = t.chain_id
			 WHERE c.project_id = ? AND t.slug = ?`,
			projectID, slug).Scan(&id)
	}
	return id, err
}

func foldTaskBlockersTransitioned(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityProjectID == nil {
		return fmt.Errorf("task event %s missing entity_project_id", evt.EventID)
	}
	var p events.TaskTransitionedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("task %s payload: %w", evt.EventID, err)
	}
	// Resolve the blocked task chain-scoped by the event's chain_slug so the
	// edge mutation targets this chain's task, not a same-slug sibling.
	blockedID, err := resolveTaskIDForBlockerFold(ctx, tx, *evt.EntityProjectID, p.ChainSlug, evt.EntitySlug)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if p.BlockerSlug != nil {
		var blockerID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT t.id FROM proj_current_tasks t JOIN proj_chain_status c ON c.id = t.chain_id
			 WHERE c.project_id = ? AND t.slug = ?`,
			*evt.EntityProjectID, *p.BlockerSlug).Scan(&blockerID); err == nil {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO proj_task_blockers (
					blocked_task_id, blocker_task_id, reason, created_at,
					last_event_id, last_event_ts
				) VALUES (?, ?, '', ?, ?, ?)`,
				blockedID, blockerID, evt.Ts, evt.EventID, evt.Ts); err != nil {
				return err
			}
		}
	}
	if p.RemovedBlockerSlug != nil {
		var removedID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT t.id FROM proj_current_tasks t JOIN proj_chain_status c ON c.id = t.chain_id
			 WHERE c.project_id = ? AND t.slug = ?`,
			*evt.EntityProjectID, *p.RemovedBlockerSlug).Scan(&removedID); err == nil {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM proj_task_blockers WHERE blocked_task_id = ? AND blocker_task_id = ?`,
				blockedID, removedID); err != nil {
				return err
			}
		}
	}
	return nil
}

func foldTaskBlockersCleanupOnClose(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityProjectID == nil {
		return nil
	}
	// Resolve the closing task's id, then delete every edge that
	// references it on either side. ErrNoRows is fine — task may not
	// be in the projection yet (rebuild order) or already retired.
	// Chain-scope the resolution by the close payload's chain_slug
	// (TaskCompleted/Cancelled/Retired all carry it) so we delete THIS
	// task's edges, not a same-slug sibling's. Empty → legacy fallback
	// (rebuild parity for pre-disambiguation events).
	var cs struct {
		ChainSlug string `json:"chain_slug"`
	}
	_ = json.Unmarshal(evt.Payload, &cs)
	closingID, err := resolveTaskIDForBlockerFold(ctx, tx, *evt.EntityProjectID, cs.ChainSlug, evt.EntitySlug)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM proj_task_blockers
		  WHERE blocked_task_id = ? OR blocker_task_id = ?`,
		closingID, closingID); err != nil {
		return fmt.Errorf("task %s blocker cleanup-on-close: %w", evt.EventID, err)
	}
	return nil
}

func (taskBlockers) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	return replayTaskEvents(ctx, tx, taskBlockers{})
}

// replayTaskEvents drives one of the task-side projections through all
// task events in event_id order. Shared by currentTasks and taskBlockers
// RebuildFromEmpty.
type taskFolder interface {
	Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error
}

func replayTaskEvents(ctx context.Context, tx *sql.Tx, folder taskFolder) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'task'
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
		if err := folder.Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild task fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}
