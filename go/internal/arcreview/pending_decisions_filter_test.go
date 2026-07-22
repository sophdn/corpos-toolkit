package arcreview

import "testing"

// TestFilterActionableDecisions_KeepsOnlyAutoAndSurface pins the re-scoped
// enqueue filter (chain quiet-and-instrument-operator-surface T3): only the
// auto_execute + surface_for_confirm partition is enqueued for the drain;
// the whole skip tier — nothing_to_file, sub-threshold confidence, and
// demoted non-auto dups — is dropped so the agent isn't injected noise it's
// told to skip.
func TestFilterActionableDecisions_KeepsOnlyAutoAndSurface(t *testing.T) {
	dup := &DedupeMatch{Slug: "existing-artifact", Similarity: 0.91}
	in := []FilingDecision{
		{Action: ActionForgeBug, Confidence: 0.95},                             // auto_execute  → keep
		{Action: ActionSkillUpdate, Confidence: 0.10},                          // surface (always) → keep
		{Action: ActionForgeVaultNote, Confidence: 0.70},                       // surface (>=0.50) → keep
		{Action: ActionForgeBug, Confidence: 0.30},                             // skip (<0.50) → DROP
		{Action: ActionNothingToFile, Confidence: 0.99},                        // skip → DROP
		{Action: ActionForgeBug, Confidence: 0.95, DedupedAgainst: dup},        // demoted to surface → keep
		{Action: ActionForgeSuggestion, Confidence: 0.20, DedupedAgainst: dup}, // demoted non-auto → skip → DROP
	}

	out := filterActionableDecisions(in)

	if len(out) != 4 {
		t.Fatalf("kept %d decisions, want 4 (1 auto + 3 surface): %v", len(out), kinds(out))
	}
	for _, d := range out {
		if d.Action == ActionNothingToFile {
			t.Error("nothing_to_file leaked into the enqueue")
		}
		if d.Action == ActionForgeBug && d.Confidence == 0.30 {
			t.Error("sub-threshold (skip-tier) decision leaked into the enqueue")
		}
		if d.Action == ActionForgeSuggestion && d.Confidence == 0.20 {
			t.Error("demoted non-auto dup (skip-tier) leaked into the enqueue")
		}
	}
}

// TestFilterActionableDecisions_AllSkipReturnsEmpty: a fire whose decisions
// are all skip-tier enqueues nothing — so writePendingDecisions skips the
// write and the drain injects nothing.
func TestFilterActionableDecisions_AllSkipReturnsEmpty(t *testing.T) {
	in := []FilingDecision{
		{Action: ActionNothingToFile, Confidence: 0.99},
		{Action: ActionForgeBug, Confidence: 0.20},
	}
	if out := filterActionableDecisions(in); len(out) != 0 {
		t.Errorf("all-skip fire kept %d decisions, want 0: %v", len(out), kinds(out))
	}
}

func kinds(ds []FilingDecision) []ActionKind {
	out := make([]ActionKind, len(ds))
	for i, d := range ds {
		out[i] = d.Action
	}
	return out
}
