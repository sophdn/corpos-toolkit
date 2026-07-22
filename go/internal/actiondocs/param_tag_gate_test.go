package actiondocs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Bug no-ci-gate-ensures-action-doc-canonical-params-round-trip-through-handlers
// gate: every action-doc TOML declares canonical param names + `[[param_aliases]]`
// TO-side names. Each name MUST be reachable through ONE of the binding
// shapes the codebase actually uses:
//
//  1. `json:"<name>"` struct tag — typed unmarshal (work package's typed
//     handlers like HandleTaskBlock).
//  2. A literal quoted `"<name>"` string in any of the binding packages
//     (the surface's own package OR `forge/` OR `dispatch/`) — catches
//     map-bound names via `rawStringParam(params, "<name>")` and
//     dispatch-layer envelope handling. The forge family binds this way
//     (forge's `schema_name` lives in `go/internal/forge/`, read via
//     rawStringParam). The admin surface is itself typed (it json.Unmarshals
//     into json-tagged structs), so it binds via form #1; the quoted-literal
//     form remains for the genuinely map-bound names (knowledge's + measure's
//     mcpparam-bound handlers, now scoped out of this gate — their derived docs
//     are pinned by their nets + binder gates).
//
// If neither binding appears, the action-doc and the handler have drifted:
// a caller using the documented canonical name will hit the silent-success-
// no-effect path the prior bug (task-block-action-doc-advertises-blocked_by-
// canonical-but-handler-only-accepts-blocker_slug, fixed in ea953e8)
// demonstrated.
//
// Source-grep gating is the option (c) implementation from that bug's
// acceptance criteria: small footprint, robust to refactors, catches the
// load-bearing case ("no code path anywhere binds this name"). A name
// shared across multiple actions in the same surface only needs to appear
// once; the gate is per-(surface, name), not per-action.
//
// False-positive shape: a param name like "slug" appears in many structs
// AND in many error messages / log statements as a literal string, so
// the check passes for every action that uses it. That's fine — the
// load-bearing case is silent absence, not selective binding. The
// synthetic-drift test below pins that a name with NO occurrence
// anywhere does fail the gate.

// internalRoot returns the absolute path to go/internal/ relative to
// this test file. The relative-path arithmetic is anchored to
// runtime.Caller(0) so the test works regardless of the directory the
// user invokes `go test` from.
func internalRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot resolve repo paths")
	}
	// thisFile is .../go/internal/actiondocs/param_tag_gate_test.go.
	// go/internal/ is one level up.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), ".."))
}

// surfaceSourceRoot returns the absolute path to go/internal/<surface>/.
// Used by the synthetic-drift test for surface-scoped sentinel checks.
func surfaceSourceRoot(t *testing.T, surface string) string {
	t.Helper()
	return filepath.Join(internalRoot(t), surface)
}

// actionDocsRoot returns the absolute path to the action-docs corpus,
// which sits beside this test file at go/internal/actiondocs/corpus
// (relocated under the Go module so go:embed can reach it — chain
// single-source-action-describe T6).
func actionDocsRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot resolve repo paths")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "corpus"))
}

// collectBindings reads every .go file under root and returns the set
// of param names that appear EITHER as `json:"<name>"` struct tags OR
// as quoted string literals `"<name>"`. The union covers both binding
// shapes documented at the top of this file. Recursive walk; only .go
// files are scanned. Build tags are not honored — a binding in a
// tagged-out file still counts, which is intentional (the gate cares
// about authorial intent, not runtime reachability).
//
// Quoted-literal matching is intentionally broad: any `"<name>"`
// occurrence in any context (struct tag, switch arm, params-map key,
// log message, error text) is accepted as evidence the name is known
// to the package. False-positives from incidental string matches are
// acceptable because the load-bearing failure mode is *total absence*
// of the name from a surface that claims it.
func collectBindings(t *testing.T, root string) map[string]bool {
	t.Helper()
	bindings := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip test files — they routinely include param names as
		// JSON-payload literals in fixture data (e.g. mustJSON(t,
		// map[string]any{"blocked_by": "x"})). Counting those as
		// bindings makes the gate pass on the failure shape we want
		// to catch.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s := string(data)

		// 1. struct-tag form: `json:"<name>"` (with optional ,omitempty).
		i := 0
		for {
			j := strings.Index(s[i:], `json:"`)
			if j < 0 {
				break
			}
			start := i + j + len(`json:"`)
			end := strings.Index(s[start:], `"`)
			if end < 0 {
				break
			}
			tag := s[start : start+end]
			if comma := strings.Index(tag, ","); comma >= 0 {
				tag = tag[:comma]
			}
			if tag != "" {
				bindings[tag] = true
			}
			i = start + end + 1
		}

		// 2. generic quoted-literal form: `"<name>"` anywhere in the
		// source. Catches strParam / rawStringParam / int64Param /
		// switch-arm / map-key shapes. Tokens that look like
		// identifiers (letter / digit / underscore only, length ≥ 2)
		// are kept; arbitrary string literals like "hello, world" are
		// not param-name candidates.
		i = 0
		for {
			j := strings.Index(s[i:], `"`)
			if j < 0 {
				break
			}
			start := i + j + 1
			end := strings.Index(s[start:], `"`)
			if end < 0 {
				break
			}
			lit := s[start : start+end]
			if looksLikeIdentifier(lit) {
				bindings[lit] = true
			}
			i = start + end + 1
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return bindings
}

// looksLikeIdentifier reports whether s is a plausible param-name
// candidate: at least 2 chars, only [A-Za-z0-9_], starts with a letter
// or underscore. Excludes most prose literals (with spaces, commas,
// colons, etc.) and bare numerics ("42", "200") that would otherwise
// pollute the binding set.
func looksLikeIdentifier(s string) bool {
	if len(s) < 2 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// TestActionDocParamCanonicalNames_RoundTripThroughHandlerBindings is the
// gate. For each action-doc, every canonical param name and every
// `[[param_aliases]].to` value must be reachable as a binding (json tag
// OR quoted literal) somewhere under `go/internal/`. The check pools
// bindings across every internal package because meta-tool actions
// frequently delegate to handlers outside their surface's package
// (e.g. work.forge → forge/; knowledge.resolve_references → refresolve/).
// The contract is "the name is bound SOMEWHERE the dispatch chain can
// reach"; if the gate widened to per-(surface, action) checking, it
// would have to know every delegation rule, which is the gate's
// authoritative source-of-truth (not something it can re-derive).
//
// Drift detection: a name that appears in ZERO packages is the load-
// bearing failure mode the gate exists to catch (the task_block bug
// pattern). Names that appear in some-but-wrong package still produce
// a true positive (binding exists), which is acceptable — that's a
// per-action wiring concern, not the gate's scope.
func TestActionDocParamCanonicalNames_RoundTripThroughHandlerBindings(t *testing.T) {
	root := actionDocsRoot(t)
	reg, err := Load(root)
	if err != nil {
		t.Fatalf("load %s: %v", root, err)
	}
	if reg.Len() == 0 {
		t.Fatalf("registry empty — expected action docs under %s", root)
	}

	// One walk over go/internal/ produces the global binding set.
	bindings := collectBindings(t, internalRoot(t))

	type miss struct {
		surface string
		action  string
		name    string
		reason  string
	}
	var misses []miss
	for _, surface := range reg.Surfaces() {
		if surface == "work" || surface == "knowledge" || surface == "measure" || surface == "admin" || surface == "ml" {
			// Every action-doc surface now derives its param names from its
			// co-located descriptor registry (chains
			// establish-action-doc-contract-on-work,
			// migrate-knowledge-action-docs-to-derive-contract,
			// migrate-measure-action-docs-to-derive-contract,
			// migrate-admin-action-docs-to-derive-contract,
			// migrate-ml-action-docs-to-derive-contract); the standing T1 parity
			// nets (contract_net_test.go + the per-surface *_contract_net_test.go)
			// pin their served output, and struct-backed params are gate-checked
			// against their json tags in-package
			// (work/knowledge/measure.Test*RegistryDerivedParams…). So this
			// name-reachability gate is redundant for every surface and scoped
			// out. knowledge's + measure's map-bound param types are additionally
			// pinned against their mcpparam binders by
			// TestActionDocParamTypes_{Knowledge,Measure}MapBoundBinderParity.
			continue
		}
		for _, doc := range reg.List(surface) {
			for _, p := range doc.Params {
				if p.Name == "" {
					continue
				}
				if !bindings[p.Name] {
					misses = append(misses, miss{
						surface: surface,
						action:  doc.Action,
						name:    p.Name,
						reason:  "canonical param name from [[params]]",
					})
				}
			}
			for _, a := range doc.ParamAliases {
				if a.To == "" {
					continue
				}
				if !bindings[a.To] {
					misses = append(misses, miss{
						surface: surface,
						action:  doc.Action,
						name:    a.To,
						reason:  fmt.Sprintf("alias TO from [[param_aliases]] (from=%q)", a.From),
					})
				}
			}
		}
	}

	if len(misses) > 0 {
		t.Errorf("action-doc canonical names not reachable as any binding under go/internal/:")
		for _, m := range misses {
			t.Errorf("  %s.%s: missing binding for %q (source: %s)", m.surface, m.action, m.name, m.reason)
		}
		t.Errorf("Fix one of: (a) add a binding (json:\"<name>\" tag OR a quoted-literal reference like strParam(params, \"<name>\")) to the handler's package under go/internal/; (b) update the action-doc TOML if the name is wrong.")
	}
}

// TestActionDocParamCanonicalNames_GateCatchesSyntheticDrift is the
// failing-test requirement from the bug's acceptance criteria: prove
// the gate logic actually catches a name that has no json tag, rather
// than passing vacuously.
//
// We construct a synthetic ActionDoc declaring a param the surface's
// real source doesn't bind, run the same gate logic the live test
// uses, and assert the miss appears.
func TestActionDocParamCanonicalNames_GateCatchesSyntheticDrift(t *testing.T) {
	// Pick a real surface so the json-tag cache is populated against
	// real source. The synthetic param name is one we KNOW isn't bound
	// anywhere under go/internal/work/ — chosen for uniqueness so the
	// test can't accidentally pass by colliding with an unrelated tag.
	const syntheticName = "param_that_definitely_does_not_exist_in_any_struct_xyzzy"

	bindings := collectBindings(t, surfaceSourceRoot(t, "work"))
	if bindings[syntheticName] {
		t.Fatalf("synthetic-drift name %q already exists in source; pick a different sentinel", syntheticName)
	}

	syntheticDoc := &ActionDoc{
		Surface: "work",
		Action:  "synthetic_action_for_drift_gate_test",
		Purpose: "test fixture; never registered against a real handler",
		Params: []Param{
			{Name: "slug", Type: "string", Required: true},
			{Name: syntheticName, Type: "string", Required: false},
		},
		ParamAliases: []ParamAlias{
			{From: "old_name", To: "another_" + syntheticName},
		},
	}

	// Run the same check as the live test, against just this doc.
	var seenSyntheticParam, seenSyntheticAliasTo bool
	for _, p := range syntheticDoc.Params {
		if p.Name == "" {
			continue
		}
		if !bindings[p.Name] && p.Name == syntheticName {
			seenSyntheticParam = true
		}
	}
	for _, a := range syntheticDoc.ParamAliases {
		if a.To == "" {
			continue
		}
		if !bindings[a.To] && a.To == "another_"+syntheticName {
			seenSyntheticAliasTo = true
		}
	}

	if !seenSyntheticParam {
		t.Errorf("gate failed to detect missing canonical param %q in surface 'work'", syntheticName)
	}
	if !seenSyntheticAliasTo {
		t.Errorf("gate failed to detect missing alias-to %q in surface 'work'", "another_"+syntheticName)
	}

	// Sympathy: a real existing binding should NOT be flagged. "slug"
	// is pervasive in work/ structs, so it must be in the bindings.
	if !bindings["slug"] {
		t.Errorf("gate over-fires: real param 'slug' should be present in work/ bindings")
	}
}
