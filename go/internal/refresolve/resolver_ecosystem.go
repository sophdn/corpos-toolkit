package refresolve

import (
	"context"

	"toolkit/internal/db"
	"toolkit/internal/ecosystem"
)

// ecosystemResolver answers a ShapeEcosystemToken reference with the
// deterministic access summary from the local-ecosystem service. It calls the
// SAME ecosystem.ResolveAccess the ecosystem.access_check action calls, so the
// parse_context orient-time answer and the explicit query can never diverge —
// the load-bearing property for killing the cold-agent correction loop (chain
// 435).
//
// Cache policy: the ecosystem tokens ride in the Catalogs snapshot (loaded like
// chain/task/bug slugs); the resolver itself queries the live DB per call, so a
// just-learned fact is answered as soon as the catalog refreshes.
type ecosystemResolver struct {
	pool *db.Pool
}

// NewEcosystemResolver constructs the ecosystem resolver. Pass a nil pool to
// ship the shell-only no-op (tests / partial wiring). Exported for tests.
func NewEcosystemResolver(pool *db.Pool) Resolver {
	return ecosystemResolver{pool: pool}
}

func (ecosystemResolver) Shape() ShapeCategory   { return ShapeEcosystemToken }
func (ecosystemResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 20} }

func (r ecosystemResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	out := HitSet{ResolverName: "ecosystem"}
	if r.pool == nil {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	sum, err := ecosystem.ResolveAccess(ctx, r.pool.DB(), ref.Token)
	if err != nil {
		out.Err = err
		out.ConfidenceTier = TierNoHit
		return out, err
	}
	if !sum.Resolved {
		// The token was in the catalog but no longer resolves (e.g. retired);
		// leave it unbound rather than assert a stale answer.
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	out.Candidates = append(out.Candidates, Candidate{
		ID:         sum.Slug,
		Title:      sum.Answer,
		Score:      1.0,
		SourceRef:  "ecosystem:" + sum.Slug,
		DebugNotes: "status=" + string(sum.Status) + " kind=" + sum.Kind,
	})
	out.ConfidenceTier = TierSingleExact
	return out, nil
}
