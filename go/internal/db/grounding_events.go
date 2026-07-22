package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// GroundingEventInsert is the Go-side payload mirroring the Rust
// shared_db::GroundingEventInsert shape, extended with span_id (added
// in migration 034_grounding_events_span_id.sql).
//
// The Go-side insert path is the online emit fired at handler exit by
// vault_search / kiwix_search / knowledge_search; the Rust offline
// processor (benchmarks/src/bin/knowledge_grounding_processor.rs)
// continues to populate next_turn_has_output and used in a separate
// post-hoc pass over MCP session logs. Both writers respect the
// (session_id, call_id) unique constraint via ON CONFLICT DO NOTHING.
//
// SpanID is the per-MCP-request id minted by the dispatcher (see
// internal/obs.SpanStart). When present, it joins grounding_events
// rows to events rows by SHARED span_id — the load-bearing seam for
// the sibling chain query-telemetry-substrate's read-write join.
type GroundingEventInsert struct {
	ProjectID         string
	SessionID         string
	CallID            string
	Action            string
	ResultsCount      int64
	SourceRefs        []string
	NextTurnHasOutput bool
	// Used is nil for unevaluated rows (the online path emits this
	// shape — usage approximation runs offline against subsequent
	// agent text); the offline processor fills it in.
	Used   *bool
	SpanID string

	// New columns from migration 037 (query-telemetry-substrate TT2).
	// All are NULLABLE except QuerySource which has a CHECK + default.

	// PromptID is the transcript JSONL `promptId` for the user-input
	// arc this row belongs to. Stop-hook stamped post-session; the
	// online emit leaves it nil and the hook fills it in.
	PromptID *string

	// ParentSpanID is the parent agent's span_id when this row was
	// emitted by a sidechain (subagent) call. Stop-hook stamped from
	// transcript isSidechain=true records; online emit nil.
	ParentSpanID *string

	// QuerySource is the discriminator separating agent-initiated
	// queries from hook-initiated. Empty string lets the migration
	// column DEFAULT 'agent_initiated' take effect.
	QuerySource string

	// UserMessageID is the transcript UUID of the user message that
	// opened the prompt this query belongs to. Set by the dispatcher
	// when known (proactive hooks always know; agent-initiated rarely);
	// otherwise nil and the column stays NULL.
	UserMessageID *string

	// QueryText is the raw query string the user/agent issued.
	// Populated at the search-call site from the action's params.query
	// field. Nil-safe for legacy callers that don't pass it yet.
	QueryText *string

	// Per-handler telemetry absorbed from the retired per-handler tables in
	// migration 046 (chain telemetry-substrate-cleanup T2). All nullable;
	// only the action-specific ones get populated:
	//   vault_search → Pass1LatencyMS, Pass2LatencyMS
	//   kiwix_search → QwenFellBack, KiwixHitsIn, KiwixHitsOut
	//   knowledge_search → none
	//
	// These are ONLINE-EMIT-ONLY runtime measurements: the live handler computes
	// them at call time. The grounding-events-processor reconstructs rows from
	// transcript JSONL, which never carried them, so a BACKFILLED row leaves them
	// NULL by construction. So `pass1_latency_ms IS NULL` reliably marks a
	// backfilled (non-online-emit) row, not a failed reranker — latency analyses
	// should filter to `IS NOT NULL` (the complete online subset). The online
	// emit always sets Pass1LatencyMS (knowledge.HandleVaultSearch, pinned by
	// TestHandleVaultSearch_WritesTelemetryRow). See bug 959.

	Pass1LatencyMS *int64
	Pass2LatencyMS *int64
	QwenFellBack   *bool
	KiwixHitsIn    *int64
	KiwixHitsOut   *int64

	// CreatedAt overrides the column's `datetime('now')` default. Nil
	// keeps the default (the online emit path leaves it nil; SQLite
	// stamps the row at insert time). Non-nil forces the override —
	// used by the grounding-events-processor's
	// --preserve-transcript-timestamps mode to backfill historical
	// rows with their original tool_use times so the
	// /inference/health-cards success predicate's ±5s proximity to
	// inference_invocations.created_at can find a match instead of dating
	// every backfilled row to "now."
	CreatedAt *string
}

// InsertGroundingEvent appends one row. Silently ignores duplicate
// (session_id, call_id) pairs so re-emitting the same call (re-tries,
// processor + handler racing on the same MCP call) does not fail —
// matches the Rust insert helper's shape.
//
// Always wraps in a write tx via the pool. The caller is the handler
// at search-exit; failure must not block the search response, so the
// caller logs and drops.
func InsertGroundingEvent(ctx context.Context, pool *Pool, ev GroundingEventInsert) error {
	refsJSON, err := json.Marshal(ev.SourceRefs)
	if err != nil {
		return fmt.Errorf("marshal source_refs: %w", err)
	}
	if ev.SourceRefs == nil {
		// json.Marshal(nil slice) yields "null"; the table default is
		// the literal "[]" string. Pre-normalize to keep the column
		// shape consistent across writers.
		refsJSON = []byte("[]")
	}
	var used sql.NullInt64
	if ev.Used != nil {
		used.Valid = true
		if *ev.Used {
			used.Int64 = 1
		}
	}
	// query_source has a NOT NULL DEFAULT 'agent_initiated' at the
	// schema level; pass empty string here when the caller didn't set
	// it and switch to the DEFAULT-via-explicit-default form so the
	// column type stays uniform across callers.
	querySource := ev.QuerySource
	if querySource == "" {
		querySource = "agent_initiated"
	}
	qwenFellBack := optBoolToNullInt(ev.QwenFellBack)
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO grounding_events
				(project_id, session_id, call_id, action,
				 results_count, source_refs, next_turn_has_output, used, span_id,
				 prompt_id, parent_span_id, query_source, user_message_id, query_text,
				 pass1_latency_ms, pass2_latency_ms, qwen_fell_back, kiwix_hits_in, kiwix_hits_out,
				 created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, datetime('now')))
			ON CONFLICT (session_id, call_id) DO NOTHING`,
			ev.ProjectID, ev.SessionID, ev.CallID, ev.Action,
			ev.ResultsCount, string(refsJSON), boolToInt(ev.NextTurnHasOutput),
			used, ev.SpanID,
			ev.PromptID, ev.ParentSpanID, querySource, ev.UserMessageID, ev.QueryText,
			ev.Pass1LatencyMS, ev.Pass2LatencyMS, qwenFellBack, ev.KiwixHitsIn, ev.KiwixHitsOut,
			ev.CreatedAt,
		)
		return err
	})
}

// InsertGroundingEventTx inserts one row inside an existing transaction
// and returns the row's id. Used by the post-session processor which
// needs the id to fan out per-row query_interactions writes in the same
// tx via [telemetry.EmitInteraction]. On ON CONFLICT DO NOTHING (the
// session+call_id pair already landed), the helper re-fetches the
// pre-existing id by SELECT so the processor can still link interactions
// to it — re-running the processor on the same JSONL is idempotent.
//
// The split from [InsertGroundingEvent] is deliberate: the online emit
// path doesn't care about the id and shouldn't pay the SELECT cost,
// while the processor needs the id to fan out telemetry rows.
func InsertGroundingEventTx(ctx context.Context, tx *sql.Tx, ev GroundingEventInsert) (int64, error) {
	refsJSON, err := json.Marshal(ev.SourceRefs)
	if err != nil {
		return 0, fmt.Errorf("marshal source_refs: %w", err)
	}
	if ev.SourceRefs == nil {
		refsJSON = []byte("[]")
	}
	var used sql.NullInt64
	if ev.Used != nil {
		used.Valid = true
		if *ev.Used {
			used.Int64 = 1
		}
	}
	querySource := ev.QuerySource
	if querySource == "" {
		querySource = "agent_initiated"
	}
	qwenFellBack := optBoolToNullInt(ev.QwenFellBack)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO grounding_events
			(project_id, session_id, call_id, action,
			 results_count, source_refs, next_turn_has_output, used, span_id,
			 prompt_id, parent_span_id, query_source, user_message_id, query_text,
			 pass1_latency_ms, pass2_latency_ms, qwen_fell_back, kiwix_hits_in, kiwix_hits_out,
			 created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, datetime('now')))
		ON CONFLICT (session_id, call_id) DO NOTHING`,
		ev.ProjectID, ev.SessionID, ev.CallID, ev.Action,
		ev.ResultsCount, string(refsJSON), boolToInt(ev.NextTurnHasOutput),
		used, ev.SpanID,
		ev.PromptID, ev.ParentSpanID, querySource, ev.UserMessageID, ev.QueryText,
		ev.Pass1LatencyMS, ev.Pass2LatencyMS, qwenFellBack, ev.KiwixHitsIn, ev.KiwixHitsOut,
		ev.CreatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("insert grounding_events: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read inserted id: %w", err)
	}
	if id == 0 {
		// ON CONFLICT DO NOTHING; re-fetch the existing row's id.
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM grounding_events WHERE session_id = ? AND call_id = ?`,
			ev.SessionID, ev.CallID)
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("re-fetch existing id: %w", err)
		}
	}
	// Repair-mode side effect: when CreatedAt is explicitly supplied
	// (--preserve-transcript-timestamps), also UPDATE any existing
	// (session_id, call_id) row's created_at to the preserved value.
	// Necessary because the ON CONFLICT clause is DO NOTHING, so a
	// historical row written under the Stop-hook's pre-fix
	// pwd-inference behavior (bug 1389) keeps its batch-insert
	// timestamp without this. The UPDATE is a no-op when the row was
	// just inserted (same value). Stays nil-safe — when CreatedAt is
	// nil, no UPDATE runs (online emit + normal Stop-hook unchanged).
	if ev.CreatedAt != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE grounding_events SET created_at = ?
			 WHERE session_id = ? AND call_id = ? AND created_at != ?`,
			*ev.CreatedAt, ev.SessionID, ev.CallID, *ev.CreatedAt,
		); err != nil {
			return 0, fmt.Errorf("repair created_at: %w", err)
		}
	}
	return id, nil
}

// DefaultProcessorDedupeWindowSeconds is the time window used by
// InsertGroundingEventTxBackstop to match a processor-side insert
// against a prior online-emit row for the same search. 60 seconds
// covers the typical tool_use → tool_result latency (vault_search
// with Qwen rerank lands in 10-30s; outliers under 60s) plus clock
// skew. Closes bug
// `grounding-events-online-emit-and-stop-hook-processor-create-duplicate-rows-per-search`.
const DefaultProcessorDedupeWindowSeconds = 60

// InsertGroundingEventTxBackstop is the processor's insert path.
// Before writing, it checks for an existing online-emit row that
// covers the same search (matched by action + source_refs + a tight
// time window around the processor's stamped created_at). On hit it
// UPDATEs that row with the processor-computed post-hoc fields
// (next_turn_has_output, used, prompt_id, parent_span_id) and returns
// its id — collapsing the steady-state duplicate into one canonical
// row that combines search-time signal with post-session enrichment.
// On miss it falls through to InsertGroundingEventTx so the processor
// stays a real backstop for sessions where the online emit dropped
// (the recordGroundingEvent log-and-drop path) or for sessions where
// the toolkit-server restarted mid-fire.
//
// Online-emit rows are detected via call_id shape: the online emit
// stamps the dispatcher's span_id (UUID), while the processor stamps
// the transcript's tool_use id (toolu_*). The SELECT filters
// call_id NOT LIKE 'toolu_%' to only consider online-emit rows.
//
// dedupWindowSeconds=0 falls back to DefaultProcessorDedupeWindowSeconds.
// Errors are propagated; an empty SourceRefs slice is normalized to
// "[]" to match the INSERT path.
func InsertGroundingEventTxBackstop(ctx context.Context, tx *sql.Tx, ev GroundingEventInsert, dedupWindowSeconds int) (int64, error) {
	if dedupWindowSeconds <= 0 {
		dedupWindowSeconds = DefaultProcessorDedupeWindowSeconds
	}
	refsJSON, err := json.Marshal(ev.SourceRefs)
	if err != nil {
		return 0, fmt.Errorf("marshal source_refs: %w", err)
	}
	if ev.SourceRefs == nil {
		refsJSON = []byte("[]")
	}
	// SELECT-then-UPDATE pattern. The created_at filter uses the
	// processor's stamped CreatedAt when present (preserve-transcript-
	// timestamps mode), otherwise the current SQLite time — same
	// behavior as the INSERT-path COALESCE.
	stampedAt := "datetime('now')"
	stampArgs := []any{ev.Action, string(refsJSON), dedupWindowSeconds, dedupWindowSeconds}
	if ev.CreatedAt != nil {
		stampedAt = "?"
		stampArgs = []any{ev.Action, string(refsJSON), *ev.CreatedAt, dedupWindowSeconds, *ev.CreatedAt, dedupWindowSeconds}
	}
	q := `SELECT id FROM grounding_events
		WHERE action = ?
		  AND source_refs = ?
		  AND call_id NOT LIKE 'toolu_%'
		  AND created_at >= datetime(` + stampedAt + `, '-' || ? || ' seconds')
		  AND created_at <= datetime(` + stampedAt + `, '+' || ? || ' seconds')
		ORDER BY created_at DESC, id DESC
		LIMIT 1`
	var existingID int64
	err = tx.QueryRowContext(ctx, q, stampArgs...).Scan(&existingID)
	switch {
	case err == sql.ErrNoRows:
		// No online-emit row in the window — processor row IS the only
		// signal for this search; fall through to the normal insert.
		return InsertGroundingEventTx(ctx, tx, ev)
	case err != nil:
		return 0, fmt.Errorf("backstop lookup: %w", err)
	}
	// Online-emit row exists — enrich it with the processor's post-hoc
	// fields rather than writing a duplicate. UPDATE only those fields
	// the processor can compute that the online emit can't:
	// next_turn_has_output, used, prompt_id, parent_span_id.
	var used sql.NullInt64
	if ev.Used != nil {
		used.Valid = true
		if *ev.Used {
			used.Int64 = 1
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE grounding_events
		SET next_turn_has_output = ?,
		    used = COALESCE(?, used),
		    prompt_id = COALESCE(?, prompt_id),
		    parent_span_id = COALESCE(?, parent_span_id)
		WHERE id = ?`,
		boolToInt(ev.NextTurnHasOutput), used, ev.PromptID, ev.ParentSpanID, existingID,
	); err != nil {
		return 0, fmt.Errorf("backstop enrich: %w", err)
	}
	return existingID, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// optBoolToNullInt encodes a *bool as a sql.NullInt64: nil → NULL, *false → 0,
// *true → 1. The qwen_fell_back column is nullable (only kiwix_search rows
// populate it); the other grounding actions leave it NULL via this helper.
func optBoolToNullInt(b *bool) sql.NullInt64 {
	if b == nil {
		return sql.NullInt64{}
	}
	v := int64(0)
	if *b {
		v = 1
	}
	return sql.NullInt64{Int64: v, Valid: true}
}
