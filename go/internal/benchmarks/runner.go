package benchmarks

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
)

// ClassifyRunResult is the per-scenario grading output produced by
// GradeClassify. Mirrors Rust runner::ClassifyResult (renamed to avoid
// collision with measure.ClassifyResult which is a DB-side record type).
type ClassifyRunResult struct {
	// Matched is true iff the returned label set equals the gold set
	// (case-insensitive; order-insensitive for multi-class) — or, for
	// Unclassifiable, the model returned "unclassifiable" with no
	// label.
	Matched bool
	// ReturnedLabels is the canonicalized labels parsed from the
	// response, in order.
	ReturnedLabels []string
	// ReturnedUnclassifiable is true iff the response was "unclassifiable".
	ReturnedUnclassifiable bool
}

// ParseClassifyResponse parses the model's response into a list of
// labels chosen from the allowed set + a boolean indicating whether the
// model returned "unclassifiable".
//
// Tolerant of leading bullets / numbering and comma-separated single-line
// responses. Mirrors Rust runner::parse_classify_response semantics
// byte-for-byte.
//
// Strips leading bullet/enumerator markers (digits allowed here so "1. " /
// "2)" numbered lists collapse), then strips trailing punctuation but
// NOT trailing digits — labels with digit suffixes (tier-0, P3, L4) must
// keep them, otherwise "tier-0" would reduce to "tier-" then "tier" and
// silently fail the allowlist match.
func ParseClassifyResponse(response string, allowed []string) ([]string, bool) {
	allowedLower := make([]string, len(allowed))
	for i, a := range allowed {
		allowedLower[i] = strings.ToLower(a)
	}
	var labels []string
	var unclassifiable bool

	// Split on newline AND comma so "low, medium" or "low\nmedium" both work.
	var pieces []string
	for _, line := range strings.Split(response, "\n") {
		for _, p := range strings.Split(line, ",") {
			pieces = append(pieces, p)
		}
	}

	isLeadStrip := func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '*' || r == '`' ||
			r == '.' || r == ')' || r == '(' || r == ':' ||
			(r >= '0' && r <= '9')
	}
	isTailStrip := func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '*' || r == '`' ||
			r == '.' || r == ')' || r == '(' || r == ':'
	}

	for _, raw := range pieces {
		cleaned := strings.TrimLeftFunc(raw, isLeadStrip)
		cleaned = strings.TrimRightFunc(cleaned, isTailStrip)
		cleaned = strings.ToLower(strings.TrimSpace(cleaned))
		if cleaned == "" {
			continue
		}
		if cleaned == "unclassifiable" {
			unclassifiable = true
			continue
		}
		for i, a := range allowedLower {
			if a == cleaned {
				canonical := allowed[i]
				// Dedupe — keep first occurrence's order.
				dup := false
				for _, l := range labels {
					if l == canonical {
						dup = true
						break
					}
				}
				if !dup {
					labels = append(labels, canonical)
				}
				break
			}
		}
	}
	return labels, unclassifiable
}

// GradeClassify grades a parsed response against the gold answer.
// Mirrors Rust runner::grade_classify byte-for-byte.
func GradeClassify(returnedLabels []string, gold ClassifyGold, returnedUnclassifiable bool) ClassifyRunResult {
	matched := false
	switch gold.Kind {
	case GoldSingleClass:
		matched = len(returnedLabels) == 1 &&
			strings.EqualFold(returnedLabels[0], gold.SingleLabel)
	case GoldMultiClass:
		if len(returnedLabels) == len(gold.MultiLabel) {
			returnedSet := make(map[string]struct{}, len(returnedLabels))
			for _, r := range returnedLabels {
				returnedSet[strings.ToLower(r)] = struct{}{}
			}
			matched = true
			for _, g := range gold.MultiLabel {
				if _, ok := returnedSet[strings.ToLower(g)]; !ok {
					matched = false
					break
				}
			}
		}
	case GoldUnclassifiable:
		matched = returnedUnclassifiable && len(returnedLabels) == 0
	}
	return ClassifyRunResult{
		Matched:                matched,
		ReturnedLabels:         returnedLabels,
		ReturnedUnclassifiable: returnedUnclassifiable,
	}
}

// GradeClassifyAccuracy returns the per-shape accuracy subscore for
// Classify: 1.0 if matched, 0.0 otherwise.
func GradeClassifyAccuracy(result ClassifyRunResult) float64 {
	if result.Matched {
		return 1.0
	}
	return 0.0
}

// GradeClassifyHonesty returns the per-shape honesty subscore for
// Classify scenarios with gold = Unclassifiable. 1.0 if the model
// honestly said "unclassifiable" with no label, 0.0 otherwise.
//
// **Only call when scenario.Gold.Kind == GoldUnclassifiable.** Scenarios
// that use a named ambiguous label (e.g. SingleClass("unclear")) do NOT
// receive a honesty subscore — accuracy covers them. The smoke_rubric
// binary warns when such scenarios are present but no Unclassifiable
// gold exists (bug 1190). To get a honesty subscore for ambiguous-
// evidence scenarios, use GoldUnclassifiable as gold.
func GradeClassifyHonesty(returnedUnclassifiable bool, returnedLabelCount int) float64 {
	if returnedUnclassifiable && returnedLabelCount == 0 {
		return 1.0
	}
	return 0.0
}

// ── L3 / L4 — tool-call JSON parsing ────────────────────────────────────

// toolResponse is the typed envelope the model emits for L3/L4/L6 tool-
// invocation scenarios. tool is nullable JSON in the wire format — the
// Pointer wrapper distinguishes "tool: null" (no-tool decision) from
// "no tool field at all" (malformed response).
type toolResponse struct {
	Tool   *string         `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Reason *string         `json:"reason"`
}

// parseToolResponse extracts a toolResponse from the model's text. Tries
// the whole response as JSON first, then falls back to the first {…} block.
// Mirrors Rust runner::detect_tool / extract_args / extract_reason's
// shared parsing strategy.
func parseToolResponse(response string) (toolResponse, bool) {
	var tr toolResponse
	trimmed := strings.TrimSpace(response)
	if err := json.Unmarshal([]byte(trimmed), &tr); err == nil {
		return tr, true
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return toolResponse{}, false
	}
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &tr); err != nil {
		return toolResponse{}, false
	}
	return tr, true
}

// DetectTool extracts the invoked tool name from a model response.
// Mirrors Rust runner::detect_tool: nil for "tool: null" / "tool: \"null\""
// / unparseable; otherwise the tool string.
func DetectTool(response string) (string, bool) {
	tr, ok := parseToolResponse(response)
	if !ok {
		return "", false
	}
	if tr.Tool == nil {
		return "", false
	}
	// Rust treats the literal string "null" the same as JSON null.
	if *tr.Tool == "null" {
		return "", false
	}
	return *tr.Tool, true
}

// ArgKind discriminates the JSON-arg-value shapes the L4 grader cares
// about. The Rust source uses serde_json::Value pattern-matching;
// the Go side projects each value into one of these kinds at parse time.
type ArgKind int

const (
	// ArgString: a JSON string value.
	ArgString ArgKind = iota
	// ArgNull: a JSON null value.
	ArgNull
	// ArgOther: any JSON value that's not a string or null
	// (number, bool, array, object). Counts as "present" for
	// ArgPresent grading but never matches an ArgExact rule.
	ArgOther
)

// ArgValue is a typed projection of one L4 tool-call argument's value.
// Keeps the package free of map[string]any in grader signatures.
type ArgValue struct {
	Kind ArgKind
	Str  string // populated iff Kind == ArgString
}

// ArgMap is a typed view onto the args object extracted from a model
// JSON response. Use Get to look up by key; the bool return reports
// presence.
type ArgMap map[string]ArgValue

// Get returns the value bound to key (or zero-value + false when absent).
func (m ArgMap) Get(key string) (ArgValue, bool) {
	v, ok := m[key]
	return v, ok
}

// ExtractArgs returns the args map from a model JSON response. Mirrors
// Rust runner::extract_args. Returns empty + ok=false when parsing
// fails, args is null, or args is not a JSON object.
func ExtractArgs(response string) (ArgMap, bool) {
	tr, ok := parseToolResponse(response)
	if !ok {
		return nil, false
	}
	if len(tr.Args) == 0 || bytes.Equal(bytes.TrimSpace(tr.Args), []byte("null")) {
		return nil, false
	}
	// Decode args into per-key json.RawMessage so each value's shape
	// can be inspected without ever declaring map[string]any.
	var rawArgs map[string]json.RawMessage
	if err := json.Unmarshal(tr.Args, &rawArgs); err != nil {
		return nil, false
	}
	if rawArgs == nil {
		return nil, false
	}
	out := make(ArgMap, len(rawArgs))
	for k, raw := range rawArgs {
		out[k] = classifyArg(raw)
	}
	return out, true
}

// classifyArg inspects one json.RawMessage and projects it onto an
// ArgValue. Strings come through unquoted; null becomes ArgNull;
// every other shape becomes ArgOther.
func classifyArg(raw json.RawMessage) ArgValue {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) || len(trimmed) == 0 {
		return ArgValue{Kind: ArgNull}
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return ArgValue{Kind: ArgString, Str: s}
		}
	}
	return ArgValue{Kind: ArgOther}
}

// ExtractReason returns the reason string from a model JSON response.
// Mirrors Rust runner::extract_reason. Returns "" + ok=false when
// reason is absent or unparseable.
func ExtractReason(response string) (string, bool) {
	tr, ok := parseToolResponse(response)
	if !ok {
		return "", false
	}
	if tr.Reason == nil {
		return "", false
	}
	return *tr.Reason, true
}

// GradeArgs grades extracted args against expected arg declarations.
// Returns true when every entry in expected is satisfied per its rule.
// Mirrors Rust runner::grade_args.
func GradeArgs(extracted ArgMap, expected []ExpectedArg) bool {
	for _, e := range expected {
		v, present := extracted.Get(e.Name)
		switch e.Rule.Kind {
		case ArgExact:
			if v.Kind != ArgString || v.Str != e.Rule.Value {
				return false
			}
		case ArgPresent:
			if !present || v.Kind == ArgNull {
				return false
			}
			if v.Kind == ArgString && v.Str == "" {
				return false
			}
		}
	}
	return true
}

// ── L5 — interpretation grading ─────────────────────────────────────────

// GradeInterpretation grades an L5 response: true when expectedAnswer
// appears in response as a case-insensitive substring. Mirrors Rust
// runner::grade_interpretation.
func GradeInterpretation(response, expectedAnswer string) bool {
	return strings.Contains(strings.ToLower(response), strings.ToLower(expectedAnswer))
}

// ── L6 — negative-case grading ──────────────────────────────────────────

// GradeL6 grades an L6 response against the expected negative-decision.
// detected is the tool the model invoked (empty + ok=false for no-tool).
// reason is the reason field from the model's JSON response (proxy for
// "asked a clarifying question"). Mirrors Rust runner::grade_l6.
func GradeL6(detected string, detectedOK bool, reason string, reasonOK bool, expected NegativeDecision) bool {
	switch expected.Kind {
	case NegativeNoTool:
		return !detectedOK
	case NegativeAskForClarification:
		return !detectedOK && reasonOK && strings.Contains(reason, "?")
	case NegativeRouteTo:
		return detectedOK && detected == expected.Target
	}
	return false
}

// ── Summarize — budget + must-mention grading ──────────────────────────

// SummarizeResult is the per-scenario Summarize grading output.
// Mirrors Rust runner::SummarizeResult.
type SummarizeResult struct {
	BudgetMet      bool `json:"budget_met"`
	FactsPreserved int  `json:"facts_preserved"`
	FactsTotal     int  `json:"facts_total"`
	SummaryLen     int  `json:"summary_len"`
}

// summarizeBudgetSlack matches Rust SUMMARIZE_BUDGET_SLACK — 10% slack
// keeps near-budget outputs from failing on punctuation differences.
const summarizeBudgetSlack = 1.10

// GradeSummarize grades a Summarize response against the real must-
// mention list. Mirrors Rust runner::grade_summarize. summary's length
// is counted in characters (runes), not bytes — matches Rust .chars().count().
func GradeSummarize(summary string, maxChars int, mustMention []string) SummarizeResult {
	summaryLen := 0
	for range summary {
		summaryLen++
	}
	budgetCap := int(float64(maxChars)*summarizeBudgetSlack + 0.5)
	lower := strings.ToLower(summary)
	preserved := 0
	for _, t := range mustMention {
		if strings.Contains(lower, strings.ToLower(t)) {
			preserved++
		}
	}
	return SummarizeResult{
		BudgetMet:      summaryLen <= budgetCap,
		FactsPreserved: preserved,
		FactsTotal:     len(mustMention),
		SummaryLen:     summaryLen,
	}
}

// GradeSummarizeAccuracy returns the facts-preserved ratio. Returns
// 0.0 for the degenerate zero-fact case. Mirrors Rust grade_summarize_accuracy.
func GradeSummarizeAccuracy(result SummarizeResult) float64 {
	if result.FactsTotal == 0 {
		return 0.0
	}
	return float64(result.FactsPreserved) / float64(result.FactsTotal)
}

// GradeSummarizeWithinBudget returns 1.0 if the budget was respected.
// Mirrors Rust grade_summarize_within_budget.
func GradeSummarizeWithinBudget(result SummarizeResult) float64 {
	if result.BudgetMet {
		return 1.0
	}
	return 0.0
}

// GradeSummarizeHonesty returns 1.0 if the fabricable fact is absent
// from the summary, 0.0 if the model invented it. Mirrors Rust
// grade_summarize_honesty.
func GradeSummarizeHonesty(summary, factNotInSource string) float64 {
	if strings.Contains(strings.ToLower(summary), strings.ToLower(factNotInSource)) {
		return 0.0
	}
	return 1.0
}

// ── Retrieve — accuracy / ranking_quality / honesty ─────────────────────

// RetrieveResult is the per-scenario Retrieve grading output.
// Mirrors Rust runner::RetrieveResult.
type RetrieveResult struct {
	PassAt1       bool     `json:"pass_at_1"`
	PassAtK       bool     `json:"pass_at_k"`
	ReturnedPaths []string `json:"returned_paths"`
}

// ClassicallyGradedRetrieve is the borrowed view onto a Retrieve
// scenario's gold spec — keeps the graders pure. Mirrors Rust struct
// of the same name.
type ClassicallyGradedRetrieve struct {
	GoldPath    string
	TopK        int
	HonestyCase bool
}

// GradeRetrieveAccuracy returns the per-shape accuracy subscore for
// Retrieve. 1.0 if gold_path is in top-K (or returned is empty for an
// honesty case). Mirrors Rust grade_retrieve_accuracy.
func GradeRetrieveAccuracy(returnedPaths []string, scenario ClassicallyGradedRetrieve) float64 {
	if scenario.HonestyCase {
		if len(returnedPaths) == 0 {
			return 1.0
		}
		return 0.0
	}
	limit := scenario.TopK
	if limit > len(returnedPaths) {
		limit = len(returnedPaths)
	}
	for i := 0; i < limit; i++ {
		if returnedPaths[i] == scenario.GoldPath {
			return 1.0
		}
	}
	return 0.0
}

// GradeRetrieveRankingQuality returns the per-shape ranking-quality
// subscore for Retrieve. 1.0 only when gold_path is at rank-1 (or, for
// honesty cases, returned is empty). Mirrors Rust grade_retrieve_ranking_quality.
func GradeRetrieveRankingQuality(returnedPaths []string, scenario ClassicallyGradedRetrieve) float64 {
	if scenario.HonestyCase {
		if len(returnedPaths) == 0 {
			return 1.0
		}
		return 0.0
	}
	if len(returnedPaths) > 0 && returnedPaths[0] == scenario.GoldPath {
		return 1.0
	}
	return 0.0
}

// GradeRetrieveHonesty returns 1.0 when the model returned empty (no
// match), 0.0 when it picked a path. Mirrors Rust grade_retrieve_honesty.
func GradeRetrieveHonesty(returnedPaths []string) float64 {
	if len(returnedPaths) == 0 {
		return 1.0
	}
	return 0.0
}
