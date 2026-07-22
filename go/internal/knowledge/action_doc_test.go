package knowledge

import (
	"testing"

	"toolkit/internal/actiondocs"
	"toolkit/internal/actionspec"
)

// action_doc_test.go pins the standing invariants of the knowledge descriptor
// registry (chain migrate-knowledge-action-docs-to-derive-contract). The
// T2-era parallel-run equivalence test (registry-derived == hand-authored corpus
// modulo the enumerated blessed corrections) retired at the T3 flip, exactly as
// the work chain's parallel-run test retired with actionSpecs: the corpus is now
// GENERATED from this registry, so byte-parity for the served docs is pinned by
// the no-diff corpus gate (action-docs-corpus-gen --check) plus the T1
// characterization net (internal/actiondocs/knowledge_contract_net_test.go),
// which the flip regenerated for the four blessed correction cells:
//
//	curation_read/promote/reject.id : "string" → "integer"  (struct field ID int64)
//	curation_bulk_action.filter     : "string" → "object"   (the field is a {origin,unscored_only} object)
//
// Each was a doc-vs-struct bug the derive contract surfaced and corrected (an
// agent following the old "string" id would hit the typed-unmarshal rejection —
// the bug-888 class). The two tests below pin invariants the net doesn't.

// TestKnowledgeRegistry_CoversCorpusActionSet pins that the registry and the
// hand-authored corpus describe the SAME set of actions — no action silently
// dropped from the registry, none orphaned in the corpus. _general is corpus-
// only cross-cutting prose (no spec), excluded.
func TestKnowledgeRegistry_CoversCorpusActionSet(t *testing.T) {
	reg, err := actiondocs.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	corpus := map[string]bool{}
	for _, n := range reg.Names("knowledge") {
		corpus[n] = true
	}
	registry := map[string]bool{}
	for _, e := range knowledgeActionRegistry {
		registry[e.Name] = true
		if !corpus[e.Name] {
			t.Errorf("registry action %q has no hand-authored corpus chunk", e.Name)
		}
	}
	for n := range corpus {
		if !registry[n] {
			t.Errorf("corpus action %q is missing from the knowledge registry", n)
		}
	}
}

// TestKnowledgeRegistryDerivedParamsHaveEmptyAuthoredType is the param-tag gate:
// a param backed by a struct field (its name matches a json tag) MUST leave its
// descriptor Type empty so the type is genuinely DERIVED, never re-authored.
// Authored Type is permitted only for a param with no backing field (the map-
// bound vault/kiwix/library/knowledge_search/report_miss actions, ParamStruct ==
// nil). Mirrors work.TestRegistryDerivedParamsHaveEmptyAuthoredType.
func TestKnowledgeRegistryDerivedParamsHaveEmptyAuthoredType(t *testing.T) {
	for _, e := range knowledgeActionRegistry {
		if e.ParamStruct == nil {
			continue
		}
		tags := map[string]bool{}
		for _, p := range actionspec.Extract(e.ParamStruct) {
			tags[p.JSONName] = true
		}
		for _, dp := range e.Doc.Params {
			if tags[dp.Name] && dp.Type != "" {
				t.Errorf("action %q param %q is struct-backed but authors Type=%q; leave it empty so it derives",
					e.Name, dp.Name, dp.Type)
			}
		}
	}
}
