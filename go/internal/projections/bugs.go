package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// currentBugs folds Bug* events into proj_current_bugs. Post-T5-bugs
// (2026-05-21) the fold constructs rows from event payload alone;
// the previous refresh-from-CRUD path is deleted. Bug ids are
// assigned at fold time via MAX+1 (event-emission order = id order
// in the live system, so replay-from-empty produces byte-identical
// ids). bugs_fts (parent-driven from the fold per design doc §7)
// is upserted alongside the projection row using DELETE-then-INSERT
// since FTS5 virtual tables don't support UPSERT.
type currentBugs struct{}

func init() { Register(currentBugs{}) }

func (currentBugs) Name() string      { return "current_bugs" }
func (currentBugs) TableName() string { return "proj_current_bugs" }

// Fold dispatches on event Type. BugReported INSERTs a new row; the
// lifecycle events (Resolved / Reopened / Stamped / Triaged) UPDATE
// the existing row; BugEdited applies a per-column UPDATE from
// payload.updated_values (T3's additive bump).
func (currentBugs) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityKind != "bug" {
		return nil
	}
	if evt.EntityProjectID == nil {
		return fmt.Errorf("bug event %s missing entity_project_id", evt.EventID)
	}
	switch evt.Type {
	case "BugReported":
		return foldBugReported(ctx, tx, evt)
	case "BugEdited":
		return foldBugEdited(ctx, tx, evt)
	case "BugResolved":
		return foldBugResolved(ctx, tx, evt)
	case "BugReopened":
		return foldBugReopened(ctx, tx, evt)
	case "BugStamped":
		return foldBugStamped(ctx, tx, evt)
	case "BugTriaged":
		return foldBugTriaged(ctx, tx, evt)
	}
	return nil
}

// RebuildFromEmpty replays Bug* events from the events table.
func (currentBugs) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'bug'
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
		if err := (currentBugs{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild bug fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}

func foldBugReported(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.BugReportedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("bug %s payload: %w", evt.EventID, err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) + 1 FROM proj_current_bugs`).Scan(&id); err != nil {
		return err
	}
	severity := "medium"
	if p.Severity != nil {
		severity = *p.Severity
	}
	// resolution_note column retired in migration 065 per Phase 4 F2.
	// The value still rides on BugResolved.payload.resolution_note —
	// it's the substrate's source of truth — but the projection no
	// longer caches it. The EventTimeline surfaces it from the event.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proj_current_bugs (
			slug, project_id, id, title, problem_statement, surface, severity,
			source, acceptance_criteria, constraints, status,
			resolution_kind, routed_chain_slug, routed_task_slug, routed_suggestion_slug,
			resolved_commit_sha, qwen_task_id, tags, filed_at, resolved_at, updated_at,
			last_event_id, last_event_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', NULL, '', '', ?, NULL, ?, ?, ?, NULL, ?, ?, ?)
		ON CONFLICT(project_id, slug) DO UPDATE SET
			-- id is INTENTIONALLY NOT updated (mirrors the chains fix for bug
			-- chain-create-fold-reassigns-pk-on-conflict-orphaning-tasks): a
			-- bug's primary key is stable for its lifetime. A re-fired
			-- BugReported for an existing (project_id, slug) refreshes content
			-- only; the MAX+1 id bound in VALUES is used solely on insert.
			title = excluded.title,
			problem_statement = excluded.problem_statement,
			surface = excluded.surface,
			severity = excluded.severity,
			source = excluded.source,
			acceptance_criteria = excluded.acceptance_criteria,
			constraints = excluded.constraints,
			routed_suggestion_slug = excluded.routed_suggestion_slug,
			qwen_task_id = excluded.qwen_task_id,
			tags = excluded.tags,
			filed_at = excluded.filed_at,
			updated_at = excluded.updated_at,
			last_event_id = excluded.last_event_id,
			last_event_ts = excluded.last_event_ts`,
		evt.EntitySlug, *evt.EntityProjectID, id,
		p.Title, p.ProblemStatement,
		nullableStringFromPtr(p.Surface), severity,
		nullableStringFromPtr(p.Source),
		acceptanceCriteriaJoined(p.AcceptanceCriteria),
		nullableStringFromPtr(p.Constraints),
		nullableStringFromPtr(p.RoutedSuggestionSlug),
		nullableQwenTaskIDFromPtr(p.QwenTaskID),
		nullableStringFromPtr(p.Tags),
		evt.Ts, evt.Ts,
		evt.EventID, evt.Ts,
	); err != nil {
		return err
	}
	// FTS5 row — parent-driven per docs/SUBSTRATE_CRUD_RETIREMENT.md §7.
	// DELETE-then-INSERT pattern: FTS5 virtual tables don't support UPSERT.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM bugs_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("bugs_fts delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO bugs_fts (rowid, title, problem_statement) VALUES (?, ?, ?)`,
		id, p.Title, p.ProblemStatement); err != nil {
		return fmt.Errorf("bugs_fts insert: %w", err)
	}
	return nil
}

// foldBugEdited applies per-column updates from payload.updated_values
// (T3's additive bump). Each map key is a CRUD column name; the value
// is the new column value as a string (lists join to "\n- " strings
// per the forge convention). Refreshes bugs_fts when title or
// problem_statement changes.
func foldBugEdited(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.BugEditedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("bug %s payload: %w", evt.EventID, err)
	}
	for _, col := range p.UpdatedFields {
		val, ok := p.UpdatedValues[col]
		if !ok {
			// Pre-T3 events lack updated_values; we can't fold them
			// payload-only. Live system pre-T5 has no remaining pre-T3
			// events for any active bug, so this branch is forensic
			// (rebuild-from-empty replay of a very-old event). Skip
			// rather than fail — the bug row's final state will be
			// reconstructed by the most-recent BugEdited that DOES
			// carry updated_values.
			continue
		}
		if !isAllowedBugColumn(col) {
			return fmt.Errorf("bug edit event %s: unknown column %q", evt.EventID, col)
		}
		// Per-column UPDATE; the column-name is whitelisted above so no
		// injection risk in the dynamic SQL.
		stmt := fmt.Sprintf(`UPDATE proj_current_bugs SET %s = ?, updated_at = ?, last_event_id = ?, last_event_ts = ? WHERE project_id = ? AND slug = ?`, col)
		if _, err := tx.ExecContext(ctx, stmt,
			val, evt.Ts, evt.EventID, evt.Ts,
			*evt.EntityProjectID, evt.EntitySlug); err != nil {
			return fmt.Errorf("bug edit column %s: %w", col, err)
		}
	}
	// Refresh bugs_fts row if title or problem_statement was edited.
	if _, titleEdited := p.UpdatedValues["title"]; titleEdited {
		return refreshBugsFTS(ctx, tx, *evt.EntityProjectID, evt.EntitySlug)
	}
	if _, psEdited := p.UpdatedValues["problem_statement"]; psEdited {
		return refreshBugsFTS(ctx, tx, *evt.EntityProjectID, evt.EntitySlug)
	}
	return nil
}

func foldBugResolved(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.BugResolvedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("bug %s payload: %w", evt.EventID, err)
	}
	// COALESCE(resolved_at, ?) preserves the first non-null value, so
	// re-routes don't overwrite the original resolved_at timestamp.
	// resolution_note retired in migration 065 (Phase 4 F2) — the
	// payload field stays in BugResolved events; the projection just
	// no longer caches it.
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_current_bugs SET
			status = ?, resolution_kind = ?,
			routed_chain_slug = ?, routed_task_slug = ?, routed_suggestion_slug = ?,
			resolved_commit_sha = ?,
			resolved_at = COALESCE(resolved_at, ?),
			updated_at = ?,
			last_event_id = ?, last_event_ts = ?
		WHERE project_id = ? AND slug = ?`,
		p.Kind, p.Kind,
		nullableStringFromPtr(p.RoutedChainSlug),
		nullableStringFromPtr(p.RoutedTaskSlug),
		nullableStringFromPtr(p.RoutedSuggestionSlug),
		p.CommitSHA,
		evt.Ts, evt.Ts,
		evt.EventID, evt.Ts,
		*evt.EntityProjectID, evt.EntitySlug,
	)
	return err
}

func foldBugReopened(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	// Note: resolved_commit_sha is intentionally NOT cleared, matching
	// the pre-T5 handler's UPDATE which set resolution_kind=NULL and
	// resolved_at=NULL but left resolved_commit_sha alone.
	// resolution_note retired in migration 065 (Phase 4 F2).
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_current_bugs SET
			status = 'open', resolution_kind = NULL,
			routed_chain_slug = '', routed_task_slug = '', routed_suggestion_slug = '',
			resolved_at = NULL,
			updated_at = ?, last_event_id = ?, last_event_ts = ?
		WHERE project_id = ? AND slug = ?`,
		evt.Ts, evt.EventID, evt.Ts,
		*evt.EntityProjectID, evt.EntitySlug,
	)
	return err
}

func foldBugStamped(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.BugStampedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("bug %s payload: %w", evt.EventID, err)
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_current_bugs SET
			resolved_commit_sha = ?, updated_at = ?,
			last_event_id = ?, last_event_ts = ?
		WHERE project_id = ? AND slug = ?`,
		p.CommitSHA, evt.Ts, evt.EventID, evt.Ts,
		*evt.EntityProjectID, evt.EntitySlug,
	)
	return err
}

func foldBugTriaged(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.BugTriagedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("bug %s payload: %w", evt.EventID, err)
	}
	// Only update columns whose to_* pointers are set (triage event may
	// carry severity-only or tags-only changes).
	if p.ToSeverity != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE proj_current_bugs SET severity = ?, updated_at = ?,
			   last_event_id = ?, last_event_ts = ?
			 WHERE project_id = ? AND slug = ?`,
			*p.ToSeverity, evt.Ts, evt.EventID, evt.Ts,
			*evt.EntityProjectID, evt.EntitySlug); err != nil {
			return err
		}
	}
	if p.ToTags != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE proj_current_bugs SET tags = ?, updated_at = ?,
			   last_event_id = ?, last_event_ts = ?
			 WHERE project_id = ? AND slug = ?`,
			*p.ToTags, evt.Ts, evt.EventID, evt.Ts,
			*evt.EntityProjectID, evt.EntitySlug); err != nil {
			return err
		}
	}
	return nil
}

// refreshBugsFTS re-syncs the bugs_fts virtual table row for the bug
// at (project_id, slug). Reads title + problem_statement from
// proj_current_bugs (post-update state) and DELETE-then-INSERTs the
// FTS row at rowid = id.
func refreshBugsFTS(ctx context.Context, tx *sql.Tx, projectID, slug string) error {
	var id int64
	var title, ps string
	if err := tx.QueryRowContext(ctx,
		`SELECT id, title, problem_statement FROM proj_current_bugs WHERE project_id = ? AND slug = ?`,
		projectID, slug).Scan(&id, &title, &ps); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM bugs_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("bugs_fts delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO bugs_fts (rowid, title, problem_statement) VALUES (?, ?, ?)`,
		id, title, ps); err != nil {
		return fmt.Errorf("bugs_fts insert: %w", err)
	}
	return nil
}

// isAllowedBugColumn is the closed list of column names BugEdited's
// updated_values map may target. Whitelist-style validation prevents
// the fold's dynamic SQL from being injection-shaped.
func isAllowedBugColumn(col string) bool {
	switch col {
	case "title", "problem_statement", "surface", "severity", "source",
		"acceptance_criteria", "constraints", "tags",
		"qwen_task_id", "routed_chain_slug", "routed_task_slug",
		"routed_suggestion_slug":
		return true
	}
	return false
}

// nullableQwenTaskIDFromPtr emits the qwen_task_id column value: NULL
// when the pointer is nil OR points to an empty string (matches the
// CRUD column's nullable semantics).
func nullableQwenTaskIDFromPtr(p *string) interface{} {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}
