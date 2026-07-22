package mcpresult

import "encoding/json"

// ErrorEnvelope is the structured-error response shape every MCP
// work-style handler returns when the request can't proceed but the
// dispatch itself succeeded. JSON parity with the previous map-based
// error literals is exact: Error always present; Hint / Action /
// EditableFields / Chains populated only when the handler had useful
// context to surface.
type ErrorEnvelope struct {
	Error          string   `json:"error"`
	Hint           string   `json:"hint,omitempty"`
	Action         string   `json:"action,omitempty"`
	EditableFields []string `json:"editable_fields,omitempty"`
	Chains         []string `json:"chains,omitempty"`
}

// MarshalOkOrError emits the envelope when err is non-nil, otherwise
// marshals ok directly. Use for result types whose success branch is
// a pointer (e.g. `*Bug`, `*ChainDetail`) — a nil ok pointer renders
// as JSON `null`, matching the prior hand-rolled behaviour.
func MarshalOkOrError[T any](ok T, err *ErrorEnvelope) ([]byte, error) {
	if err != nil {
		return json.Marshal(err)
	}
	return json.Marshal(ok)
}

// MarshalOkOrErrorList emits the envelope when err is non-nil,
// otherwise marshals the slice — coercing a nil slice to `[]` so the
// wire format distinguishes "no matches" from "tool error". The
// distinction matters for the dashboard contract on TaskListResult /
// TaskBlockersResult / ChainFindResult and mirrors the prior
// hand-rolled `if r.List == nil { return json.Marshal([]T{}) }` shape.
func MarshalOkOrErrorList[T any](ok []T, err *ErrorEnvelope) ([]byte, error) {
	if err != nil {
		return json.Marshal(err)
	}
	if ok == nil {
		return json.Marshal([]T{})
	}
	return json.Marshal(ok)
}
