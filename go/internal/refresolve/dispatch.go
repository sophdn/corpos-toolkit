package refresolve

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DispatchOptions tunes the dispatcher's per-call behavior. Zero
// values give the design-doc defaults.
type DispatchOptions struct {
	// TotalBudget caps the dispatcher's wall-clock per call. Zero
	// uses 2 seconds (design doc §3.6).
	TotalBudget time.Duration
	// PerResolverMultiplier caps each resolver's runtime as
	// PerResolverMultiplier × Cost().TypicalMs. Zero uses 4.
	PerResolverMultiplier int
	// MaxCandidatesPerResolver bounds the candidate count each
	// resolver may return after dispatcher post-processing. Zero
	// uses 10 (T3 acceptance criterion).
	MaxCandidatesPerResolver int
}

// applyDefaults fills zero-valued fields with the design defaults.
func (o DispatchOptions) applyDefaults() DispatchOptions {
	if o.TotalBudget == 0 {
		o.TotalBudget = 2 * time.Second
	}
	if o.PerResolverMultiplier == 0 {
		o.PerResolverMultiplier = 4
	}
	if o.MaxCandidatesPerResolver == 0 {
		o.MaxCandidatesPerResolver = 10
	}
	return o
}

// ErrResolverBudgetExceeded marks a HitSet produced by a resolver
// that ran past its per-resolver budget. The dispatcher surfaces
// this on HitSet.Err so the action handler can report it to the
// caller without aborting other resolvers.
var ErrResolverBudgetExceeded = errors.New("resolver budget exceeded")

// ErrTotalBudgetExceeded marks resolvers that didn't run because
// the dispatcher hit its total budget cap before reaching them.
var ErrTotalBudgetExceeded = errors.New("dispatcher total budget exceeded")

// Dispatch is the orchestration entrypoint for T3. Given a list of
// Reference outputs from Detect, it consults the registry and
// returns the resolved HitSet per Reference.
//
// Semantics:
//
//  1. Resolvers run in priority order (per shapePriority). Higher-
//     priority shapes resolve first.
//  2. For tokens that match multiple shapes, the dispatcher short-
//     circuits lower-priority resolvers when a higher-priority
//     resolver returns TierSingleExact. Different tokens have
//     independent short-circuit state.
//  3. Resolver failures (HitSet.Err non-nil from Resolve, OR Go
//     error return) DO NOT abort other resolvers — best-effort
//     dispatch. The failure surfaces on the per-Reference HitSet.
//  4. Each resolver runs under a per-resolver context with a
//     timeout of opts.PerResolverMultiplier × Cost().TypicalMs.
//  5. The dispatcher itself runs under a total budget; resolvers
//     not reached when the budget fires get a HitSet with
//     ErrTotalBudgetExceeded.
//  6. Per-resolver candidate lists trim to
//     opts.MaxCandidatesPerResolver entries (preserving order; the
//     resolver is responsible for ranking).
//
// Returns a map keyed by Reference. If a Reference has no
// registered resolver for its shape, its map value is a HitSet
// with TierNoHit, ResolverName empty, and Err set to a
// "no resolver" error.
func Dispatch(ctx context.Context, registry *Registry, refs []Reference, opts DispatchOptions) (map[Reference]HitSet, error) {
	if registry == nil {
		return nil, errors.New("registry required")
	}
	opts = opts.applyDefaults()

	out := make(map[Reference]HitSet, len(refs))
	if len(refs) == 0 {
		return out, nil
	}

	ctx, cancel := context.WithTimeout(ctx, opts.TotalBudget)
	defer cancel()

	// Group references by Token so we can apply per-token short-
	// circuit. Order within each token is shape-priority (input
	// refs from Detect are already sorted by StartPos then
	// priority; preserve that order within each token group).
	groups := groupByToken(refs)

	// Resolve each token group sequentially (token groups are
	// independent; a token group's resolvers run in priority
	// order). Sequential keeps the implementation simple; if
	// latency becomes a problem, parallelizing across token groups
	// is the obvious extension.
	for _, group := range groups {
		shortCircuited := false
		for _, ref := range group {
			if shortCircuited {
				out[ref] = HitSet{
					ResolverName:   "",
					ConfidenceTier: TierNoHit,
					Candidates:     nil,
					// Note: short-circuit is a normal control-flow
					// outcome, not an error.
				}
				continue
			}
			if ctx.Err() != nil {
				out[ref] = HitSet{
					ConfidenceTier: TierNoHit,
					Err:            ErrTotalBudgetExceeded,
				}
				continue
			}
			res, ok := registry.Get(ref.Shape)
			if !ok {
				out[ref] = HitSet{
					ConfidenceTier: TierNoHit,
					Err:            fmt.Errorf("no resolver registered for shape %q", ref.Shape),
				}
				continue
			}
			hs := runResolverBounded(ctx, res, ref, opts)
			// Trim candidates to MaxCandidatesPerResolver.
			if len(hs.Candidates) > opts.MaxCandidatesPerResolver {
				hs.Candidates = hs.Candidates[:opts.MaxCandidatesPerResolver]
			}
			// Tier classification — single source of truth lives
			// here; resolvers don't classify their own tier.
			hs.ConfidenceTier = classifyTier(hs.Candidates, ref.Shape)
			out[ref] = hs

			// Short-circuit lower-priority resolvers for the same
			// token if this hit is single_exact.
			if hs.ConfidenceTier == TierSingleExact {
				shortCircuited = true
			}
		}
	}
	return out, nil
}

// runResolverBounded calls res.Resolve under a per-resolver
// context timeout. Captures the wall-clock cost; on context
// cancellation, returns ErrResolverBudgetExceeded on the HitSet.
func runResolverBounded(ctx context.Context, res Resolver, ref Reference, opts DispatchOptions) HitSet {
	budget := time.Duration(res.Cost().TypicalMs) * time.Duration(opts.PerResolverMultiplier) * time.Millisecond
	if budget <= 0 {
		// Resolver didn't declare a cost; give it a generous
		// default so reasonable resolvers still complete.
		budget = 500 * time.Millisecond
	}
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type resolveResult struct {
		hs  HitSet
		err error
	}
	done := make(chan resolveResult, 1)
	start := time.Now()
	go func() {
		hs, err := res.Resolve(rctx, ref)
		done <- resolveResult{hs: hs, err: err}
	}()
	select {
	case rr := <-done:
		cost := time.Since(start).Milliseconds()
		rr.hs.ResolverName = string(res.Shape())
		rr.hs.RetrievalCostMs = cost
		// Convert Go error from Resolve into HitSet.Err per the
		// design doc §3.3 contract: programmer errors propagate
		// upward, but tool-call errors are recorded on the HitSet
		// and dispatch continues.
		if rr.err != nil && rr.hs.Err == nil {
			rr.hs.Err = rr.err
		}
		return rr.hs
	case <-rctx.Done():
		cost := time.Since(start).Milliseconds()
		return HitSet{
			ResolverName:    string(res.Shape()),
			ConfidenceTier:  TierNoHit,
			Candidates:      nil,
			RetrievalCostMs: cost,
			Err:             ErrResolverBudgetExceeded,
		}
	}
}

// groupByToken partitions refs into per-Token groups, preserving
// input order within each group. Tokens with multiple shapes
// produce groups whose entries appear in input order (which Detect
// guarantees is shape-priority order).
func groupByToken(refs []Reference) [][]Reference {
	if len(refs) == 0 {
		return nil
	}
	// Preserve a first-seen-position ordering for the groups so
	// callers reading the result map don't see surprising
	// reordering across runs.
	seen := make(map[string]int, len(refs))
	groups := [][]Reference{}
	for _, r := range refs {
		idx, ok := seen[r.Token]
		if !ok {
			seen[r.Token] = len(groups)
			groups = append(groups, []Reference{r})
		} else {
			groups[idx] = append(groups[idx], r)
		}
	}
	return groups
}

// _unused references the sync package so future helpers that need
// concurrency primitives don't see an immediate import-cycle.
var _ = sync.Mutex{}
