package arcreview

import (
	"testing"
)

// staging_test.go — T4 of chain arc-close-decision-authoring-split.
// Asserts the NEW staged-for-authoring behavior the split introduces
// (the "after" net; the "before" net lives in
// partition_characterization_test.go).

// TestStaging_InScopeBodyHeavyAutoBandIsStaged verifies that vault_note
// and memory_write in the auto-execute band land in StagedForAuthoring
// (not AutoExecute) and carry the StagedForAuthoring flag — so both
// dispatch surfaces render an authoring prompt instead of forging Qwen's
// body.
func TestStaging_InScopeBodyHeavyAutoBandIsStaged(t *testing.T) {
	for _, action := range []ActionKind{ActionForgeVaultNote, ActionMemoryWrite} {
		got := partitionDecisions([]FilingDecision{charDecision(t, action, 0.95)})
		if len(got.AutoExecute) != 0 {
			t.Fatalf("%s @0.95: expected NOT in AutoExecute, got %d", action, len(got.AutoExecute))
		}
		if len(got.StagedForAuthoring) != 1 {
			t.Fatalf("%s @0.95: expected 1 staged, got %d", action, len(got.StagedForAuthoring))
		}
		if !got.StagedForAuthoring[0].StagedForAuthoring {
			t.Fatalf("%s @0.95: staged decision must carry StagedForAuthoring=true", action)
		}
		// Qwen's draft body must be retained in the payload for the T5
		// fallback — staging hides it from the verbatim-forge directive,
		// it does not discard it.
		if len(got.StagedForAuthoring[0].Payload) == 0 {
			t.Fatalf("%s @0.95: staged decision must retain Qwen draft payload for fallback", action)
		}
	}
}

// TestStaging_OutOfScopeKindsStillAutoExecute verifies bug + suggestion
// (auto-execute actions, but OUT of v1 scope) still land in AutoExecute
// at high confidence and are NOT staged.
func TestStaging_OutOfScopeKindsStillAutoExecute(t *testing.T) {
	for _, action := range []ActionKind{ActionForgeBug, ActionForgeSuggestion} {
		got := partitionDecisions([]FilingDecision{charDecision(t, action, 0.95)})
		if len(got.AutoExecute) != 1 {
			t.Fatalf("%s @0.95: expected 1 auto-execute, got %d", action, len(got.AutoExecute))
		}
		if len(got.StagedForAuthoring) != 0 {
			t.Fatalf("%s @0.95: must NOT be staged (out of v1 scope), got %d staged", action, len(got.StagedForAuthoring))
		}
		if got.AutoExecute[0].StagedForAuthoring {
			t.Fatalf("%s @0.95: must not carry StagedForAuthoring flag", action)
		}
	}
}

// TestStaging_DedupedInScopeIsDemotedNotStaged verifies the design rule:
// a same-session / existing-artifact dedupe match on an in-scope kind is
// DEMOTED (to surface_for_confirm), not staged for fresh authoring —
// there is nothing new to author. (T6 refines this to "enrich existing".)
func TestStaging_DedupedInScopeIsDemotedNotStaged(t *testing.T) {
	d := charDecision(t, ActionForgeVaultNote, 0.95)
	d.SameSessionDedupedAgainst = &SameSessionMatch{EventID: "prior", Similarity: 0.9}
	got := partitionDecisions([]FilingDecision{d})
	if len(got.StagedForAuthoring) != 0 {
		t.Fatalf("deduped in-scope decision must NOT be staged, got %d staged", len(got.StagedForAuthoring))
	}
	if len(got.SurfaceForConfirm) != 1 {
		t.Fatalf("deduped in-scope @0.95 should demote to surface_for_confirm, got %+v", got)
	}
}
