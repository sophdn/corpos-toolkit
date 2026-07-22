// Package actiondocs loads per-action MCP documentation chunks from a
// <surface>/<action>.toml directory tree and exposes a typed lookup API.
// In production the corpus is go:embed'd into the binary from the
// sibling corpus/ directory (see embed.go + LoadEmbedded); the on-disk
// Load(dir) path is the dev/hot-reload override.
//
// ## Intended use
//
// **Workflow served:** agents need random-access lookup of per-action
// documentation ("does bug_resolve accept sha as commit_sha?") without
// scanning the whole *Description constant in main.go. This registry
// holds the parsed corpus so admin.action_describe(surface, action) can
// return one chunk on demand.
//
// **Invocation pattern:** call LoadEmbedded() once at server startup
// (or Load(dir) for the dev override). Call Get(surface, action) for a
// single-chunk lookup (returns the doc + ok flag; ok=false on miss).
// Call List(surface) to enumerate every real action under a surface in
// lexicographic order; the reserved "_general" action name is excluded
// from List but findable via Get for surface-wide cross-cutting prose.
//
// **Success shape:** Load returns (*Registry, error); the error is
// reserved for I/O failures on the top-level dir (an absent dir is NOT
// an error — Load returns an empty registry so the binary runs without
// the corpus). Per-file parse failures land in ParseErrors() so the
// startup banner can log them without the binary fatalling on a single
// bad chunk.
//
// **Non-goals:** does not own the schema shape (declared in
// corpus/_schema.toml, documentary-only at this stage),
// does not enforce param-type vocabulary (the chunk's `params[].type`
// is free-form string), does not hot-reload on SIGHUP (Load-once at
// startup is the only entry point), does not own the admin.action_describe
// MCP handler (lives in internal/admin and ships in T4).
package actiondocs
