package refresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"toolkit/internal/knowledge"
)

// kiwixBridgeResolver bridges to knowledge.HandleKnowledgeSearch
// (the unified surface that already routes across kiwix references)
// for offline-doc snippets when external_technical confidence
// warrants. PARSE_CONTEXT §3.2.4.
//
// Going through knowledge_search rather than kiwix_search direct
// avoids the per-corpus ZIM-routing problem that kiwix_search needs
// solved at the caller (kiwix_search requires zim_id). The unified
// surface already handles that and exposes kiwix-sourced hits
// alongside vault/library/slug pointers; we filter to
// source_type="kiwix_reference" so the bridge surfaces only kiwix
// candidates.
type kiwixBridgeResolver struct {
	deps    knowledge.Deps
	project string
}

// NewKiwixBridgeResolver constructs the kiwix_bridge resolver with
// knowledge.Deps + project scope. Exported for tests.
func NewKiwixBridgeResolver(deps knowledge.Deps, project string) Resolver {
	return kiwixBridgeResolver{deps: deps, project: project}
}

func (kiwixBridgeResolver) Shape() ShapeCategory   { return ShapeKiwixBridge }
func (kiwixBridgeResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 500} }

func (r kiwixBridgeResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	type knowledgeSearchParams struct {
		Query  string `json:"query"`
		TopK   int    `json:"top_k"`
		Source string `json:"source"`
	}
	params, err := json.Marshal(knowledgeSearchParams{
		Query:  ref.Token,
		TopK:   5,
		Source: "reference_resolution",
	})
	if err != nil {
		return HitSet{Err: fmt.Errorf("marshal knowledge_search params: %w", err)}, nil
	}
	result, err := knowledge.HandleKnowledgeSearch(ctx, r.deps, r.project, params)
	if err != nil {
		return HitSet{Err: fmt.Errorf("knowledge_search: %w", err)}, nil
	}
	if result.Error != "" {
		return HitSet{Err: errors.New(result.Error)}, nil
	}
	out := HitSet{ResolverName: "kiwix_bridge"}
	for i, p := range result.Results {
		if p.SourceType != "kiwix_reference" {
			continue
		}
		score := 0.0
		if p.QualityScore != nil {
			score = *p.QualityScore
		}
		positionDecay := 1.0 - float64(i)*0.1
		if positionDecay < 0.1 {
			positionDecay = 0.1
		}
		out.Candidates = append(out.Candidates, Candidate{
			ID:         p.SourceRef,
			Title:      p.Question,
			Score:      score * positionDecay,
			SourceRef:  "kiwix:" + p.SourceRef,
			DebugNotes: fmt.Sprintf("invoke_when=%s usage=%d", p.InvokeWhen, p.UsageCount),
		})
	}
	if len(out.Candidates) == 0 {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	// Kiwix hits are best treated as "for further reading" —
	// weak_domain matches the dispatcher's existing convention for
	// external technical lookups.
	out.ConfidenceTier = TierWeakDomain
	return out, nil
}
