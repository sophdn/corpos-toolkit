package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// ghost is a rejected record event captured as a persistent fumble record —
// the in-memory shape insertGhost writes to the ghosts table (migration 084).
type ghost struct {
	SpanID         string
	SessionID      string
	ProjectID      *string
	AttemptedType  string
	EntityKind     string
	EntitySlug     string
	Reason         string
	RewritePayload json.RawMessage
}

// insertGhost persists one rejected record event to the ghosts table inside
// the record transaction, and — when a session is present — also writes a
// pending_decisions row so the ghost surfaces to the operating agent at the
// next Stop via the EXISTING pending_decisions → Stop-hook seam (reuse, not
// a new mechanism; EMIT_SURFACE_PHASE2 §5). The pending_decisions row is its
// own independent row carrying a single ghost decision; the arc-close
// consumer tolerates a decision whose action it doesn't auto-file (it leaves
// it staged), which is exactly right — a ghost is for the agent to REWRITE,
// not to auto-execute.
func insertGhost(ctx context.Context, tx *sql.Tx, g ghost) error {
	rewrite := g.RewritePayload
	if len(rewrite) == 0 {
		rewrite = json.RawMessage("{}")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ghosts
			(span_id, session_id, project_id, attempted_type, entity_kind, entity_slug, reason, rewrite_payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		g.SpanID, nullIfEmpty(g.SessionID), g.ProjectID, g.AttemptedType,
		nullIfEmpty(g.EntityKind), nullIfEmpty(g.EntitySlug), g.Reason, string(rewrite),
	); err != nil {
		return fmt.Errorf("insert ghost: %w", err)
	}

	// Surface via the pending_decisions seam only when anchored to a session
	// — an unanchored ghost (test / non-session context) is still recorded +
	// counted, it just isn't pushed at anyone's Stop.
	if g.SessionID == "" {
		return nil
	}
	decisionsJSON, err := json.Marshal([]ghostDecision{{
		Action:     "rewrite_rejected_record",
		Payload:    rewrite,
		Confidence: 1.0,
		Reasoning:  fmt.Sprintf("record(%s) on %s/%s was rejected: %s", g.AttemptedType, g.EntityKind, g.EntitySlug, g.Reason),
	}})
	if err != nil {
		return fmt.Errorf("marshal ghost decision: %w", err)
	}
	triggersJSON := []byte(`["record-rejection"]`)
	arcSummary := fmt.Sprintf("record rejected a %s event; rewrite or drop it", g.AttemptedType)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pending_decisions
			(event_id, project_id, target_session_id, decisions_json, triggers_json, arc_summary, authoring_state)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"ghost-"+g.SpanID, g.ProjectID, g.SessionID, string(decisionsJSON), string(triggersJSON), arcSummary, "staged",
	); err != nil {
		return fmt.Errorf("insert ghost pending_decision: %w", err)
	}
	return nil
}

// ghostDecision is the FilingDecision-compatible JSON shape a ghost takes in
// pending_decisions.decisions_json. Encoded directly (not via the arcreview
// package) to avoid a work→arcreview import; the field tags match
// arcreview.FilingDecision so the existing claim/fallback consumer unmarshals
// it cleanly. The "rewrite_rejected_record" action is intentionally outside
// the arc-close auto-file enum, so the consumer surfaces it for the agent
// rather than auto-executing.
type ghostDecision struct {
	Action     string          `json:"action"`
	Payload    json.RawMessage `json:"payload"`
	Confidence float64         `json:"confidence"`
	Reasoning  string          `json:"reasoning"`
}

// GhostFumbleCount is one row of the rejection / fumble projection: how many
// times a given event shape was rejected by the record surface.
type GhostFumbleCount struct {
	AttemptedType string `json:"attempted_type"`
	Count         int    `json:"count"`
}

// GhostFumbleCounts is the rejection / fumble projection (EMIT_SURFACE_PHASE2
// §6): per-attempted-type rejection counts over the ghosts table — the
// "rejection count per shape" half of the forge-shape-liveness gap (the
// "success count per shape" half lives in the entity projections). A count
// query over a direct-write table, not an event fold, so it never risks the
// entity projections.
func GhostFumbleCounts(ctx context.Context, pool *db.Pool) ([]GhostFumbleCount, error) {
	rows, err := pool.DB().QueryContext(ctx,
		`SELECT attempted_type, COUNT(*) FROM ghosts GROUP BY attempted_type ORDER BY COUNT(*) DESC, attempted_type ASC`)
	if err != nil {
		return nil, fmt.Errorf("query ghost fumble counts: %w", err)
	}
	defer rows.Close()
	var out []GhostFumbleCount
	for rows.Next() {
		var c GhostFumbleCount
		if err := rows.Scan(&c.AttemptedType, &c.Count); err != nil {
			return nil, fmt.Errorf("scan ghost fumble count: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// nullIfEmpty maps "" → SQL NULL so optional ghost columns store NULL rather
// than empty strings.
func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// ghostSessionFromCtx pulls the MCP session id for anchoring a ghost. Thin
// wrapper so the record handler reads clearly.
func ghostSessionFromCtx(ctx context.Context) string {
	return events.MCPSessionIDFromContext(ctx)
}
