// Package actionspec is the typed SOURCE contract for action-doc param shapes
// (chain establish-action-doc-contract-on-work). It is distinct from
// internal/actiondocs, which parses and serves the generated TOML corpus — this
// package is the source those docs derive from.
//
// ## Intended use
//
// **Workflow served:** deriving a handler's documented param shape from its
// typed param struct rather than re-declaring it by hand. The shape-extractor
// reads a param struct's exported json-tagged fields and maps each field's Go
// kind to the spec type vocabulary, so a param's documented type IS its struct
// field kind — making doc-type drift (the bug-888 class) impossible by
// construction. The action descriptor + registry (T3) build on this to replace
// the hand-authored work.actionSpecs catalog.
//
// **Invocation pattern:** `actionspec.Extract(reflect.TypeOf(xParams{}))`
// returns the ordered `[]Param` ({json name, spec type}) for a param struct;
// `actionspec.SpecType(fieldType)` maps a single Go field type to its spec-type
// string. Callers are the registry merge (which fills each descriptor param's
// type from the struct) and tests.
//
// **Success shape:** an ordered `[]Param` in struct declaration order, one entry
// per documented-eligible field; spec-type strings drawn from the fixed
// vocabulary `int64` / `string` / `bool` / `string[]` / `object[]` / `object` /
// `json` (the same vocabulary the corpus generator's docType() bridges into the
// doc-TOML vocabulary).
//
// **Non-goals:** does not author param names, order, required-ness, descriptions,
// or aliases — those live in the action descriptor (the authored half); does not
// parse or serve the corpus (internal/actiondocs); does not promote embedded
// struct fields (flat param structs only — it panics on an embedded field so the
// boundary is loud); does not recurse into nested struct params (a struct field
// is one `object`/`object[]` param, documented via its description + example).
package actionspec
