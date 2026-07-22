package admin

import (
	"testing"

	"toolkit/internal/actiondocs"
	"toolkit/internal/actionspec"
)

// action_doc_test.go pins the standing invariants of the admin descriptor
// registry (chain migrate-admin-action-docs-to-derive-contract).
//
// The T2-era parallel-run equivalence test (TestAdminRegistryReproducesCorpus —
// registry-derived doc == embedded hand-authored corpus, byte-exact) RETIRED at
// the T3 flip, exactly as the work + knowledge + measure chains' parallel-run
// tests retired: the corpus is now GENERATED from this registry, so byte-parity
// for the served docs is pinned by the no-diff corpus gate
// (action-docs-corpus-gen --check) plus the T1 characterization net
// (internal/actiondocs/admin_contract_net_test.go). admin introduced NO blessed
// delta — every documented param (vault_search_metrics.since/recent_n,
// action_describe.surface/action) already matched its struct field kind, so the
// flip regenerated the corpus with zero net change (the cleanest per-surface
// migration in the family). The two invariant tests below pin what the net
// doesn't and stay past T3.

// TestAdminRegistry_CoversCorpusActionSet pins that the registry and the
// hand-authored corpus describe the SAME set of actions. _general is corpus-only
// cross-cutting prose (no spec), excluded.
func TestAdminRegistry_CoversCorpusActionSet(t *testing.T) {
	reg, err := actiondocs.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	corpus := map[string]bool{}
	for _, n := range reg.Names("admin") {
		corpus[n] = true
	}
	registry := map[string]bool{}
	for _, e := range adminActionRegistry {
		registry[e.Name] = true
		if !corpus[e.Name] {
			t.Errorf("registry action %q has no hand-authored corpus chunk", e.Name)
		}
	}
	for n := range corpus {
		if !registry[n] {
			t.Errorf("corpus action %q is missing from the admin registry", n)
		}
	}
}

// TestAdminRegistryDerivedParamsHaveEmptyAuthoredType is the param-tag gate: a
// param backed by a struct field (its name matches a json tag) MUST leave its
// descriptor Type empty so the type is genuinely DERIVED, never re-authored.
// Authored Type is permitted only for a param with no backing field — admin has
// none (it is fully struct-backed; every documented param derives). Mirrors
// work.TestRegistryDerivedParamsHaveEmptyAuthoredType +
// measure.TestMeasureRegistryDerivedParamsHaveEmptyAuthoredType.
func TestAdminRegistryDerivedParamsHaveEmptyAuthoredType(t *testing.T) {
	for _, e := range adminActionRegistry {
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
