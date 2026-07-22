package arcreview_test

import (
	"encoding/json"
	"testing"

	"toolkit/internal/arcreview"
)

// F3 of chain arc-close-filing-review-dedupe-and-noise-reduction.
// Tests pin the contract:
//   (a) decisions matching a same-session prior payload acquire
//       SameSessionDedupedAgainst.
//   (b) prior decisions for a DIFFERENT action kind don't match.
//   (c) nil result / empty priors don't panic.
//   (d) env-var threshold override respected.

func makeBugDecisionForSessionDedupe(t *testing.T, title, problem string) arcreview.FilingDecision {
	t.Helper()
	raw, _ := json.Marshal(arcreview.ForgeBugPayload{
		Title:            title,
		ProblemStatement: problem,
	})
	return arcreview.FilingDecision{
		Action:     arcreview.ActionForgeBug,
		Payload:    raw,
		Confidence: 0.9,
	}
}

func makeVaultNoteDecisionForSessionDedupe(t *testing.T, title, body string) arcreview.FilingDecision {
	t.Helper()
	raw, _ := json.Marshal(arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    title,
		Body:     body,
	})
	return arcreview.FilingDecision{
		Action:     arcreview.ActionForgeVaultNote,
		Payload:    raw,
		Confidence: 0.9,
	}
}

// priorFromDecision converts a FilingDecision into a PriorSessionDecision
// fixture, using the EXPORTED PayloadSignature so the test's
// signature matches the production tokeniser exactly. Drift in
// production tokenisation surfaces as test failures here.
func priorFromDecision(t *testing.T, eventID string, d arcreview.FilingDecision) arcreview.PriorSessionDecision {
	t.Helper()
	return arcreview.PriorSessionDecision{
		EventID:   eventID,
		Action:    d.Action,
		Signature: arcreview.PayloadSignature(&d),
	}
}

// (a) Decision matching a prior same-session payload acquires
// SameSessionDedupedAgainst. Models the realistic case: two arc-
// close fires within one session see overlapping snapshot content,
// so Qwen produces near-verbatim repeat-proposals. Similarity well
// above the 0.40 threshold.
func TestApplySameSessionDedupe_MatchesPriorPayloadSameAction(t *testing.T) {
	currentDecision := makeBugDecisionForSessionDedupe(t,
		"Orphan precommit stashes accumulating from gofmt fires",
		"The precommit script detects orphan precommit-fmt stashes from dead parent PIDs accumulating across sessions. Operator must drop them manually.",
	)
	priorDecision := makeBugDecisionForSessionDedupe(t,
		"Orphan precommit stashes accumulating from gofmt fires",
		"The precommit script detects orphan precommit-fmt stashes from dead parent PIDs accumulating across sessions and lacks a cleanup pass.",
	)
	priors := []arcreview.PriorSessionDecision{
		priorFromDecision(t, "019e4c00-aaaa-bbbb-cccc-prior-event-id", priorDecision),
	}
	result := &arcreview.ArcReviewResult{Decisions: []arcreview.FilingDecision{currentDecision}}
	arcreview.ApplySameSessionDedupe(result, priors)

	got := result.Decisions[0].SameSessionDedupedAgainst
	if got == nil {
		t.Fatalf("expected SameSessionDedupedAgainst non-nil for matching same-session payload")
	}
	if got.EventID != "019e4c00-aaaa-bbbb-cccc-prior-event-id" {
		t.Errorf("EventID: got %q, want prior event id", got.EventID)
	}
	if got.Similarity < 0.40 {
		t.Errorf("similarity too low: %.3f", got.Similarity)
	}
}

// (b) Prior decisions for a different action kind don't match.
func TestApplySameSessionDedupe_DifferentActionDoesNotMatch(t *testing.T) {
	currentDecision := makeBugDecisionForSessionDedupe(t,
		"Orphan precommit stashes accumulating after gofmt",
		"Stashes accumulate after parent PID dies.",
	)
	priorVaultNote := makeVaultNoteDecisionForSessionDedupe(t,
		"Orphan precommit stashes accumulating after gofmt",
		"Stashes accumulate after parent PID dies. Cross-project pattern documented here.",
	)
	priors := []arcreview.PriorSessionDecision{
		priorFromDecision(t, "prior-event-id", priorVaultNote),
	}
	result := &arcreview.ArcReviewResult{Decisions: []arcreview.FilingDecision{currentDecision}}
	arcreview.ApplySameSessionDedupe(result, priors)

	if result.Decisions[0].SameSessionDedupedAgainst != nil {
		t.Errorf("cross-action match should NOT trip (bug vs vault_note are different surfaces)")
	}
}

// (c) Nil result / empty priors don't panic.
func TestApplySameSessionDedupe_NilSafety(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("ApplySameSessionDedupe panicked: %v", r)
		}
	}()
	arcreview.ApplySameSessionDedupe(nil, nil)
	arcreview.ApplySameSessionDedupe(nil, []arcreview.PriorSessionDecision{})
	arcreview.ApplySameSessionDedupe(&arcreview.ArcReviewResult{}, nil)
	arcreview.ApplySameSessionDedupe(&arcreview.ArcReviewResult{}, []arcreview.PriorSessionDecision{})
}

// (d) Env-var threshold override raises the bar — high threshold
// causes loose-similarity decisions to NOT match. Pair chosen to
// exercise the threshold itself: some overlap but neither full
// containment nor high Jaccard, so a 0.99 threshold suppresses what
// a default 0.40 would catch. (Pre-similarity-hybrid this test used
// near-identical content and asserted threshold-rejection via Jaccard
// alone; containment correctly identifies near-identical pairs at
// sim=1.0 regardless of threshold, so the test now uses content that
// shares a topic but not phrasing.)
func TestApplySameSessionDedupe_RespectsEnvVarThreshold(t *testing.T) {
	t.Setenv("TOOLKIT_ARCCLOSE_DEDUPE_SESSION_JACCARD_THRESHOLD", "0.99")
	currentDecision := makeBugDecisionForSessionDedupe(t,
		"Orphan precommit stashes accumulating after gofmt",
		"Format-only stashes from dead PIDs accumulate forever; reaper needed.",
	)
	priorDecision := makeBugDecisionForSessionDedupe(t,
		"Reaper for dead-PID stash artifacts left by precommit",
		"Background cleanup should drop empty orphaned stash entries on next gate run.",
	)
	priors := []arcreview.PriorSessionDecision{
		priorFromDecision(t, "prior-event-id", priorDecision),
	}
	result := &arcreview.ArcReviewResult{Decisions: []arcreview.FilingDecision{currentDecision}}
	arcreview.ApplySameSessionDedupe(result, priors)

	if result.Decisions[0].SameSessionDedupedAgainst != nil {
		t.Errorf("threshold 0.99 should reject loose-similarity pair; got %+v",
			result.Decisions[0].SameSessionDedupedAgainst)
	}
}

// nothing_to_file decisions never match (no payload to compare).
func TestApplySameSessionDedupe_NothingToFileIgnored(t *testing.T) {
	result := &arcreview.ArcReviewResult{
		Decisions: []arcreview.FilingDecision{
			{Action: arcreview.ActionNothingToFile, Confidence: 1.0},
		},
	}
	priors := []arcreview.PriorSessionDecision{
		{EventID: "prior", Action: arcreview.ActionForgeBug,
			Signature: map[string]struct{}{"some": {}, "tokens": {}}},
	}
	arcreview.ApplySameSessionDedupe(result, priors)
	if result.Decisions[0].SameSessionDedupedAgainst != nil {
		t.Errorf("nothing_to_file should not be matched against prior decisions")
	}
}
