package arcreview_test

import (
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/arcreview"
)

// Helpers --------------------------------------------------------

func decisionForgeVaultNote(t *testing.T, p arcreview.ForgeVaultNotePayload) arcreview.FilingDecision {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal vault note payload: %v", err)
	}
	return arcreview.FilingDecision{
		Action:     arcreview.ActionForgeVaultNote,
		Payload:    raw,
		Confidence: 0.9,
	}
}

func decisionForgeBug(t *testing.T, p arcreview.ForgeBugPayload) arcreview.FilingDecision {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal bug payload: %v", err)
	}
	return arcreview.FilingDecision{
		Action:     arcreview.ActionForgeBug,
		Payload:    raw,
		Confidence: 0.9,
	}
}

func decisionForgeSuggestion(t *testing.T, p arcreview.ForgeSuggestionPayload) arcreview.FilingDecision {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal suggestion payload: %v", err)
	}
	return arcreview.FilingDecision{
		Action:     arcreview.ActionForgeSuggestion,
		Payload:    raw,
		Confidence: 0.9,
	}
}

// --- ForgeVaultNote: test-restatement rejection ----------------

// 2+ test markers → VaultNoteTestRestatement. F1 corpus
// category C-test-docstring-restatement is 45% of all forge_vault_note
// proposals; this is the highest-yield single rule.
func TestCheckBoilerplate_VaultNoteRejectsMultipleTestMarkers(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "Some learning",
		Body: `When the test runs, t.Errorf fires on mismatch.
Specifically the func TestHandle case in handler_test.go checks
the expected output. The blurb describes the assertion shape.`,
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteTestRestatement {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteTestRestatement)
	}
}

// >60% code-block ratio → VaultNoteHighCodeRatio. Body with brief
// prose framing one large code block — the kind of "vault note that
// is mostly the code it describes" shape F1 corpus flagged as noise.
func TestCheckBoilerplate_VaultNoteRejectsHighCodeRatio(t *testing.T) {
	body := "Intro.\n" +
		"```go\n" +
		"func Example1() { return }\n" +
		"func Example2() { return }\n" +
		"func Example3() { return }\n" +
		"func Example4() { return }\n" +
		"func Example5() { return }\n" +
		"func Example6() { return }\n" +
		"func Example7() { return }\n" +
		"func Example8() { return }\n" +
		"func Example9() { return }\n" +
		"func Example10() { return }\n" +
		"```\n"
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "reference",
		Title:    "Example reference",
		Body:     body,
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteHighCodeRatio {
		t.Errorf("reason: got %q, want %q (body=%q)", got, arcreview.VaultNoteHighCodeRatio, body)
	}
}

// Short body with no paragraph break → VaultNoteTooShort.
func TestCheckBoilerplate_VaultNoteRejectsShortNoBreak(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "A small thought",
		Body:     "A single-sentence body that paraphrases something we already wrote elsewhere.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteTooShort {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteTooShort)
	}
}

// Substantive vault note (real synthesis, paragraph breaks, no test
// markers, low code ratio) → not rejected.
func TestCheckBoilerplate_VaultNoteAcceptsSubstantiveBody(t *testing.T) {
	body := strings.Repeat(
		"This is the first paragraph of a meaningful vault decision that synthesises across multiple sessions.\n\n"+
			"The second paragraph names the decision rationale and the trade-offs considered.\n\n"+
			"The third paragraph documents the supersession trigger and what the next-iteration choice would be.\n\n",
		1,
	)
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "decision",
		Title:    "A substantive decision",
		Body:     body,
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.BoilerplateNotRejected {
		t.Errorf("substantive vault note rejected with reason %q; want accepted", got)
	}
}

// --- ForgeVaultNote: implementation-diary + outcome-paraphrase ----

// "This note captures..." opener → VaultNoteImplementationDiaryStarter.
// Pattern observed 3+ times during chain
// arc-close-filing-review-dedupe-and-noise-reduction's own development.
func TestCheckBoilerplate_VaultNoteRejectsThisNoteCapturesOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "Some learning",
		Body:     "This note captures the detailed steps and rationale for adding telemetry fields to monitor filter pipeline engagement.\n\nFurther context: the change went through a build + test cycle.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

func TestCheckBoilerplate_VaultNoteRejectsDocumentingOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "Documenting the envelope shape requirement for task creation",
		Body:     "When creating tasks via the forge call, chain_slug must be included within the fields object and not at the top level.\n\nSecond paragraph adding more detail.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

// "was tested showing X%" → VaultNoteOutcomeParaphrase.
func TestCheckBoilerplate_VaultNoteRejectsOutcomeParaphrase(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "reference",
		Title:    "A reference note",
		Body:     "The F4 rule set was tested showing 45% effectiveness against the labelled corpus.\n\nFurther details about the harness.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteOutcomeParaphrase {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteOutcomeParaphrase)
	}
}

// "improvement over baseline" → VaultNoteOutcomeParaphrase.
func TestCheckBoilerplate_VaultNoteRejectsImprovementOverBaseline(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "decision",
		Title:    "A decision note",
		Body:     "The new filter caught 45% of corpus rows, an improvement over the previous baseline of 34%.\n\nDetails follow.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteOutcomeParaphrase {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteOutcomeParaphrase)
	}
}

// --- F4 v2 regression suite (2026-05-22, F5 retrospective) ---
//
// Each test corresponds to a noise filing observed in the post-
// telemetry measurement window (N=9 fires; 7 noise) that the v1
// diary-opener regex set missed. Body openers pinned verbatim from
// the observed events.

func TestCheckBoilerplate_VaultNoteV2_DuringTheProcessOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "Neighbour-Pollution in Tag Suggestions",
		Body:     "During the process of applying tags to vault entries, the neighbour-based tag suggestion method resulted in high neighbour-pollution.\n\nMore detail follows.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

func TestCheckBoilerplate_VaultNoteV2_DuringTheVaultSweepTriageOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "False-positive Retire Signal from Borderline No Signal Heuristic",
		Body:     "During the vault-sweep triage process, entries with structured content (such as numbered H2 sections) were mistakenly classified for retirement.\n\nMore prose.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

func TestCheckBoilerplate_VaultNoteV2_DuringTheVaultCleanupOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "Heuristic Triage for Vault Entries",
		Body:     "During the vault cleanup attempt (T6), a heuristic-based triage script was used to classify vault entries for possible retirement.\n\nMore.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

func TestCheckBoilerplate_VaultNoteV2_TheProcessForOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "reference",
		Title:    "Cross-subdirectory Vault Linkage Process",
		Body:     "The process for generating and applying cross-subdirectory wikilinks within a vault involves several steps:\n\n1. Surface\n2. Apply",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

func TestCheckBoilerplate_VaultNoteV2_ThisNoteDocumentsOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "reference",
		Title:    "Implementing Intra-Subdir Wikilinks",
		Body:     "This note documents the process and decisions involved in implementing intra-subdir wikilinks within the vault-linkage script.\n\nDetails.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

func TestCheckBoilerplate_VaultNoteV2_DocumentsTheXProcessOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "Cross-subdirectory Vault Linkage Process",
		Body:     "Documents the cross-subdirectory linkage process in a vault, a reusable reference for future tasks.\n\nMore.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

func TestCheckBoilerplate_VaultNoteV2_RecurringGotchaOpener(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "Neighbour-Pollution Recurring Gotcha",
		Body:     "The neighbour-pollution issue is a recurring gotcha that needs to be documented for future reference.\n\nMore.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteImplementationDiaryStarter {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteImplementationDiaryStarter)
	}
}

// Negative test for over-match: a substantive opener using
// vocabulary that *doesn't* match the narrative-voice shape (no
// "during the X" / "the X for Y" / "this note Y" prefix) should
// pass. Pins the over-match boundary so future regex extensions
// don't accidentally widen the catch.
func TestCheckBoilerplate_VaultNoteV2_SubstantiveSynthesisOpenerPasses(t *testing.T) {
	body := "Two distinct axes of risk surfaced during the Rust-to-Go migration: schema-shape divergence between sqlx and database/sql, and dispatch-typed-returns drift.\n\n" +
		"Each axis applies cross-project. Future agents porting between ORMs with different schema-validation disciplines should evaluate axis-1 first — schema enforcement (compile / runtime / none) is the structural foreground.\n\n" +
		"## Related\n\n- pattern across projects\n- future agents apply this when porting\n"
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "decision",
		Title:    "Two axes of risk in cross-language ORM ports",
		Body:     body,
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.BoilerplateNotRejected {
		t.Errorf("substantive synthesis opener rejected with %q; v2 should not over-match", got)
	}
}

// "successfully implemented" → VaultNoteOutcomeParaphrase.
func TestCheckBoilerplate_VaultNoteRejectsSuccessfullyImplemented(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "learning",
		Title:    "A learning",
		Body:     "The dedupe pipeline was successfully implemented across the three filter mechanisms.\n\nFollow-up paragraph.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.VaultNoteOutcomeParaphrase {
		t.Errorf("reason: got %q, want %q", got, arcreview.VaultNoteOutcomeParaphrase)
	}
}

// Negative: a vault note that uses outcome-shape vocabulary in a
// substantive cross-project synthesis should NOT trip the rule.
// Discriminator: body names a cross-project pattern or generalises;
// my current rules can't distinguish that semantically, so this
// test pins a known weakness (it WILL fail-reject; documents the
// false-positive risk for the F5 retrospective to measure).
func TestCheckBoilerplate_VaultNoteAcceptsRealSynthesisWithoutDiaryShape(t *testing.T) {
	d := decisionForgeVaultNote(t, arcreview.ForgeVaultNotePayload{
		NoteKind: "decision",
		Title:    "Substrate cues: pointer-to-inline conversion when pointers are non-binding",
		Body:     "When a substrate emits a recommended_action field on a candidate envelope, two implementations are available: pointer-on-detection and inline-on-detection.\n\nThe decision: convert pointer-on-detection to inline-on-detection for any substrate cue where the pointer is non-binding and the load-bearing content fits in a reasonable byte budget.\n\nLessons captured: the pattern generalises across multiple substrate cues, not just the parse_context skill body case.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.BoilerplateNotRejected {
		t.Errorf("real-synthesis vault note rejected with reason %q; want accepted", got)
	}
}

// --- ForgeBug: operator-error marker rejection -----------------

// problem_statement contains an operator-error marker → BugOperatorErrorMarker.
// F1 corpus category D-operator-error.
func TestCheckBoilerplate_BugRejectsOperatorErrorMarker(t *testing.T) {
	for _, marker := range []string{
		"binary not found",
		"forgot to run",
		"missing in expected location",
	} {
		t.Run("marker="+marker, func(t *testing.T) {
			d := decisionForgeBug(t, arcreview.ForgeBugPayload{
				Title:            "Build problem",
				ProblemStatement: "During the smoke test, the " + marker + " in go/bin/, requiring an explicit build step.",
			})
			if got := arcreview.CheckBoilerplate(d); got != arcreview.BugOperatorErrorMarker {
				t.Errorf("reason: got %q, want %q (marker=%q)", got, arcreview.BugOperatorErrorMarker, marker)
			}
		})
	}
}

// Real bug (no operator-error markers in problem_statement) → not rejected.
func TestCheckBoilerplate_BugAcceptsRealDefectDescription(t *testing.T) {
	d := decisionForgeBug(t, arcreview.ForgeBugPayload{
		Title:            "Cursor off-by-one in pagination",
		ProblemStatement: "The cursor advances one row past the last visible page boundary; symptom: the next-page fetch skips the trailing row consistently.",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.BoilerplateNotRejected {
		t.Errorf("real-bug payload rejected with reason %q; want accepted", got)
	}
}

// --- ForgeSuggestion: generic-title + placeholder rejections ---

// Generic title + short problem_statement → SuggestionGenericTitle.
// F1 corpus category F-insufficient-payload-boilerplate.
func TestCheckBoilerplate_SuggestionRejectsGenericTitleWithoutSpecifics(t *testing.T) {
	cases := []string{
		"Add a regression test for parse-context",
		"Refactor body-inliner",
		"Document arcreview",
	}
	for _, title := range cases {
		t.Run("title="+title, func(t *testing.T) {
			d := decisionForgeSuggestion(t, arcreview.ForgeSuggestionPayload{
				Title:            title,
				ProblemStatement: "Short; under 200 chars.",
			})
			if got := arcreview.CheckBoilerplate(d); got != arcreview.SuggestionGenericTitle {
				t.Errorf("reason: got %q, want %q (title=%q)", got, arcreview.SuggestionGenericTitle, title)
			}
		})
	}
}

// Same generic-shaped title but problem_statement carries enough
// specifics (>200 chars) → accepted.
func TestCheckBoilerplate_SuggestionAcceptsGenericTitleWithSpecifics(t *testing.T) {
	d := decisionForgeSuggestion(t, arcreview.ForgeSuggestionPayload{
		Title: "Add a regression test for parse-context",
		ProblemStatement: strings.Repeat(
			"The body-inliner's eligibility check has no coverage for the new ShapeSkillCandidate shape after commit fcb62e5. ",
			3,
		),
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.BoilerplateNotRejected {
		t.Errorf("specifics-carrying suggestion rejected with reason %q; want accepted", got)
	}
}

// source field contains YYYY-MM-DD placeholder → SuggestionPlaceholderDate.
func TestCheckBoilerplate_SuggestionRejectsPlaceholderDateInSource(t *testing.T) {
	d := decisionForgeSuggestion(t, arcreview.ForgeSuggestionPayload{
		Title:            "Improve some non-generic-shaped thing",
		ProblemStatement: strings.Repeat("body body body body body body body body body body. ", 30),
		Source:           "session retro on YYYY-MM-DD",
	})
	if got := arcreview.CheckBoilerplate(d); got != arcreview.SuggestionPlaceholderDate {
		t.Errorf("reason: got %q, want %q", got, arcreview.SuggestionPlaceholderDate)
	}
}

// --- Negative cases: actions not subject to F4 rules ---------

// nothing_to_file action: returns BoilerplateNotRejected unconditionally.
func TestCheckBoilerplate_NothingToFilePassesThrough(t *testing.T) {
	d := arcreview.FilingDecision{Action: arcreview.ActionNothingToFile}
	if got := arcreview.CheckBoilerplate(d); got != arcreview.BoilerplateNotRejected {
		t.Errorf("nothing_to_file should pass; got reason %q", got)
	}
}

// skill_update action: not currently covered by F4 rules; should
// pass through (F4 may add rules later).
func TestCheckBoilerplate_SkillUpdatePassesThrough(t *testing.T) {
	d := arcreview.FilingDecision{
		Action:  arcreview.ActionSkillUpdate,
		Payload: json.RawMessage(`{"skill_slug":"foo","patch_kind":"add_section","content":"…"}`),
	}
	if got := arcreview.CheckBoilerplate(d); got != arcreview.BoilerplateNotRejected {
		t.Errorf("skill_update should pass under current F4 rules; got reason %q", got)
	}
}
