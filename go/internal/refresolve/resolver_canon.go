package refresolve

import (
	"context"

	"toolkit/internal/db"
	"toolkit/internal/ecosystem"
)

// canonResolver answers a ShapeCanonToken reference with the current canonical
// identity from the ecosystem canon map. It calls the SAME ecosystem.ResolveCanon
// the ecosystem.canon_resolve action calls, so the parse_context orient-time
// answer and the explicit query can never diverge. This is what surfaces "that
// name is retired -> canonical is X" the moment a stale token appears in a
// message, before stale-canon can propagate.
type canonResolver struct {
	pool *db.Pool
}

// NewCanonResolver constructs the canon resolver. Pass a nil pool to ship the
// shell-only no-op (tests / partial wiring). Exported for tests.
func NewCanonResolver(pool *db.Pool) Resolver {
	return canonResolver{pool: pool}
}

func (canonResolver) Shape() ShapeCategory   { return ShapeCanonToken }
func (canonResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 20} }

func (r canonResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	out := HitSet{ResolverName: "canon"}
	if r.pool == nil {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	rec, err := ecosystem.ResolveCanon(ctx, r.pool.DB(), ref.Token)
	if err != nil {
		out.Err = err
		out.ConfidenceTier = TierNoHit
		return out, err
	}
	if !rec.Resolved {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	out.Candidates = append(out.Candidates, Candidate{
		ID:         rec.Slug,
		Title:      rec.Answer,
		Score:      1.0,
		SourceRef:  "canon:" + rec.Slug,
		DebugNotes: "status=" + rec.Status + " kind=" + rec.Kind,
	})
	out.ConfidenceTier = TierSingleExact
	return out, nil
}
