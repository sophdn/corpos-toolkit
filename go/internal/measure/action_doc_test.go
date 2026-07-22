package measure

import (
	"testing"

	"toolkit/internal/actiondocs"
	"toolkit/internal/actionspec"
)

// action_doc_test.go pins the standing invariants of the measure descriptor
// registry (chain migrate-measure-action-docs-to-derive-contract).
//
// The T2-era parallel-run equivalence test (TestMeasureRegistryReproducesCorpus —
// registry-derived doc == embedded hand-authored corpus modulo the enumerated
// blessed delta) RETIRED at the T3 flip, exactly as the work + knowledge chains'
// parallel-run tests retired: the corpus is now GENERATED from this registry, so
// byte-parity for the served docs is pinned by the no-diff corpus gate
// (action-docs-corpus-gen --check) plus the T1 characterization net
// (internal/actiondocs/measure_contract_net_test.go), which the flip regenerated
// for the single blessed correction cell:
//
//	bench_run.override_flags : "object" → "object[]"  (struct field []benchFlagPairCLI)
//
// The []benchFlagPairCLI slice derives to object[]; "List of {flag, value}
// entries" was always more accurately a list — the batch.ops-class correction
// from docs/ACTION_DOC_CONTRACT.md. The two tests below pin invariants the net
// doesn't.

// TestMeasureRegistry_CoversCorpusActionSet pins that the registry and the
// hand-authored corpus describe the SAME set of actions. _general is corpus-only
// cross-cutting prose (no spec), excluded.
func TestMeasureRegistry_CoversCorpusActionSet(t *testing.T) {
	reg, err := actiondocs.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	corpus := map[string]bool{}
	for _, n := range reg.Names("measure") {
		corpus[n] = true
	}
	registry := map[string]bool{}
	for _, e := range measureActionRegistry {
		registry[e.Name] = true
		if !corpus[e.Name] {
			t.Errorf("registry action %q has no hand-authored corpus chunk", e.Name)
		}
	}
	for n := range corpus {
		if !registry[n] {
			t.Errorf("corpus action %q is missing from the measure registry", n)
		}
	}
}

// TestMeasureRegistryDerivedParamsHaveEmptyAuthoredType is the param-tag gate: a
// param backed by a struct field (its name matches a json tag) MUST leave its
// descriptor Type empty so the type is genuinely DERIVED, never re-authored.
// Authored Type is permitted only for a param with no backing field (the
// map-bound classify_* / benchmark_query actions, ParamStruct == nil). Mirrors
// work.TestRegistryDerivedParamsHaveEmptyAuthoredType +
// knowledge.TestKnowledgeRegistryDerivedParamsHaveEmptyAuthoredType.
func TestMeasureRegistryDerivedParamsHaveEmptyAuthoredType(t *testing.T) {
	for _, e := range measureActionRegistry {
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
