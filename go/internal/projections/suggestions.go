package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/events"
)

// currentSuggestions folds Suggestion* events into proj_current_suggestions.
// Post-T5-suggestions: payload-only construction. The previous
// "refresh from CRUD" path is deleted. Suggestion ids are assigned at
// fold time via MAX+1 (event-emission order = id order in the live
// system, so replay-from-empty produces byte-identical ids).
//
// FTS5 coupling: suggestions_fts (created by migration 054) is parent-
// driven from the projection per docs/SUBSTRATE_CRUD_RETIREMENT.md §7.
// The fold writes both proj_current_suggestions AND suggestions_fts in
// the same tx.
type currentSuggestions struct{}

func init() { Register(currentSuggestions{}) }

func (currentSuggestions) Name() string      { return "current_suggestions" }
func (currentSuggestions) TableName() string { return "proj_current_suggestions" }

// Fold dispatches on event Type. SuggestionReported INSERTs a new row
// (with MAX+1 id); SuggestionResolved / Reopened / Stamped UPDATE the
// existing row by (project_id, slug).
func (currentSuggestions) Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	if evt.EntityKind != "suggestion" {
		return nil
	}
	if evt.EntityProjectID == nil {
		return fmt.Errorf("suggestion event %s missing entity_project_id", evt.EventID)
	}
	switch evt.Type {
	case "SuggestionReported":
		return foldSuggestionReported(ctx, tx, evt)
	case "SuggestionEdited":
		return foldSuggestionEdited(ctx, tx, evt)
	case "SuggestionResolved":
		return foldSuggestionResolved(ctx, tx, evt)
	case "SuggestionReopened":
		return foldSuggestionReopened(ctx, tx, evt)
	case "SuggestionStamped":
		return foldSuggestionStamped(ctx, tx, evt)
	}
	return nil
}

// RebuildFromEmpty replays Suggestion* events from the events table to
// repopulate proj_current_suggestions. Post-T5-suggestions the CRUD
// table is no longer the snapshot source; events ARE the source.
func (currentSuggestions) RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		WHERE entity_kind = 'suggestion'
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
		if err := (currentSuggestions{}).Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("rebuild suggestion fold %s: %w", evt.EventID, err)
		}
	}
	return rows.Err()
}

func foldSuggestionReported(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.SuggestionReportedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("suggestion %s payload: %w", evt.EventID, err)
	}
	// MAX+1 id assignment. Event order is event_id order (UUIDv7
	// timestamp prefix), which matches CRUD-insert order in the live
	// system, so replay produces the same ids.
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) + 1 FROM proj_current_suggestions`).Scan(&id); err != nil {
		return err
	}
	priority := "medium"
	if p.Priority != nil {
		priority = *p.Priority
	}
	// resolution_note column retired in migration 065 per Phase 4 F2.
	// The value still rides on SuggestionResolved.payload.resolution_note —
	// it's the substrate's source of truth — but the projection no
	// longer caches it. The EventTimeline surfaces it from the event.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proj_current_suggestions (
			slug, project_id, id, title, problem_statement, surface, priority,
			source, acceptance_criteria, constraints, status,
			resolution_kind, routed_chain_slug, routed_task_slug, routed_bug_slug,
			resolved_commit_sha, tags, filed_at, resolved_at, updated_at,
			last_event_id, last_event_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', NULL, '', '', '', NULL, ?, ?, NULL, ?, ?, ?)
		ON CONFLICT(project_id, slug) DO UPDATE SET
			-- id is INTENTIONALLY NOT updated (mirrors the chains fix for bug
			-- chain-create-fold-reassigns-pk-on-conflict-orphaning-tasks): a
			-- suggestion's primary key is stable for its lifetime. A re-fired
			-- SuggestionReported for an existing (project_id, slug) refreshes
			-- content only; the MAX+1 id bound in VALUES is used solely on insert.
			title = excluded.title,
			problem_statement = excluded.problem_statement,
			surface = excluded.surface,
			priority = excluded.priority,
			source = excluded.source,
			acceptance_criteria = excluded.acceptance_criteria,
			constraints = excluded.constraints,
			tags = excluded.tags,
			filed_at = excluded.filed_at,
			updated_at = excluded.updated_at,
			last_event_id = excluded.last_event_id,
			last_event_ts = excluded.last_event_ts`,
		evt.EntitySlug, *evt.EntityProjectID, id,
		p.Title, p.ProblemStatement,
		nullableStringFromPtr(p.Surface), priority,
		nullableStringFromPtr(p.Source),
		acceptanceCriteriaJoined(p.AcceptanceCriteria),
		nullableStringFromPtr(p.Constraints),
		nullableStringFromPtr(p.Tags),
		evt.Ts, evt.Ts,
		evt.EventID, evt.Ts,
	); err != nil {
		return err
	}
	// FTS5 row — parent-driven per docs/SUBSTRATE_CRUD_RETIREMENT.md §7.
	// FTS5 virtual tables don't support UPSERT; use DELETE-then-INSERT.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM suggestions_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("suggestions_fts delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO suggestions_fts (rowid, title, problem_statement) VALUES (?, ?, ?)`,
		id, p.Title, p.ProblemStatement); err != nil {
		return fmt.Errorf("suggestions_fts insert: %w", err)
	}
	return nil
}

// foldSuggestionEdited applies per-column updates from
// payload.updated_values. Mirrors foldBugEdited: each map key is a CRUD
// column name; the value is the new column value as a string (lists
// join to "\n- " strings per the forge convention). Refreshes
// suggestions_fts when title or problem_statement changes. Added by bug
// `forge-edit-on-lifecycle-fields-bypasses-state-machine-and-suggestion-
// edit-broken`.
func foldSuggestionEdited(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.SuggestionEditedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("suggestion %s payload: %w", evt.EventID, err)
	}
	for _, col := range p.UpdatedFields {
		val, ok := p.UpdatedValues[col]
		if !ok {
			// No updated_values entry — can't fold payload-only. The
			// forge_edit path always populates updated_values, so this
			// is forensic (replay of a hand-crafted event). Skip rather
			// than fail.
			continue
		}
		if !isAllowedSuggestionColumn(col) {
			return fmt.Errorf("suggestion edit event %s: unknown column %q", evt.EventID, col)
		}
		// Per-column UPDATE; col is whitelisted above so the dynamic SQL
		// carries no injection risk.
		stmt := fmt.Sprintf(`UPDATE proj_current_suggestions SET %s = ?, updated_at = ?, last_event_id = ?, last_event_ts = ? WHERE project_id = ? AND slug = ?`, col)
		if _, err := tx.ExecContext(ctx, stmt,
			val, evt.Ts, evt.EventID, evt.Ts,
			*evt.EntityProjectID, evt.EntitySlug); err != nil {
			return fmt.Errorf("suggestion edit column %s: %w", col, err)
		}
	}
	if _, titleEdited := p.UpdatedValues["title"]; titleEdited {
		return refreshSuggestionsFTS(ctx, tx, *evt.EntityProjectID, evt.EntitySlug)
	}
	if _, psEdited := p.UpdatedValues["problem_statement"]; psEdited {
		return refreshSuggestionsFTS(ctx, tx, *evt.EntityProjectID, evt.EntitySlug)
	}
	return nil
}

func foldSuggestionResolved(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.SuggestionResolvedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("suggestion %s payload: %w", evt.EventID, err)
	}
	// kind → status mapping. Mirrors work/suggestion.go's resolve handler.
	status := p.Kind
	// resolution_note retired in migration 065 (Phase 4 F2) — the
	// payload field stays in SuggestionResolved events; the projection
	// just no longer caches it.
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_current_suggestions SET
			status = ?, resolution_kind = ?,
			routed_chain_slug = ?, routed_task_slug = ?, routed_bug_slug = ?,
			resolved_commit_sha = ?,
			resolved_at = ?, updated_at = ?,
			last_event_id = ?, last_event_ts = ?
		WHERE project_id = ? AND slug = ?`,
		status, p.Kind,
		nullableStringFromPtr(p.RoutedChainSlug),
		nullableStringFromPtr(p.RoutedTaskSlug),
		nullableStringFromPtr(p.RoutedBugSlug),
		p.CommitSHA,
		evt.Ts, evt.Ts,
		evt.EventID, evt.Ts,
		*evt.EntityProjectID, evt.EntitySlug,
	)
	return err
}

func foldSuggestionReopened(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	// resolution_note retired in migration 065 (Phase 4 F2).
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_current_suggestions SET
			status = 'open', resolution_kind = NULL,
			resolved_at = NULL, resolved_commit_sha = NULL,
			updated_at = ?, last_event_id = ?, last_event_ts = ?
		WHERE project_id = ? AND slug = ?`,
		evt.Ts, evt.EventID, evt.Ts,
		*evt.EntityProjectID, evt.EntitySlug,
	)
	return err
}

func foldSuggestionStamped(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	var p events.SuggestionStampedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return fmt.Errorf("suggestion %s payload: %w", evt.EventID, err)
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE proj_current_suggestions SET
			resolved_commit_sha = ?, updated_at = ?,
			last_event_id = ?, last_event_ts = ?
		WHERE project_id = ? AND slug = ?`,
		p.CommitSHA, evt.Ts, evt.EventID, evt.Ts,
		*evt.EntityProjectID, evt.EntitySlug,
	)
	return err
}

// refreshSuggestionsFTS re-syncs the suggestions_fts virtual table row
// for the suggestion at (project_id, slug). Reads title +
// problem_statement from proj_current_suggestions (post-update state)
// and DELETE-then-INSERTs the FTS row at rowid = id. Mirrors
// refreshBugsFTS.
func refreshSuggestionsFTS(ctx context.Context, tx *sql.Tx, projectID, slug string) error {
	var id int64
	var title, ps string
	if err := tx.QueryRowContext(ctx,
		`SELECT id, title, problem_statement FROM proj_current_suggestions WHERE project_id = ? AND slug = ?`,
		projectID, slug).Scan(&id, &title, &ps); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM suggestions_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("suggestions_fts delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO suggestions_fts (rowid, title, problem_statement) VALUES (?, ?, ?)`,
		id, title, ps); err != nil {
		return fmt.Errorf("suggestions_fts insert: %w", err)
	}
	return nil
}

// isAllowedSuggestionColumn is the closed list of column names
// SuggestionEdited's updated_values map may target. Whitelist-style
// validation keeps the fold's dynamic SQL injection-safe. Mirrors
// isAllowedBugColumn with suggestion-native vocabulary: `priority` (not
// severity), `routed_bug_slug` (not routed_suggestion_slug), no
// qwen_task_id. Lifecycle-owned columns (status, resolution_kind,
// resolution_note, resolved_commit_sha) are deliberately EXCLUDED —
// those transition via suggestion_resolve / _reopen / _stamp, and
// status carries set_by="suggestion_resolve" so forge_edit rejects it
// pre-emit.
func isAllowedSuggestionColumn(col string) bool {
	switch col {
	case "title", "problem_statement", "surface", "priority", "source",
		"acceptance_criteria", "constraints", "tags",
		"routed_chain_slug", "routed_task_slug", "routed_bug_slug":
		return true
	}
	return false
}

// nullableStringFromPtr converts an optional payload string pointer to
// the canonical column representation: empty string for unset (matching
// the bugs / suggestions CRUD default), or the pointed value.
func nullableStringFromPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// acceptanceCriteriaJoined collapses the payload's []string field to
// the column's joined-on-newline-dash representation, matching the
// canonical conversion used by forge create.
func acceptanceCriteriaJoined(items []string) string {
	if len(items) == 0 {
		return ""
	}
	joined := ""
	for i, item := range items {
		if i > 0 {
			joined += "\n- "
		}
		joined += item
	}
	return joined
}
