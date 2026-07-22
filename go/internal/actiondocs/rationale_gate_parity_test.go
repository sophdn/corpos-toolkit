package actiondocs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"toolkit/internal/actiondocs"
)

// Bug 1437: parity gate between action-manifests/dispatch-policy.toml
// and the per-action envelope_requirements in the action-docs corpus.
// Every (surface, action) the dispatcher enforces requires_rationale=true
// for must declare a matching envelope_requirements entry in its
// action-doc TOML. Without this gate the two sources of truth drift
// silently — the policy file rejects calls the docs don't predict.
//
// The check is bidirectional:
//   - policy says requires_rationale=true → action-doc has envelope_requirements
//     with field=="rationale"
//   - action-doc has envelope_requirements with field=="rationale" → policy
//     has requires_rationale=true for that (surface, action)
//
// Either direction failing means an agent gets surprised at call time —
// the failure modes are symmetric (docs promise less than policy enforces
// OR docs promise more than policy enforces).
func TestActionDocs_DispatchPolicyParity_RationaleEnvelopeRequirement(t *testing.T) {
	corpus, err := actiondocs.Load(productionActionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	if errs := corpus.ParseErrors(); len(errs) > 0 {
		t.Fatalf("action-docs corpus has parse errors: %+v", errs)
	}

	policyPath := productionDispatchPolicyPath(t)
	policyEntries := loadDispatchPolicyEntries(t, policyPath)

	// Build the policy set: keys "surface.action" where requires_rationale==true.
	policyRequires := make(map[[2]string]struct{}, len(policyEntries))
	for k, gates := range policyEntries {
		if gates.RequiresRationale {
			policyRequires[k] = struct{}{}
		}
	}

	// Direction 1: every policy entry with requires_rationale=true must
	// have a matching envelope_requirements entry in the action-doc.
	for key := range policyRequires {
		surface, action := key[0], key[1]
		doc, ok := corpus.Get(surface, action)
		if !ok {
			// An action declared in dispatch-policy without an action-doc
			// is a separate authoring gap — we report it but don't
			// double-fail the parity check.
			t.Errorf("dispatch-policy declares requires_rationale for %s.%s but no action-doc chunk exists", surface, action)
			continue
		}
		if !hasRationaleEnvelopeRequirement(doc.EnvelopeRequirements) {
			t.Errorf("%s.%s: dispatch-policy.requires_rationale=true but action-doc envelope_requirements missing field=\"rationale\"; agents reading action_describe(%q, %q) will miss the gate", surface, action, surface, action)
		}
	}

	// Direction 2: every action-doc envelope_requirements with
	// field=="rationale" must match a policy entry with requires_rationale=true.
	for _, surface := range corpus.Surfaces() {
		for _, doc := range corpus.List(surface) {
			if !hasRationaleEnvelopeRequirement(doc.EnvelopeRequirements) {
				continue
			}
			key := [2]string{surface, doc.Action}
			if _, ok := policyRequires[key]; !ok {
				t.Errorf("%s.%s: action-doc envelope_requirements declares field=\"rationale\" but dispatch-policy does NOT set requires_rationale=true; docs promise enforcement the dispatcher won't deliver", surface, doc.Action)
			}
		}
	}
}

func hasRationaleEnvelopeRequirement(reqs []actiondocs.EnvelopeRequirement) bool {
	for _, r := range reqs {
		if r.Field == "rationale" && r.Required {
			return true
		}
	}
	return false
}

// productionDispatchPolicyPath returns the absolute path to
// action-manifests/dispatch-policy.toml relative to this test file.
func productionDispatchPolicyPath(t *testing.T) string {
	t.Helper()
	return filepath.Clean(filepath.Join("..", "..", "..", "action-manifests", "dispatch-policy.toml"))
}

// loadDispatchPolicyEntries parses the dispatch-policy TOML into a map
// keyed by (surface, action). Lives here rather than importing the
// policy package so the test stays at the actiondocs layer and doesn't
// pull a dispatch-layer dependency.
func loadDispatchPolicyEntries(t *testing.T, path string) map[[2]string]policyGate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dispatch-policy %s: %v", path, err)
	}
	var raw map[string]map[string]policyGate
	if err := toml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse dispatch-policy %s: %v", path, err)
	}
	out := make(map[[2]string]policyGate)
	for surface, actions := range raw {
		for action, gate := range actions {
			out[[2]string{surface, action}] = gate
		}
	}
	return out
}

// policyGate mirrors policy.Gates locally — the test purposefully avoids
// importing the dispatch/policy package so the parity check stays
// dependency-flat. Field tags MUST mirror policy.Gates exactly.
type policyGate struct {
	RequiresRationale bool `toml:"requires_rationale"`
}
