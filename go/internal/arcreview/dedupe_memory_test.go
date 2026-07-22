package arcreview_test

import (
	"encoding/json"
	"testing"

	"toolkit/internal/arcreview"
)

// Regression tests for bug `arc-close-filing-review-no-dedup-against-
// existing-artifacts`.
//
// Pre-fix:
//   1. F2 dedup explicitly skipped ActionMemoryWrite — decisionTitle
//      returned empty, so PayloadSignature was empty, so FindBestMatch
//      bailed before computing anything. Demonstrated this session
//      2026-05-23: a memory_write decision for `check-for-batch-
//      capability` slipped past the dedup even though the canonically-
//      equivalent `feedback-batch-primitive-for-multi-op-mcp` was
//      already in the session's memory dir.
//   2. F2's signature was title-only, so semantically-identical-but-
//      textually-different bug titles fell below threshold. The same
//      session's forge_bug decision (title "Agent did not utilize
//      batch forge capability") vs existing bug (title "Agent makes
//      N sequential forge calls instead of one work.batch...")
//      computed Jaccard ~0.18 < 0.30 threshold; folding problem
//      statements into the signature pushed it above threshold.

// TestApplyExistingArtifactDedupe_MemoryWriteAgainstPriorMemory: the
// auto-execute path proposes a memory_write for a friction the
// session already filed as a memory; F2 must catch the dup and
// annotate.
func TestApplyExistingArtifactDedupe_MemoryWriteAgainstPriorMemory(t *testing.T) {
	payload := arcreview.MemoryWritePayload{
		MemoryKind:  "feedback",
		Name:        "check-for-batch-capability",
		Description: "Check for batch capability when forging multiple similar ops on a single surface.",
		Body:        "When forging N≥3 ops of similar shape on a single surface, action_describe `batch` first — the work surface has one.",
	}
	raw, _ := json.Marshal(payload)
	result := &arcreview.ArcReviewResult{
		Decisions: []arcreview.FilingDecision{
			{Action: arcreview.ActionMemoryWrite, Payload: raw, Confidence: 0.9},
		},
	}
	index := arcreview.ExistingArtifactsByKind{
		Memories: []arcreview.ExistingArtifact{
			{
				Slug:             "feedback-batch-primitive-for-multi-op-mcp",
				Title:            "When about to make N≥3 mutating MCP calls of similar shape on one surface, action_describe the surface's `batch` primitive first",
				ProblemStatement: "When forging N≥3 mutating MCP calls of similar shape on one surface, action_describe the surface's `batch` first to confirm fit, then construct one batch call. The work surface's batch is general over allowlisted mutating ops, not just handoff seams.",
			},
		},
	}
	arcreview.ApplyExistingArtifactDedupe(result, index)
	d := result.Decisions[0]
	if d.DedupedAgainst == nil {
		t.Fatalf("memory_write decision should be annotated with DedupedAgainst (pre-fix bug: F2 skipped ActionMemoryWrite entirely)")
	}
	if d.DedupedAgainst.Slug != "feedback-batch-primitive-for-multi-op-mcp" {
		t.Errorf("matched slug: got %q, want %q", d.DedupedAgainst.Slug, "feedback-batch-primitive-for-multi-op-mcp")
	}
}

// TestApplyExistingArtifactDedupe_ForgeBugRicherSignature: the actual
// pair from session 2026-05-23. Title-only Jaccard ~0.18 (below the
// 0.30 default); title+problem_statement Jaccard rises above the
// threshold so the dup gets caught.
func TestApplyExistingArtifactDedupe_ForgeBugRicherSignature(t *testing.T) {
	proposed := arcreview.ForgeBugPayload{
		Title: "Agent did not utilize batch forge capability",
		ProblemStatement: "The agent did not use the 'batch' forge capability when forging 18 similar ops on a single surface, " +
			"leading to multiple separate forge calls instead of a single batch call. This resulted in unnecessary " +
			"round-trips and envelope rationales.",
	}
	raw, _ := json.Marshal(proposed)
	result := &arcreview.ArcReviewResult{
		Decisions: []arcreview.FilingDecision{
			{Action: arcreview.ActionForgeBug, Payload: raw, Confidence: 0.9},
		},
	}
	index := arcreview.ExistingArtifactsByKind{
		Bugs: []arcreview.ExistingArtifact{
			{
				Slug:  "agent-missed-work-batch-primitive-on-multi-forge-sweep",
				Title: "Agent makes N sequential forge calls instead of one work.batch when surface offers a batching primitive",
				ProblemStatement: "Observed during the dashboard-controls-bar-unification chain forge. The agent forged 1 chain + 17 child tasks via 18 sequential mcp__toolkit-server__work.forge calls without first calling action_describe(work, batch) to check whether batch could collapse the sequence. " +
					"The reflex gap: agent did not action_describe an unfamiliar action before assuming its scope.",
			},
		},
	}
	arcreview.ApplyExistingArtifactDedupe(result, index)
	d := result.Decisions[0]
	if d.DedupedAgainst == nil {
		t.Fatalf("forge_bug decision should be deduped against the canonically-equivalent existing bug. " +
			"Pre-fix: title-only Jaccard ~0.18 fell below 0.30 threshold. " +
			"Post-fix: title+problem_statement Jaccard rises above threshold and the dup gets caught.")
	}
	if d.DedupedAgainst.Slug != "agent-missed-work-batch-primitive-on-multi-forge-sweep" {
		t.Errorf("matched slug: got %q, want %q", d.DedupedAgainst.Slug, "agent-missed-work-batch-primitive-on-multi-forge-sweep")
	}
}

// TestApplyExistingArtifactDedupe_MemoryWriteNoFalsePositive: a
// memory_write decision that's NOT a dup of any existing memory must
// pass through unannotated. Defensive — broadening signature shouldn't
// catch unrelated entries.
func TestApplyExistingArtifactDedupe_MemoryWriteNoFalsePositive(t *testing.T) {
	payload := arcreview.MemoryWritePayload{
		MemoryKind:  "user",
		Name:        "completely-unrelated-memory",
		Description: "Something about cron daemon configuration timezones.",
		Body:        "Use TZ=UTC for cron daemon to avoid daylight-saving drift.",
	}
	raw, _ := json.Marshal(payload)
	result := &arcreview.ArcReviewResult{
		Decisions: []arcreview.FilingDecision{
			{Action: arcreview.ActionMemoryWrite, Payload: raw, Confidence: 0.9},
		},
	}
	index := arcreview.ExistingArtifactsByKind{
		Memories: []arcreview.ExistingArtifact{
			{
				Slug:             "feedback-batch-primitive-for-multi-op-mcp",
				Title:            "Check batch primitive before sequential calls",
				ProblemStatement: "Forge sequence reflex.",
			},
		},
	}
	arcreview.ApplyExistingArtifactDedupe(result, index)
	if result.Decisions[0].DedupedAgainst != nil {
		t.Errorf("unrelated memory_write should not match; got %+v", result.Decisions[0].DedupedAgainst)
	}
}
