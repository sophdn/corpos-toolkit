// Package mcpparam hosts the standard MCP-dispatch parameter extractors
// used by every handler whose entry point receives a json.RawMessage.
//
// ## Intended use
//
// **Workflow served:** MCP meta-tool handlers (work, knowledge, measure,
// admin) accept their parameter bag as a single json.RawMessage; each
// handler then plucks a small set of typed fields from that bag with
// silent-default semantics (absent or wrong-typed → zero/default).
// Before extraction this package, three packages each carried a
// duplicate strParam/int64Param pair — they now share one.
//
// **Invocation pattern:** import `toolkit/internal/mcpparam` and call
// `mcpparam.String(params, "query")`, `mcpparam.Int64(params, "top_k",
// 5)`, or `mcpparam.Int64Opt(params, "since")` directly; the package
// is stateless.
//
// **Success shape:** String returns "" for absent/wrong-typed/missing;
// Int64 returns the supplied default; Int64Opt returns a nil pointer.
// All three preserve the prior silent-default semantics — no coercion
// (e.g. string-as-int64 parsing), no panics on malformed JSON.
//
// **Non-goals:** not for HTTP-query parameter parsing — that path uses
// `*http.Request` and lives in `observehttp.boolParam` /
// `observehttp.optSince`; not a validation surface (handlers still
// check required-but-empty after extraction); does not coerce between
// kinds (`"42"` does not parse as Int64 — strict typing matches the
// dispatch contract).
package mcpparam
