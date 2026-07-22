package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// RegisterSessionResult is the register_session action's return shape.
type RegisterSessionResult struct {
	Status string `json:"status"`
}

// registerSessionParams is the action's input: the Claude Code session id and
// its transcript path. The project rides in the dispatch envelope (not params).
type registerSessionParams struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// HandleRegisterSession UPSERTs one row into session_registry for the project,
// via the container's writer mutex (Pool.WithWrite) — so the Stop hook no longer
// opens the canonical DB file directly (the cross-mount-namespace WAL hazard the
// post-cutover single-writer model forbids; see bug
// wired-stop-hooks-open-db-directly-and-target-stale-mcp-servers-path-and-3000).
// The substrate-side review observer reads session_registry to resolve "which
// transcript do I review when a substrate event lands for project X?"
// (arcreview/observer.go::lookupActiveSession).
//
// Idempotent: ON CONFLICT(session_id) refreshes the row. Returns status=ok.
func HandleRegisterSession(ctx context.Context, deps Deps, project string, params json.RawMessage) (RegisterSessionResult, error) {
	var p registerSessionParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RegisterSessionResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if strings.TrimSpace(p.SessionID) == "" {
		return RegisterSessionResult{}, fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(p.TranscriptPath) == "" {
		return RegisterSessionResult{}, fmt.Errorf("transcript_path is required")
	}

	err := deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `
INSERT INTO session_registry (session_id, project_id, transcript_path, last_active_at, updated_at)
VALUES (?, ?, ?, datetime('now'), datetime('now'))
ON CONFLICT(session_id) DO UPDATE SET
    project_id      = excluded.project_id,
    transcript_path = excluded.transcript_path,
    last_active_at  = excluded.last_active_at,
    updated_at      = excluded.updated_at`,
			p.SessionID, project, p.TranscriptPath)
		return e
	})
	if err != nil {
		return RegisterSessionResult{}, fmt.Errorf("session_registry upsert: %w", err)
	}
	return RegisterSessionResult{Status: "ok"}, nil
}
