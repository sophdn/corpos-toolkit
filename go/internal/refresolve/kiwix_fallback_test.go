package refresolve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"toolkit/internal/events"
	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// stubKiwix is the canonical canned search used across T8 tests.
// Returns up to `limit` results from a fixed list so tests can
// observe the cap + ordering without standing up a real kiwix
// process.
func stubKiwix(results []refresolve.KiwixFallbackHit) refresolve.KiwixFallbackSearchFn {
	return func(ctx context.Context, query string, limit int) ([]refresolve.KiwixFallbackHit, error) {
		if limit > len(results) {
			limit = len(results)
		}
		return results[:limit], nil
	}
}

func stubKiwixErr(err error) refresolve.KiwixFallbackSearchFn {
	return func(ctx context.Context, query string, limit int) ([]refresolve.KiwixFallbackHit, error) {
		return nil, err
	}
}

// Three canned kiwix hits so the cap=2 logic shows up in test output.
var cannedKiwixHits = []refresolve.KiwixFallbackHit{
	{SourceRef: "kiwix:devdocs/goland", Title: "GoLand: Project Structure", Snippet: "Configure project SDK and source roots…", Score: 0.91},
	{SourceRef: "kiwix:devdocs/intellij", Title: "IntelliJ Project Model", Snippet: "Module/library/SDK layer overview…", Score: 0.82},
	{SourceRef: "kiwix:wikipedia/ide", Title: "Integrated development environment", Snippet: "An IDE is software combining editor + build…", Score: 0.55},
}

// Gate arms: no-hit envelope + external-technical no-hit ref + write-
// shape intent → up to 2 fallback refs with the new tier and policy
// stamped.
func TestResolveKiwixFallback_GateArmsOnQualifyingEnvelope(t *testing.T) {
	already := []refresolve.ResolvedReference{
		// Only weak-domain / fuzzy-multi refs in the envelope — no
		// single_exact, so the gate isn't blocked on condition 1.
		{Shape: refresolve.ShapeDomainTerm, ConfidenceTier: refresolve.TierWeakDomain},
	}
	noHit := []refresolve.Reference{
		{Token: "GoLand", Shape: refresolve.ShapeExternalTechnical},
	}
	refs, tel := refresolve.ResolveKiwixFallback(
		context.Background(), already, noHit, refresolve.IntentImplement,
		"how do I configure GoLand's project structure", stubKiwix(cannedKiwixHits),
	)
	if !tel.Fired {
		t.Fatalf("expected gate to fire; got telemetry %+v", tel)
	}
	if tel.SuppressedReason != "" {
		t.Errorf("expected empty SuppressedReason on fire; got %q", tel.SuppressedReason)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs (cap); got %d", len(refs))
	}
	if tel.CandidatesReturned != 2 {
		t.Errorf("telemetry CandidatesReturned = %d, want 2", tel.CandidatesReturned)
	}
	for _, r := range refs {
		if r.ConfidenceTier != refresolve.TierLowConfidenceFallback {
			t.Errorf("ref %s: ConfidenceTier=%q, want %q", r.Token, r.ConfidenceTier, refresolve.TierLowConfidenceFallback)
		}
		if r.CachePolicy != string(refresolve.PolicyShortThreeTurns) {
			t.Errorf("ref %s: CachePolicy=%q, want short-3-turns", r.Token, r.CachePolicy)
		}
		if r.RecommendedAction != refresolve.PresentMentionAsPossiblyRelevant {
			t.Errorf("ref %s: RecommendedAction=%q, want mention_as_possibly_relevant", r.Token, r.RecommendedAction)
		}
		if !strings.Contains(r.PresentedAs, "orientation pointer") {
			t.Errorf("ref %s: PresentedAs missing orientation marker: %s", r.Token, r.PresentedAs)
		}
	}
}

// Suppress: envelope already has a single_exact hit.
func TestResolveKiwixFallback_SuppressOnSingleExact(t *testing.T) {
	already := []refresolve.ResolvedReference{
		{Token: "some-chain", Shape: refresolve.ShapeChainSlug, ConfidenceTier: refresolve.TierSingleExact},
	}
	noHit := []refresolve.Reference{
		{Token: "GoLand", Shape: refresolve.ShapeExternalTechnical},
	}
	refs, tel := refresolve.ResolveKiwixFallback(
		context.Background(), already, noHit, refresolve.IntentImplement,
		"a message", stubKiwix(cannedKiwixHits),
	)
	if len(refs) != 0 {
		t.Errorf("expected 0 refs on single_exact suppress; got %d", len(refs))
	}
	if tel.Fired {
		t.Errorf("expected Fired=false; got true")
	}
	if tel.SuppressedReason != "single_exact_present" {
		t.Errorf("SuppressedReason=%q, want single_exact_present", tel.SuppressedReason)
	}
	if tel.KiwixSearchLatencyMs != 0 {
		t.Errorf("expected 0 latency on suppress; got %d", tel.KiwixSearchLatencyMs)
	}
}

// Suppress: intent shape ∈ {status, list, summarize}.
func TestResolveKiwixFallback_SuppressOnReadShapeIntent(t *testing.T) {
	for _, intent := range []refresolve.IntentShape{refresolve.IntentStatus, refresolve.IntentList, refresolve.IntentSummarize} {
		already := []refresolve.ResolvedReference{
			{Shape: refresolve.ShapeDomainTerm, ConfidenceTier: refresolve.TierWeakDomain},
		}
		noHit := []refresolve.Reference{
			{Token: "X", Shape: refresolve.ShapeExternalTechnical},
		}
		refs, tel := refresolve.ResolveKiwixFallback(
			context.Background(), already, noHit, intent, "msg", stubKiwix(cannedKiwixHits),
		)
		if len(refs) != 0 {
			t.Errorf("intent=%s: expected 0 refs; got %d", intent, len(refs))
		}
		if tel.SuppressedReason != "read_shape_intent" {
			t.Errorf("intent=%s: SuppressedReason=%q, want read_shape_intent", intent, tel.SuppressedReason)
		}
	}
}

// Suppress: no qualifying no-hit refs (no external_technical / domain_term).
func TestResolveKiwixFallback_SuppressOnNoQualifyingNoHits(t *testing.T) {
	already := []refresolve.ResolvedReference{
		{Shape: refresolve.ShapeChainSlug, ConfidenceTier: refresolve.TierFuzzyMulti},
	}
	noHit := []refresolve.Reference{
		// path / chain_slug / etc. shapes don't qualify
		{Token: "some/path", Shape: refresolve.ShapePath},
		{Token: "missing-chain", Shape: refresolve.ShapeChainSlug},
	}
	refs, tel := refresolve.ResolveKiwixFallback(
		context.Background(), already, noHit, refresolve.IntentImplement, "msg", stubKiwix(cannedKiwixHits),
	)
	if len(refs) != 0 {
		t.Errorf("expected 0 refs; got %d", len(refs))
	}
	if tel.SuppressedReason != "no_qualifying_no_hits" {
		t.Errorf("SuppressedReason=%q, want no_qualifying_no_hits", tel.SuppressedReason)
	}
}

// Anti-amplification: kiwix_bridge already produced candidates in the
// envelope → fallback suppresses, even though all the other gates pass.
func TestResolveKiwixFallback_SuppressWhenBridgeAlreadyFired(t *testing.T) {
	already := []refresolve.ResolvedReference{
		{
			Token: "GoLand", Shape: refresolve.ShapeKiwixBridge,
			ConfidenceTier: refresolve.TierWeakDomain,
			TopCandidates:  []refresolve.Candidate{{ID: "kiwix:devdocs/x", Title: "x"}},
		},
	}
	noHit := []refresolve.Reference{
		{Token: "GoLand", Shape: refresolve.ShapeExternalTechnical},
	}
	refs, tel := refresolve.ResolveKiwixFallback(
		context.Background(), already, noHit, refresolve.IntentImplement, "msg", stubKiwix(cannedKiwixHits),
	)
	if len(refs) != 0 {
		t.Errorf("expected 0 refs; got %d", len(refs))
	}
	if tel.SuppressedReason != "kiwix_bridge_already_fired" {
		t.Errorf("SuppressedReason=%q, want kiwix_bridge_already_fired", tel.SuppressedReason)
	}
}

// Empty-candidates bridge ref does NOT count as fired — the whole
// point of T8 is to retry on the message text when the bridge's
// per-token query came up empty.
func TestResolveKiwixFallback_BridgeNoHitDoesNotSuppress(t *testing.T) {
	already := []refresolve.ResolvedReference{
		{
			Token:          "GoLand",
			Shape:          refresolve.ShapeKiwixBridge,
			ConfidenceTier: refresolve.TierNoHit,
			TopCandidates:  nil,
		},
	}
	noHit := []refresolve.Reference{
		{Token: "GoLand", Shape: refresolve.ShapeExternalTechnical},
		{Token: "GoLand", Shape: refresolve.ShapeKiwixBridge},
	}
	refs, tel := refresolve.ResolveKiwixFallback(
		context.Background(), already, noHit, refresolve.IntentImplement, "msg", stubKiwix(cannedKiwixHits),
	)
	if !tel.Fired {
		t.Fatalf("expected gate to fire when bridge produced no candidates; got telemetry %+v", tel)
	}
	if len(refs) != 2 {
		t.Errorf("expected 2 fallback refs; got %d", len(refs))
	}
}

// nil search → no evaluation (degraded boot). Empty IntentShape is
// the sentinel the emit pathway uses to skip event emission.
func TestResolveKiwixFallback_NilSearchSkipsEvaluation(t *testing.T) {
	noHit := []refresolve.Reference{
		{Token: "X", Shape: refresolve.ShapeExternalTechnical},
	}
	refs, tel := refresolve.ResolveKiwixFallback(
		context.Background(), nil, noHit, refresolve.IntentImplement, "msg", nil,
	)
	if len(refs) != 0 {
		t.Errorf("expected 0 refs; got %d", len(refs))
	}
	if tel.IntentShape != "" {
		t.Errorf("expected empty telemetry sentinel; got IntentShape=%q", tel.IntentShape)
	}
}

// Kiwix search error: gate evaluated to fire (latency recorded) but
// CandidatesReturned=0.  T10's reason-mix dashboard distinguishes this
// from a real "fired and found nothing" via the non-zero latency.
func TestResolveKiwixFallback_KiwixErrorIsFireWithZeroCandidates(t *testing.T) {
	already := []refresolve.ResolvedReference{
		{Shape: refresolve.ShapeDomainTerm, ConfidenceTier: refresolve.TierWeakDomain},
	}
	noHit := []refresolve.Reference{
		{Token: "X", Shape: refresolve.ShapeExternalTechnical},
	}
	refs, tel := refresolve.ResolveKiwixFallback(
		context.Background(), already, noHit, refresolve.IntentImplement, "msg", stubKiwixErr(errors.New("kiwix down")),
	)
	if !tel.Fired {
		t.Errorf("expected Fired=true on kiwix error; got false")
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 refs on kiwix error; got %d", len(refs))
	}
	if tel.CandidatesReturned != 0 {
		t.Errorf("CandidatesReturned=%d, want 0", tel.CandidatesReturned)
	}
}

// Domain-term no-hit also qualifies (not only external-technical).
func TestResolveKiwixFallback_DomainTermNoHitQualifies(t *testing.T) {
	already := []refresolve.ResolvedReference{
		{Shape: refresolve.ShapeBugSlug, ConfidenceTier: refresolve.TierWeakDomain},
	}
	noHit := []refresolve.Reference{
		{Token: "context lasagne", Shape: refresolve.ShapeDomainTerm},
	}
	refs, tel := refresolve.ResolveKiwixFallback(
		context.Background(), already, noHit, refresolve.IntentImplement, "msg", stubKiwix(cannedKiwixHits[:1]),
	)
	if !tel.Fired {
		t.Errorf("expected fire on domain_term no-hit; got telemetry %+v", tel)
	}
	if len(refs) != 1 {
		t.Errorf("expected 1 ref; got %d", len(refs))
	}
}

// End-to-end through HandleParseContext: an unresolved external-
// technical token in the message with a stub search injected
// produces low-confidence fallback refs in the envelope.
func TestHandleParseContext_KiwixFallbackEndToEnd(t *testing.T) {
	pool := testutil.NewTestDB(t)
	registry := refresolve.NewRegistry()
	deps := refresolve.HandlerDeps{
		Pool:                  pool,
		Project:               "mcp-servers",
		Registry:              registry,
		Cache:                 refresolve.NewParseContextCache(),
		WorkStateCache:        refresolve.NewWorkStateCache(),
		DisciplineFireTracker: refresolve.NewDisciplineFireTracker(),
		KiwixFallbackSearch:   stubKiwix(cannedKiwixHits),
		// Deliberately omit DriftFireTracker: the test runs in the
		// repo's own Go test process, which the drift surface reads
		// as a live stdio MCP whose binary file may be marked deleted
		// after each test compile. shouldSurface() is nil-safe — no
		// tracker → no drift Candidate → no incidental TierSingleExact
		// that would suppress our gate.
	}
	ctx := events.WithMCPSessionID(context.Background(), "kiwix-fallback-e2e")
	// audit-intent message with a Title-Case multi-word phrase. audit
	// is chosen because its discipline map (vault-filing, suggestion-
	// filing) both gate on conditional predicates that don't match
	// this message — so the discipline surface stays silent and
	// doesn't add a TierSingleExact ref that would suppress the gate.
	// IntentImplement always surfaces coding-philosophy (single-exact)
	// which would suppress; IntentVerify and IntentFix similarly.
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "audit GoLand Project Structure usage in this repo"})
	r, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil {
		t.Fatal(err)
	}
	low := 0
	for _, ref := range r.References {
		if ref.ConfidenceTier == refresolve.TierLowConfidenceFallback {
			low++
		}
	}
	if low == 0 {
		t.Errorf("expected at least one low_confidence_fallback ref; got %d in envelope of %d refs",
			low, len(r.References))
	}
}
