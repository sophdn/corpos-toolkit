// Package mcpresult hosts the shared error-envelope shape and the
// success-or-error MarshalJSON helpers every MCP work-style handler
// uses to encode its result.
//
// ## Intended use
//
// **Workflow served:** handlers return one named result struct per
// action (BugReadResult, ChainStateResult, etc.). Each struct holds
// either a typed success branch or an `Err *ErrorEnvelope`, with a
// custom MarshalJSON that picks the populated branch. Before this
// package, every result type carried its own 5-line MarshalJSON
// duplicate; mcpresult centralises the branch-selection logic so
// per-type MarshalJSON shrinks to a one-line delegate while field
// names stay handler-specific (tests assert on `.Bug`, `.Detail`,
// `.List`, etc., not a uniform `.OK`).
//
// **Invocation pattern:** import `toolkit/internal/mcpresult` and
// delegate MarshalJSON to one of the two helpers:
//
//	func (r BugReadResult) MarshalJSON() ([]byte, error) {
//	    return mcpresult.MarshalOkOrError(r.Bug, r.Err)
//	}
//
//	func (r TaskListResult) MarshalJSON() ([]byte, error) {
//	    return mcpresult.MarshalOkOrErrorList(r.List, r.Err)
//	}
//
// **Success shape:** when Err is non-nil, both helpers marshal the
// envelope. When Err is nil: MarshalOkOrError marshals the OK branch
// directly (nil pointer renders as JSON `null`, matching the prior
// hand-rolled behaviour); MarshalOkOrErrorList coerces a nil slice
// to `[]` so callers can distinguish "no matches" from "tool error"
// (the dashboard contract).
//
// **Non-goals:** does NOT cover multi-branch discriminator shapes
// (BugListResult's titles_only/verbose/default, ChainStatusResult's
// HasList/HasSingle, LibraryFindResult's multi-mode); those stay
// hand-rolled — forcing them in would either lose information or
// require a second generic that costs more than it saves. Does NOT
// cover result types that inline ErrorEnvelope fields with omitempty
// (BugResolveResult, ChainCloseResult, TaskTransitionResult,
// TaskEditResult, ShaStampResult, RoadmapPreviewResult) — those have
// a different wire shape that doesn't bifurcate at a top-level Err
// pointer.
package mcpresult
