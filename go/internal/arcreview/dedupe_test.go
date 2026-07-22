package arcreview_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/arcreview"
	"toolkit/internal/testutil"
)

// F2 of chain arc-close-filing-review-dedupe-and-noise-reduction.
// These tests pin the contract:
//   (a) high-similarity proposed decisions acquire DedupedAgainst.
//   (b) low-similarity decisions pass through unchanged.
//   (c) FindBestMatch returns nil when the proposed title's token
//       set is empty.
//   (d) FindBestMatch returns the highest-similarity match (not the
//       first match above threshold).
//   (e) Jaccard threshold reads from the env var override when set.

func TestFindBestMatch_HighSimilarityForgeSuggestion(t *testing.T) {
	// "Orphan Precommit-fmt Stashes" (the proposed Qwen filing F1
	// corpus showed at session 2026-05-20) vs the actually-filed
	// suggestion "Automatic cleanup of orphaned precommit-fmt
	// stashes". Token overlap measured 0.29 in the F1 spot-check
	// — Jaccard 0.30 threshold catches it; 0.40 does not.
	index := arcreview.ExistingArtifactsByKind{
		Suggestions: []arcreview.ExistingArtifact{
			{Slug: "cleanup-orphan-precommit-fmt-stashes",
				Title: "Automatic cleanup of orphaned precommit-fmt stashes"},
		},
	}
	match := arcreview.FindBestMatch(
		arcreview.ActionForgeSuggestion,
		arcreview.TitleTokens("Orphan Precommit-fmt Stashes"),
		index,
		0.30,
	)
	if match == nil {
		t.Fatalf("FindBestMatch returned nil; expected match at threshold 0.30")
	}
	if match.Slug != "cleanup-orphan-precommit-fmt-stashes" {
		t.Errorf("matched slug: got %q, want %q", match.Slug, "cleanup-orphan-precommit-fmt-stashes")
	}
	if match.Similarity < 0.20 {
		t.Errorf("similarity too low: %.3f", match.Similarity)
	}
}

func TestFindBestMatch_LowSimilarityNoMatch(t *testing.T) {
	index := arcreview.ExistingArtifactsByKind{
		Bugs: []arcreview.ExistingArtifact{
			{Slug: "existing-bug", Title: "Something completely unrelated to the proposed payload"},
		},
	}
	match := arcreview.FindBestMatch(
		arcreview.ActionForgeBug,
		arcreview.TitleTokens("Cursor off-by-one in pagination"),
		index,
		0.30,
	)
	if match != nil {
		t.Errorf("expected no match; got %+v", match)
	}
}

func TestFindBestMatch_EmptyProposedTitle(t *testing.T) {
	index := arcreview.ExistingArtifactsByKind{
		Bugs: []arcreview.ExistingArtifact{{Slug: "x", Title: "Something"}},
	}
	match := arcreview.FindBestMatch(arcreview.ActionForgeBug, arcreview.TitleTokens(""), index, 0.30)
	if match != nil {
		t.Errorf("empty proposed title should never match; got %+v", match)
	}
}

func TestFindBestMatch_ReturnsHighestSimilarity(t *testing.T) {
	// Two candidates; the second is a closer match. Verify the
	// best (highest similarity) wins.
	index := arcreview.ExistingArtifactsByKind{
		Bugs: []arcreview.ExistingArtifact{
			{Slug: "low-match", Title: "Some general bug about payments"},
			{Slug: "high-match", Title: "Cursor off-by-one in pagination logic"},
		},
	}
	match := arcreview.FindBestMatch(
		arcreview.ActionForgeBug,
		arcreview.TitleTokens("Cursor off-by-one in pagination"),
		index,
		0.30,
	)
	if match == nil {
		t.Fatalf("expected match")
	}
	if match.Slug != "high-match" {
		t.Errorf("matched slug: got %q, want %q (highest-similarity should win)", match.Slug, "high-match")
	}
}

func TestApplyExistingArtifactDedupe_AnnotatesMatchedDecisions(t *testing.T) {
	bugPayload := arcreview.ForgeBugPayload{
		Title:            "Cursor pagination",
		ProblemStatement: "issue X",
	}
	rawBug, _ := json.Marshal(bugPayload)

	result := &arcreview.ArcReviewResult{
		Decisions: []arcreview.FilingDecision{
			{
				Action:     arcreview.ActionForgeBug,
				Payload:    rawBug,
				Confidence: 0.9,
			},
		},
	}
	index := arcreview.ExistingArtifactsByKind{
		Bugs: []arcreview.ExistingArtifact{
			{Slug: "existing-pagination-bug", Title: "Cursor pagination off-by-one"},
		},
	}
	arcreview.ApplyExistingArtifactDedupe(result, index)

	d := result.Decisions[0]
	if d.DedupedAgainst == nil {
		t.Fatalf("decision should be annotated with DedupedAgainst")
	}
	if d.DedupedAgainst.Slug != "existing-pagination-bug" {
		t.Errorf("DedupedAgainst.Slug: got %q, want %q",
			d.DedupedAgainst.Slug, "existing-pagination-bug")
	}
}

func TestApplyExistingArtifactDedupe_NoMatchPassesThrough(t *testing.T) {
	bugPayload := arcreview.ForgeBugPayload{
		Title:            "Cursor pagination",
		ProblemStatement: "issue X",
	}
	rawBug, _ := json.Marshal(bugPayload)
	result := &arcreview.ArcReviewResult{
		Decisions: []arcreview.FilingDecision{
			{Action: arcreview.ActionForgeBug, Payload: rawBug, Confidence: 0.9},
		},
	}
	// No bugs in index → no match possible.
	index := arcreview.ExistingArtifactsByKind{}
	arcreview.ApplyExistingArtifactDedupe(result, index)
	if result.Decisions[0].DedupedAgainst != nil {
		t.Errorf("decision shouldn't be annotated when index is empty; got %+v",
			result.Decisions[0].DedupedAgainst)
	}
}

func TestApplyExistingArtifactDedupe_NilResultNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("ApplyExistingArtifactDedupe(nil, ...) panicked: %v", r)
		}
	}()
	arcreview.ApplyExistingArtifactDedupe(nil, arcreview.ExistingArtifactsByKind{})
}

// Threshold env-var override. When TOOLKIT_ARCCLOSE_DEDUPE_JACCARD_THRESHOLD
// is set to a parseable float in [0,1], it overrides the default 0.30.
func TestApplyExistingArtifactDedupe_RespectsEnvVarThreshold(t *testing.T) {
	// Set threshold to 0.99 — basically nothing should match.
	t.Setenv("TOOLKIT_ARCCLOSE_DEDUPE_JACCARD_THRESHOLD", "0.99")
	bugPayload := arcreview.ForgeBugPayload{
		Title:            "Cursor pagination",
		ProblemStatement: "issue X",
	}
	rawBug, _ := json.Marshal(bugPayload)
	result := &arcreview.ArcReviewResult{
		Decisions: []arcreview.FilingDecision{
			{Action: arcreview.ActionForgeBug, Payload: rawBug, Confidence: 0.9},
		},
	}
	index := arcreview.ExistingArtifactsByKind{
		Bugs: []arcreview.ExistingArtifact{
			{Slug: "existing", Title: "Cursor pagination off-by-one"},
		},
	}
	arcreview.ApplyExistingArtifactDedupe(result, index)
	if result.Decisions[0].DedupedAgainst != nil {
		t.Errorf("threshold 0.99 should reject all matches; got %+v",
			result.Decisions[0].DedupedAgainst)
	}
}

// TestLoadExistingArtifactsForDedupe_IncludesVaultNotes is the regression
// for bug arc-close-dedup-misses-semantically-duplicative-decisions. Root
// cause: vault notes are stored in knowledge_pointers with
// source_type='vault' (the canonical literal — see pointers/normalize.go,
// pointers/integrity.go, curation/sources/vault_note.go), but
// LoadExistingArtifactsForDedupe queried source_type='vault-note' — a
// literal nothing writes. So the VaultNotes dedup arm matched 0 rows and
// EVERY forge_vault_note proposal passed F2 unconditionally (live DB:
// 255 'vault' rows, 0 'vault-note'; this session, 5/5 duplicative vault-note
// proposals surfaced un-demoted). Every prior dedupe test built the index
// in-memory, so none exercised this query — the bug shipped untested.
//
// This test hits the real query: seed one 'vault' pointer, confirm it loads
// into the index, and confirm a duplicating forge_vault_note proposal is now
// demoted by F2.
func TestLoadExistingArtifactsForDedupe_IncludesVaultNotes(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	const proj = "mcp-servers"
	testutil.SeedProject(t, pool, proj)

	const noteQuestion = "Historical data recovery workflow and testing"
	const noteBody = "Reconstruct point-in-time corpora from the events ledger: dry-run first, then an idempotent backfill keyed by group not row."
	if _, err := pool.DB().ExecContext(ctx, `
		INSERT INTO knowledge_pointers
			(project_id, source_type, source_ref, question, invoke_when, description, slug)
		VALUES (?, 'vault', ?, ?, ?, ?, ?)`,
		proj, "learnings/general/2026-05-24_recovery.md", noteQuestion,
		"when recovering historical event data", noteBody, "recovery-workflow",
	); err != nil {
		t.Fatalf("seed vault pointer: %v", err)
	}

	idx, err := arcreview.LoadExistingArtifactsForDedupe(ctx, pool, proj)
	if err != nil {
		t.Fatalf("LoadExistingArtifactsForDedupe: %v", err)
	}
	// The load-bearing assertion: the 'vault' pointer must reach the dedupe
	// index. Pre-fix (query literal 'vault-note') this is 0 → the arm is inert.
	if len(idx.VaultNotes) != 1 {
		t.Fatalf("VaultNotes index has %d rows, want 1 — the query's source_type literal must match what ingestion writes ('vault'), not 'vault-note'", len(idx.VaultNotes))
	}
	if idx.VaultNotes[0].Title != noteQuestion {
		t.Errorf("vault note title: got %q, want %q", idx.VaultNotes[0].Title, noteQuestion)
	}

	// End-to-end: a forge_vault_note proposal duplicating the seeded note is
	// now caught by F2 (pre-fix it passed unconditionally — the session noise).
	raw, _ := json.Marshal(arcreview.ForgeVaultNotePayload{
		Title: "Historical Data Recovery Workflow and Testing",
		Body:  "Rebuild point-in-time corpora from the events ledger; dry-run then idempotent backfill.",
	})
	res := &arcreview.ArcReviewResult{Decisions: []arcreview.FilingDecision{
		{Action: arcreview.ActionForgeVaultNote, Payload: raw, Confidence: 0.9},
	}}
	arcreview.ApplyExistingArtifactDedupe(res, idx)
	if res.Decisions[0].DedupedAgainst == nil {
		t.Error("a forge_vault_note duplicating an existing vault note should be deduped, but DedupedAgainst is nil — the vault-note arm is still inert")
	}
}
