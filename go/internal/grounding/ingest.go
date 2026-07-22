package grounding

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"toolkit/internal/db"
)

// IngestRequest is the ingest_grounding action's wire payload: the host binary
// parses the transcript (Parse) and POSTs its grounding output here, so the emit +
// projection fold land via the SINGLE-WRITER container instead of a host direct-open
// of the canonical DB (the post-cutover / cross-mount-namespace WAL invariant). The
// project rides in the dispatch envelope, not the params.
//
// Entries' element type is package-private but its fields are exported, so it
// round-trips through JSON unchanged — the binary and the container share this exact
// Go type, so no separate wire schema can drift.
type IngestRequest struct {
	ParentSpanID            string           `json:"parent_span_id"`
	PreserveTranscriptTimes bool             `json:"preserve_transcript_timestamps"`
	Events                  []ProcessedEvent `json:"events"`
	Entries                 []jsonlEntry     `json:"entries"`
}

// HandleIngest runs the emit half (grounding rows + interactions + resolutions, with
// the read-side projection fold) over the binary-parsed events+entries inside one
// container write tx. The container's telemetry fold hook (SetFoldHook at server
// startup) folds the read-side projections in the same tx. Idempotent — re-POSTing
// the same transcript hits the same ON CONFLICT / pre-check guards as the --db path.
func HandleIngest(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (Result, error) {
	if strings.TrimSpace(project) == "" {
		return Result{}, fmt.Errorf("project is required")
	}
	var req IngestRequest
	if len(params) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return Result{}, fmt.Errorf("parse ingest_grounding params: %w", err)
		}
	}
	var r Result
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var e error
		r, e = ProcessParsed(ctx, tx, project, req.ParentSpanID, req.PreserveTranscriptTimes, req.Events, req.Entries)
		return e
	})
	if err != nil {
		return Result{}, fmt.Errorf("ingest_grounding: %w", err)
	}
	return r, nil
}
