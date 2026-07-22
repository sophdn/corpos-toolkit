package refresolve_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"toolkit/internal/refresolve"
)

// mockResolver is a configurable Resolver for dispatch tests.
// Each test constructs it with the shape it covers, the HitSet to
// return (often varying ConfidenceTier to drive short-circuit
// logic), an optional error, and an optional sleep so latency-cap
// tests can force budget exhaustion.
type mockResolver struct {
	shape      refresolve.ShapeCategory
	candidates []refresolve.Candidate
	resolveErr error
	sleep      time.Duration
	typicalMs  int64
	calls      int // mutated under test goroutines; tests run them sequentially
}

func (m *mockResolver) Shape() refresolve.ShapeCategory { return m.shape }
func (m *mockResolver) Cost() refresolve.ResolverCostHint {
	if m.typicalMs == 0 {
		return refresolve.ResolverCostHint{TypicalMs: 10}
	}
	return refresolve.ResolverCostHint{TypicalMs: m.typicalMs}
}
func (m *mockResolver) Resolve(ctx context.Context, _ refresolve.Reference) (refresolve.HitSet, error) {
	m.calls++
	if m.sleep > 0 {
		select {
		case <-time.After(m.sleep):
		case <-ctx.Done():
			return refresolve.HitSet{}, ctx.Err()
		}
	}
	if m.resolveErr != nil {
		return refresolve.HitSet{}, m.resolveErr
	}
	return refresolve.HitSet{Candidates: m.candidates}, nil
}

// Acceptance (a): each resolver receives its Reference and returns
// a correctly-shaped HitSet.
func TestDispatch_ResolverShape(t *testing.T) {
	registry := refresolve.NewRegistry()
	chainHit := &mockResolver{
		shape: refresolve.ShapeChainSlug,
		candidates: []refresolve.Candidate{{
			ID: "alpha", Title: "alpha chain", Score: 1.0, SourceRef: "chain:alpha",
		}},
	}
	registry.Register(chainHit)

	ref := refresolve.Reference{Token: "alpha", Shape: refresolve.ShapeChainSlug, Confidence: 1.0}
	result, err := refresolve.Dispatch(context.Background(), registry, []refresolve.Reference{ref}, refresolve.DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	hs, ok := result[ref]
	if !ok {
		t.Fatalf("missing result for %+v", ref)
	}
	if len(hs.Candidates) != 1 {
		t.Errorf("want 1 candidate, got %d", len(hs.Candidates))
	}
	if hs.ConfidenceTier != refresolve.TierSingleExact {
		t.Errorf("want single_exact, got %s", hs.ConfidenceTier)
	}
	if hs.ResolverName != string(refresolve.ShapeChainSlug) {
		t.Errorf("ResolverName: %q", hs.ResolverName)
	}
	if chainHit.calls != 1 {
		t.Errorf("want 1 call, got %d", chainHit.calls)
	}
}

// Acceptance (b): Dispatch orchestrates priority — a token
// matching multiple shapes with a TierSingleExact on the higher
// priority shape short-circuits the lower-priority resolver.
func TestDispatch_PriorityShortCircuit(t *testing.T) {
	registry := refresolve.NewRegistry()
	chainR := &mockResolver{
		shape: refresolve.ShapeChainSlug,
		candidates: []refresolve.Candidate{{
			ID: "alpha", Title: "alpha chain", Score: 1.0, SourceRef: "chain:alpha",
		}},
	}
	taskR := &mockResolver{
		shape: refresolve.ShapeTaskSlug,
		candidates: []refresolve.Candidate{{
			ID: "alpha", Title: "alpha task", Score: 1.0, SourceRef: "task:alpha",
		}},
	}
	registry.Register(chainR)
	registry.Register(taskR)

	chainRef := refresolve.Reference{Token: "alpha", Shape: refresolve.ShapeChainSlug, Confidence: 1.0}
	taskRef := refresolve.Reference{Token: "alpha", Shape: refresolve.ShapeTaskSlug, Confidence: 1.0}

	result, err := refresolve.Dispatch(context.Background(), registry,
		[]refresolve.Reference{chainRef, taskRef}, refresolve.DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if chainR.calls != 1 {
		t.Errorf("chain resolver should run, calls=%d", chainR.calls)
	}
	if taskR.calls != 0 {
		t.Errorf("task resolver should be short-circuited, calls=%d", taskR.calls)
	}
	chainHS := result[chainRef]
	taskHS := result[taskRef]
	if chainHS.ConfidenceTier != refresolve.TierSingleExact {
		t.Errorf("chain tier: %s", chainHS.ConfidenceTier)
	}
	if taskHS.ConfidenceTier != refresolve.TierNoHit {
		t.Errorf("task tier (short-circuited): %s", taskHS.ConfidenceTier)
	}
}

// Acceptance (c): a resolver failure returns a HitSet with
// ConfidenceTier=TierNoHit and the error in the Err field; does
// NOT abort other resolvers.
func TestDispatch_ResolverFailureBestEffort(t *testing.T) {
	registry := refresolve.NewRegistry()
	failR := &mockResolver{
		shape:      refresolve.ShapeChainSlug,
		resolveErr: errors.New("simulated DB timeout"),
	}
	okR := &mockResolver{
		shape: refresolve.ShapeBugSlug,
		candidates: []refresolve.Candidate{{
			ID: "buggy", Title: "buggy bug", Score: 1.0, SourceRef: "bug:buggy",
		}},
	}
	registry.Register(failR)
	registry.Register(okR)

	failRef := refresolve.Reference{Token: "alpha", Shape: refresolve.ShapeChainSlug, Confidence: 1.0}
	okRef := refresolve.Reference{Token: "buggy", Shape: refresolve.ShapeBugSlug, Confidence: 1.0}
	result, err := refresolve.Dispatch(context.Background(), registry,
		[]refresolve.Reference{failRef, okRef}, refresolve.DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	failHS := result[failRef]
	if failHS.ConfidenceTier != refresolve.TierNoHit {
		t.Errorf("failing resolver should produce no_hit, got %s", failHS.ConfidenceTier)
	}
	if failHS.Err == nil {
		t.Errorf("failing resolver should record Err on HitSet")
	}
	okHS := result[okRef]
	if okHS.ConfidenceTier != refresolve.TierSingleExact {
		t.Errorf("non-failing resolver should still hit, got %s", okHS.ConfidenceTier)
	}
}

// Acceptance (d): dispatcher caps total resolution time per call.
// A resolver that sleeps longer than its per-resolver budget gets
// short-circuited; the dispatcher records ErrResolverBudgetExceeded.
func TestDispatch_PerResolverBudgetExceeded(t *testing.T) {
	registry := refresolve.NewRegistry()
	slow := &mockResolver{
		shape:     refresolve.ShapeDomainTerm,
		sleep:     200 * time.Millisecond,
		typicalMs: 5, // budget = 5 * 4 = 20ms; sleep is 200ms
	}
	registry.Register(slow)
	ref := refresolve.Reference{Token: "Slow Thing", Shape: refresolve.ShapeDomainTerm, Confidence: 0.8}
	result, err := refresolve.Dispatch(context.Background(), registry,
		[]refresolve.Reference{ref}, refresolve.DispatchOptions{TotalBudget: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	hs := result[ref]
	if hs.ConfidenceTier != refresolve.TierNoHit {
		t.Errorf("budget-exceeded resolver: tier %s", hs.ConfidenceTier)
	}
	if !errors.Is(hs.Err, refresolve.ErrResolverBudgetExceeded) {
		t.Errorf("Err: %v want %v", hs.Err, refresolve.ErrResolverBudgetExceeded)
	}
}

// Total budget exhaustion — a long-running first resolver eats the
// whole dispatcher budget; subsequent resolvers see ctx canceled
// and report ErrTotalBudgetExceeded.
func TestDispatch_TotalBudgetExceeded(t *testing.T) {
	registry := refresolve.NewRegistry()
	slow := &mockResolver{
		shape:     refresolve.ShapeChainSlug,
		sleep:     200 * time.Millisecond,
		typicalMs: 1000, // per-resolver budget = 4s, won't trip
	}
	other := &mockResolver{
		shape:      refresolve.ShapeBugSlug,
		candidates: []refresolve.Candidate{{ID: "x", Title: "x", Score: 1.0}},
	}
	registry.Register(slow)
	registry.Register(other)
	slowRef := refresolve.Reference{Token: "slow-chain", Shape: refresolve.ShapeChainSlug, Confidence: 1.0}
	otherRef := refresolve.Reference{Token: "x", Shape: refresolve.ShapeBugSlug, Confidence: 1.0}

	result, err := refresolve.Dispatch(context.Background(), registry,
		[]refresolve.Reference{slowRef, otherRef},
		refresolve.DispatchOptions{TotalBudget: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	otherHS := result[otherRef]
	if !errors.Is(otherHS.Err, refresolve.ErrTotalBudgetExceeded) {
		t.Errorf("second resolver should see total-budget-exceeded; got %v", otherHS.Err)
	}
}

// Candidates exceeding MaxCandidatesPerResolver are trimmed.
func TestDispatch_CandidateLimit(t *testing.T) {
	registry := refresolve.NewRegistry()
	manyCands := make([]refresolve.Candidate, 25)
	for i := range manyCands {
		manyCands[i] = refresolve.Candidate{ID: "x", Title: "x", Score: 0.5}
	}
	r := &mockResolver{shape: refresolve.ShapeChainSlug, candidates: manyCands}
	registry.Register(r)
	ref := refresolve.Reference{Token: "x", Shape: refresolve.ShapeChainSlug}
	result, err := refresolve.Dispatch(context.Background(), registry,
		[]refresolve.Reference{ref}, refresolve.DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	hs := result[ref]
	if len(hs.Candidates) != 10 {
		t.Errorf("want 10 candidates after trim, got %d", len(hs.Candidates))
	}
}

// No-resolver-registered Reference returns no_hit + descriptive error.
func TestDispatch_NoResolverForShape(t *testing.T) {
	registry := refresolve.NewRegistry()
	ref := refresolve.Reference{Token: "foo", Shape: refresolve.ShapeForgeSchema}
	result, err := refresolve.Dispatch(context.Background(), registry,
		[]refresolve.Reference{ref}, refresolve.DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	hs := result[ref]
	if hs.ConfidenceTier != refresolve.TierNoHit {
		t.Errorf("tier: %s", hs.ConfidenceTier)
	}
	if hs.Err == nil {
		t.Errorf("expected error about missing resolver")
	}
}

// Tier classification rules are the dispatcher's responsibility —
// not per-resolver. Test the dispatch-layer classification for
// each tier rule.
func TestDispatch_TierClassification(t *testing.T) {
	tests := []struct {
		name       string
		shape      refresolve.ShapeCategory
		candidates []refresolve.Candidate
		wantTier   refresolve.ConfidenceTier
	}{
		{
			name:       "single_exact_chain",
			shape:      refresolve.ShapeChainSlug,
			candidates: []refresolve.Candidate{{Score: 1.0}},
			wantTier:   refresolve.TierSingleExact,
		},
		{
			name:       "single_low_score_to_fuzzy",
			shape:      refresolve.ShapeChainSlug,
			candidates: []refresolve.Candidate{{Score: 0.7}},
			wantTier:   refresolve.TierFuzzyMulti,
		},
		{
			name:       "multi_to_fuzzy",
			shape:      refresolve.ShapeChainSlug,
			candidates: []refresolve.Candidate{{Score: 1.0}, {Score: 1.0}},
			wantTier:   refresolve.TierFuzzyMulti,
		},
		{
			name:       "domain_term_weak",
			shape:      refresolve.ShapeDomainTerm,
			candidates: []refresolve.Candidate{{Score: 0.3}},
			wantTier:   refresolve.TierWeakDomain,
		},
		{
			name:       "domain_term_single_exact",
			shape:      refresolve.ShapeDomainTerm,
			candidates: []refresolve.Candidate{{Score: 0.85}},
			wantTier:   refresolve.TierSingleExact,
		},
		{
			name:       "no_candidates",
			shape:      refresolve.ShapeChainSlug,
			candidates: nil,
			wantTier:   refresolve.TierNoHit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := refresolve.NewRegistry()
			registry.Register(&mockResolver{
				shape:      tt.shape,
				candidates: tt.candidates,
			})
			ref := refresolve.Reference{Token: "x", Shape: tt.shape, Confidence: 1.0}
			result, err := refresolve.Dispatch(context.Background(), registry,
				[]refresolve.Reference{ref}, refresolve.DispatchOptions{})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			hs := result[ref]
			if hs.ConfidenceTier != tt.wantTier {
				t.Errorf("tier: got %s want %s", hs.ConfidenceTier, tt.wantTier)
			}
		})
	}
}

// Registry.All returns resolvers in shape-priority order.
func TestRegistry_AllSortedByPriority(t *testing.T) {
	registry := refresolve.NewRegistry()
	registry.Register(&mockResolver{shape: refresolve.ShapeDomainTerm})
	registry.Register(&mockResolver{shape: refresolve.ShapeChainSlug})
	registry.Register(&mockResolver{shape: refresolve.ShapeBugSlug})
	shapes := registry.Shapes()
	if len(shapes) != 3 {
		t.Fatalf("want 3 shapes, got %d", len(shapes))
	}
	wantOrder := []refresolve.ShapeCategory{
		refresolve.ShapeChainSlug,
		refresolve.ShapeBugSlug,
		refresolve.ShapeDomainTerm,
	}
	for i, want := range wantOrder {
		if shapes[i] != want {
			t.Errorf("Shapes()[%d]: got %s want %s", i, shapes[i], want)
		}
	}
}

// Empty refs list returns empty map, no error.
func TestDispatch_EmptyRefs(t *testing.T) {
	registry := refresolve.NewRegistry()
	result, err := refresolve.Dispatch(context.Background(), registry, nil, refresolve.DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("want empty result, got %d entries", len(result))
	}
}

// Nil registry returns error (programmer mistake — not a runtime
// degradation).
func TestDispatch_NilRegistry(t *testing.T) {
	_, err := refresolve.Dispatch(context.Background(), nil, nil, refresolve.DispatchOptions{})
	if err == nil {
		t.Errorf("want error for nil registry")
	}
}
