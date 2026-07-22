package arcreview

import (
	"encoding/json"
	"regexp"
	"strings"
)

// F4 of chain arc-close-filing-review-dedupe-and-noise-reduction:
// content-validation rejection rules that catch the boilerplate /
// operator-error / restatement filings characterised in F1's
// labelled corpus (see docs/ARC_CLOSE_FILING_REVIEW_DEDUPE.md §2).
//
// Distinct from ValidateDecision in schema.go, which does
// SHAPE validation (action enum, payload struct, required fields).
// CheckBoilerplate operates on the CONTENT of an otherwise-valid
// decision and returns a typed rejection code when the payload
// matches one of the noise patterns.

// BoilerplateRejectionReason enumerates the F4 rejection codes.
// Empty string means "not rejected; passes content validation."
type BoilerplateRejectionReason string

const (
	// BoilerplateNotRejected is the empty-string sentinel returned
	// when no rule trips. Callers test `if reason != "" { reject }`.
	BoilerplateNotRejected BoilerplateRejectionReason = ""

	// VaultNoteTestRestatement: forge_vault_note body contains 2+
	// test-marker substrings (// @blurb, t.Errorf, etc.) — almost
	// certainly a paraphrase of test docstrings just authored.
	// rule justification: F1 corpus category C-test-docstring-restatement (18 of 40 forge_vault_note rows).
	VaultNoteTestRestatement BoilerplateRejectionReason = "vault_note_test_restatement"

	// VaultNoteHighCodeRatio: forge_vault_note body has >60% of its
	// lines inside fenced code blocks. Code-heavy "decisions" rarely
	// carry cross-project insight; the code itself is the durable
	// artifact.
	// rule justification: F1 corpus category C-test-docstring-restatement.
	VaultNoteHighCodeRatio BoilerplateRejectionReason = "vault_note_high_code_block_ratio"

	// VaultNoteTooShort: body < 400 chars AND no paragraph break.
	// Bodies this short rarely carry novel synthesis worth a vault
	// entry; they're typically single-thought restatements.
	// rule justification: F1 corpus category C-test-docstring-restatement.
	VaultNoteTooShort BoilerplateRejectionReason = "vault_note_too_short_no_paragraph_break"

	// BugOperatorErrorMarker: forge_bug problem_statement contains
	// markers indicating the "bug" is actually operator error (binary
	// missing because the operator didn't build, etc.).
	// rule justification: F1 corpus category D-operator-error (3 of 11 forge_bug rows).
	BugOperatorErrorMarker BoilerplateRejectionReason = "bug_operator_error_marker"

	// SuggestionGenericTitle: forge_suggestion title matches generic
	// patterns ("Add a regression test for X", "Refactor X", "Document X")
	// without a problem_statement long enough to add specifics.
	// rule justification: F1 corpus category F-insufficient-payload-boilerplate.
	SuggestionGenericTitle BoilerplateRejectionReason = "suggestion_generic_title_no_specifics"

	// SuggestionPlaceholderDate: forge_suggestion source field
	// contains the literal "YYYY-MM-DD" placeholder (belt-and-
	// suspenders for pre-commit-b870665 arcs; commit b870665 fixed
	// the upstream prompt-shape but cached / old corpus arcs may
	// still surface the literal).
	// rule justification: F1 corpus + commit b870665.
	SuggestionPlaceholderDate BoilerplateRejectionReason = "suggestion_placeholder_date_in_source"

	// VaultNoteImplementationDiaryStarter: forge_vault_note title or
	// body opening sentence starts with diary-style framing —
	// "This note captures...", "Documenting...", "This learning
	// documents...". The note IS the just-committed work, narrated;
	// no cross-project pattern named.
	// rule justification: 3+ live filings observed during chain
	// arc-close-filing-review-dedupe-and-noise-reduction development
	// (events 019e4c20-d0be / 019e4cce / 019e4ce7 on 2026-05-21).
	VaultNoteImplementationDiaryStarter BoilerplateRejectionReason = "vault_note_implementation_diary_starter"

	// VaultNoteOutcomeParaphrase: forge_vault_note body contains
	// past-tense outcome language with a specific metric or
	// implementation detail. The note is paraphrasing the commit
	// message, not synthesising cross-project insight.
	// Examples observed: "was tested showing 45%", "successfully
	// implemented", "improvement over baseline".
	// rule justification: same 3+ live filings as above.
	VaultNoteOutcomeParaphrase BoilerplateRejectionReason = "vault_note_outcome_paraphrase"
)

// ---- Heuristic constants -----------------------------------------

// vaultNoteCodeBlockRatioThreshold is the maximum ratio of body
// lines that may sit inside fenced code blocks before the vault_note
// is rejected as code-heavy. F1 spot-check found 18 of 40 historical
// forge_vault_note proposals matched this shape.
const vaultNoteCodeBlockRatioThreshold = 0.60

// vaultNoteTestMarkerMinHits is the threshold count of test-marker
// substrings (// @blurb, t.Errorf, etc.) above which the body is
// classified as test-docstring-restatement.
const vaultNoteTestMarkerMinHits = 2

// vaultNoteShortBodyMaxLen is the upper bound on body length for the
// "too short, no paragraph break" rejection. Bodies under this size
// without a `\n\n` paragraph break almost never carry novel synthesis.
const vaultNoteShortBodyMaxLen = 400

// suggestionShortProblemMinLen is the lower bound on problem_statement
// length below which a generic-title suggestion is rejected. Specifics
// take >200 chars to articulate.
const suggestionShortProblemMinLen = 200

// vaultNoteDiaryStarterRegexes match title or body-opener phrasing
// that signals the note is paraphrasing the just-committed work
// rather than synthesising a cross-project lesson. Anchored to the
// title OR the first ~80 characters of the body (Qwen's restatement
// shape opens with the diary phrasing; later sentences may not).
//
// F4 v2 (2026-05-22 per F5 retrospective of chain 618): extended to
// cover the "During the..." narrative-voice opener that dominated 7
// of 9 post-telemetry fires the v1 regex set missed. Specific noun
// alternations after "during the" / "the X for" / etc. are
// load-bearing — bare "During the..." would over-match legitimate
// cross-project prose ("During the migration, two distinct patterns
// emerged...").
var vaultNoteDiaryStarterRegexes = []*regexp.Regexp{
	// v1 patterns (commit 59a8f2a).
	regexp.MustCompile(`(?i)^\s*this note captures (the|some|a)?`),
	regexp.MustCompile(`(?i)^\s*documenting (the|a|some)?`),
	regexp.MustCompile(`(?i)^\s*(this|the) (decision|learning|reference) documents`),
	// v2 patterns (F5 retrospective addition).
	// "During the {process,session,implementation,...}..." — the noun
	// alternation is the over-match guard (bare "during the" matches
	// far too many legitimate prose openings). The [\w\- ]{0,30}
	// optional gap allows compound modifiers like "vault-sweep
	// triage" or "vault cleanup" between "the" and the trigger noun.
	regexp.MustCompile(`(?i)^\s*during the [\w\- ]{0,30}(process|session|implementation|cleanup|attempt|development|migration|sweep|chain|triage|conversation|task|work)\b`),
	// "The {process,approach,method,technique,implementation,strategy} for..."
	// — narrating a procedure that just shipped.
	regexp.MustCompile(`(?i)^\s*the (process|approach|method|technique|implementation|strategy) for `),
	// "This note {documents,describes,records,serves}..." — broader than
	// v1's "this note captures" which missed sibling verbs.
	regexp.MustCompile(`(?i)^\s*this note (documents|describes|records|serves)`),
	// "{This,The} {note,decision,learning,reference} {documents,describes,captures,serves as}..."
	// — sibling of the v1 "documents" rule with broader verb coverage.
	regexp.MustCompile(`(?i)^\s*(this|the) (note|decision|learning|reference) (documents|describes|captures|serves as)`),
	// "Documents the X process / Documents the X for Y" body shape —
	// narrative-voice opener (no leading subject; the verb leads).
	regexp.MustCompile(`(?i)^\s*documents the .{0,40}(process|approach|strategy|method)`),
	// "The {N}-X issue is a recurring gotcha..." — common shape for
	// Qwen paraphrasing a single observed issue as a "learning".
	regexp.MustCompile(`(?i)^\s*the .{0,40} (issue|problem|gotcha) (is|was) a recurring`),
}

// vaultNoteOutcomeParaphraseRegexes match body phrasings that
// describe a just-committed change in past-tense outcome form. The
// note narrates "what was done" with metrics / specifics rather
// than naming a cross-project pattern.
var vaultNoteOutcomeParaphraseRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)was tested.{0,40}showing \d+%`),
	regexp.MustCompile(`(?i)improvement over .{0,40}baseline`),
	regexp.MustCompile(`(?i)successfully (implemented|tested|committed|landed|built|ran|verified)`),
	regexp.MustCompile(`(?i)were (successfully )?(implemented|tested|committed|landed|added)`),
}

// vaultNoteTestMarkers are substrings whose presence signals the body
// paraphrases test code rather than synthesising a cross-project lesson.
// Extend as new test-shape patterns surface (Rust markers: assert!,
// #[test]; Python: def test_, pytest.mark; etc.).
var vaultNoteTestMarkers = []string{
	"// @blurb",
	"expect(",
	"t.Errorf",
	"t.Fatalf",
	"t.Run",
	"TestHandle",
	"func Test",
	"assert!(",
	"#[test]",
}

// bugOperatorErrorMarkers are lowercase substrings whose presence in
// a forge_bug problem_statement indicates the "bug" is actually
// operator error (typically a workaround the operator applied to a
// mistake they made themselves).
var bugOperatorErrorMarkers = []string{
	"binary not found",
	"file not found",
	"forgot to run",
	"missing in expected location",
	"not found in expected",
	"before the command could be executed",
	"required a workaround",
	"had to be rebuilt",
}

// suggestionGenericTitleRegexes match titles that are TOO GENERIC to
// be actionable without specifics in problem_statement. A title like
// "Add a regression test for X" must be accompanied by which test,
// which scenario, what coverage gap — otherwise it's boilerplate.
var suggestionGenericTitleRegexes = []*regexp.Regexp{
	regexp.MustCompile(`^Add (a )?regression test for [A-Za-z\-_]+$`),
	regexp.MustCompile(`^Refactor [A-Za-z\-_]+$`),
	regexp.MustCompile(`^Document [A-Za-z\-_]+$`),
	regexp.MustCompile(`^Improve [A-Za-z\-_ ]{1,30}$`),
}

// ---- Public API --------------------------------------------------

// RejectedDecision pairs a FilingDecision with the F4 rejection
// reason that caused the dispatcher to skip it. Threaded through
// ArcReviewResult so audit / measurement consumers can see what
// the validator rejected and why.
type RejectedDecision struct {
	Decision FilingDecision             `json:"decision"`
	Reason   BoilerplateRejectionReason `json:"reason"`
}

// CheckBoilerplate scans a FilingDecision's content payload against
// the F4 rejection rule set. Returns BoilerplateNotRejected (the
// empty string) when no rule trips; returns the first matching
// rejection code otherwise.
//
// Caller contract: invoke ONLY after ValidateDecision returns nil.
// CheckBoilerplate assumes the payload shape is well-formed; it
// will silently return BoilerplateNotRejected on a payload that
// fails to decode into the action-specific struct.
func CheckBoilerplate(d FilingDecision) BoilerplateRejectionReason {
	switch d.Action {
	case ActionForgeVaultNote:
		var p ForgeVaultNotePayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return BoilerplateNotRejected
		}
		return checkVaultNoteBoilerplate(p)
	case ActionForgeBug:
		var p ForgeBugPayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return BoilerplateNotRejected
		}
		return checkBugBoilerplate(p)
	case ActionForgeSuggestion:
		var p ForgeSuggestionPayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return BoilerplateNotRejected
		}
		return checkSuggestionBoilerplate(p)
	}
	return BoilerplateNotRejected
}

// ---- Per-action checks -------------------------------------------

func checkVaultNoteBoilerplate(p ForgeVaultNotePayload) BoilerplateRejectionReason {
	body := p.Body
	if body == "" {
		return BoilerplateNotRejected
	}
	// Implementation-diary starter → narration, not synthesis.
	// Check the title AND the body's opening sentence (first 80
	// chars covers the typical Qwen restatement-opener).
	bodyOpener := body
	if len(bodyOpener) > 80 {
		bodyOpener = bodyOpener[:80]
	}
	for _, re := range vaultNoteDiaryStarterRegexes {
		if re.MatchString(p.Title) || re.MatchString(bodyOpener) {
			return VaultNoteImplementationDiaryStarter
		}
	}
	// Past-tense outcome paraphrase → narrating the commit, not
	// synthesising cross-project insight.
	for _, re := range vaultNoteOutcomeParaphraseRegexes {
		if re.MatchString(body) {
			return VaultNoteOutcomeParaphrase
		}
	}
	// Test-marker hits → restatement.
	markerHits := 0
	for _, marker := range vaultNoteTestMarkers {
		if strings.Contains(body, marker) {
			markerHits++
			if markerHits >= vaultNoteTestMarkerMinHits {
				return VaultNoteTestRestatement
			}
		}
	}
	// Code-block ratio → code-heavy.
	if codeBlockRatio(body) > vaultNoteCodeBlockRatioThreshold {
		return VaultNoteHighCodeRatio
	}
	// Short body, no paragraph break → single-thought restatement.
	if len(body) < vaultNoteShortBodyMaxLen && !strings.Contains(body, "\n\n") {
		return VaultNoteTooShort
	}
	return BoilerplateNotRejected
}

func checkBugBoilerplate(p ForgeBugPayload) BoilerplateRejectionReason {
	lowered := strings.ToLower(p.ProblemStatement)
	for _, marker := range bugOperatorErrorMarkers {
		if strings.Contains(lowered, marker) {
			return BugOperatorErrorMarker
		}
	}
	return BoilerplateNotRejected
}

func checkSuggestionBoilerplate(p ForgeSuggestionPayload) BoilerplateRejectionReason {
	title := strings.TrimSpace(p.Title)
	for _, re := range suggestionGenericTitleRegexes {
		if re.MatchString(title) && len(p.ProblemStatement) < suggestionShortProblemMinLen {
			return SuggestionGenericTitle
		}
	}
	if strings.Contains(p.Source, "YYYY-MM-DD") {
		return SuggestionPlaceholderDate
	}
	return BoilerplateNotRejected
}

// ---- Helpers -----------------------------------------------------

// codeBlockRatio returns the fraction of body lines that sit inside
// fenced code blocks (between matching triple-backtick lines). The
// fence lines themselves don't count as inside; only lines BETWEEN
// a pair of fences. Unclosed fences are tolerated — lines after an
// unclosed opening fence count as inside.
func codeBlockRatio(body string) float64 {
	lines := strings.Split(body, "\n")
	if len(lines) == 0 {
		return 0
	}
	inBlock := false
	inside := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inBlock = !inBlock
			continue
		}
		if inBlock {
			inside++
		}
	}
	return float64(inside) / float64(len(lines))
}
