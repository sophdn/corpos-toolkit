package benchmarks

import (
	"strings"
	"testing"
)

// allowed mirrors Rust runner::tests::allowed — a thin string-slice
// constructor used across the parser tests.
func allowedLabels(strs ...string) []string {
	out := make([]string, len(strs))
	copy(out, strs)
	return out
}

// ── parse_classify_response — bullet/number/whitespace tolerance ──────

// Mirrors Rust runner::parse_strips_bullets_and_lower_cases.
func TestParseClassifyResponse_StripsBulletsAndLowerCases(t *testing.T) {
	labels, unclassifiable := ParseClassifyResponse("- LOW", allowedLabels("low", "medium", "high"))
	if len(labels) != 1 || labels[0] != "low" {
		t.Errorf("labels: want [low], got %v", labels)
	}
	if unclassifiable {
		t.Errorf("unclassifiable: want false, got true")
	}
}

// Mirrors Rust runner::parse_handles_comma_separated.
func TestParseClassifyResponse_HandlesCommaSeparated(t *testing.T) {
	labelsSet := allowedLabels("clippy", "test-infrastructure", "benchmarks")
	labels, _ := ParseClassifyResponse("clippy, benchmarks, test-infrastructure", labelsSet)
	if len(labels) != 3 {
		t.Errorf("expected 3 labels, got %d: %v", len(labels), labels)
	}
	want := map[string]bool{"clippy": true, "benchmarks": true, "test-infrastructure": true}
	for _, l := range labels {
		if !want[l] {
			t.Errorf("unexpected label: %s", l)
		}
	}
}

// Pin the trailing-digit-preservation invariant from the parse_classify_response
// docstring: "tier-0" must NOT reduce to "tier" via aggressive tail-strip.
func TestParseClassifyResponse_PreservesTrailingDigitsOnLabels(t *testing.T) {
	labels, _ := ParseClassifyResponse("tier-0", allowedLabels("tier-0", "tier-1", "tier-2"))
	if len(labels) != 1 || labels[0] != "tier-0" {
		t.Errorf("want [tier-0], got %v (trailing digits got stripped — regression)", labels)
	}
}

// Pin unclassifiable detection.
func TestParseClassifyResponse_DetectsUnclassifiable(t *testing.T) {
	labels, unclassifiable := ParseClassifyResponse("unclassifiable", allowedLabels("low", "medium"))
	if len(labels) != 0 {
		t.Errorf("want no labels, got %v", labels)
	}
	if !unclassifiable {
		t.Errorf("want unclassifiable=true, got false")
	}
}

// Pin numeric-prefix stripping ("1. foo" → "foo").
func TestParseClassifyResponse_StripsNumericListPrefix(t *testing.T) {
	labels, _ := ParseClassifyResponse("1. low\n2. medium", allowedLabels("low", "medium", "high"))
	if len(labels) != 2 || labels[0] != "low" || labels[1] != "medium" {
		t.Errorf("want [low medium], got %v", labels)
	}
}

// Pin dedupe behavior — same label twice = once in output.
func TestParseClassifyResponse_DedupesLabels(t *testing.T) {
	labels, _ := ParseClassifyResponse("low, low, medium, low", allowedLabels("low", "medium", "high"))
	if len(labels) != 2 || labels[0] != "low" || labels[1] != "medium" {
		t.Errorf("want [low medium] (dedup-keep-first), got %v", labels)
	}
}

// Pin: labels not in the allowed set get dropped silently.
func TestParseClassifyResponse_DropsOffAllowedLabels(t *testing.T) {
	labels, _ := ParseClassifyResponse("low, off-rubric-label", allowedLabels("low", "medium"))
	if len(labels) != 1 || labels[0] != "low" {
		t.Errorf("want [low] (off-rubric dropped), got %v", labels)
	}
}

// ── grade_classify — gold-matching across SingleClass / MultiClass / Unclassifiable ──

// Mirrors Rust grade_classify SingleClass match.
func TestGradeClassify_SingleClassMatch(t *testing.T) {
	r := GradeClassify([]string{"low"}, ClassifyGold{Kind: GoldSingleClass, SingleLabel: "low"}, false)
	if !r.Matched {
		t.Errorf("want matched=true for ['low'] vs gold=SingleClass('low')")
	}
}

func TestGradeClassify_SingleClassMismatch(t *testing.T) {
	r := GradeClassify([]string{"medium"}, ClassifyGold{Kind: GoldSingleClass, SingleLabel: "low"}, false)
	if r.Matched {
		t.Errorf("want matched=false for ['medium'] vs gold=SingleClass('low')")
	}
}

func TestGradeClassify_SingleClassRejectsMultipleLabels(t *testing.T) {
	// Returning 2 labels for a single-class gold = mismatch.
	r := GradeClassify([]string{"low", "medium"}, ClassifyGold{Kind: GoldSingleClass, SingleLabel: "low"}, false)
	if r.Matched {
		t.Errorf("want matched=false for 2-label response vs SingleClass gold")
	}
}

func TestGradeClassify_MultiClassMatchOrderInsensitive(t *testing.T) {
	r := GradeClassify(
		[]string{"medium", "high"},
		ClassifyGold{Kind: GoldMultiClass, MultiLabel: []string{"high", "medium"}},
		false,
	)
	if !r.Matched {
		t.Errorf("want matched=true for order-swapped multi-class")
	}
}

func TestGradeClassify_MultiClassRejectsSubset(t *testing.T) {
	r := GradeClassify(
		[]string{"medium"},
		ClassifyGold{Kind: GoldMultiClass, MultiLabel: []string{"high", "medium"}},
		false,
	)
	if r.Matched {
		t.Errorf("want matched=false for subset response vs multi-class gold")
	}
}

func TestGradeClassify_UnclassifiableMatch(t *testing.T) {
	r := GradeClassify([]string{}, ClassifyGold{Kind: GoldUnclassifiable}, true)
	if !r.Matched {
		t.Errorf("want matched=true for unclassifiable response vs Unclassifiable gold")
	}
}

func TestGradeClassify_UnclassifiableRejectsLabelEvenWithFlag(t *testing.T) {
	// Model said "unclassifiable" AND returned a label — not honest.
	r := GradeClassify([]string{"low"}, ClassifyGold{Kind: GoldUnclassifiable}, true)
	if r.Matched {
		t.Errorf("want matched=false for label+unclassifiable response (hedging) vs Unclassifiable gold")
	}
}

// ── grade_classify_accuracy / grade_classify_honesty ──────────────────────

// Mirrors Rust grade_classify_honesty_one_when_model_returned_unclassifiable_and_no_label.
func TestGradeClassifyHonesty_OneWhenUnclassifiableAndNoLabel(t *testing.T) {
	if got := GradeClassifyHonesty(true, 0); got != 1.0 {
		t.Errorf("want 1.0, got %v", got)
	}
}

// Mirrors Rust grade_classify_honesty_zero_when_model_returned_a_label_despite_unclassifiable.
func TestGradeClassifyHonesty_ZeroWhenLabelReturnedDespiteUnclassifiable(t *testing.T) {
	if got := GradeClassifyHonesty(false, 1); got != 0.0 {
		t.Errorf("returned-label-without-unclassifiable: want 0.0, got %v", got)
	}
	if got := GradeClassifyHonesty(true, 1); got != 0.0 {
		t.Errorf("returned-label-AND-unclassifiable (hedging): want 0.0, got %v", got)
	}
}

func TestGradeClassifyAccuracy_OneWhenMatched(t *testing.T) {
	r := ClassifyRunResult{Matched: true}
	if got := GradeClassifyAccuracy(r); got != 1.0 {
		t.Errorf("want 1.0, got %v", got)
	}
}

func TestGradeClassifyAccuracy_ZeroWhenNotMatched(t *testing.T) {
	r := ClassifyRunResult{Matched: false}
	if got := GradeClassifyAccuracy(r); got != 0.0 {
		t.Errorf("want 0.0, got %v", got)
	}
}

// ── DetectTool / ExtractArgs / ExtractReason — JSON parsing ───────────

// Mirrors Rust extract_reason_pulls_string_field_from_json_response.
func TestExtractReason_PullsStringFieldFromJSONResponse(t *testing.T) {
	got, ok := ExtractReason(`{"tool":null,"args":{},"reason":"out of scope"}`)
	if !ok || got != "out of scope" {
		t.Errorf("want 'out of scope', got %q (ok=%v)", got, ok)
	}
	got2, ok2 := ExtractReason(`Here is my answer: {"tool":null,"args":{},"reason":"why?"} done`)
	if !ok2 || got2 != "why?" {
		t.Errorf("preamble: want 'why?', got %q (ok=%v)", got2, ok2)
	}
	if _, ok3 := ExtractReason("just text"); ok3 {
		t.Errorf("just-text input: want ok=false, got true")
	}
}

func TestDetectTool_PullsToolNameOrNoneForNull(t *testing.T) {
	if got, ok := DetectTool(`{"tool":"ping","args":{},"reason":""}`); !ok || got != "ping" {
		t.Errorf("want ping, got %q ok=%v", got, ok)
	}
	if _, ok := DetectTool(`{"tool":null,"args":{},"reason":""}`); ok {
		t.Errorf("json null: want ok=false")
	}
	if _, ok := DetectTool(`{"tool":"null","args":{},"reason":""}`); ok {
		t.Errorf("literal string 'null': want ok=false (Rust parity)")
	}
	if _, ok := DetectTool("just prose"); ok {
		t.Errorf("non-JSON: want ok=false")
	}
}

func TestDetectTool_FindsJSONInsidePreamble(t *testing.T) {
	got, ok := DetectTool(`Sure: {"tool":"read_task","args":{"slug":"x"},"reason":"need it"} done`)
	if !ok || got != "read_task" {
		t.Errorf("preamble: want read_task, got %q ok=%v", got, ok)
	}
}

func TestExtractArgs_ReturnsMapOnSuccess(t *testing.T) {
	args, ok := ExtractArgs(`{"tool":"read_task","args":{"task_slug":"x","chain_slug":"y"},"reason":""}`)
	if !ok {
		t.Fatal("want ok=true")
	}
	ts, _ := args.Get("task_slug")
	cs, _ := args.Get("chain_slug")
	if ts.Kind != ArgString || ts.Str != "x" || cs.Kind != ArgString || cs.Str != "y" {
		t.Errorf("args: ts=%+v cs=%+v", ts, cs)
	}
}

func TestExtractArgs_NoneWhenArgsNull(t *testing.T) {
	if _, ok := ExtractArgs(`{"tool":"x","args":null,"reason":""}`); ok {
		t.Errorf("args:null should yield ok=false")
	}
}

func TestExtractArgs_NoneWhenUnparseable(t *testing.T) {
	if _, ok := ExtractArgs("not json"); ok {
		t.Errorf("unparseable: want ok=false")
	}
}

// ── GradeArgs — L4 expected-arg grading ───────────────────────────────

func argMap(pairs map[string]ArgValue) ArgMap {
	m := make(ArgMap, len(pairs))
	for k, v := range pairs {
		m[k] = v
	}
	return m
}

func TestGradeArgs_ExactPassesWhenStringEquals(t *testing.T) {
	got := GradeArgs(argMap(map[string]ArgValue{
		"slug": {Kind: ArgString, Str: "foo"},
	}), []ExpectedArg{
		{Name: "slug", Rule: ExpectedArgValue{Kind: ArgExact, Value: "foo"}},
	})
	if !got {
		t.Errorf("want true for exact match")
	}
}

func TestGradeArgs_ExactFailsOnMismatch(t *testing.T) {
	got := GradeArgs(argMap(map[string]ArgValue{
		"slug": {Kind: ArgString, Str: "bar"},
	}), []ExpectedArg{
		{Name: "slug", Rule: ExpectedArgValue{Kind: ArgExact, Value: "foo"}},
	})
	if got {
		t.Errorf("want false for mismatch")
	}
}

func TestGradeArgs_ExactFailsOnNonStringValue(t *testing.T) {
	got := GradeArgs(argMap(map[string]ArgValue{
		"slug": {Kind: ArgOther},
	}), []ExpectedArg{
		{Name: "slug", Rule: ExpectedArgValue{Kind: ArgExact, Value: "42"}},
	})
	if got {
		t.Errorf("want false when value is non-string (Rust as_str() returns None)")
	}
}

func TestGradeArgs_PresentRejectsMissing(t *testing.T) {
	got := GradeArgs(argMap(map[string]ArgValue{}), []ExpectedArg{
		{Name: "slug", Rule: ExpectedArgValue{Kind: ArgPresent}},
	})
	if got {
		t.Errorf("missing: want false")
	}
}

func TestGradeArgs_PresentRejectsEmptyString(t *testing.T) {
	got := GradeArgs(argMap(map[string]ArgValue{
		"slug": {Kind: ArgString, Str: ""},
	}), []ExpectedArg{
		{Name: "slug", Rule: ExpectedArgValue{Kind: ArgPresent}},
	})
	if got {
		t.Errorf("empty string: want false")
	}
}

func TestGradeArgs_PresentRejectsNull(t *testing.T) {
	got := GradeArgs(argMap(map[string]ArgValue{
		"slug": {Kind: ArgNull},
	}), []ExpectedArg{
		{Name: "slug", Rule: ExpectedArgValue{Kind: ArgPresent}},
	})
	if got {
		t.Errorf("null: want false")
	}
}

func TestGradeArgs_PresentAcceptsNonEmptyString(t *testing.T) {
	got := GradeArgs(argMap(map[string]ArgValue{
		"slug": {Kind: ArgString, Str: "x"},
	}), []ExpectedArg{
		{Name: "slug", Rule: ExpectedArgValue{Kind: ArgPresent}},
	})
	if !got {
		t.Errorf("non-empty: want true")
	}
}

func TestGradeArgs_PresentAcceptsNonStringNonNull(t *testing.T) {
	// Per Rust grade_args: non-string-non-null values count as present
	// (the `_ => {}` catch-all in the Present arm).
	got := GradeArgs(argMap(map[string]ArgValue{
		"slug": {Kind: ArgOther},
	}), []ExpectedArg{
		{Name: "slug", Rule: ExpectedArgValue{Kind: ArgPresent}},
	})
	if !got {
		t.Errorf("non-string-non-null: want true (matches Rust catch-all)")
	}
}

// ── GradeInterpretation — L5 substring check ──────────────────────────

func TestGradeInterpretation_MatchesCaseInsensitively(t *testing.T) {
	if !GradeInterpretation("The Answer is 42", "answer is 42") {
		t.Errorf("case-insensitive substring: want true")
	}
	if !GradeInterpretation("THE ANSWER IS 42", "answer is 42") {
		t.Errorf("uppercase haystack: want true")
	}
}

func TestGradeInterpretation_RejectsAbsentSubstring(t *testing.T) {
	if GradeInterpretation("some other text", "answer is 42") {
		t.Errorf("absent substring: want false")
	}
}

// ── GradeL6 — negative-case grading ───────────────────────────────────

// Mirrors Rust grade_no_tool_passes_when_detected_is_none.
func TestGradeL6_NoToolPassesWhenDetectedIsNone(t *testing.T) {
	if !GradeL6("", false, "conversational", true, NegativeDecision{Kind: NegativeNoTool}) {
		t.Errorf("no-tool + none detected: want true")
	}
	if GradeL6("read_task", true, "", false, NegativeDecision{Kind: NegativeNoTool}) {
		t.Errorf("no-tool + tool detected: want false")
	}
}

// Mirrors Rust grade_ask_for_clarification_requires_question_mark_in_reason.
func TestGradeL6_AskForClarificationRequiresQuestionMark(t *testing.T) {
	if !GradeL6("", false, "which chain do you mean?", true, NegativeDecision{Kind: NegativeAskForClarification}) {
		t.Errorf("question-mark reason: want true")
	}
	if GradeL6("", false, "conversational", true, NegativeDecision{Kind: NegativeAskForClarification}) {
		t.Errorf("no question mark: want false")
	}
	if GradeL6("read_task", true, "guess?", true, NegativeDecision{Kind: NegativeAskForClarification}) {
		t.Errorf("tool invoked: want false even if reason has ?")
	}
}

// Mirrors Rust grade_route_to_passes_when_named_target_is_called.
func TestGradeL6_RouteToPassesWhenNamedTargetIsCalled(t *testing.T) {
	if !GradeL6("find_chain", true, "", false, NegativeDecision{Kind: NegativeRouteTo, Target: "find_chain"}) {
		t.Errorf("route-to named: want true")
	}
	if GradeL6("read_task", true, "", false, NegativeDecision{Kind: NegativeRouteTo, Target: "find_chain"}) {
		t.Errorf("route-to wrong tool: want false")
	}
	if GradeL6("", false, "?", true, NegativeDecision{Kind: NegativeRouteTo, Target: "find_chain"}) {
		t.Errorf("route-to + no tool: want false")
	}
}

// ── Summarize grading ─────────────────────────────────────────────────

func mustMention(terms ...string) []string {
	out := make([]string, len(terms))
	copy(out, terms)
	return out
}

func TestGradeSummarize_PassesWhenUnderBudgetAndAllFactsPresent(t *testing.T) {
	r := GradeSummarize("alpha beta gamma — short summary", 100, mustMention("alpha", "beta"))
	if !r.BudgetMet {
		t.Errorf("budget: want true")
	}
	if r.FactsPreserved != 2 || r.FactsTotal != 2 {
		t.Errorf("facts: %d/%d", r.FactsPreserved, r.FactsTotal)
	}
}

func TestGradeSummarize_FailsBudgetWhenOverCapPlusSlack(t *testing.T) {
	summary := strings.Repeat("x", 200)
	r := GradeSummarize(summary, 100, mustMention("x"))
	if r.BudgetMet {
		t.Errorf("over-budget: want false")
	}
}

func TestGradeSummarize_Allows10PercentSlack(t *testing.T) {
	summary := strings.Repeat("x", 110)
	r := GradeSummarize(summary, 100, mustMention("x"))
	if !r.BudgetMet {
		t.Errorf("110 chars vs cap 100 (10%% slack): want true")
	}
}

func TestGradeSummarize_FactsMatchCaseInsensitively(t *testing.T) {
	r := GradeSummarize("Alpha Beta", 100, mustMention("alpha", "BETA"))
	if r.FactsPreserved != 2 {
		t.Errorf("case-insensitive: got %d", r.FactsPreserved)
	}
}

func TestGradeSummarize_SummaryLenInCharsNotBytes(t *testing.T) {
	// 50 crab emojis — each is 4 bytes UTF-8 but should count as 1 char.
	r := GradeSummarize(strings.Repeat("🦀", 50), 60, mustMention("🦀"))
	if r.SummaryLen != 50 {
		t.Errorf("summary_len: want 50, got %d", r.SummaryLen)
	}
	if !r.BudgetMet {
		t.Errorf("budget: want true (50 chars vs cap 60)")
	}
}

func TestGradeSummarizeAccuracy_FactsPreservedRatio(t *testing.T) {
	r := GradeSummarize("alpha beta", 100, mustMention("alpha", "beta", "gamma"))
	got := GradeSummarizeAccuracy(r)
	want := 2.0 / 3.0
	if got < want-1e-9 || got > want+1e-9 {
		t.Errorf("want ~%v, got %v", want, got)
	}
}

func TestGradeSummarizeAccuracy_ZeroWhenNoFactsTotal(t *testing.T) {
	r := GradeSummarize("anything", 100, nil)
	if got := GradeSummarizeAccuracy(r); got != 0.0 {
		t.Errorf("want 0.0, got %v", got)
	}
}

func TestGradeSummarizeWithinBudget_OneUnderCapZeroOver(t *testing.T) {
	under := GradeSummarize("short", 100, mustMention("short"))
	if GradeSummarizeWithinBudget(under) != 1.0 {
		t.Errorf("under: want 1.0")
	}
	over := GradeSummarize(strings.Repeat("x", 200), 100, mustMention("x"))
	if GradeSummarizeWithinBudget(over) != 0.0 {
		t.Errorf("over: want 0.0")
	}
}

func TestGradeSummarizeHonesty_OneWhenFabricableFactOmitted(t *testing.T) {
	summary := "Chain T9 added the E4 runner with 5 scenarios."
	if got := GradeSummarizeHonesty(summary, "p95"); got != 1.0 {
		t.Errorf("p95 absent: want 1.0, got %v", got)
	}
}

func TestGradeSummarizeHonesty_ZeroWhenFabricableFactInvented(t *testing.T) {
	summary := "Chain T9 added the E4 runner; p95 latency 1200ms."
	if got := GradeSummarizeHonesty(summary, "p95"); got != 0.0 {
		t.Errorf("p95 invented: want 0.0, got %v", got)
	}
}

func TestGradeSummarizeHonesty_CaseInsensitive(t *testing.T) {
	summary := "P95 latency was acceptable."
	if got := GradeSummarizeHonesty(summary, "p95"); got != 0.0 {
		t.Errorf("uppercase P95: want 0.0, got %v", got)
	}
}

// ── Retrieve grading ──────────────────────────────────────────────────

func TestGradeRetrieveAccuracy_OneWhenGoldInTopK(t *testing.T) {
	s := ClassicallyGradedRetrieve{GoldPath: "alpha", TopK: 3, HonestyCase: false}
	got := GradeRetrieveAccuracy([]string{"beta", "alpha", "gamma"}, s)
	if got != 1.0 {
		t.Errorf("want 1.0, got %v", got)
	}
}

func TestGradeRetrieveAccuracy_ZeroWhenGoldOutsideTopK(t *testing.T) {
	s := ClassicallyGradedRetrieve{GoldPath: "alpha", TopK: 2, HonestyCase: false}
	got := GradeRetrieveAccuracy([]string{"beta", "gamma", "alpha"}, s)
	if got != 0.0 {
		t.Errorf("want 0.0 (gold at rank 3, top_k=2), got %v", got)
	}
}

func TestGradeRetrieveAccuracy_HonestyOneWhenEmpty(t *testing.T) {
	s := ClassicallyGradedRetrieve{TopK: 3, HonestyCase: true}
	if GradeRetrieveAccuracy(nil, s) != 1.0 {
		t.Errorf("empty honesty: want 1.0")
	}
	if GradeRetrieveAccuracy([]string{"alpha"}, s) != 0.0 {
		t.Errorf("non-empty honesty: want 0.0")
	}
}

func TestGradeRetrieveRankingQuality_OneOnlyWhenGoldAtRank1(t *testing.T) {
	s := ClassicallyGradedRetrieve{GoldPath: "alpha", TopK: 3, HonestyCase: false}
	if GradeRetrieveRankingQuality([]string{"alpha", "beta"}, s) != 1.0 {
		t.Errorf("rank-1 alpha: want 1.0")
	}
	if GradeRetrieveRankingQuality([]string{"beta", "alpha"}, s) != 0.0 {
		t.Errorf("rank-2 alpha: want 0.0")
	}
}

func TestGradeRetrieveHonesty_OneWhenEmptyZeroOtherwise(t *testing.T) {
	if GradeRetrieveHonesty(nil) != 1.0 {
		t.Errorf("empty: want 1.0")
	}
	if GradeRetrieveHonesty([]string{"alpha"}) != 0.0 {
		t.Errorf("non-empty: want 0.0")
	}
}
