package arcreview

import (
	"strings"
	"testing"
)

func TestComposeArcSummaryPrompt_IncludesSnapshot(t *testing.T) {
	snap := Snapshot{
		Messages: []Message{
			{Role: "user", Content: "what are we working on?"},
			{Role: "assistant", Content: "T4 arcreview package"},
		},
		EstimatedTokens: 12,
	}
	sys, user := ComposeArcSummaryPrompt(snap)
	if !strings.Contains(sys, "summarize") && !strings.Contains(sys, "summary") {
		t.Fatalf("arc-summary system prompt should mention summarization, got %q", sys)
	}
	if !strings.Contains(user, "what are we working on?") {
		t.Fatalf("arc-summary user prompt should include snapshot content, got %q", user)
	}
	if !strings.Contains(user, "T4 arcreview package") {
		t.Fatalf("arc-summary user prompt should include assistant turn, got %q", user)
	}
}

func TestComposeReviewPrompt_TruncatedHeaderSurfaces(t *testing.T) {
	snap := Snapshot{
		Messages:        []Message{{Role: "user", Content: "x"}},
		Truncated:       true,
		EstimatedTokens: 100,
	}
	_, user := ComposeReviewPrompt(snap, "summary text", []string{"counter_user_turns_5"}, nil)
	if !strings.Contains(user, "snapshot truncated") {
		t.Fatalf("review prompt should mark truncated snapshots, got %q", user)
	}
	if !strings.Contains(user, "summary text") {
		t.Fatalf("review prompt should include arc summary, got %q", user)
	}
	if !strings.Contains(user, "counter_user_turns_5") {
		t.Fatalf("review prompt should name trigger signals, got %q", user)
	}
}

func TestComposeReviewPrompt_EmptySummaryFallback(t *testing.T) {
	snap := Snapshot{Messages: []Message{{Role: "user", Content: "x"}}}
	_, user := ComposeReviewPrompt(snap, "", nil, nil)
	if !strings.Contains(user, "no arc summary") {
		t.Fatalf("missing arc summary should hit the fallback string, got %q", user)
	}
	if !strings.Contains(user, "none — fire issued without trigger metadata") {
		t.Fatalf("empty triggers should hit the fallback string, got %q", user)
	}
	if strings.Contains(user, "ALREADY FILED") {
		t.Fatalf("ALREADY FILED block must be omitted when recentFilings is nil/empty, got %q", user)
	}
}

func TestComposeReviewPrompt_AlreadyFiledBlockAppearsWhenSupplied(t *testing.T) {
	snap := Snapshot{Messages: []Message{{Role: "user", Content: "x"}}}
	recent := []RecentFiling{
		{Kind: "bug", Slug: "first-bug-slug", Title: "First bug title"},
		{Kind: "bug", Slug: "second-bug-slug", Title: "Second bug title"},
	}
	_, user := ComposeReviewPrompt(snap, "", nil, recent)
	if !strings.Contains(user, "ALREADY FILED IN THIS ARC") {
		t.Fatalf("ALREADY FILED block must surface when recentFilings is non-empty, got %q", user)
	}
	for _, r := range recent {
		if !strings.Contains(user, r.Slug) {
			t.Errorf("ALREADY FILED block must name slug %q, got %q", r.Slug, user)
		}
		if !strings.Contains(user, r.Title) {
			t.Errorf("ALREADY FILED block must name title %q, got %q", r.Title, user)
		}
	}
}

func TestReviewSystemPrompt_NamesAllActions(t *testing.T) {
	for _, action := range allActions {
		if !strings.Contains(reviewSystemPrompt(), string(action)) {
			t.Fatalf("review system prompt must name action %q so Qwen sees the closed enum",
				string(action))
		}
	}
}

func TestReviewSystemPrompt_NamesPrescriptiveLanguage(t *testing.T) {
	// Key prescriptive phrases per design §Review-prompt. Drift here
	// is a real signal that the prompt has lost its prescriptive edge.
	mustContain := []string{
		"Be ACTIVE",
		"NOT be",
		"missed learning opportunity",
		"PREFERENCE ORDER",
		"AMEND or SUPERSEDE",
		"DO NOT FILE",
		"SIGNAL TAXONOMY",
	}
	for _, phrase := range mustContain {
		if !strings.Contains(reviewSystemPrompt(), phrase) {
			t.Fatalf("review system prompt must contain prescriptive phrase %q", phrase)
		}
	}
}

// F7 of chain arc-close-filing-review-dedupe-and-noise-reduction:
// the prompt must surface the content-shape anti-patterns from
// docs/CONTENT_ROUTING_TAXONOMY.md §3 so Qwen suppresses noise
// filings at source rather than relying on F4's downstream filter.
// Drift here = silent loss of the source-side gate.
func TestReviewSystemPrompt_NamesContentShapeAntiPatterns(t *testing.T) {
	mustContain := []string{
		"CONTENT-SHAPE ANTI-PATTERNS",
		"DIARY-STYLE BODY OPENER",
		"This note captures",
		"OUTCOME PARAPHRASE",
		"was tested showing",
		"COMMIT-SPECIFIC NARRATIVE",
		"PROCEDURAL HOW-TO IN VAULT SHAPE",
		"OPERATOR-ERROR WORKAROUND",
		"UNDER-400-WORD BODY",
	}
	prompt := reviewSystemPrompt()
	for _, phrase := range mustContain {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("review system prompt must surface F7 anti-pattern phrase %q", phrase)
		}
	}
}

// Convention drift detector: every placeholder in the forge_*
// payload examples should wrap the to-be-filled-in value in angle
// brackets so Qwen substitutes rather than rendering the literal.
// The forge_suggestion source slot was the lone outlier before
// (rendered "session retro on YYYY-MM-DD" verbatim into the filed
// suggestion's source field; see suggestion
// `arcreview-forge-suggestion-source-placeholder-rendered-literally`).
// Pin the fix: prompt MUST NOT contain the bare-literal form.
func TestReviewSystemPrompt_ForgeSuggestionSourcePlaceholderIsAngleBracketed(t *testing.T) {
	prompt := reviewSystemPrompt()
	const badLiteral = `"source": "session retro on YYYY-MM-DD"`
	if strings.Contains(prompt, badLiteral) {
		t.Errorf("review prompt contains the bare-literal forge_suggestion source placeholder %q; Qwen will render it verbatim instead of substituting. Wrap in angle brackets per the schema's convention.", badLiteral)
	}
	// Positive assertion: the angle-bracketed convention now applies.
	const goodForm = `"source": "<session retro on YYYY-MM-DD`
	if !strings.Contains(prompt, goodForm) {
		t.Errorf("review prompt missing the angle-bracketed forge_suggestion source placeholder; expected to find a substring starting with %q.", goodForm)
	}
}
