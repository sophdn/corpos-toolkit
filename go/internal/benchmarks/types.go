package benchmarks

import (
	"toolkit/internal/qwenretrieve"
)

// ExpectedFieldValueKind discriminates ExpectedFieldValue variants.
type ExpectedFieldValueKind int

const (
	// FieldExact requires the field's serialized value to equal Value exactly.
	FieldExact ExpectedFieldValueKind = iota
	// FieldPresent requires the field to be non-null and non-empty; Value/GoldSet unused.
	FieldPresent
	// FieldOneOf requires the field's value to equal one of OneOf (case-insensitive).
	FieldOneOf
	// FieldContains requires the field's serialized value to contain Value
	// as a substring (case-insensitive).
	FieldContains
	// FieldJaccardAtLeast requires the field's parsed tag set to overlap
	// GoldSet with Jaccard ≥ Threshold. Right rule for surface-tag-style
	// fields where strict equality is too harsh.
	FieldJaccardAtLeast
)

// ExpectedFieldValue is the per-field grading rule for Extract scenarios.
// Ported from Rust enum ExpectedFieldValue; the Go shape uses a tagged
// struct (Kind discriminator + per-variant fields) rather than a sealed
// interface, matching the broader Go-side payload pattern in events/.
type ExpectedFieldValue struct {
	Kind      ExpectedFieldValueKind
	Value     string   // used by FieldExact, FieldContains
	OneOf     []string // used by FieldOneOf
	GoldSet   []string // used by FieldJaccardAtLeast
	Threshold float64  // used by FieldJaccardAtLeast (0.0–1.0)
}

// ExtractGoldOutput is the per-scenario gold-output spec for Extract.
// Fields not listed are unconstrained (the model can fill or skip).
type ExtractGoldOutput struct {
	Fields map[string]ExpectedFieldValue
}

// ExtractTaskInput is the agent-side prose + source the extract
// dispatcher receives. Ported from Rust inference_clients::dispatcher::
// ExtractTaskInput; benchmarks-local since Go's measure surface doesn't
// expose extract as a separate dispatch path (callers compose inline).
type ExtractTaskInput struct {
	ProblemStatement string
	Source           string
	Project          string
}

// SchemaField is one field in an ExtractContext's target schema. Ported
// from Rust dispatcher::SchemaField; benchmarks-local for the same reason.
type SchemaField struct {
	Name        string
	Description string
	Required    bool
	Kind        string
	ValidValues []string // optional enumeration; nil when unconstrained
}

// ExtractContext is the schema context the dispatcher attaches to an
// extract call. Ported from Rust dispatcher::ExtractContext.
type ExtractContext struct {
	SchemaName   string
	SchemaFields []SchemaField
}

// ExtractScenario is one Extract scenario — prose + schema → gold-graded TOML.
type ExtractScenario struct {
	Slug        string
	TaskInput   ExtractTaskInput
	Context     ExtractContext
	GoldOutput  ExtractGoldOutput
	AbsentField string // empty unless this is an honesty case
}

// ── Classify ────────────────────────────────────────────────────────────

// ClassifyGoldKind discriminates ClassifyGold variants.
type ClassifyGoldKind int

const (
	// GoldSingleClass: exactly the one label in SingleLabel.
	GoldSingleClass ClassifyGoldKind = iota
	// GoldMultiClass: the set of labels in MultiLabel (order-insensitive).
	GoldMultiClass
	// GoldUnclassifiable: honesty case — no label fits.
	GoldUnclassifiable
)

// ClassifyGold is the gold answer for a Classify scenario.
type ClassifyGold struct {
	Kind        ClassifyGoldKind
	SingleLabel string   // used by GoldSingleClass
	MultiLabel  []string // used by GoldMultiClass
}

// ClassifyScenario is one Classify scenario — prose + rubric slug → gold label(s).
//
// Translate-behavior-not-syntax: the Rust shape carried a full
// ClassifyContext (labels, mode, rubric prose, worked_examples). The Go
// shape carries RubricSlug instead — the runner resolves it via
// rubric.Registry.Get to recover the equivalent context. This matches
// Go's TOML-loaded rubric pattern where context lives in the rubric
// file, not in the scenario.
type ClassifyScenario struct {
	Slug       string
	Text       string // the prose to classify (was ClassifyTaskInput.text in Rust)
	RubricSlug string // resolves to a RubricDef via rubric.Registry
	Gold       ClassifyGold
}

// ── Retrieve ────────────────────────────────────────────────────────────

// RetrieveScenario is one Retrieve scenario — query + candidate list →
// gold path at rank-1 (or honest "no match" for honesty cases).
type RetrieveScenario struct {
	Slug        string
	TaskInput   qwenretrieve.RetrieveTaskInput // reuse Go's existing type
	Context     qwenretrieve.RetrieveContext   // reuse Go's existing type
	GoldPath    string
	HonestyCase bool
}

// ── Summarize (placeholder, deferred per T1 scope) ─────────────────────

// SummarizeTaskInput is the text + parameters for a summarize call.
// Placeholder; ported for shape parity with Rust until Summarize work
// re-enters scope. Benchmarks-local.
type SummarizeTaskInput struct {
	Text        string
	TargetLen   int
	MustMention []string
}

// SummarizeContext is the budget + constraints for a summarize call.
// Placeholder. Benchmarks-local.
type SummarizeContext struct {
	MaxTokens int
}

// SummarizeScenario is the placeholder scenario type for the deferred
// Summarize shape. No scenarios authored against this until a future
// chain re-enters the Summarize offload work.
type SummarizeScenario struct {
	Slug            string
	TaskInput       SummarizeTaskInput
	Context         SummarizeContext
	FactNotInSource string // empty unless this is an honesty case
}

// ── L3 / L4 / L5 / L6 tool-shape types ─────────────────────────────────

// ToolSpec is one tool entry surfaced to the model in the prompt.
// Mirrors Rust scenarios::ToolSpec.
type ToolSpec struct {
	Name        string
	Description string
}

// Scenario is one Layer 3 scenario — tests tool-selection only.
// Mirrors Rust scenarios::Scenario.
type Scenario struct {
	ID                  string
	ToolName            string
	UserPrompt          string
	AvailableTools      []ToolSpec
	InvokedContextually bool
}

// ExpectedArgValueKind discriminates the L4 argument-expectation shape.
type ExpectedArgValueKind int

const (
	// ArgExact requires the argument's value to equal Value exactly.
	ArgExact ExpectedArgValueKind = iota
	// ArgPresent requires the argument to be present and non-empty.
	ArgPresent
)

// ExpectedArgValue is an L4 per-argument expectation. Mirrors Rust
// scenarios::ExpectedArgValue (tagged-struct shape vs Rust's enum).
type ExpectedArgValue struct {
	Kind  ExpectedArgValueKind
	Value string // used by ArgExact
}

// ExpectedArg pairs an argument name with its expected value rule.
// Rust used a (&str, ExpectedArgValue) tuple; Go favors named fields.
type ExpectedArg struct {
	Name string
	Rule ExpectedArgValue
}

// L4Scenario extends L3 with a project context and expected-args spec.
// ContextSnapshot is the prose rendered into the prompt (the runner
// stores ProjectSnapshot.Render() output here).
type L4Scenario struct {
	ID              string
	ToolName        string
	UserPrompt      string
	AvailableTools  []ToolSpec
	ContextSnapshot string
	ExpectedArgs    []ExpectedArg
}

// L5Scenario is one Layer 5 scenario — tests model's ability to
// interpret a synthetic tool output and extract the actionable fact.
// Mirrors Rust scenarios_l5::L5Scenario.
type L5Scenario struct {
	ID             string
	ToolName       string
	ToolOutput     string
	Question       string
	ExpectedAnswer string
}

// NegativeDecisionKind discriminates the L6 expected-decision variants.
type NegativeDecisionKind int

const (
	// NegativeNoTool: the model should NOT invoke any tool.
	NegativeNoTool NegativeDecisionKind = iota
	// NegativeAskForClarification: the model should ask the user a
	// question (reason field must contain '?').
	NegativeAskForClarification
	// NegativeRouteTo: the model should invoke the named target tool.
	NegativeRouteTo
)

// NegativeDecision is the L6 expected verdict for a negative-case
// scenario. Mirrors Rust scenarios_l6::NegativeDecision.
type NegativeDecision struct {
	Kind   NegativeDecisionKind
	Target string // used by NegativeRouteTo
}

// L6Scenario is one Layer 6 scenario — tests negative-case decisions
// (no-tool / clarify / route-to). Mirrors Rust scenarios_l6::L6Scenario.
type L6Scenario struct {
	ID               string
	ToolName         string
	UserPrompt       string
	AvailableTools   []ToolSpec
	ExpectedDecision NegativeDecision
}
