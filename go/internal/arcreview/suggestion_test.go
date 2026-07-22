package arcreview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestForgeSuggestion_HighConfidenceAutoExecutes is the headline
// regression for chain `agent-suggestion-box` T9: at confidence ≥ 0.90
// the dispatch partition routes a forge_suggestion decision into the
// AutoExecute bucket alongside forge_bug / forge_vault_note / memory_write.
// The downstream hook reads AutoExecute and dispatches the actual
// mcp__toolkit-server__work forge(kind=suggestion, …) call.
func TestForgeSuggestion_HighConfidenceAutoExecutes(t *testing.T) {
	body, _ := json.Marshal(ForgeSuggestionPayload{
		Title:            "roadmap_list lacks FTS5 coverage other lists have",
		ProblemStatement: "Other lists back onto FTS5; roadmap_list scans every row.",
		Priority:         "medium",
	})
	decisions := []FilingDecision{
		{Action: ActionForgeSuggestion, Confidence: 0.91, Payload: body, Reasoning: "valid proposal"},
	}
	got := partitionDecisions(decisions)
	if len(got.AutoExecute) != 1 {
		t.Fatalf("0.91 forge_suggestion must auto-execute; partition was %+v", got)
	}
	if got.AutoExecute[0].Action != ActionForgeSuggestion {
		t.Errorf("auto-executed action: got %q, want forge_suggestion", got.AutoExecute[0].Action)
	}
}

// TestForgeSuggestion_BorderlineConfidenceSurfacesForConfirm pins the
// 0.86 case: above the 0.50 surface floor but below the 0.90 auto-execute
// cutoff, the decision must land in SurfaceForConfirm — the user (or the
// confirm-required envelope handler) makes the call rather than the
// dispatcher firing it autonomously.
func TestForgeSuggestion_BorderlineConfidenceSurfacesForConfirm(t *testing.T) {
	body, _ := json.Marshal(ForgeSuggestionPayload{
		Title:            "consider renaming the foo helper",
		ProblemStatement: "Current name is ambiguous when read alongside Foo type.",
		Priority:         "low",
	})
	decisions := []FilingDecision{
		{Action: ActionForgeSuggestion, Confidence: 0.86, Payload: body, Reasoning: "borderline proposal"},
	}
	got := partitionDecisions(decisions)
	if len(got.AutoExecute) != 0 {
		t.Fatalf("0.86 forge_suggestion must NOT auto-execute; got AutoExecute=%+v", got.AutoExecute)
	}
	if len(got.SurfaceForConfirm) != 1 {
		t.Fatalf("0.86 forge_suggestion must surface for confirm; partition was %+v", got)
	}
}

// TestForgeSuggestion_LowConfidenceSkipped pins the floor: below 0.50
// the decision is skipped entirely (matches the floor for every other
// auto-executable action — the floor is shared even when the auto-
// execute cutoff per-action diverges).
func TestForgeSuggestion_LowConfidenceSkipped(t *testing.T) {
	body, _ := json.Marshal(ForgeSuggestionPayload{
		Title:            "speculative refactor",
		ProblemStatement: "weak signal",
		Priority:         "low",
	})
	decisions := []FilingDecision{
		{Action: ActionForgeSuggestion, Confidence: 0.30, Payload: body, Reasoning: "noisy"},
	}
	got := partitionDecisions(decisions)
	if len(got.Skip) != 1 {
		t.Fatalf("0.30 forge_suggestion must skip; partition was %+v", got)
	}
}

// TestValidateDecision_RejectsInvalidSuggestionPriority pins that the
// validator catches a bug-side severity-shape priority value
// (e.g. "critical") before it leaves the parser, matching the bug-side
// severity-enum check.
func TestValidateDecision_RejectsInvalidSuggestionPriority(t *testing.T) {
	body, _ := json.Marshal(ForgeSuggestionPayload{
		Title:            "x",
		ProblemStatement: "y",
		Priority:         "critical",
	})
	d := FilingDecision{Action: ActionForgeSuggestion, Confidence: 0.9, Payload: body}
	err := ValidateDecision(d)
	if err == nil {
		t.Fatalf("expected ErrInvalidPayloadField for priority=critical, got nil")
	}
	fieldErr, ok := err.(*ErrInvalidPayloadField)
	if !ok || fieldErr.Field != "priority" {
		t.Fatalf("expected ErrInvalidPayloadField on field=priority, got %T %v", err, err)
	}
}

// TestValidateDecision_AcceptsValidSuggestion is the happy path for a
// minimal valid ForgeSuggestionPayload.
func TestValidateDecision_AcceptsValidSuggestion(t *testing.T) {
	body, _ := json.Marshal(ForgeSuggestionPayload{
		Title:            "x",
		ProblemStatement: "y",
		Priority:         "medium",
	})
	d := FilingDecision{Action: ActionForgeSuggestion, Confidence: 0.9, Payload: body}
	if err := ValidateDecision(d); err != nil {
		t.Fatalf("valid suggestion rejected: %v", err)
	}
}

// TestReviewSystemPrompt_IncludesFrictionVsSuggestionDefinition pins the
// load-from-skill-file behavior. The compose-time splice must surface
// the friction-vs-suggestion distinction verbatim (or the embedded
// fallback) so the Qwen prompt expresses the same rule the
// suggestion-filing-discipline skill body defines for the agent.
func TestReviewSystemPrompt_IncludesFrictionVsSuggestionDefinition(t *testing.T) {
	prompt := reviewSystemPrompt()
	if !strings.Contains(prompt, "FRICTION vs SUGGESTION") {
		t.Fatalf("review system prompt missing FRICTION vs SUGGESTION section header")
	}
	if !strings.Contains(prompt, "forge_suggestion") {
		t.Fatalf("review system prompt must name forge_suggestion in the output schema")
	}
	if !strings.Contains(prompt, "priority") {
		t.Fatalf("review system prompt must name 'priority' (suggestion-native vocab, not severity)")
	}
	// Definition signature phrases — present in both the skill body
	// and the embedded fallback.
	for _, phrase := range []string{
		"interrupted the normal flow",
		"unintentional in our design",
		"go against past decisions",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("review system prompt missing definition phrase %q", phrase)
		}
	}
}

// TestReviewSystemPrompt_IncludesSuggestionWorthyTaxonomy pins the
// dial-in step from suggestion arcreview-signal-taxonomy-missing-suggestion
// -worthy-arm: the SIGNAL TAXONOMY block in reviewSystemBase grows a
// "Suggestion-worthy:" subsection parallel to the existing four kinds
// (Vault-worthy / Bug-worthy / Skill-worthy / Memory-worthy) so Qwen's
// scan loop has explicit pattern-match targets for suggestion-shape
// observations. Without the subsection, the schema reaches Qwen but
// the scan-language stays anchored to the four legacy kinds and
// suggestions never surface — observed empirically: 6 arc-close fires
// across the agent-suggestion-box T9 + dashboard sessions returned 0
// forge_suggestion decisions despite the schema being live.
func TestReviewSystemPrompt_IncludesSuggestionWorthyTaxonomy(t *testing.T) {
	prompt := reviewSystemPrompt()
	if !strings.Contains(prompt, "Suggestion-worthy") {
		t.Fatalf("review system prompt missing Suggestion-worthy subsection in SIGNAL TAXONOMY block")
	}
	// At least 3 of the 6 scan-target patterns must be present; the
	// loose threshold lets the prose evolve without snapping the test
	// every time a bullet's wording is tightened. If the prompt drops
	// below 3 named targets, the scan loop has effectively lost its
	// pattern-match anchor and the test fails — pointing the reader
	// at the suggestion that drove the original addition.
	scanTargets := []string{
		"PROSE DRIFT",
		"MISSING TEST",
		"SHARED COMPONENT OPPORTUNITY",
		"REDUNDANT CONTENT",
		"CONVENTION DRIFT",
		"ERGONOMIC NIT",
	}
	hits := 0
	for _, pat := range scanTargets {
		if strings.Contains(prompt, pat) {
			hits++
		}
	}
	if hits < 3 {
		t.Errorf("review system prompt's Suggestion-worthy subsection lists only %d scan targets (want ≥3 of %v); the scan loop loses its pattern-match anchor", hits, scanTargets)
	}
}

// TestFrictionVsSuggestionDefinition_ReadsFromSkillFile drops a custom
// skill body into a temp dir, points the loader at it via the
// TOOLKIT_SUGGESTION_SKILL_ROOT env var, and verifies the runtime read
// extracts the blockquote. Resets the sync.Once-cached value via a
// fresh sub-test process — handled by running this AFTER the
// default-path test so the cache fingerprint hasn't been frozen
// elsewhere.
func TestFrictionVsSuggestionDefinition_ReadsFromSkillFile(t *testing.T) {
	// This test relies on the loader being called for the first time
	// in this binary. If TestReviewSystemPrompt above ran first, the
	// sync.Once is already done. Skip with a note in that case rather
	// than producing a misleading false-pass.
	if suggestionDefinitionAlreadyLoaded() {
		t.Skip("suggestion definition cache is already populated for this test process; env-injection test runs in isolation")
	}

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "suggestion-filing-discipline")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `---
name: suggestion-filing-discipline
---

# Suggestion Filing Discipline

## The verbatim friction-vs-suggestion definition

> Custom test definition. The override line one.
> The override line two for verification.

## Next section`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("TOOLKIT_SUGGESTION_SKILL_ROOT", dir)

	got := FrictionVsSuggestionDefinition()
	if !strings.Contains(got, "Custom test definition") {
		t.Errorf("loader didn't pick up the override file: got %q", got)
	}
}

// suggestionDefinitionAlreadyLoaded reports whether sync.Once has
// already fired in this test binary. Used by env-injection tests to
// skip cleanly rather than test the cached value (which would have
// been populated from the default $HOME path).
func suggestionDefinitionAlreadyLoaded() bool {
	return suggestionDefinition != ""
}
