package knowledge

import (
	"context"
	"log/slog"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/knowledge/kiwix"
	"toolkit/internal/obs"
	"toolkit/internal/telemetry"
)

// HandlerTelemetry carries the per-action telemetry columns that
// migration 046 absorbed onto grounding_events from the retired
// per-handler tables (vault_search_invocations, kiwix_offload_invocations).
// All fields nil → the column stays NULL (the default for grounding
// actions that don't populate this signal, e.g. knowledge_search).
type HandlerTelemetry struct {
	// vault_search-specific: latency of pass 1 (candidate prefilter) and
	// pass 2 (specificity rerank).
	Pass1LatencyMS *int64
	Pass2LatencyMS *int64

	// kiwix_search-specific: whether the Qwen rerank fell back to the
	// original kiwix order, plus the raw and returned hit counts.
	QwenFellBack *bool
	KiwixHitsIn  *int64
	KiwixHitsOut *int64
}

// recordGroundingEvent is the shared online-emit path for the three
// read-side knowledge handlers — vault_search, kiwix_search,
// knowledge_search. Each handler calls this at search-exit to record
// what was returned to the agent under the dispatcher's per-request
// span_id; the sibling chain's query-telemetry-substrate joins these
// rows to the events ledger by SHARED span_id.
//
// Cross-substrate contract (T5 acceptance criteria item (b)+(d)):
//
//   - The span_id on this row MUST equal the span_id on every events
//     row emitted by write-side handlers in the same MCP request. The
//     dispatcher stamps the span on ctx ONCE per tools/call; this
//     helper reads from ctx and NEVER regenerates.
//   - When ctx has no obs span (pre-T5 caller, test driver that didn't
//     wire dispatch), the helper falls back to events.SpanIDFromContext
//     (which auto-mints one). The row still lands; the absence of a
//     dispatch-minted span shows up as missing parent_span_id at
//     join-render time.
//
// Identity columns inherited from migration 019_grounding_events.sql
// (session_id, call_id, action) are populated from the span as well:
// session_id = span.TraceID (the request root), call_id = span.ID.
// The unique (session_id, call_id) constraint keeps duplicate emits
// (re-tries) silently no-op via ON CONFLICT DO NOTHING. Write failure
// is logged and dropped — telemetry must never block the search
// response.
//
// `extras` carries the per-handler telemetry columns absorbed by
// migration 046 (chain telemetry-substrate-cleanup T2). Pass an empty
// HandlerTelemetry{} for actions that don't have any (knowledge_search).
func recordGroundingEvent(ctx context.Context, pool *db.Pool, projectID, action, queryText string, resultsCount int64, refs []string, extras HandlerTelemetry) {
	var spanID, sessionID, callID string
	if s := obs.SpanFromContext(ctx); s != nil {
		spanID = s.ID
		callID = s.ID
		sessionID = s.TraceID
	} else {
		fallback, _ := events.SpanIDFromContext(ctx)
		spanID = fallback
		callID = fallback
		sessionID = fallback
	}
	// query-telemetry-substrate TT2 fields. query_source flows through
	// ctx (default agent_initiated; the future proactive hook stamps
	// proactive_hook). user_message_id is stamped by the dispatcher
	// when known; otherwise nil and the column stays NULL.
	source := string(telemetry.QuerySourceFromContext(ctx))
	var userMsgIDPtr *string
	if id := telemetry.UserMessageIDFromContext(ctx); id != "" {
		userMsgIDPtr = &id
	}
	var queryTextPtr *string
	if queryText != "" {
		queryTextPtr = &queryText
	}
	ev := db.GroundingEventInsert{
		ProjectID:    projectID,
		SessionID:    sessionID,
		CallID:       callID,
		Action:       action,
		ResultsCount: resultsCount,
		SourceRefs:   refs,
		// next_turn_has_output and used are unset on the online write —
		// the Rust offline processor's separate analysis pass fills
		// them in by scanning subsequent agent turns. prompt_id and
		// parent_span_id are similarly Stop-hook-stamped post-session.
		SpanID:        spanID,
		QuerySource:   source,
		UserMessageID: userMsgIDPtr,
		QueryText:     queryTextPtr,

		Pass1LatencyMS: extras.Pass1LatencyMS,
		Pass2LatencyMS: extras.Pass2LatencyMS,
		QwenFellBack:   extras.QwenFellBack,
		KiwixHitsIn:    extras.KiwixHitsIn,
		KiwixHitsOut:   extras.KiwixHitsOut,
	}
	if err := db.InsertGroundingEvent(ctx, pool, ev); err != nil {
		obs.Logger(ctx).Warn("grounding_events: write failed",
			slog.String("action", action),
			slog.String("err", err.Error()),
		)
	}
}

// groundingRefsFromVault flattens ranked vault entries to the source_refs
// array stored on the grounding_events row. Returns paths (the vault's
// natural identifier for an entry).
func groundingRefsFromVault(results []VaultResultEntry) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Path)
	}
	return out
}

// groundingRefsFromKiwix flattens kiwix hits to source_refs. Encodes as
// zim_id/article_slug so a downstream consumer can disambiguate hits
// from different ZIMs.
func groundingRefsFromKiwix(hits []kiwix.SearchHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ArticleRef.ZimID+"/"+h.ArticleRef.Slug)
	}
	return out
}

// groundingRefsFromPointers flattens knowledge_search pointer results to
// source_refs. Encodes as source_type:source_ref so downstream readers
// can route by surface (vault note vs library entry vs task/chain/bug).
func groundingRefsFromPointers(results []KnowledgePointerResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.SourceType+":"+r.SourceRef)
	}
	return out
}
