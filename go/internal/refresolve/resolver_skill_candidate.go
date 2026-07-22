package refresolve

import "context"

// skillCandidateResolver handles ShapeSkillCandidate references —
// the weak-boundary sibling of skillTriggerResolver. Both resolvers
// consult the same SkillManifest's trigger index; the difference is
// upstream: detectSkillCandidate emits refs where boundaryOKCatalog
// rejects but boundaryOK accepts (keyword as prefix of a kebab token,
// e.g., "parse-context" inside the chain slug "parse-context-skill-...").
//
// The resolver delegates to skillTriggerResolver's lookup machinery
// (shared manifest index) and overrides only ResolverName so
// telemetry can distinguish strict vs weak-boundary surfacing.
//
// Recommendation softening happens downstream in formatResolved —
// when ShapeSkillCandidate lands with TierSingleExact, the action is
// PresentMentionAsPossiblyRelevant (not PresentUseDirectly), so the
// body inliner's eligibility check (Shape ∈ {SkillTrigger,
// DisciplineSkill} AND Tier == TierSingleExact) excludes this shape
// by the Shape filter — the weak-boundary path never inlines bodies.
type skillCandidateResolver struct {
	inner Resolver
}

// NewSkillCandidateResolver constructs the weak-boundary skill
// resolver. Internally wraps a skillTriggerResolver for the same
// manifest so the trigger-keyword → skill lookup is shared.
func NewSkillCandidateResolver(manifest *SkillManifest) Resolver {
	return skillCandidateResolver{inner: NewSkillTriggerResolver(manifest)}
}

func (r skillCandidateResolver) Shape() ShapeCategory   { return ShapeSkillCandidate }
func (r skillCandidateResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 5} }

func (r skillCandidateResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	hs, err := r.inner.Resolve(ctx, ref)
	if err == nil {
		hs.ResolverName = "skill_candidate"
	}
	return hs, err
}
