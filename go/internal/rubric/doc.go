// Package rubric parses classify-response text from a model into a
// typed `ParseResult` against a fixed label set.
//
// ## Intended use
//
// **Workflow served:** handlers that classify a model response against
// a labelled rubric (severity, surface, verdict, etc.) need to detect
// single-label, multi-label, none-matched, and explicit-unclassifiable
// outcomes from arbitrary free-text responses; this package owns that
// parser.
//
// **Invocation pattern:** `r := rubric.ParseSingleClass(response,
// allowedLabels)` returning a `ParseResult{Kind, Label, Labels}` where
// `Kind` is one of `ParsedSingle | ParsedMultiple | ParsedNone |
// ParsedUnclassifiable`.
//
// **Success shape:** the parsed kind plus the matched label (single)
// or labels (multiple); callers branch on `Kind` to record the label
// on the row or flag the row for review.
//
// **Non-goals:** not a model client (callers run the model and pass the
// response text), not a rubric loader (rubric definitions live as TOML
// and are loaded by internal/measure), not a scoring engine — pure
// text parsing with no side effects.
package rubric
