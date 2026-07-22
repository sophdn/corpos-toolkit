package refresolve

// STEP 7 (post-refactor net densification) of chain
// refactor-handler-parse-context-core. Direct unit tests of the units STEP 6
// extracted — harvesting the testability dividend the refactor bought and
// closing the boundary gaps the coarse end-to-end net left. Internal test
// package so it can drive the unexported helpers directly. ADDITIVE: no
// step-2 parity assertion is touched.

import (
	"context"
	"errors"
	"testing"

	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// resolveSessionID precedence: param > MCP-session > span-trace > empty.
// Closes the tier-3 (span-trace) gap STEP 6 surfaced by extracting this unit
// (it was uncovered pre-refactor; step 6 touched the surface, so we pin it).
func TestResolveSessionID_PrecedenceTiers(t *testing.T) {
	ctxMCP := events.WithMCPSessionID(context.Background(), "mcp-id")

	if got := resolveSessionID(ctxMCP, parseContextParams{SessionID: "param-id"}); got != "param-id" {
		t.Errorf("tier 1 (explicit param) must win: got %q", got)
	}
	if got := resolveSessionID(ctxMCP, parseContextParams{}); got != "mcp-id" {
		t.Errorf("tier 2 (MCP-session id): got %q", got)
	}

	ctxSpan, end := obs.SpanStart(context.Background(), "densify-span")
	defer end(nil)
	span := obs.SpanFromContext(ctxSpan)
	if span == nil {
		t.Fatal("expected a span on the ctx")
	}
	if got := resolveSessionID(ctxSpan, parseContextParams{}); got != span.TraceID {
		t.Errorf("tier 3 (span trace-id): got %q want %q", got, span.TraceID)
	}

	if got := resolveSessionID(context.Background(), parseContextParams{}); got != "" {
		t.Errorf("tier 4 (nothing available) must be empty: got %q", got)
	}
}

// assembleReferences input-class combinations, reached cleanly at the unit
// boundary (combinations that are awkward to drive end-to-end).
func TestAssembleReferences_BucketsAndInvariants(t *testing.T) {
	chainCand := []Candidate{{ID: "c", Title: "chain c", Score: 1.0, SourceRef: "chain:c", DebugNotes: "status=open"}}

	resolvedRef := Reference{Token: "c", Shape: ShapeChainSlug}
	noHitRef := Reference{Token: "ghost", Shape: ShapeBugSlug}
	failRef := Reference{Token: "boom", Shape: ShapeBugSlug}

	refs := []Reference{resolvedRef, noHitRef, failRef}
	hits := map[Reference]HitSet{
		resolvedRef: {ConfidenceTier: TierSingleExact, Candidates: chainCand},
		noHitRef:    {ConfidenceTier: TierNoHit},
		failRef:     {ConfidenceTier: TierNoHit, Err: errors.New("resolver exploded")},
	}
	groundingIDs := []int64{11, 22, 33}

	refsOut, noHits, partials, cHits, cMisses := assembleReferences(
		refs, hits, nil, nil, groundingIDs, false /*includeNoHits*/, false /*cacheActive*/)

	if len(refsOut) != 1 || refsOut[0].Token != "c" {
		t.Fatalf("want 1 resolved ref (c), got %+v", refsOut)
	}
	if refsOut[0].GroundingEventID != 11 {
		t.Errorf("grounding id should stamp by index: got %d want 11", refsOut[0].GroundingEventID)
	}
	if len(noHits) != 1 || noHits[0] != "ghost" {
		t.Errorf("want NoHitTokens=[ghost] (failRef goes to partials, not no-hits): got %v", noHits)
	}
	if len(partials) != 1 {
		t.Errorf("want 1 partial failure (boom): got %v", partials)
	}
	if cHits != 0 || cMisses != 0 {
		t.Errorf("cacheActive=false → no cache counts: got hits=%d misses=%d", cHits, cMisses)
	}

	// includeNoHits=true keeps the no-hit ref in References instead of collapsing.
	refsOut2, noHits2, _, _, _ := assembleReferences(
		[]Reference{noHitRef}, hits, nil, nil, nil, true /*includeNoHits*/, false)
	if len(refsOut2) != 1 || len(noHits2) != 0 {
		t.Errorf("includeNoHits=true: want 1 ref, 0 no-hit-tokens; got refs=%d noHits=%v", len(refsOut2), noHits2)
	}
}

// Invariant: a token that resolves under one shape AND no-hits under another
// is dropped from NoHitTokens (intersect(refs,no_hit)=∅, bug 1426).
func TestAssembleReferences_DupTokenNoHitDroppedWhenResolvedElsewhere(t *testing.T) {
	asChain := Reference{Token: "dup", Shape: ShapeChainSlug}
	asBug := Reference{Token: "dup", Shape: ShapeBugSlug}
	refs := []Reference{asChain, asBug}
	hits := map[Reference]HitSet{
		asChain: {ConfidenceTier: TierSingleExact, Candidates: []Candidate{{ID: "dup", Title: "t", Score: 1.0, SourceRef: "chain:dup"}}},
		asBug:   {ConfidenceTier: TierNoHit},
	}
	refsOut, noHits, _, _, _ := assembleReferences(refs, hits, nil, nil, nil, false, false)
	if len(refsOut) != 1 {
		t.Errorf("want 1 resolved ref for the chain shape; got %d", len(refsOut))
	}
	if len(noHits) != 0 {
		t.Errorf("dup token resolved under chain shape → must NOT appear in NoHitTokens; got %v", noHits)
	}
}

// Cache counting + FromCache/CachePolicy stamping at the unit boundary.
func TestAssembleReferences_CacheHitStampAndCounts(t *testing.T) {
	ref := Reference{Token: "c", Shape: ShapeChainSlug}
	hs := HitSet{ConfidenceTier: TierSingleExact, Candidates: []Candidate{{ID: "c", Title: "t", Score: 1.0, SourceRef: "chain:c"}}}
	refs := []Reference{ref}
	hits := map[Reference]HitSet{ref: hs}
	cachedHits := map[Reference]HitSet{ref: hs}
	cachedPolicies := map[Reference]CachePolicy{ref: PolicyForShape(ShapeChainSlug)}

	refsOut, _, _, cHits, cMisses := assembleReferences(refs, hits, cachedHits, cachedPolicies, nil, false, true /*cacheActive*/)
	if cHits != 1 || cMisses != 0 {
		t.Errorf("cached ref → 1 hit 0 miss; got hits=%d misses=%d", cHits, cMisses)
	}
	if len(refsOut) != 1 || !refsOut[0].FromCache {
		t.Errorf("resolved-from-cache ref must have FromCache=true; got %+v", refsOut)
	}
	if refsOut[0].CachePolicy != string(PolicyForShape(ShapeChainSlug)) {
		t.Errorf("cached ref carries its stored policy; got %q", refsOut[0].CachePolicy)
	}
}

// Leaf presentation formatters — close the present.go gaps (presentWeakDomain
// was 0% covered) exposed when STEP 6 (D6) lifted the family into present.go.
func TestPresentFormatters_LeafArms(t *testing.T) {
	ref := Reference{Token: "thing", Shape: ShapeDomainTerm}

	if got := presentWeakDomain(ref, nil); got == "" {
		t.Error("presentWeakDomain with no candidates should still return a string")
	}
	weak := presentWeakDomain(ref, []Candidate{{SourceRef: "vault:note.md", Score: 0.3}})
	if weak == "" {
		t.Error("presentWeakDomain with a candidate should return a non-empty string")
	}

	// projectFromSourceRef: bare chain slug → no project; project-qualified → project.
	if got := projectFromSourceRef("chain:slug"); got != "" {
		t.Errorf("chain:slug has no project segment; got %q", got)
	}
	if got := projectFromSourceRef("bug:proj/slug"); got != "proj" {
		t.Errorf("bug:proj/slug → proj; got %q", got)
	}

	// stripSourcePrefix: strips the `<type>:` prefix; bare string is unchanged.
	if got := stripSourcePrefix("skill:foo/bar"); got != "foo/bar" {
		t.Errorf("strip prefix: got %q", got)
	}
	if got := stripSourcePrefix("nopfx"); got != "nopfx" {
		t.Errorf("no-prefix string unchanged: got %q", got)
	}

	// A couple of presentSingleExact shape arms (the switch was 50% covered).
	for _, tc := range []struct{ shape ShapeCategory }{{ShapePath}, {ShapeToolName}, {ShapeForgeSchema}, {ShapeProjectName}} {
		r := Reference{Token: "x", Shape: tc.shape}
		out := formatResolved(r, HitSet{ConfidenceTier: TierSingleExact, Candidates: []Candidate{{ID: "x", Title: "T", Score: 1.0, SourceRef: "x:y", DebugNotes: "n"}}})
		if out.PresentedAs == "" {
			t.Errorf("shape %s: PresentedAs should be non-empty", tc.shape)
		}
		if out.RecommendedAction != PresentUseDirectly {
			t.Errorf("shape %s single_exact → use_directly; got %s", tc.shape, out.RecommendedAction)
		}
	}
}
