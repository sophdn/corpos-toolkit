package arcreview

import (
	"encoding/json"
	"testing"
)

// partition_characterization_test.go — T3 of chain
// arc-close-decision-authoring-split. The REGRESSION GATE.
//
// Pins the CURRENT decision-execution behavior (partitionDecisions +
// filterActionableDecisions) across the full input matrix —
// {action kind} × {confidence band} × {dedupe marker} — BEFORE T4 changes
// the auto-execute path for body-heavy kinds (forge_vault_note,
// memory_write).
//
// Combinatorial completeness is the point (refactoring-discipline's
// characterization net): every equivalence class is asserted so that when
// T4 deliberately changes a small set of cells (in-scope kinds at the
// auto-execute band), the change is isolated and visible — the rest of the
// matrix staying green is the no-collateral-damage proof.
//
// CONTRACT FOR T4: the cells marked `// CHANGES IN T4` below pin TODAY's
// behavior (vault_note / memory_write at >= autoExecuteConfidence currently
// land in AutoExecute). T4 will UPDATE exactly those assertions to the new
// staged-for-authoring behavior. Every other cell MUST remain unchanged.

// charDecision builds a shape-valid FilingDecision for the given action at
// the given confidence. partitionDecisions only reads Action / Confidence /
// the dedupe markers, but a valid payload keeps the fixtures realistic and
// lets the same helper feed validation-adjacent tests.
func charDecision(t *testing.T, action ActionKind, confidence float64) FilingDecision {
	t.Helper()
	var payload json.RawMessage
	switch action {
	case ActionForgeBug:
		payload = mustJSON(t, ForgeBugPayload{Title: "t", ProblemStatement: "p"})
	case ActionForgeVaultNote:
		payload = mustJSON(t, ForgeVaultNotePayload{NoteKind: "learning", Title: "t", Body: "b"})
	case ActionMemoryWrite:
		payload = mustJSON(t, MemoryWritePayload{MemoryKind: "project", Name: "n", Description: "d", Body: "b"})
	case ActionForgeSuggestion:
		payload = mustJSON(t, ForgeSuggestionPayload{Title: "t", ProblemStatement: "p"})
	case ActionSkillUpdate:
		payload = mustJSON(t, SkillUpdatePayload{SkillSlug: "s", PatchKind: "add_section", Content: "c"})
	case ActionNothingToFile:
		payload = nil
	}
	return FilingDecision{Action: action, Confidence: confidence, Payload: payload, Reasoning: "x"}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// partitionBucket names the partition a decision lands in, for table
// assertions.
type partitionBucket string

const (
	bucketAuto    partitionBucket = "auto_execute"
	bucketStaged  partitionBucket = "staged_for_authoring"
	bucketConfirm partitionBucket = "surface_for_confirm"
	bucketSkip    partitionBucket = "skip"
)

// bucketOf runs partitionDecisions on a single decision and returns which
// bucket it landed in (exactly one is expected to be non-empty).
func bucketOf(t *testing.T, d FilingDecision) partitionBucket {
	t.Helper()
	got := partitionDecisions([]FilingDecision{d})
	n := len(got.AutoExecute) + len(got.StagedForAuthoring) + len(got.SurfaceForConfirm) + len(got.Skip)
	if n != 1 {
		t.Fatalf("expected exactly 1 partitioned decision, got %d (%+v)", n, got)
	}
	switch {
	case len(got.AutoExecute) == 1:
		return bucketAuto
	case len(got.StagedForAuthoring) == 1:
		return bucketStaged
	case len(got.SurfaceForConfirm) == 1:
		return bucketConfirm
	default:
		return bucketSkip
	}
}

// TestCharacterize_PartitionMatrix pins the full action × confidence-band
// matrix for the NON-deduped case. Confidence values bracket both
// thresholds (autoExecuteConfidence = 0.90, surfaceConfidence = 0.50)
// including exact-boundary values.
func TestCharacterize_PartitionMatrix(t *testing.T) {
	// Sanity: the constants this matrix is keyed against. If a future tune
	// moves them, this test should be re-derived deliberately, not silently.
	if autoExecuteConfidence != 0.90 {
		t.Fatalf("characterization assumes autoExecuteConfidence=0.90, got %v", autoExecuteConfidence)
	}
	if surfaceConfidence != 0.50 {
		t.Fatalf("characterization assumes surfaceConfidence=0.50, got %v", surfaceConfidence)
	}

	cases := []struct {
		name       string
		action     ActionKind
		confidence float64
		want       partitionBucket
	}{
		// ---- forge_bug (auto-execute action; OUT of v1 split scope) ----
		{"bug/0.95", ActionForgeBug, 0.95, bucketAuto},
		{"bug/0.90-boundary", ActionForgeBug, 0.90, bucketAuto},
		{"bug/0.89", ActionForgeBug, 0.89, bucketConfirm},
		{"bug/0.70", ActionForgeBug, 0.70, bucketConfirm},
		{"bug/0.50-boundary", ActionForgeBug, 0.50, bucketConfirm},
		{"bug/0.49", ActionForgeBug, 0.49, bucketSkip},
		{"bug/0.30", ActionForgeBug, 0.30, bucketSkip},

		// ---- forge_vault_note (IN v1 scope — body-heavy) --------------
		// T4 FLIPPED: auto-execute band now STAGES for agent authoring
		// instead of auto-forging Qwen's body. Confirm/skip bands unchanged.
		{"vault/0.95", ActionForgeVaultNote, 0.95, bucketStaged},
		{"vault/0.90-boundary", ActionForgeVaultNote, 0.90, bucketStaged},
		{"vault/0.89", ActionForgeVaultNote, 0.89, bucketConfirm},
		{"vault/0.70", ActionForgeVaultNote, 0.70, bucketConfirm},
		{"vault/0.50-boundary", ActionForgeVaultNote, 0.50, bucketConfirm},
		{"vault/0.49", ActionForgeVaultNote, 0.49, bucketSkip},
		{"vault/0.30", ActionForgeVaultNote, 0.30, bucketSkip},

		// ---- memory_write (IN v1 scope — body-heavy) ------------------
		// T4 FLIPPED: auto-execute band now STAGES for agent authoring.
		{"mem/0.95", ActionMemoryWrite, 0.95, bucketStaged},
		{"mem/0.90-boundary", ActionMemoryWrite, 0.90, bucketStaged},
		{"mem/0.89", ActionMemoryWrite, 0.89, bucketConfirm},
		{"mem/0.70", ActionMemoryWrite, 0.70, bucketConfirm},
		{"mem/0.50-boundary", ActionMemoryWrite, 0.50, bucketConfirm},
		{"mem/0.49", ActionMemoryWrite, 0.49, bucketSkip},
		{"mem/0.30", ActionMemoryWrite, 0.30, bucketSkip},

		// ---- forge_suggestion (auto-execute action; OUT of v1 scope) --
		{"sugg/0.95", ActionForgeSuggestion, 0.95, bucketAuto},
		{"sugg/0.90-boundary", ActionForgeSuggestion, 0.90, bucketAuto},
		{"sugg/0.89", ActionForgeSuggestion, 0.89, bucketConfirm},
		{"sugg/0.50-boundary", ActionForgeSuggestion, 0.50, bucketConfirm},
		{"sugg/0.49", ActionForgeSuggestion, 0.49, bucketSkip},

		// ---- skill_update (NEVER auto-executes, any confidence) -------
		{"skill/0.99", ActionSkillUpdate, 0.99, bucketConfirm},
		{"skill/0.90", ActionSkillUpdate, 0.90, bucketConfirm},
		{"skill/0.50", ActionSkillUpdate, 0.50, bucketConfirm},
		{"skill/0.10", ActionSkillUpdate, 0.10, bucketConfirm},

		// ---- nothing_to_file (ALWAYS skip) ----------------------------
		{"nothing/0.99", ActionNothingToFile, 0.99, bucketSkip},
		{"nothing/0.40", ActionNothingToFile, 0.40, bucketSkip},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bucketOf(t, charDecision(t, tc.action, tc.confidence))
			if got != tc.want {
				t.Fatalf("action=%s conf=%.2f: want bucket %q, got %q",
					tc.action, tc.confidence, tc.want, got)
			}
		})
	}
}

// TestCharacterize_PartitionDedupeDemotion pins the F2 (DedupedAgainst) and
// F3 (SameSessionDedupedAgainst) demotion behavior: a deduped auto-execute
// candidate demotes one bucket — AutoExecute→SurfaceForConfirm,
// SurfaceForConfirm→Skip — regardless of confidence. This composes with the
// split: a staged-for-authoring decision that is also a same-session dup is
// handled by T6, but the BASE demotion shape is pinned here.
func TestCharacterize_PartitionDedupeDemotion(t *testing.T) {
	mkF2 := func(action ActionKind, conf float64) FilingDecision {
		d := charDecision(t, action, conf)
		d.DedupedAgainst = &DedupeMatch{} // non-nil marker is what partition reads
		return d
	}
	mkF3 := func(action ActionKind, conf float64) FilingDecision {
		d := charDecision(t, action, conf)
		d.SameSessionDedupedAgainst = &SameSessionMatch{}
		return d
	}

	cases := []struct {
		name string
		d    FilingDecision
		want partitionBucket
	}{
		// F2: high-confidence auto-action deduped → demoted to confirm.
		{"f2/vault/0.95->confirm", mkF2(ActionForgeVaultNote, 0.95), bucketConfirm},
		{"f2/bug/0.95->confirm", mkF2(ActionForgeBug, 0.95), bucketConfirm},
		{"f2/mem/0.90->confirm", mkF2(ActionMemoryWrite, 0.90), bucketConfirm},
		// F2: mid-confidence auto-action deduped → demoted to skip.
		{"f2/vault/0.70->skip", mkF2(ActionForgeVaultNote, 0.70), bucketSkip},
		// F3: same demotion shape.
		{"f3/vault/0.95->confirm", mkF3(ActionForgeVaultNote, 0.95), bucketConfirm},
		{"f3/mem/0.70->skip", mkF3(ActionMemoryWrite, 0.70), bucketSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bucketOf(t, tc.d); got != tc.want {
				t.Fatalf("%s: want %q, got %q", tc.name, tc.want, got)
			}
		})
	}
}

// TestCharacterize_FilterActionableDecisions pins the substrate-observer's
// dispatch filter (filterActionableDecisions): it returns auto_execute +
// surface_for_confirm and DROPS skip. This is what reaches the agent via
// pending_decisions. T4 must keep staged-for-authoring decisions ACTIONABLE
// (the agent has to see them to author), so this baseline matters.
func TestCharacterize_FilterActionableDecisions(t *testing.T) {
	decisions := []FilingDecision{
		charDecision(t, ActionForgeVaultNote, 0.95), // staged -> actionable (T4)
		charDecision(t, ActionForgeBug, 0.70),       // confirm -> actionable
		charDecision(t, ActionMemoryWrite, 0.30),    // skip  -> dropped
		charDecision(t, ActionNothingToFile, 0.99),  // skip  -> dropped
		charDecision(t, ActionSkillUpdate, 0.99),    // confirm -> actionable
	}
	got := filterActionableDecisions(decisions)
	if len(got) != 3 {
		t.Fatalf("expected 3 actionable (auto+staged+confirm), got %d: %+v", len(got), got)
	}
	// Order: auto, then staged, then confirm (filterActionableDecisions
	// appends in that order). Here auto is empty, so the staged vault_note
	// comes first — and it carries StagedForAuthoring=true (T4).
	if got[0].Action != ActionForgeVaultNote {
		t.Fatalf("expected vault_note (staged) first, got %s", got[0].Action)
	}
	if !got[0].StagedForAuthoring {
		t.Fatalf("expected vault_note to carry StagedForAuthoring=true after partition")
	}
}
