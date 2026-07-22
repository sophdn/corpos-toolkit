package sys

import (
	"toolkit/internal/dispatch"
)

// BuildTable returns the sys surface's dispatch.Table — the read-only
// introspection actions (ps / ports / units / containers), pure handlers, ungated
// (observation cannot mutate state).
//
// The gated `exec` action is RETIRED from this surface
// (corpos-substrate-topology T6, decompose-not-delete): exec is host-loop work
// that corpos now owns natively (corpos/internal/sysorgan, which delegates these
// introspection actions back here), and the distroless toolkit image has no
// /bin/sh, so toolkit exec was vestigial. The exec implementation
// (exec_action.go / exec_runner.go) is RETAINED as the parity oracle, just no
// longer registered as a surface action. Pairs with the BuildTable functions in
// internal/work, internal/ml, etc.
func BuildTable() dispatch.Table {
	return dispatch.Table{
		"ps":         dispatch.AdaptParamsOnly(HandlePS),
		"ports":      dispatch.AdaptParamsOnly(HandlePorts),
		"units":      dispatch.AdaptParamsOnly(HandleUnits),
		"containers": dispatch.AdaptParamsOnly(HandleContainers),
	}
}
