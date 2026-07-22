package actiondocs_test

import (
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/actiondocs"
)

// Bug 1436: agents calling admin.action_describe to learn an action's
// parameters miss that mutating actions REQUIRE a top-level `rationale`
// field at the dispatch envelope, because rationale is a dispatcher-
// level gate (enforced via policy.Registry.ValidateRationale) rather
// than a per-action schema field. The fix documents the gate in each
// surface's _general action-doc chunk so action_describe(surface,
// _general) surfaces it.
//
// This test pins the documentation as present in every surface that
// has rationale-gated actions in action-manifests/dispatch-policy.toml.
// If a future surface adds rationale-gated actions, this test starts
// failing until the matching _general.toml documents the gate.
func TestActionDocs_RationaleGateDocumentedInGeneralChunks(t *testing.T) {
	root := productionActionDocsRoot(t)
	registry, err := actiondocs.Load(root)
	if err != nil {
		t.Fatalf("Load(%s): %v", root, err)
	}
	if errs := registry.ParseErrors(); len(errs) > 0 {
		t.Fatalf("production action-docs corpus has parse errors: %+v", errs)
	}

	// Surfaces with rationale-gated actions per action-manifests/dispatch-policy.toml.
	// Update this set when policy changes.
	surfacesWithRationaleGate := []string{"work", "knowledge", "measure", "admin"}

	for _, surface := range surfacesWithRationaleGate {
		doc, ok := registry.Get(surface, "_general")
		if !ok {
			t.Errorf("surface %q has no _general chunk; agents won't see surface-wide rationale-gate documentation", surface)
			continue
		}
		notes := strings.ToLower(doc.Notes)
		// The marker phrase is intentional and consistent across surfaces
		// so the parity test is unambiguous. If you want to rename the
		// marker, do it everywhere at once.
		if !strings.Contains(notes, "rationale") {
			t.Errorf("surface %q _general chunk does not mention 'rationale' — agents calling action_describe will miss the envelope-level gate", surface)
		}
		if !strings.Contains(notes, "envelope") {
			t.Errorf("surface %q _general chunk mentions rationale but not 'envelope' — the docs must make clear the field is envelope-level (next to action/params/project) not inside params", surface)
		}
	}
}

// productionActionDocsRoot returns the absolute path to the production
// action-docs corpus relative to this test file. Mirrors the
// actionDocsRoot helper in param_tag_gate_test.go but exported under a
// distinct name so the two helpers can coexist without rename churn
// when param_tag_gate_test is in the same package.
func productionActionDocsRoot(t *testing.T) string {
	t.Helper()
	// go/internal/actiondocs/corpus/ — the corpus sits beside this test
	// file's package dir (relocated under the Go module for go:embed,
	// chain single-source-action-describe T6). go test runs with cwd at
	// the package dir, so the relative path resolves to the corpus.
	return filepath.Clean("corpus")
}
