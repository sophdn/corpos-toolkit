package refresolve_test

import (
	"context"
	"testing"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// frictionTestCases is the canonical set of representative inputs
// used for both the unit tests below AND the supersession
// verification document at
// process-docs/adhoc/reference-resolution-friction-supersession-verification.md.
//
// Keep in sync with that doc — same phrases, same expected match
// substrings.
var frictionTestCases = []struct {
	name    string
	message string
	wantHit bool
	wantSub string // expected matched substring (case may vary; matcher is case-insensitive)
}{
	{
		name:    "phrase_paper_cut",
		message: "also noted: the gofmt drift on bug.go is a paper cut.",
		wantHit: true,
		wantSub: "also noted",
	},
	{
		name:    "phrase_could_file",
		message: "the dispatch error message could file as a separate bug.",
		wantHit: true,
		wantSub: "could file",
	},
	{
		name:    "phrase_workaround",
		message: "I had to add a workaround for the migration ordering.",
		wantHit: true,
		wantSub: "workaround",
	},
	{
		name:    "phrase_shouldnt_have_to",
		message: "we shouldn't have to remember to run scripts/sync-migrations.sh by hand.",
		wantHit: true,
		wantSub: "shouldn't have to",
	},
	{
		name:    "phrase_thats_weird",
		message: "That's weird — the test failed but the binary built clean.",
		wantHit: true,
		wantSub: "That's weird",
	},
	{
		name:    "no_friction_in_neutral_message",
		message: "Working on chain-name and reviewing the task list.",
		wantHit: false,
	},
	{
		name:    "no_friction_in_trivial_message",
		message: "thanks, do it.",
		wantHit: false,
	},
}

// Friction-shape detection — each canonical input produces (or
// doesn't produce) a ShapeFrictionShape Reference per the table.
func TestDetect_FrictionShapePatterns(t *testing.T) {
	for _, tc := range frictionTestCases {
		t.Run(tc.name, func(t *testing.T) {
			d := refresolve.NewDetector(refresolve.Catalogs{}, nil)
			refs, err := d.Detect(context.Background(), tc.message)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			var got *refresolve.Reference
			for i := range refs {
				if refs[i].Shape == refresolve.ShapeFrictionShape {
					got = &refs[i]
					break
				}
			}
			if tc.wantHit {
				if got == nil {
					t.Fatalf("want friction_shape reference, got none (all: %+v)", refs)
				}
				if tc.wantSub != "" && got.Token != tc.wantSub &&
					!substringMatchesCaseInsensitive(got.Token, tc.wantSub) {
					t.Errorf("Token: got %q, want substring %q", got.Token, tc.wantSub)
				}
			} else {
				if got != nil {
					t.Errorf("unexpected friction_shape reference: %+v", *got)
				}
			}
		})
	}
}

func substringMatchesCaseInsensitive(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle ||
			containsCI(haystack, needle))
}
func containsCI(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	S := lower(s)
	N := lower(sub)
	for i := 0; i+len(N) <= len(S); i++ {
		if S[i:i+len(N)] == N {
			return true
		}
	}
	return false
}
func lower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return string(out)
}

// T6 acceptance: the resolve_references handler returns a filing-
// suggestion PresentedAs when friction_shape is detected.
func TestHandleResolveReferences_FrictionSuggestion(t *testing.T) {
	pool := testutil.NewTestDB(t)
	registry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{
		Pool:    pool,
		Project: "mcp-servers",
	})
	deps := refresolve.HandlerDeps{Pool: pool, Project: "mcp-servers", Registry: registry}
	params := mustMarshalParams(t, resolveRefsParams{
		MessageText: "also noted: the rebuild loop is annoying. paper cut, but documented.",
	})
	result, err := refresolve.HandleResolveReferences(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleResolveReferences: %v", err)
	}
	var got *refresolve.ResolvedReference
	for i := range result.References {
		if result.References[i].Shape == refresolve.ShapeFrictionShape {
			got = &result.References[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("want friction_shape ResolvedReference, got %+v", result.References)
	}
	if got.RecommendedAction != refresolve.PresentUseDirectly {
		t.Errorf("RecommendedAction: %s", got.RecommendedAction)
	}
	if !containsCI(got.PresentedAs, "consider filing") {
		t.Errorf("PresentedAs missing filing-suggestion verb: %q", got.PresentedAs)
	}
	if !containsCI(got.PresentedAs, "forge") {
		t.Errorf("PresentedAs missing forge action name: %q", got.PresentedAs)
	}
}
