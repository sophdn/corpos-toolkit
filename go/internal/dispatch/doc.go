// Package dispatch routes MCP meta-tool invocations to action handlers
// and enforces cross-cutting policy (rationale-required, span minting,
// actor inference) at the dispatch boundary.
//
// ## Intended use
//
// **Workflow served:** the MCP server lands a JSON-RPC call → dispatch
// picks the action handler from the meta-tool's action table, decodes
// `params` into a typed Go struct, enforces dispatch policy (T3
// rationale-required, T5 span minting, transport-derived actor kind),
// and invokes the handler with a span-scoped ctx.
//
// **Invocation pattern:** at startup, `disp := dispatch.NewMetaTool("work",
// workTable, policy.Policy{...})`; the MCP runtime calls `disp.Tool(...)`
// per request; the dispatcher routes by `action` to the registered
// handler. Handlers are registered via `dispatch.Adapt` / `AdaptNoParams`
// / `AdaptParamsOnly`, which is the single JSON-marshaling seam.
//
// **Success shape:** the action handler's named result struct widened
// to `any` once by the adapter; on error a typed `dispatch.Error`
// carrying surface, kind, and hint — including rationale-required hints
// when policy rejected the call before the handler ran.
//
// **Non-goals:** does not own action implementation (handlers live in
// internal/work, internal/forge, internal/measure, etc.), does not
// retry, does not run structured spans itself (delegates to internal/obs),
// does not emit events (handlers call internal/events.Emit inside their
// write tx).
package dispatch
