package sys

// action_doc.go is the descriptor-registry seam for the sys surface's action
// docs (the per-surface instantiation of the contract in
// docs/ACTION_DOC_CONTRACT.md). Each param's TYPE is DERIVED from the handler's
// typed param struct; only the irreducible semantics (purpose, params, errors,
// notes, returns) are authored here. The generated corpus (corpus/sys/*.toml) +
// admin.action_describe(sys, X) derive from this registry via SysActionSpecs().

import (
	"reflect"

	"toolkit/internal/actionspec"
)

var psDoc = actionspec.ActionDoc{
	Purpose: "Enumerate live processes by reading /proc directly (no ps dependency). Read-only and ungated.",
	Params: []actionspec.DocParam{
		{Name: "contains", Required: false, Description: "Only include processes whose command contains this substring."},
		{Name: "limit", Required: false, Description: "Cap the number of rows returned (0 = all). Rows are ordered by pid."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "PSResult",
		Description: "processes (rows of {pid, ppid, state, command, rss_kb}) and count.",
	},
	Notes: "Self-defined (the harness has no process tool). /proc/<pid>/stat is parsed by splitting comm at the last ')'; rss_kb = rss_pages * pagesize/1024. command is the full cmdline (space-joined), or [comm] for kernel threads. See go/internal/sys/testdata/INTROSPECTION_CONTRACT.md.",
}

var portsDoc = actionspec.ActionDoc{
	Purpose: "List listening TCP and bound UDP sockets (ss-equivalent), with the owning pid/process when visible. Read-only and ungated.",
	Params: []actionspec.DocParam{
		{Name: "proto", Required: false, Description: "Restrict to a protocol: tcp or udp."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "the ss tool is not installed", Message: "sys.ports: ss not found on PATH"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "PortsResult",
		Description: "ports (rows of {proto, state, local_addr, local_port, pid, process}) and count.",
	},
	Notes: "Backed by `ss -tulnpH`. The owning pid/process comes from ss's users:((...)) column, present only for the caller's own sockets without root. The address column is split on its last ':' so IPv6 and scoped addresses parse. See go/internal/sys/testdata/INTROSPECTION_CONTRACT.md.",
}

var unitsDoc = actionspec.ActionDoc{
	Purpose: "List systemd-user units. Read-only and ungated.",
	Params: []actionspec.DocParam{
		{Name: "type", Required: false, Description: "Unit type to list (default service)."},
		{Name: "active_only", Required: false, Description: "Only active units (default false → include inactive via --all)."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "the systemctl tool is not installed", Message: "sys.units: systemctl not found on PATH"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "UnitsResult",
		Description: "units (rows of {unit, load, active, sub, description}) and count.",
	},
	Notes: "Backed by `systemctl --user list-units --type=<type> -o json`. Self-defined. See go/internal/sys/testdata/INTROSPECTION_CONTRACT.md.",
}

var containersDoc = actionspec.ActionDoc{
	Purpose: "List containers from podman and docker, each row tagged with its runtime. Read-only and ungated; fail-soft when a runtime is absent.",
	Params: []actionspec.DocParam{
		{Name: "running_only", Required: false, Description: "Only running containers (default false → include stopped via -a)."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "ContainersResult",
		Description: "containers (rows of {runtime, id, image, names, state, status, ports}), count, and runtimes_queried (which runtimes were probed).",
	},
	Notes: "podman ps --format json (a JSON array) and docker ps --format '{{json .}}' (newline-delimited objects) are parsed into a unified row shape. An absent runtime contributes no rows and is omitted from runtimes_queried. See go/internal/sys/testdata/INTROSPECTION_CONTRACT.md.",
}

// The exec action doc is RETIRED from this surface (corpos-substrate-topology
// T6): exec is host-loop work corpos owns natively now. The exec_action.go /
// exec_runner.go implementation is retained as the parity oracle, but it is no
// longer registered as a sys surface action, so it is dropped from the action-doc
// registry + the generated corpus.

// sysActionRegistry is the ordered, co-located descriptor registry — the single
// source of the sys surface's action docs (introspection only post-T6).
var sysActionRegistry = []actionspec.ActionEntry{
	{Name: "ps", Doc: psDoc, ParamStruct: reflect.TypeOf(PSParams{})},
	{Name: "ports", Doc: portsDoc, ParamStruct: reflect.TypeOf(PortsParams{})},
	{Name: "units", Doc: unitsDoc, ParamStruct: reflect.TypeOf(UnitsParams{})},
	{Name: "containers", Doc: containersDoc, ParamStruct: reflect.TypeOf(ContainersParams{})},
}

// SysActionSpecs returns the sys surface's full action catalog, derived from the
// co-located descriptor registry. This is what the corpus generator projects
// into corpus/sys/*.toml and what admin.action_describe(sys, X) serves.
func SysActionSpecs() []actionspec.ActionSpec {
	return actionspec.DeriveSpecs(sysActionRegistry)
}
