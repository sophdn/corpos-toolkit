package refresolve

import (
	"context"
	"regexp"
	"strings"
)

// IntentShape enumerates the closed directive-intent vocabulary T4
// pinned in docs/PARSE_CONTEXT.md §13.2. The values mirror the JSON
// wire shape exactly so envelope serialization is the typed
// identity. Speculative shapes are explicitly disallowed (T4 §13.8
// cold-pickup guard); extending the vocabulary requires a new chain
// task that revisits the design.
type IntentShape string

const (
	IntentVerify    IntentShape = "verify"
	IntentImplement IntentShape = "implement"
	IntentFix       IntentShape = "fix"
	IntentAudit     IntentShape = "audit"
	// IntentExecute — drive named existing work (a chain/task, or the
	// open-bug backlog) to completion. Added by chain parse-context-
	// directive-intent-extension (docs/PARSE_CONTEXT.md §14): a
	// re-grounding against grounding_events.query_text found this the
	// single dominant directive shape in real usage (~36% of distinct
	// prompts — "work through X", "complete X on a new worktree",
	// "pick up X", "finish up X", "X please resume") yet entirely
	// absent from the original §13.2 vocabulary. THIS chain is the
	// §13.2-mandated authorized vocabulary revisit.
	IntentExecute   IntentShape = "execute"
	IntentExplain   IntentShape = "explain"
	IntentSummarize IntentShape = "summarize"
	IntentStatus    IntentShape = "status"
	IntentList      IntentShape = "list"
	IntentNone      IntentShape = "none"
)

// IntentResult is the typed output the detector returns. Surfaces in
// the parse_context envelope's top-level `intent` field after JSON
// marshalling. Pattern matches always emit Confidence=1.0; a future
// Qwen rubric path (T4 §13.5, opt-in only) would emit the rubric
// score and DetectedVia="rubric".
type IntentResult struct {
	Shape       IntentShape `json:"shape"`
	Confidence  float64     `json:"confidence"`
	DetectedVia string      `json:"detected_via"` // "pattern" | "rubric" | "default"
}

// politePrefix strips a leading polite preamble ("please ", "could
// you ", "can you ") so the verb-anchor patterns match the actual
// directive. Operates on a single-pass normalized string; idempotent.
var politePrefix = regexp.MustCompile(`(?i)^\s*(please\s+|could you\s+|can you\s+|i'?d like (you to\s+)?|would you\s+)`)

// intentPatterns maps each shape to the regex patterns that match
// its representative phrasings. All patterns are case-insensitive
// and anchored at message start (after politePrefix strip) where
// the design's "leading verb" semantics matter; a few targeted
// inline patterns catch idioms that aren't sentence-initial
// ("any cleanup to do?", "are there any …?").
//
// Pattern set is empirical — calibrated against the §13.7 fixture
// (go/internal/refresolve/testdata/directive_intent_fixtures.json).
// Recall target: ≥80% per T4 §13.5; coverage is verified by
// TestDetectIntent_FixtureRecall.
var intentPatterns = map[IntentShape][]*regexp.Regexp{
	IntentImplement: {
		regexp.MustCompile(`(?i)^(implement|wire(\s|$)|build\s|ship\s|create\s)`),
		regexp.MustCompile(`(?i)^add\s+(a\s+)?(new\s+)?\w+`),
	},
	IntentFix: {
		regexp.MustCompile(`(?i)^(fix\s|repair\s|debug\s)`),
		regexp.MustCompile(`(?i)\b(track\s+(it|them|that)\s+down|debug\s+it)\b`),
		regexp.MustCompile(`(?i)\bplease\s+(repair|fix|resolve)\b`),
		regexp.MustCompile(`(?i)isn'?t\s+\w+(\s+\w+){0,3}\s+—\s*please\s+(repair|fix)`),
		regexp.MustCompile(`(?i)\bcorrupts?\s+\w+(\s+\w+){0,3};\s*fix`),
		// "I'd like the banner to work properly" → fix (T4-named prompt).
		// The framing "I want X to work [properly]" without "please
		// implement" / "please add" is a defect-repair directive; the
		// agent is naming a thing that isn't working today.
		regexp.MustCompile(`(?i)\bto work\s+(properly|correctly|right)\b`),
		// solve fold (§14.3): "solve our open bugs" / "clear the bug
		// backlog" is fix-intent applied to the whole open-bug set —
		// folded into fix so it inherits fix's work-state (open bugs)
		// + bug-fixing-discipline rather than minting a near-duplicate
		// shape. Scoped to backlog/solve-our phrasings so a bare "solve"
		// in prose doesn't over-match.
		regexp.MustCompile(`(?i)^solve\b`),
		regexp.MustCompile(`(?i)\bsolve\s+(our|the|all)\b`),
		regexp.MustCompile(`(?i)\b(clear|empty|drain|clean\s+out)\s+(the\s+|our\s+)?(bug\s+)?backlog\b`),
	},
	IntentVerify: {
		regexp.MustCompile(`(?i)^(verify\s|confirm\s|sanity[\s\-]check\s|double[\s\-]check\s|make sure\s)`),
		regexp.MustCompile(`(?i)\b(sanity[\s\-]check|double[\s\-]check|make sure)\b`),
		regexp.MustCompile(`(?i)^are\s+the\s+`),
	},
	IntentAudit: {
		regexp.MustCompile(`(?i)^(audit\s|survey\s)`),
		regexp.MustCompile(`(?i)\bany cleanup\b`),
		regexp.MustCompile(`(?i)^are there any\b`),
		regexp.MustCompile(`(?i)\blook across\b`),
		regexp.MustCompile(`(?i)^what'?s the state of\s+(open|all)\b`),
		// Refactor-shape directives with NO literal trigger keyword
		// ("this function does too much, break it apart") fold into
		// audit: refactor-intent ⊆ audit semantics (survey current
		// state for debt/cleanup candidates). Chain refactor-intent-
		// discipline-surfacing chose this over minting a new
		// IntentRefactor shape. refactorShapePattern is reused as the
		// refactoring-discipline conditional in intentDisciplineMap so
		// classification and discipline-gating stay in lockstep.
		refactorShapePattern,
		// review fold (§14.3): "review X" / "take a look at X" / "look
		// through X" is survey-the-current-state intent — folded into
		// audit so it inherits audit's work-state + conditional
		// disciplines. NOTE bare "look at" is deliberately EXCLUDED: it
		// false-positives on bug reports ("when I look at the front end
		// … the page is broken") which are fix/none, not review.
		regexp.MustCompile(`(?i)^review\b`),
		regexp.MustCompile(`(?i)\btake a look\b`),
		regexp.MustCompile(`(?i)\blook through\b`),
	},
	IntentExecute: {
		// The chain-execution verb family (§14.1 grounded inventory).
		// Sentence-initial (after polite strip) verbs:
		regexp.MustCompile(`(?i)^(complete|work\s+(through|on)|pick\s+up|continue|resume|start|finish(\s+(out|up))?|drive|carry\s+on|proceed\s+with)\b`),
		// Non-initial idioms: "<slug> please work through this chain",
		// "pick up where we left off", "continue working on X".
		regexp.MustCompile(`(?i)\b(pick\s+up|work\s+through|continue\s+(work|working)|finish\s+(out|up))\b`),
		// "<slug> please resume" (resume not sentence-initial).
		regexp.MustCompile(`(?i)\bplease\s+resume\b`),
		// "let's finish/revisit/wrap …" — first-person-plural execution.
		regexp.MustCompile(`(?i)^let'?s\s+(finish|revisit|wrap)\b`),
		// "re-resolve chain X and task Y" — re-run resolution over named work.
		regexp.MustCompile(`(?i)\bre-?resolve\b`),
	},
	IntentExplain: {
		regexp.MustCompile(`(?i)^(explain\s|describe\s)`),
		regexp.MustCompile(`(?i)^(what does\b|how does\b|how do you\b|what'?s the difference\b|what'?s the role of\b)`),
	},
	IntentSummarize: {
		regexp.MustCompile(`(?i)^(summarize\s|tl;?dr\b|recap\s|summary of\b)`),
		regexp.MustCompile(`(?i)\bgive me a\s+(brief\s+)?(overview|summary)\b`),
		regexp.MustCompile(`(?i)\bbrief overview\b`),
	},
	IntentStatus: {
		regexp.MustCompile(`(?i)^(what'?s the status\b|status of\b|where are we\b|current state of\b)`),
		regexp.MustCompile(`(?i)^is\s+\w+\s+\w*\s*(still\s+)?open\?`),
		regexp.MustCompile(`(?i)^show me .* (in-progress|pending|active)\b`),
	},
	IntentList: {
		regexp.MustCompile(`(?i)^(list\s|enumerate\s)`),
		regexp.MustCompile(`(?i)^show me\s+(all|every)\b`),
		regexp.MustCompile(`(?i)^what\s+\w+\s+(are|skills are)\s+installed\b`),
	},
}

// refactorShapePattern matches refactor-intent directives that carry
// NO literal refactoring-discipline trigger keyword — the gap the
// keyword skill_trigger path misses ("this function does too much,
// break it apart"; "make this easier to follow"). It plays two roles
// in lockstep:
//   - here, as an IntentAudit detection pattern, routing such prompts
//     to the audit intent;
//   - in intentDisciplineMap (discipline_intent.go), as the Conditional
//     that gates refactoring-discipline so only refactor-flavored
//     audits surface it — a generic "audit X for unused exports" stays
//     a plain audit.
//
// Deliberately scoped to keyword-free phrasings: literal triggers
// (refactor / restructure / extract / simplify this / …) are already
// surfaced by the keyword skill_trigger path, and the discipline-intent
// dedup drops any overlap. Calibrated against the §13.7 fixture's audit
// bucket so it does not reclassify existing prompts.
var refactorShapePattern = regexp.MustCompile(`(?i)\b(` +
	`(does|doing|do)\s+(way\s+)?too\s+much|` +
	`too\s+many\s+(responsibilities|concerns)|` +
	`break\s+(it|this|that|them|this\s+\w+|the\s+\w+|that\s+\w+)\s+(apart|up|down|into|out)|` +
	`split\s+(it|this|that|them|this\s+\w+|the\s+\w+|that\s+\w+)\s+(apart|up|into|out)|` +
	`easier\s+to\s+(follow|read|understand|maintain|reason)|` +
	`hard(er)?\s+to\s+(follow|read|understand|maintain)|` +
	`(this|that|it)\s+is\s+(too|overly|really)\s+(long|complex|complicated|tangled|convoluted|nested)|` +
	`untangle|disentangle` +
	`)\b`)

// DetectIntent is the pure-Go pattern-based directive-intent
// detector. Pattern-first per T4 §13.5; deterministic, stateless,
// concurrency-safe.
//
// Returns IntentResult{Shape: IntentNone, Confidence: 0,
// DetectedVia: "default"} for messages that don't match any shape's
// pattern — the rest of parse_context proceeds normally. Callers
// MAY consult an IntentClassifier as a fallback (handler-orchestrated,
// not embedded here; the Qwen path stays opt-in).
//
// Performance: each match runs against the normalized leading slice
// of the message (capped at 512 chars to bound the regex cost on
// pathological inputs). All patterns compile once at package init
// via the package-level vars. Typical: sub-100μs per call.
//
// Priority: shapes are checked in intentPriority order; the first
// shape whose patterns match wins. T4 §13.5 prescribes
// fix > implement > verify > audit > status > list > explain >
// summarize > none — calibrated against the §13.7 fixture so
// "please implement that fix after filing it" classifies as
// implement (leading verb is the directive; "fix" is a noun).
// §14.3 inserts execute below audit: an execution directive that also
// carries a verify/fix/audit verb keeps the more-specific shape.
func DetectIntent(message string) IntentResult {
	if message == "" {
		return IntentResult{Shape: IntentNone, DetectedVia: "default"}
	}
	// Cap the slice we run regex against — the matchers are anchored
	// at start anyway and pathological multi-MB messages shouldn't
	// stall parse_context.
	slice := message
	if len(slice) > 512 {
		slice = slice[:512]
	}
	// Strip polite prefix so verb-anchor patterns trigger on the
	// actual directive ("please implement X" → "implement X").
	stripped := politePrefix.ReplaceAllString(strings.TrimSpace(slice), "")

	// Shapes evaluated in strict priority order; first match wins.
	// The "implement" branch fires on leading "implement"/"add"/etc.
	// — beating "fix" when the message reads "implement that fix"
	// because the pattern is sentence-initial after the polite strip.
	ordered := []IntentShape{
		IntentImplement,
		IntentFix,
		IntentVerify,
		IntentAudit,
		// IntentExecute sits BELOW the specific action verbs (§14.3):
		// a verify/fix/audit prompt that also mentions an execution verb
		// ("please sanity check then work through X", "start this chain"
		// after a "sanity check") keeps its more-specific shape because
		// that shape is evaluated first. Above status/list/explain/
		// summarize, which it never collides with (disjoint verb stems).
		IntentExecute,
		IntentStatus,
		IntentList,
		IntentExplain,
		IntentSummarize,
	}
	for _, shape := range ordered {
		for _, re := range intentPatterns[shape] {
			if re.MatchString(stripped) {
				return IntentResult{Shape: shape, Confidence: 1.0, DetectedVia: "pattern"}
			}
		}
	}
	return IntentResult{Shape: IntentNone, DetectedVia: "default"}
}

// IntentClassifier is the optional Qwen-rubric backstop named in
// T4 §13.6. Production HandlerDeps may set this to nil
// (pattern-only mode); tests inject a stub when exercising the
// fallback path. Must return promptly (≤500ms target per T4) and
// degrade gracefully on classifier outage.
//
// Today the interface ships unimplemented: the pattern set already
// covers ≥80% of the §13.7 fixture per T4's latency-budget gate, so
// the Qwen rubric path is deferred to a future chain (T4 §13.5
// names TOOLKIT_PARSE_CONTEXT_INTENT_RUBRIC=1 as the opt-in flag
// when an implementer lands).
type IntentClassifier interface {
	ClassifyIntent(ctx context.Context, message string) (IntentResult, error)
}
