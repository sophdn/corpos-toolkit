// Package admin implements the admin meta-tool — project/host CRUD,
// server health, schema reload, server version, vault search metrics,
// and the remote-execution surface.
//
// ## Intended use
//
// **Workflow served:** agents and the dashboard need to configure projects
// and hosts, check server health, reload schemas at runtime, read vault
// retrieval metrics, and run shell commands across the machines registered
// in the hosts table.
//
// **Invocation pattern:** dispatched via the admin meta-tool; each action
// takes a typed params struct and returns a named result struct (e.g.
// HealthResult, HostRegisterResult). BuildTable returns the dispatch.Table
// the MCP server wires at startup.
//
// **Success shape:** a JSON object matching the action's named result
// struct. Widening to `any` happens once per action through
// dispatch.Adapt / AdaptNoParams / AdaptParamsOnly — that adapter is the
// single JSON-marshaling seam in this package.
//
// **Non-goals:** does not own benchmark results (see internal/measure),
// does not run the remote shell beyond an os/exec invocation, does not
// duplicate forge's CRUD discipline — admin's CRUD is config-shaped, not
// artifact-shaped.
package admin
