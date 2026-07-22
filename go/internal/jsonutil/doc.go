// Package jsonutil hosts small JSON-shape helpers that several handler
// packages otherwise duplicate or strand inside their own files.
//
// ## Intended use
//
// **Workflow served:** handlers that parse json.RawMessage scalars
// (forge field coercion, work skeleton-list rendering, knowledge
// optional-fold telemetry counts) share these primitives so the
// coercion rules and nil-fold semantics don't drift across surfaces.
//
// **Invocation pattern:** import `toolkit/internal/jsonutil` and call
// `jsonutil.ScalarToString(raw)` or `jsonutil.SumOptInt64(a, b)`
// directly; the package is stateless and has no constructor.
//
// **Success shape:** ScalarToString returns the display string of one
// JSON scalar (matching the original any-based fmt.Sprint(v) path);
// SumOptInt64 returns a pointer to the sum of two optional int64s with
// nil+nil=nil semantics.
//
// **Non-goals:** not a general JSON-manipulation library (use
// encoding/json directly for that); does not classify nested shapes
// (use forge.describeJSONShape or jsonShapeOf in handlers when that
// is needed); does not import any project package, so callers
// anywhere in the import graph can use it without cycle risk.
package jsonutil
