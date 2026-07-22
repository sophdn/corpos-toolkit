package work

import (
	"testing"

	"toolkit/internal/actionspec"
)

// Post-T4 note: the parallel-run equivalence test (registry-derived ==
// actionSpecs) retired with actionSpecs itself. The registry is now the single
// source; byte-parity for every doc consumer is gated by the T1 characterization
// net (internal/actiondocs/contract_net_test.go), which exercises
// HandleWorkActions / describe / the corpus / the /admin payload. The two tests
// below pin invariants the net doesn't: the param-tag gate and the
// identifier-group round-trip.

// TestRegistryDerivedParamsHaveEmptyAuthoredType is the param-tag gate: a param
// backed by a struct field (its name matches a json tag) MUST leave its
// descriptor Type empty so the type is genuinely derived, never re-authored.
// Authored Type is permitted only for a param with no backing field (the forge
// family / task_edit / roadmap_list authored exceptions + chain_close's
// closure_summary custom-unmarshal key + task_search.verbose, read outside
// taskSearchParams).
func TestRegistryDerivedParamsHaveEmptyAuthoredType(t *testing.T) {
	for _, e := range actionRegistry {
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

// TestIdentifierGroupRoundTrips pins the acceptance criterion that the
// 'id OR slug' identifier-group is representable in the descriptor and round-
// trips: no new construct, just optional params (the one-of is handler-
// enforced). chain_state carries slug + id as optional, with chain_id aliasing
// id and chain/chain_slug aliasing slug.
func TestIdentifierGroupRoundTrips(t *testing.T) {
	cs, ok := derivedByName(t, "chain_state")
	if !ok {
		t.Fatal("chain_state not in registry")
	}
	want := map[string]ActionParam{
		"slug":       {Name: "slug", Type: "string", Required: false},
		"chain":      {Name: "chain", Type: "string", Required: false, AliasOf: "slug"},
		"chain_slug": {Name: "chain_slug", Type: "string", Required: false, AliasOf: "slug"},
		"id":         {Name: "id", Type: "int64", Required: false},
		"chain_id":   {Name: "chain_id", Type: "int64", Required: false, AliasOf: "id"},
	}
	got := map[string]ActionParam{}
	for _, p := range cs.Params {
		got[p.Name] = ActionParam{Name: p.Name, Type: p.Type, Required: p.Required, AliasOf: p.AliasOf}
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("chain_state missing identifier param %q", name)
			continue
		}
		if g.Type != w.Type || g.Required != w.Required || g.AliasOf != w.AliasOf {
			t.Errorf("chain_state param %q: got {type:%s required:%v aliasOf:%q} want {type:%s required:%v aliasOf:%q}",
				name, g.Type, g.Required, g.AliasOf, w.Type, w.Required, w.AliasOf)
		}
	}
}

func derivedByName(t *testing.T, name string) (ActionSpec, bool) {
	t.Helper()
	for _, s := range deriveActionSpecs() {
		if s.Name == name {
			return s, true
		}
	}
	return ActionSpec{}, false
}
