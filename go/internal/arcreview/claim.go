package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/arcreview/arcparams"
	"toolkit/internal/db"
)

// PendingDecisionsClaimResult is the action's return shape. Claimed
// carries one row per dispatched decision set; the caller (Stop hook)
// formats each row into a system-reminder block on stdout. Empty Claimed
// is the steady-state "nothing to dispatch" outcome.
type PendingDecisionsClaimResult struct {
	Claimed []PendingDecisionsRow `json:"claimed"`
}

// PendingDecisionsRow is one claimed pending-decisions row, decoded back
// into typed FilingDecision / triggers fields for the caller.
type PendingDecisionsRow struct {
	ID              int64            `json:"id"`
	EventID         string           `json:"event_id"`
	TargetSessionID string           `json:"target_session_id"`
	Decisions       []FilingDecision `json:"decisions"`
	Triggers        []string         `json:"triggers"`
	ArcSummary      string           `json:"arc_summary,omitempty"`
	CreatedAt       string           `json:"created_at"`
}

// defaultClaimLimit caps a single claim call when the caller doesn't
// supply one. The Stop hook typically claims everything pending for a
// project, but the limit prevents pathologically large dispatches from
// landing in one system-reminder block.
const defaultClaimLimit = 10

// HandlePendingDecisionsClaim atomically SELECTs the oldest undispatched
// pending_decisions rows for the project and UPDATEs them to dispatched
// in a single tx. The write tx (pool.WithWrite acquires the writer mutex)
// serializes concurrent claims so each row is dispatched exactly once
// even when two Stops for the same project fire within milliseconds —
// see docs/ARC_CLOSE_FILING_REVIEW_SUBSTRATE_LISTENER.md §Q3, §Q4.
//
// Returns an empty Claimed slice (status=ok) when no rows are pending;
// that's the steady-state outcome and is NOT an error.
func HandlePendingDecisionsClaim(ctx context.Context, deps Deps, project string, params json.RawMessage) (PendingDecisionsClaimResult, error) {
	var p arcparams.PendingDecisionsClaimParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return PendingDecisionsClaimResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.SessionID == "" {
		return PendingDecisionsClaimResult{}, fmt.Errorf("session_id is required")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultClaimLimit
	}
	if deps.Pool == nil {
		return PendingDecisionsClaimResult{}, fmt.Errorf("pool not configured")
	}

	rows, err := claimPendingDecisions(ctx, deps.Pool, project, p.SessionID, limit)
	if err != nil {
		return PendingDecisionsClaimResult{}, fmt.Errorf("claim pending_decisions: %w", err)
	}
	if rows == nil {
		rows = []PendingDecisionsRow{}
	}
	return PendingDecisionsClaimResult{Claimed: rows}, nil
}

// claimPendingDecisions runs the SELECT + UPDATE inside one write tx.
// The Pool's write mutex serializes concurrent callers; the in-tx
// SELECT-then-UPDATE plus WHERE dispatched_at IS NULL guarantees
// exactly-once dispatch even across multiple processes (the second
// caller's SELECT returns zero rows because the first's UPDATE
// already-committed flipped dispatched_at).
func claimPendingDecisions(ctx context.Context, pool *db.Pool, project, dispatchSessionID string, limit int) ([]PendingDecisionsRow, error) {
	var claimed []PendingDecisionsRow
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// Bug 945: scope the claim to target_session_id, NOT project alone.
		// Each pending_decisions row is targeted at the session whose arc was
		// reviewed (writePendingDecisions sets target_session_id). A session's
		// drain hook passes its own session_id; it must claim ONLY rows targeted
		// at it. Filtering by project alone let a second concurrent same-project
		// session (multi-agent) drain another session's arc-close decisions and
		// inject them — with the >=0.85 auto-execute directive — into the wrong
		// conversation (cross-session bleed into the shared vault/memory organ).
		rs, err := tx.QueryContext(ctx, `
			SELECT id, event_id, target_session_id, decisions_json, triggers_json, arc_summary, created_at
			FROM pending_decisions
			WHERE project_id = ?
			  AND target_session_id = ?
			  AND dispatched_at IS NULL
			ORDER BY created_at, id
			LIMIT ?
		`, project, dispatchSessionID, limit)
		if err != nil {
			return fmt.Errorf("select: %w", err)
		}
		var ids []int64
		for rs.Next() {
			var (
				row           PendingDecisionsRow
				decisionsJSON string
				triggersJSON  string
				arcSummary    sql.NullString
			)
			if err := rs.Scan(&row.ID, &row.EventID, &row.TargetSessionID, &decisionsJSON, &triggersJSON, &arcSummary, &row.CreatedAt); err != nil {
				_ = rs.Close()
				return fmt.Errorf("scan: %w", err)
			}
			if err := json.Unmarshal([]byte(decisionsJSON), &row.Decisions); err != nil {
				_ = rs.Close()
				return fmt.Errorf("decode decisions_json (id=%d): %w", row.ID, err)
			}
			if err := json.Unmarshal([]byte(triggersJSON), &row.Triggers); err != nil {
				_ = rs.Close()
				return fmt.Errorf("decode triggers_json (id=%d): %w", row.ID, err)
			}
			if arcSummary.Valid {
				row.ArcSummary = arcSummary.String
			}
			claimed = append(claimed, row)
			ids = append(ids, row.ID)
		}
		if err := rs.Close(); err != nil {
			return fmt.Errorf("close rows: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}

		// Build the IN list with the right number of placeholders.
		// db.Args concentrates the stdlib `...any` boundary per the
		// reference/2026-05-15_go-mcp-dispatch-typed-returns-pattern.md
		// pattern — bare `any` is forbidden outside internal/db.
		placeholders := make([]byte, 0, 2*len(ids))
		updateArgs := db.NewArgs().AddString(dispatchSessionID)
		for i, id := range ids {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			updateArgs.AddInt64(id)
		}
		updateSQL := fmt.Sprintf(`
			UPDATE pending_decisions
			SET dispatched_at = datetime('now'),
			    dispatch_session_id = ?
			WHERE id IN (%s)
		`, placeholders)
		if _, err := tx.ExecContext(ctx, updateSQL, updateArgs.Slice()...); err != nil {
			return fmt.Errorf("update: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}
