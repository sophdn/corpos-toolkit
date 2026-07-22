package refresolve

import (
	"context"
)

// disciplineSkillResolver surfaces discipline-skill bodies when their
// trigger condition fires on the detected reference set. The
// load-bearing innovation that lets disciplines move from ambient-
// loaded to lazy (PARSE_CONTEXT §3.2.5).
//
// Phase 3 (this commit) ships the resolver shell + the friction_shape
// → bug-filing-discipline trigger, which is the only trigger condition
// active today via the substrate's existing friction detector.
// Additional trigger conditions (vault-pull-discipline on domain_term
// hits, coding-philosophy on code-being-written shape, etc.) land in
// follow-on commits as each trigger condition's source-of-truth is
// formalized (PARSE_CONTEXT §11 open item 2).
//
// Cache policy: PolicyReEvaluatePerCall — the trigger condition is a
// shape-match check that's cheap to re-evaluate every call; caching
// adds no benefit (PARSE_CONTEXT §3.3).
type disciplineSkillResolver struct {
	manifest *SkillManifest
}

// NewDisciplineSkillResolver constructs the discipline_skill resolver
// with an explicit manifest. Exported so tests outside the package
// can build registries without going through BuildProductionRegistry.
func NewDisciplineSkillResolver(manifest *SkillManifest) Resolver {
	return disciplineSkillResolver{manifest: manifest}
}

func (r disciplineSkillResolver) Shape() ShapeCategory   { return ShapeDisciplineSkill }
func (r disciplineSkillResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 5} }

// Resolve looks up the discipline-skill named by ref.Token in the
// manifest. The detector (detectDisciplineSkill) is responsible for
// emitting ShapeDisciplineSkill references with Token=discipline-skill-name
// when a trigger condition fires. The resolver just maps name → body.
func (r disciplineSkillResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	out := HitSet{ResolverName: "discipline_skill"}
	if r.manifest == nil {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	for _, entry := range r.manifest.Skills {
		if entry.Name == ref.Token {
			out.Candidates = []Candidate{{
				ID:         entry.Name,
				Title:      "discipline " + entry.Name,
				Score:      1.0,
				SourceRef:  "skill:" + entry.BodyPath,
				DebugNotes: "triggered_by=" + ref.DetectionMethod,
			}}
			out.ConfidenceTier = TierSingleExact
			return out, nil
		}
	}
	out.ConfidenceTier = TierNoHit
	return out, nil
}
