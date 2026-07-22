package refresolve

import (
	"context"
	"fmt"
	"sort"
)

// skillTriggerResolver maps a trigger keyword to the skill(s) that
// declare it. Powered by the skill manifest read at LoadCatalogs
// time. Reference-resolution-migration T5.
//
// Cost is ~5ms — pure in-memory map lookup. The resolver returns a
// HitSet with one Candidate per matching skill; multi-match
// (two skills sharing a keyword) is rare but legal — the dispatcher
// produces TierFuzzyMulti in that case, asking the agent to pick.
type skillTriggerResolver struct {
	manifest *SkillManifest
}

// NewSkillTriggerResolver constructs the skill_trigger resolver with
// an explicit manifest. Exported so tests outside the package can
// build registries without going through BuildProductionRegistry.
func NewSkillTriggerResolver(manifest *SkillManifest) Resolver {
	return skillTriggerResolver{manifest: manifest}
}

func (r skillTriggerResolver) Shape() ShapeCategory   { return ShapeSkillTrigger }
func (r skillTriggerResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 5} }

func (r skillTriggerResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	out := HitSet{ResolverName: "skill_trigger"}
	if r.manifest == nil {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	idx := r.manifest.TriggerIndex()
	entries := idx[ref.Token]
	if len(entries) == 0 {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	// Deterministic order for tests + telemetry: sort by skill name.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for _, e := range entries {
		out.Candidates = append(out.Candidates, Candidate{
			ID:         e.Name,
			Title:      "skill " + e.Name,
			Score:      1.0,
			SourceRef:  "skill:" + e.BodyPath,
			DebugNotes: fmt.Sprintf("bucket=%s trigger=%s", e.Bucket, ref.Token),
		})
	}
	if len(out.Candidates) == 1 {
		out.ConfidenceTier = TierSingleExact
	} else {
		out.ConfidenceTier = TierFuzzyMulti
	}
	return out, nil
}
