package ml

import (
	"testing"

	"toolkit/internal/actionspec"
)

// action_doc_test.go pins the standing invariants of the ml descriptor registry
// (chain migrate-ml-action-docs-to-derive-contract).
//
// Unlike work and knowledge, ml's T2 has NO "registry-derived == current corpus"
// parallel-run test: the current corpus documents inference's params/returns/
// errors as PROSE inside `notes` (no structured blocks), so the registry-derived
// doc deliberately does NOT reproduce it — the T3 flip restructures the prose into
// struct-derived [[params]] + [[errors]] + [returns], a reviewed re-baseline of
// the T1 net. Byte-parity for the served docs is then pinned by the no-diff corpus
// gate (action-docs-corpus-gen --check) plus the T1 characterization net
// (internal/actiondocs/ml_contract_net_test.go). The tests below pin invariants
// the net doesn't: that the single action stays struct-derived (the contract's
// whole point) and that the registry mirrors the dispatch table.

// TestMLRegistryDerivedParamsHaveEmptyAuthoredType is the param-tag gate: a param
// backed by a struct field (its name matches a json tag) MUST leave its descriptor
// Type empty so the type is genuinely DERIVED, never re-authored. ml has no
// map-bound actions, so EVERY ml param must derive. Mirrors
// knowledge.TestKnowledgeRegistryDerivedParamsHaveEmptyAuthoredType.
func TestMLRegistryDerivedParamsHaveEmptyAuthoredType(t *testing.T) {
	for _, e := range mlActionRegistry {
		if e.ParamStruct == nil {
			t.Errorf("action %q has a nil ParamStruct; every ml action is struct-backed and must derive its param types", e.Name)
			continue
		}
		tags := map[string]bool{}
		for _, p := range actionspec.Extract(e.ParamStruct) {
			tags[p.JSONName] = true
		}
		for _, dp := range e.Doc.Params {
			if !tags[dp.Name] {
				t.Errorf("action %q param %q matches no json tag on its param struct; it would fall back to an authored (empty) type", e.Name, dp.Name)
			}
			if tags[dp.Name] && dp.Type != "" {
				t.Errorf("action %q param %q is struct-backed but authors Type=%q; leave it empty so it derives",
					e.Name, dp.Name, dp.Type)
			}
		}
	}
}

// TestMLRegistryDerivesInferenceParamTypes pins that inference's params derive the
// expected types + order from InferenceParams — the T2 deliverable. model_id /
// grounding_event_id are int64; task is string; features_data ([]float32) and
// features_shape ([]int64) are object[] (the contract's uniform slice derivation;
// their numeric element shape lives in the param description).
func TestMLRegistryDerivesInferenceParamTypes(t *testing.T) {
	specs := MLActionSpecs()
	var inference *actionspec.ActionSpec
	for i := range specs {
		if specs[i].Name == "inference" {
			inference = &specs[i]
			break
		}
	}
	if inference == nil {
		t.Fatalf("MLActionSpecs() has no `inference` entry; got %d specs", len(specs))
	}

	type want struct {
		name     string
		typ      string
		required bool
	}
	wants := []want{
		{"model_id", "int64", false},
		{"task", "string", false},
		{"features_data", "object[]", true},
		{"features_shape", "object[]", true},
		{"grounding_event_id", "int64", false},
	}
	if len(inference.Params) != len(wants) {
		t.Fatalf("inference has %d params, want %d: %+v", len(inference.Params), len(wants), inference.Params)
	}
	for i, w := range wants {
		got := inference.Params[i]
		if got.Name != w.name {
			t.Errorf("param %d: name = %q, want %q (order is authoritative from the descriptor)", i, got.Name, w.name)
		}
		if got.Type != w.typ {
			t.Errorf("param %q: derived Type = %q, want %q", w.name, got.Type, w.typ)
		}
		if got.Required != w.required {
			t.Errorf("param %q: Required = %v, want %v", w.name, got.Required, w.required)
		}
	}
}

// TestMLRegistry_MatchesStaticDispatchTable pins that the descriptor registry
// covers exactly the ml surface's statically-registered actions — no action
// silently dropped from the registry, none registered without a descriptor. The
// always-on table (nil deps, no convenience actions) is just `inference`; per-task
// convenience actions register dynamically at model-promotion time and carry no
// co-located descriptor by design, so they are out of scope for the corpus.
func TestMLRegistry_MatchesStaticDispatchTable(t *testing.T) {
	registry := map[string]bool{}
	for _, e := range mlActionRegistry {
		registry[e.Name] = true
	}
	table := map[string]bool{}
	for name := range BuildTable(TableDeps{}) {
		table[name] = true
	}
	for name := range table {
		if !registry[name] {
			t.Errorf("dispatch action %q has no descriptor in mlActionRegistry", name)
		}
	}
	for name := range registry {
		if !table[name] {
			t.Errorf("registry action %q is not registered on the static ml dispatch table", name)
		}
	}
}
