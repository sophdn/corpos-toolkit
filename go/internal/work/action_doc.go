package work

// action_doc.go is the descriptor-registry seam for the work surface's action
// docs (chain establish-action-doc-contract-on-work). It is the single source
// of the work surface's action docs: each param's TYPE is DERIVED from the
// handler's typed param struct, and only the irreducible semantics (purpose,
// param name-list/order/required/description/alias-of, value-aliases, errors,
// notes, envelope-requirements, example, returns) are authored, in a co-located
// Go descriptor.
//
// The descriptor types + the derive-merge were factored into package actionspec
// (the surface-agnostic source contract) by chain
// migrate-knowledge-action-docs-to-derive-contract so a second surface reuses
// ONE implementation. Work aliases them below; the co-located descriptors
// (chainStatusDoc, taskReadDoc, …) and the actionRegistry build on these names
// unchanged. Byte-parity across the move is gated by the T1 characterization net
// (internal/actiondocs/contract_net_test.go). See docs/ACTION_DOC_CONTRACT.md.

import "toolkit/internal/actionspec"

// Aliases to the shared descriptor types. DocParam is the authored half of one
// documented param (Type stays empty for struct-backed params — the param-tag
// gate); ActionDoc is the co-located authored doc; ActionEntry binds an action
// name to its descriptor + param-struct reflect.Type.
type (
	DocParam    = actionspec.DocParam
	ActionDoc   = actionspec.ActionDoc
	ActionEntry = actionspec.ActionEntry
)

// deriveActionSpecs walks the ordered registry and produces the full catalog
// the consumers read (work_actions / CallShape / the corpus generator /
// WorkDescription) via the shared merge.
func deriveActionSpecs() []ActionSpec {
	return actionspec.DeriveSpecs(actionRegistry)
}
