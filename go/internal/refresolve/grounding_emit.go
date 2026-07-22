package refresolve

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/obs"
	"toolkit/internal/telemetry"
)

// emitGroundingEvents writes one grounding_events row per detected
// reference (per design doc §6.2 — Path A: query_source set to the
// first-class 'reference_resolution' enum value added by migration
// 040). Returns the row IDs in the same order as `refs`. Failure of
// any individual emit is logged at WARN and that index of the
// returned slice is 0 — the resolution flow proceeds.
//
// Each reference gets a synthesized call_id ("<span>#r<i>") so the
// (session_id, call_id) UNIQUE constraint on grounding_events
// admits N rows from one tools/call. session_id and span_id come
// from ctx (set by the dispatcher's obs.SpanStart); when ctx has no
// span (test driver, fallback path), events.SpanIDFromContext
// supplies a deterministic fallback.
func emitGroundingEvents(ctx context.Context, deps HandlerDeps, actionName string, messageText string, refs []Reference, hits map[Reference]HitSet) []int64 {
	out := make([]int64, len(refs))
	if deps.Pool == nil || len(refs) == 0 {
		return out
	}
	var spanID, sessionID string
	if s := obs.SpanFromContext(ctx); s != nil {
		spanID = s.ID
		sessionID = s.TraceID
	} else {
		fallback, _ := events.SpanIDFromContext(ctx)
		spanID = fallback
		sessionID = fallback
	}
	var userMsgIDPtr *string
	if id := telemetry.UserMessageIDFromContext(ctx); id != "" {
		userMsgIDPtr = &id
	}
	var queryTextPtr *string
	if messageText != "" {
		queryTextPtr = &messageText
	}

	// Single tx for all emits — atomic per resolve_references call.
	// Each ref writes BOTH a grounding_events row AND a side-table row
	// (reference_resolution_emits, migration 042). The side-table indexes
	// shape / tier / resolver_name / presentation_recommendation /
	// presented_as / retrieval_cost_ms / ml_confidence_score so the
	// Context Pull Inspector (RF3) can filter without re-parsing
	// source_refs prefixes. Failure mid-batch rolls everything back;
	// caller proceeds with zeroed grounding_event_ids (the resolution
	// result still ships, just without the per-row telemetry FKs).
	err := deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		for i, ref := range refs {
			callID := fmt.Sprintf("%s#r%d", spanID, i)
			hs := hits[ref]
			refStrings := candidateSourceRefs(hs.Candidates)
			ev := db.GroundingEventInsert{
				ProjectID:     deps.Project,
				SessionID:     sessionID,
				CallID:        callID,
				Action:        actionName,
				ResultsCount:  int64(len(hs.Candidates)),
				SourceRefs:    refStrings,
				SpanID:        spanID,
				QuerySource:   "reference_resolution",
				UserMessageID: userMsgIDPtr,
				QueryText:     queryTextPtr,
			}
			id, insErr := db.InsertGroundingEventTx(ctx, tx, ev)
			if insErr != nil {
				return fmt.Errorf("insert grounding_events for ref %d (%s): %w", i, ref.Token, insErr)
			}
			out[i] = id

			// formatResolved gives us PresentedAs + RecommendedAction
			// keyed off the same (ref, hs) pair the output path uses
			// downstream — emit-side and result-side stay in lock-step.
			formatted := formatResolved(ref, hs)
			emit := db.ReferenceResolutionEmitInsert{
				GroundingEventID:           id,
				Shape:                      string(ref.Shape),
				ConfidenceScore:            ref.Confidence,
				DetectionMethod:            ref.DetectionMethod,
				StartPos:                   ref.StartPos,
				EndPos:                     ref.EndPos,
				ConfidenceTier:             string(hs.ConfidenceTier),
				PresentationRecommendation: string(formatted.RecommendedAction),
				PresentedAs:                formatted.PresentedAs,
				ResolverName:               hs.ResolverName,
				RetrievalCostMs:            hs.RetrievalCostMs,
				// MLConfidenceScore is nil until T7's classifier backfills.
			}
			if emitErr := db.InsertReferenceResolutionEmitTx(ctx, tx, emit); emitErr != nil {
				return fmt.Errorf("insert reference_resolution_emits for ref %d (%s): %w", i, ref.Token, emitErr)
			}
		}
		return nil
	})
	if err != nil {
		obs.Logger(ctx).Warn("refresolve: grounding_events emit failed; resolution returned without telemetry FKs",
			slog.String("err", err.Error()),
			slog.Int("ref_count", len(refs)),
		)
		// Zero out the IDs since the tx rolled back.
		for i := range out {
			out[i] = 0
		}
	}
	return out
}

// candidateSourceRefs collects Candidate.SourceRef values for the
// grounding_events.source_refs JSON array column.
//
// ENCODING (JOIN TRAP — see suggestion #18): each resolver sets SourceRef
// in its own `<type>:<rest>` shape (chain:/bug:/skill:/memory:/vault:/
// kiwix:<book>::<entry>). This is NOT the same encoding as
// knowledge_pointers.source_ref (`<scope>::<slug>`, built by
// forge/indexsync.go) — a direct JOIN between the two columns is a ~100%
// miss; normalize first. See
// vault/reference/2026-05-23_source-ref-encoding-divergence-grounding-events-vs-knowledge-pointers.md.
func candidateSourceRefs(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if c.SourceRef != "" {
			out = append(out, c.SourceRef)
		}
	}
	return out
}
