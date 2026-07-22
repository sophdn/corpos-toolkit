package refresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"toolkit/internal/knowledge"
)

// vaultCandidateResolver bridges to knowledge.HandleVaultSearch for
// cross-project insights when the message's domain shape warrants.
// PARSE_CONTEXT §3.2.3.
//
// The detection-side rule (when to emit a ShapeVaultCandidate
// reference) is intentionally tight: only domain_term hits warrant
// the call, since vault search is one of the most expensive
// resolvers (Qwen-backed rerank). The gating logic lives in
// detect_vault_candidate.go (second-pass detector that consumes
// the primary domain_term references).
//
// Cost: ~1500ms typical (Qwen rerank). Cache policy:
// indefinite-within-session (vault content rarely changes mid-conv).
type vaultCandidateResolver struct {
	deps knowledge.Deps
}

// NewVaultCandidateResolver constructs the vault_candidate resolver
// with knowledge.Deps. Exported for tests.
func NewVaultCandidateResolver(deps knowledge.Deps) Resolver {
	return vaultCandidateResolver{deps: deps}
}

func (vaultCandidateResolver) Shape() ShapeCategory   { return ShapeVaultCandidate }
func (vaultCandidateResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 1500} }

func (r vaultCandidateResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	type vaultSearchParams struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	params, err := json.Marshal(vaultSearchParams{Query: ref.Token, TopK: 5})
	if err != nil {
		return HitSet{Err: fmt.Errorf("marshal vault_search params: %w", err)}, nil
	}
	result, err := knowledge.HandleVaultSearch(ctx, r.deps, params)
	if err != nil {
		return HitSet{Err: fmt.Errorf("vault_search: %w", err)}, nil
	}
	if result.Error != "" {
		return HitSet{Err: errors.New(result.Error)}, nil
	}
	out := HitSet{ResolverName: "vault_candidate"}
	if len(result.Results) == 0 {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	for _, entry := range result.Results {
		out.Candidates = append(out.Candidates, Candidate{
			ID:         entry.Path,
			Title:      entry.Title,
			Score:      1.0 - float64(entry.Rank)*0.1,
			SourceRef:  "vault:" + entry.Path,
			DebugNotes: fmt.Sprintf("rank=%d", entry.Rank),
		})
	}
	// Vault hits are heuristic by nature; even the top hit is a
	// "consider this" rather than a definitive binding. Surface as
	// weak_domain so the agent treats the candidates as suggestions.
	out.ConfidenceTier = TierWeakDomain
	return out, nil
}
