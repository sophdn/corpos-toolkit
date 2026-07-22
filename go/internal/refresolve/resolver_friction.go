package refresolve

import (
	"context"
	"fmt"
)

// frictionResolver returns a filing suggestion rather than a
// binding. The detector flagged the message as containing observed
// friction; the resolver's job is to surface "consider filing as
// a bug via forge(...)" at the agent's natural decision point —
// the supersession story for the friction-filing-reminder Stop
// hook.
//
// Per design doc §7 + open question #3: this resolver is the
// "uniform-contract" path — every shape goes through
// Resolve(ctx, ref) (HitSet, error), even when the shape produces
// a suggestion instead of a candidate-list binding. The Candidate
// here carries the filing template as DebugNotes; the handler's
// presentSingleExact formatter for ShapeFrictionShape uses that
// to compose the PresentedAs string.
type frictionResolver struct{}

func (frictionResolver) Shape() ShapeCategory   { return ShapeFrictionShape }
func (frictionResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 1} }

func (frictionResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	// Return a single Candidate so the dispatcher classifies as
	// TierSingleExact (single hit, score 1.0). The handler's
	// formatter recognizes ShapeFrictionShape and produces a
	// filing-suggestion PresentedAs string.
	return HitSet{
		Candidates: []Candidate{{
			ID:         "filing-suggestion",
			Title:      "consider filing as a bug",
			Score:      1.0,
			SourceRef:  "suggestion:forge-bug",
			DebugNotes: fmt.Sprintf("matched friction phrase: %q", ref.Token),
		}},
	}, nil
}
