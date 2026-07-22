// Package fs implements the owned filesystem meta-tool — Read/Write/Edit/Grep/
// Glob/LS plus Move/Remove reimplemented as toolkit-server actions so the agent
// navigates AND mutates the repo through surfaces we own rather than the
// substrate-blind harness tools.
//
// ## Intended use
//
// **Workflow served:** agents need to read files, search text, expand globs,
// list directories, and relocate or delete paths. The harness ships generic
// versions of these, but they are blind to our substrate (event log,
// knowledge_pointers, doc.go intended-use blocks, CODEMAP, projections). Owning
// them lets the DEFAULT stay byte-for-byte faithful to the harness behaviour
// (the parity floor that gates the deny-list swap) while opt-in modes layer
// substrate-native orientation on top. The move/remove primitives close the
// last capability-floor gap: file relocation and deletion in pure Go, so they
// work for any model and in the distroless container without shelling out to
// mv/rm.
//
// **Invocation pattern:** dispatched via the fs meta-tool; each action takes a
// typed params struct (ReadParams, GrepParams, GlobParams, LSParams) and returns
// a named result struct (ReadResult, …). BuildTable returns the dispatch.Table
// the MCP server wires at startup. Read actions need no DB; substrate-native
// upgrade modes take an optional *db.Pool via Deps.
//
// **Success shape:** a JSON object matching the action's named result struct —
// e.g. ReadResult.Content carries the numbered-line text (parity-faithful to the
// harness Read), GrepResult.Matches carries file:line:content rows. Widening to
// `any` happens once per action through dispatch.Adapt / AdaptParamsOnly — the
// single JSON-marshaling seam.
//
// **Non-goals:** does not own task/bug/vault state (see internal/work,
// internal/knowledge); does not run arbitrary commands (that is sys.exec); the
// parity default never injects substrate context — that is strictly opt-in so
// the common read stays predictable and token-cheap. fs.move refuses to clobber
// an existing destination and fs.remove refuses a non-empty directory without an
// explicit recursive flag — the mutation surface stays deliberately
// conservative.
package fs
