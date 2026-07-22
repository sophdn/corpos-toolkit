package refresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"toolkit/internal/knowledge"
)

// kiwixFallbackMaxCandidates caps the per-envelope fallback
// surfacings per chain parse-context-lean-orienting T8 acceptance
// criteria. Two is the hail-mary budget — more dilutes the orientation
// signal, fewer rarely helps the agent triangulate.
const kiwixFallbackMaxCandidates = 2

// kiwixFallbackSnippetMax caps the snippet length surfaced in the
// PresentedAs string so a long offline doc body doesn't blow the
// envelope budget. Matches the kiwix_bridge convention loosely.
const kiwixFallbackSnippetMax = 200

// kiwixFallbackSuppressedReason enumerates the closed set of suppress
// branches matching the schema enum. Empty string means the gate
// fired.
type kiwixFallbackSuppressedReason string

const (
	kiwixFallbackFired                kiwixFallbackSuppressedReason = ""
	kiwixFallbackReasonSingleExact    kiwixFallbackSuppressedReason = "single_exact_present"
	kiwixFallbackReasonReadShape      kiwixFallbackSuppressedReason = "read_shape_intent"
	kiwixFallbackReasonBridgeFired    kiwixFallbackSuppressedReason = "kiwix_bridge_already_fired"
	kiwixFallbackReasonNoQualifyingNH kiwixFallbackSuppressedReason = "no_qualifying_no_hits"
)

// kiwixFallbackReadShapeIntents is the closed set of intent shapes
// that suppress the fallback. T8 §"Trigger condition (3)" — status /
// list / summarize are read-shape prompts where empty results most
// often mean "nothing to list," not "please search docs."
var kiwixFallbackReadShapeIntents = map[IntentShape]bool{
	IntentStatus:    true,
	IntentList:      true,
	IntentSummarize: true,
}

// KiwixFallbackTelemetry is the per-call summary the handler stamps
// onto the ParseContextKiwixFallbackFired event. Distinct from the
// returned ResolvedReference slice so the emit doesn't have to walk
// the list to extract counts.
type KiwixFallbackTelemetry struct {
	IntentShape          string
	Fired                bool
	SuppressedReason     string
	CandidatesReturned   int
	KiwixSearchLatencyMs int
}

// KiwixFallbackSearchFn is the injectable search dependency. The
// production wire-up (NewKiwixFallbackSearcherFromKnowledge) calls
// knowledge.HandleKnowledgeSearch with the kiwix-reference filter
// and unpacks results into KiwixFallbackHit. Tests pass a stub.
//
// Returns at most `limit` results, ranked by knowledge_search's own
// scoring (kiwix relevance × position decay).
type KiwixFallbackSearchFn func(ctx context.Context, query string, limit int) ([]KiwixFallbackHit, error)

// KiwixFallbackHit is one search result the T8 fallback gate consumes.
// Stripped to the minimum fields the ResolvedReference composer needs
// — production-side mapped from knowledge.HandleKnowledgeSearch's
// pointer entries; test-side built inline.
type KiwixFallbackHit struct {
	SourceRef string
	Title     string
	Snippet   string
	Score     float64
}

// ResolveKiwixFallback evaluates the T8 gate against the
// already-resolved envelope and, when the gate arms, queries kiwix
// for orientation pointers. Returns up to kiwixFallbackMaxCandidates
// ResolvedReferences plus per-call telemetry.
//
// Inputs:
//   - alreadyResolved: the envelope's References slice as currently
//     built — used to inspect for TierSingleExact hits, no-hit shape
//     mix, and any prior kiwix_bridge surfacings.
//   - noHitRefs: the original Reference list filtered to those whose
//     dispatched HitSet returned TierNoHit. The handler can compute
//     this once and pass it in; the gate needs to know the SHAPE of
//     no-hits, which the de-duplicated NoHitTokens slice on the
//     envelope strips.
//   - intent: T5's directive intent shape.
//   - message: the original parse_context input — used as the kiwix
//     query when the gate arms.
//   - search: injectable kiwix search; nil disables the gate
//     (handler may pass nil in degraded boot / smoke).
//
// Returns (refs, telemetry). On any suppress branch refs is empty and
// telemetry.SuppressedReason names the branch. On the fire branch
// refs has len ≤ 2 and telemetry.Fired=true.
//
// The function never returns an error: kiwix search failures degrade
// to a fire-with-zero-candidates outcome (telemetry.Fired stays true
// because the gate evaluated to fire; CandidatesReturned=0 records
// the upstream failure). Surrounding parse_context must not fail on
// orientation-surface errors.
func ResolveKiwixFallback(
	ctx context.Context,
	alreadyResolved []ResolvedReference,
	noHitRefs []Reference,
	intent IntentShape,
	message string,
	search KiwixFallbackSearchFn,
) ([]ResolvedReference, KiwixFallbackTelemetry) {
	tel := KiwixFallbackTelemetry{IntentShape: string(intent)}

	if search == nil {
		// Degraded boot: no search injected. Skip silently — neither
		// fire nor a meaningful suppress reason. The handler treats
		// the empty-telemetry case as "didn't evaluate."
		tel.IntentShape = ""
		return nil, tel
	}

	// Gate ordering: cheapest checks first so suppressed_reason
	// reflects the dominant block.
	if kiwixFallbackEnvelopeHasSingleExact(alreadyResolved) {
		tel.SuppressedReason = string(kiwixFallbackReasonSingleExact)
		return nil, tel
	}
	if kiwixFallbackReadShapeIntents[intent] {
		tel.SuppressedReason = string(kiwixFallbackReasonReadShape)
		return nil, tel
	}
	if !kiwixFallbackHasQualifyingNoHit(noHitRefs) {
		tel.SuppressedReason = string(kiwixFallbackReasonNoQualifyingNH)
		return nil, tel
	}
	if kiwixFallbackBridgeAlreadyFired(alreadyResolved) {
		tel.SuppressedReason = string(kiwixFallbackReasonBridgeFired)
		return nil, tel
	}

	// Gate fires: query kiwix with the message text. Per AskUser
	// confirm: the whole message gives kiwix phrasing context, not
	// just the per-token query the bridge already missed on.
	start := time.Now()
	results, err := search(ctx, message, kiwixFallbackMaxCandidates)
	tel.KiwixSearchLatencyMs = int(time.Since(start).Milliseconds())
	tel.Fired = true
	if err != nil {
		// Fire-with-zero-candidates — gate evaluated to fire but the
		// upstream failed. T10's reason-mix dashboard distinguishes
		// this from "fired and found nothing" via the latency field
		// being non-zero (the kiwix call ran).
		return nil, tel
	}
	if len(results) > kiwixFallbackMaxCandidates {
		results = results[:kiwixFallbackMaxCandidates]
	}
	out := make([]ResolvedReference, 0, len(results))
	for _, r := range results {
		out = append(out, kiwixFallbackRef(r))
	}
	tel.CandidatesReturned = len(out)
	return out, tel
}

// kiwixFallbackEnvelopeHasSingleExact reports whether any already-
// resolved reference carries TierSingleExact. The T8 gate suppresses
// when at least one strong hit landed — orientation pointers add
// noise the agent doesn't need.
func kiwixFallbackEnvelopeHasSingleExact(refs []ResolvedReference) bool {
	for _, r := range refs {
		if r.ConfidenceTier == TierSingleExact {
			return true
		}
	}
	return false
}

// kiwixFallbackHasQualifyingNoHit reports whether the no-hit ref set
// contains at least one external_technical or domain_term shape. The
// gate arms only on these shapes: "unresolved-domain" content the
// agent might want offline-doc orientation for.
//
// Note: the envelope's NoHitTokens slice strips shape information
// (it's de-duplicated by token), so the handler passes through the
// original Reference list filtered to no-hits — that retains shape.
func kiwixFallbackHasQualifyingNoHit(noHitRefs []Reference) bool {
	for _, r := range noHitRefs {
		if r.Shape == ShapeExternalTechnical || r.Shape == ShapeDomainTerm {
			return true
		}
	}
	return false
}

// kiwixFallbackBridgeAlreadyFired reports whether the existing
// kiwix_bridge resolver already produced Candidates in this envelope.
// "Produced candidates" = at least one ShapeKiwixBridge ref with
// non-empty TopCandidates; the no-hit bridge case (zero candidates,
// TierNoHit) does NOT count and the fallback is allowed to retry
// with the message-text query.
func kiwixFallbackBridgeAlreadyFired(refs []ResolvedReference) bool {
	for _, r := range refs {
		if r.Shape == ShapeKiwixBridge && len(r.TopCandidates) > 0 {
			return true
		}
	}
	return false
}

// kiwixFallbackRef composes a ResolvedReference for one fallback
// kiwix hit. ConfidenceTier is the new TierLowConfidenceFallback so
// consumers can recognise the hail-mary nature; RecommendedAction is
// PresentMentionAsPossiblyRelevant — these are orientation pointers,
// not authoritative hits.
//
// Token is the kiwix entry's SourceRef (e.g. "devdocs:goland/path/to/page")
// because there's no message-level token to attribute these to — the
// envelope's no_hit_tokens already cover the user-supplied terms.
func kiwixFallbackRef(r KiwixFallbackHit) ResolvedReference {
	snippet := r.Snippet
	if len(snippet) > kiwixFallbackSnippetMax {
		snippet = snippet[:kiwixFallbackSnippetMax] + "…"
	}
	title := strings.TrimSpace(r.Title)
	if title == "" {
		title = "(untitled kiwix entry)"
	}
	presented := fmt.Sprintf("[orientation pointer — low confidence] %s — %s", title, snippet)
	return ResolvedReference{
		Token:             r.SourceRef,
		Shape:             ShapeKiwixBridge,
		ConfidenceTier:    TierLowConfidenceFallback,
		PresentedAs:       presented,
		RecommendedAction: PresentMentionAsPossiblyRelevant,
		CachePolicy:       string(PolicyShortThreeTurns),
		TopCandidates: []Candidate{{
			ID:         r.SourceRef,
			Title:      title,
			Score:      r.Score,
			SourceRef:  r.SourceRef,
			DebugNotes: fmt.Sprintf("source=kiwix-fallback score=%.2f", r.Score),
		}},
	}
}

// NewKiwixFallbackSearcherFromKnowledge is the production wire-up: a
// KiwixFallbackSearchFn that calls knowledge.HandleKnowledgeSearch
// and filters to kiwix-reference results. Mirrors kiwixBridgeResolver's
// pattern verbatim — same Source tag ("reference_resolution"), same
// SourceType filter ("kiwix_reference"), same top_k=5 fetch (the
// per-envelope cap of 2 happens at the gate level so we have headroom
// if a kiwix result lacks a usable title).
func NewKiwixFallbackSearcherFromKnowledge(deps knowledge.Deps, project string) KiwixFallbackSearchFn {
	return func(ctx context.Context, query string, limit int) ([]KiwixFallbackHit, error) {
		type knowledgeSearchParams struct {
			Query  string `json:"query"`
			TopK   int    `json:"top_k"`
			Source string `json:"source"`
		}
		params, err := json.Marshal(knowledgeSearchParams{
			Query:  query,
			TopK:   5,
			Source: "reference_resolution",
		})
		if err != nil {
			return nil, fmt.Errorf("marshal knowledge_search params: %w", err)
		}
		result, err := knowledge.HandleKnowledgeSearch(ctx, deps, project, params)
		if err != nil {
			return nil, fmt.Errorf("knowledge_search: %w", err)
		}
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		out := make([]KiwixFallbackHit, 0, limit)
		for i, p := range result.Results {
			if p.SourceType != "kiwix_reference" {
				continue
			}
			if len(out) >= limit {
				break
			}
			score := 0.0
			if p.QualityScore != nil {
				score = *p.QualityScore
			}
			positionDecay := 1.0 - float64(i)*0.1
			if positionDecay < 0.1 {
				positionDecay = 0.1
			}
			out = append(out, KiwixFallbackHit{
				SourceRef: p.SourceRef,
				Title:     p.Question,
				Snippet:   p.InvokeWhen,
				Score:     score * positionDecay,
			})
		}
		return out, nil
	}
}
