package refresolve

// classifyTier assigns a ConfidenceTier to a HitSet's Candidates
// per the rules in docs/REFERENCE_RESOLUTION.md §3.4. Single source
// of truth — resolvers do NOT classify their own tier; the
// dispatcher classifies after each Resolve returns.
//
// Inputs:
//   - candidates: the resolver's returned Candidates (may be empty)
//   - shape: the shape category being resolved (governs domain-term
//     special-case thresholds)
//
// Rules:
//   - 0 candidates                                  → TierNoHit
//   - 1 candidate with Score ≥ 0.95                 → TierSingleExact
//   - 2..5 candidates                                → TierFuzzyMulti
//   - 1 candidate with Score < 0.95                  → TierFuzzyMulti
//   - >5 candidates (any score distribution)        → TierFuzzyMulti
//
// Domain-term special case: when shape == ShapeDomainTerm,
// candidates with maximum Score < 0.5 reclassify to TierWeakDomain.
// External-technical mirrors the domain-term threshold: scores ≥
// 0.8 with a single hit promote to TierSingleExact, otherwise
// TierWeakDomain.
//
// Per-resolver tier thresholds (open question #2 in the design
// doc) would live in dispatch-policy.toml when wired; this file
// hard-codes the defaults. Tests assert each rule.
func classifyTier(candidates []Candidate, shape ShapeCategory) ConfidenceTier {
	n := len(candidates)
	if n == 0 {
		return TierNoHit
	}
	maxScore := candidates[0].Score
	for _, c := range candidates[1:] {
		if c.Score > maxScore {
			maxScore = c.Score
		}
	}

	switch shape {
	case ShapeDomainTerm:
		if maxScore < 0.5 {
			return TierWeakDomain
		}
		if n == 1 && maxScore >= 0.8 {
			return TierSingleExact
		}
		return TierFuzzyMulti
	case ShapeExternalTechnical:
		if n == 1 && maxScore >= 0.8 {
			return TierSingleExact
		}
		return TierWeakDomain
	default:
		if n == 1 && maxScore >= 0.95 {
			return TierSingleExact
		}
		return TierFuzzyMulti
	}
}
