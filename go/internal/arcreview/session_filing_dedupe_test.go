package arcreview

import (
	"encoding/json"
	"testing"
)

// session_filing_dedupe_test.go — T6 of chain
// arc-close-decision-authoring-split. The same-session dedup guard.

func vaultDecision(t *testing.T, title string) FilingDecision {
	t.Helper()
	payload, _ := json.Marshal(ForgeVaultNotePayload{NoteKind: "decision", Title: title, Body: "b"})
	return FilingDecision{Action: ActionForgeVaultNote, Confidence: 0.95, Payload: payload, Reasoning: "r"}
}

// TestEnrich_NearSameSessionFilingDowngrades: a high-confidence vault-note
// decision whose title matches an artifact the agent already filed this
// session is marked EnrichExisting and lands in surface_for_confirm — NOT
// staged as a new note.
func TestEnrich_NearSameSessionFilingDowngrades(t *testing.T) {
	result := ArcReviewResult{Decisions: []FilingDecision{
		vaultDecision(t, "the decider author split contract"),
	}}
	agentFilings := []RecentFiling{
		{Kind: "vault_note", Slug: "decider-author-split-contract", Title: "decider author split contract"},
	}
	ApplyEnrichExistingDedupe(&result, agentFilings)

	d := result.Decisions[0]
	if d.EnrichExisting == nil {
		t.Fatalf("expected EnrichExisting set for a near same-session filing")
	}
	if d.EnrichExisting.Slug != "decider-author-split-contract" {
		t.Fatalf("expected match slug, got %q", d.EnrichExisting.Slug)
	}
	// Partition must demote it out of staging into surface_for_confirm.
	part := partitionDecisions(result.Decisions)
	if len(part.StagedForAuthoring) != 0 {
		t.Fatalf("enrich-existing decision must NOT be staged, got %d staged", len(part.StagedForAuthoring))
	}
	if len(part.SurfaceForConfirm) != 1 {
		t.Fatalf("enrich-existing decision should surface_for_confirm, got %+v", part)
	}
}

// TestEnrich_NoMatchStaysStaged: a vault-note with no same-session filing
// match is untouched and still stages (the T4 path).
func TestEnrich_NoMatchStaysStaged(t *testing.T) {
	result := ArcReviewResult{Decisions: []FilingDecision{
		vaultDecision(t, "an entirely unrelated topic about widgets"),
	}}
	ApplyEnrichExistingDedupe(&result, []RecentFiling{
		{Kind: "vault_note", Slug: "something-else", Title: "kubernetes ingress timeout tuning"},
	})
	if result.Decisions[0].EnrichExisting != nil {
		t.Fatalf("did not expect a match for an unrelated title")
	}
	part := partitionDecisions(result.Decisions)
	if len(part.StagedForAuthoring) != 1 {
		t.Fatalf("unmatched body-heavy decision should still stage, got %+v", part)
	}
}

// TestEnrich_KindIsolation: a vault-note decision does NOT dedupe against a
// same-titled BUG the agent filed — cross-kind collisions aren't duplicates.
func TestEnrich_KindIsolation(t *testing.T) {
	result := ArcReviewResult{Decisions: []FilingDecision{
		vaultDecision(t, "wrapper swallows exit code"),
	}}
	ApplyEnrichExistingDedupe(&result, []RecentFiling{
		{Kind: "bug", Slug: "wrapper-swallows-exit-code", Title: "wrapper swallows exit code"},
	})
	if result.Decisions[0].EnrichExisting != nil {
		t.Fatalf("vault-note must not dedupe against a bug filing (kind isolation)")
	}
}
