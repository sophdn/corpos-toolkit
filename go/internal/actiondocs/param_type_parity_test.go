package actiondocs

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Bug `no-type-parity-gate-between-action-docs-and-handler-param-structs`
// (889, follow-on from 888). The sibling name-reachability gate
// (param_tag_gate_test.go) proves every documented param NAME binds to a
// handler struct tag, but it does NOT check that the documented `type`
// matches the Go field's KIND. Bug 888 (task_block: task_id rendered
// `optional_string`, struct field int64) and its predecessor 883 both
// slipped through precisely this gap — an agent follows the doc type,
// passes a stringified id, and eats
// `cannot unmarshal string into Go struct field … of type int64`.
//
// This gate closes that axis. For each action-doc param documented as a
// PRIMITIVE type (integer / string / bool), it locates the struct
// field(s) carrying the matching `json:"<name>"` tag in the handler
// packages for that param's surface, and fails when every such field is
// a primitive of a DIFFERENT family than the doc declares.
//
// ── Scope decisions (deliberate, documented) ────────────────────────
//   - Per-SURFACE package scoping. Unlike the name gate (which pools
//     bindings globally because its failure mode is total absence), a
//     TYPE check is inherently per-binding: the same json tag name can
//     be int64 on one surface and a string elsewhere. So struct fields
//     are collected only from the package(s) that back the doc's surface.
//     surfacePackages carries that authoritative-source-of-truth the name
//     gate declined to re-derive (and the delegation a surface needs — e.g.
//     a surface whose handlers live partly in another package).
//   - Conservative primitive-only rule. We only flag when the doc type
//     is a primitive, EVERY in-scope field with that tag is also a
//     primitive, and none match the doc family. Object / list / custom
//     / pointer-to-struct field types are treated as "can't confidently
//     compare" and skipped — this keeps the false-positive surface near
//     zero while still catching the int64-vs-string class that bit 888.
//   - A param with no struct field in scope (bound outside the surface's
//     typed structs) is skipped — len(got)==0. All action-doc surfaces (work,
//     knowledge, measure, admin, ml) now derive their docs from typed handler
//     structs and/or descriptor-authored types, so no surface remains
//     hand-authored for this gate to cover; map-bound param types are pinned by
//     the mcpparam binder-parity tests below, and each surface's served output
//     by its T1 contract net.

// surfacePackages maps an action-doc surface to the go/internal package
// dirs whose param structs back that surface's typed handlers. A surface
// absent from this map is skipped by the gate (out of scope).
//
// `work`, `knowledge`, `measure`, `admin`, and `ml` are deliberately NOT here:
// their action docs now DERIVE their param types from the handler structs
// and/or author them in the descriptor (chains
// establish-action-doc-contract-on-work,
// migrate-knowledge-action-docs-to-derive-contract,
// migrate-measure-action-docs-to-derive-contract,
// migrate-admin-action-docs-to-derive-contract,
// migrate-ml-action-docs-to-derive-contract), so a doc-type-vs-struct-kind
// check is tautological for the struct-backed actions. Their derived output is
// pinned byte-exact by the standing T1 parity nets (contract_net_test.go + the
// per-surface *_contract_net_test.go) instead. The MAP-BOUND actions (which
// have no param struct to reflect) are covered by their own binder gates —
// knowledge's TestActionDocParamTypes_KnowledgeMapBoundBinderParity and
// measure's TestActionDocParamTypes_MeasureMapBoundBinderParity below. No
// surface remains hand-authored, so the map is empty and this gate is now a
// no-op — kept as the landing spot should a future hand-authored surface appear.
var surfacePackages = map[string][]string{}

// TestActionDocParamTypes_MatchHandlerStructFieldKinds is the type gate for any
// still-hand-authored surface (currently none — see surfacePackages): each
// documented primitive param type must match the kind of the handler struct
// field carrying that json tag. All migrated surfaces are excluded — their docs
// derive from the structs by
// construction (see surfacePackages), so the check is tautological there; the T1
// parity nets pin their output instead.
func TestActionDocParamTypes_MatchHandlerStructFieldKinds(t *testing.T) {
	reg, err := Load(actionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	if reg.Len() == 0 {
		t.Fatalf("registry empty — expected action docs under %s", actionDocsRoot(t))
	}
	internalDir := internalRoot(t)

	type miss struct{ surface, action, name, docType, goFams string }
	var misses []miss

	// flag records a type mismatch when the param's doc family is a gated
	// primitive, every in-scope struct field with that json tag is also a
	// primitive, and none match the doc family — the bug-888 shape.
	flag := func(surface, action, name, typeStr, df string, fams map[string]map[string]bool) {
		if df == "" {
			return // doc type is object/list/unknown — not gated
		}
		got := fams[name]
		if len(got) == 0 {
			return // no struct field in scope → params-map handler
		}
		allPrimitive, matched := true, false
		for f := range got {
			if !primitiveFamily[f] {
				allPrimitive = false
			}
			if f == df {
				matched = true
			}
		}
		if allPrimitive && !matched {
			misses = append(misses, miss{
				surface: surface, action: action, name: name,
				docType: typeStr, goFams: famSetString(got),
			})
		}
	}

	for _, surface := range reg.Surfaces() {
		pkgs, ok := surfacePackages[surface]
		if !ok {
			continue // surface not mapped to handler packages → out of scope
		}
		fams := collectStructFieldFamilies(t, internalDir, pkgs)

		for _, doc := range reg.List(surface) {
			for _, p := range doc.Params {
				flag(surface, doc.Action, p.Name, p.Type, docPrimitiveFamily(p.Type), fams)
			}
		}
	}

	if len(misses) > 0 {
		t.Errorf("action-doc param TYPES disagree with handler struct field kinds:")
		for _, m := range misses {
			t.Errorf("  %s.%s: param %q documented type=%q but the struct field(s) are %s — align the doc `type` to the Go field kind (e.g. int64 fields document as `integer`, with a note that stringified values fail the typed unmarshal).",
				m.surface, m.action, m.name, m.docType, m.goFams)
		}
	}
}

// TestActionDocParamTypes_GateCatchesSyntheticDrift proves the gate
// catches a documented-string param backed only by an int64 field —
// the exact bug-888 shape — rather than passing vacuously.
func TestActionDocParamTypes_GateCatchesSyntheticDrift(t *testing.T) {
	internalDir := internalRoot(t)
	// Logic fixture: the work/forge packages carry a known int64 field
	// (taskBlockParams.TaskID, json:"task_id"). work is no longer a gated
	// surface (its docs derive from these structs), but it stays the clearest
	// fixture for proving the gate's comparison logic catches the 888 shape.
	fams := collectStructFieldFamilies(t, internalDir, []string{"work", "construct"})

	// task_id is taskBlockParams.TaskID int64 — exclusively integer on the
	// work surface. A synthetic doc declaring it optional_string is the
	// 888 shape; the gate must flag it.
	got := fams["task_id"]
	if len(got) == 0 {
		t.Fatal("expected a struct field json:\"task_id\" in the work/forge packages")
	}
	df := docPrimitiveFamily("optional_string")
	allPrimitive, matched := true, false
	for f := range got {
		if !primitiveFamily[f] {
			allPrimitive = false
		}
		if f == df {
			matched = true
		}
	}
	if !(allPrimitive && !matched) {
		t.Errorf("gate failed to flag the 888 shape: task_id families=%s vs doc optional_string", famSetString(got))
	}

	// Sympathy: the real (fixed) doc type `integer` must NOT be flagged.
	dfInt := docPrimitiveFamily("integer")
	matchedInt := false
	for f := range got {
		if f == dfInt {
			matchedInt = true
		}
	}
	if !matchedInt {
		t.Errorf("gate over-fires: task_id documented `integer` should match its int64 field; families=%s", famSetString(got))
	}
}

// collectStructFieldFamilies parses every non-test .go file under the
// given package dirs and returns json-tag-name → set of Go type families
// across all struct fields carrying that tag.
func collectStructFieldFamilies(t *testing.T, internalDir string, pkgs []string) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
		dir := filepath.Join(internalDir, pkg)
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// A genuine parse error fails `go build`; the type gate skips
				// the file rather than double-reporting.
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				st, ok := n.(*ast.StructType)
				if !ok {
					return true
				}
				for _, field := range st.Fields.List {
					if field.Tag == nil {
						continue
					}
					tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
					jsonTag := tag.Get("json")
					if jsonTag == "" {
						continue
					}
					name := strings.Split(jsonTag, ",")[0]
					if name == "" || name == "-" {
						continue
					}
					fam := goFamily(exprTypeString(field.Type))
					if out[name] == nil {
						out[name] = map[string]bool{}
					}
					out[name][fam] = true
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return out
}

// primitiveFamily is the set of Go type families the gate compares
// strictly (integer vs string vs bool). Object / list families are
// treated as "don't compare".
var primitiveFamily = map[string]bool{"integer": true, "string": true, "bool": true}

// docPrimitiveFamily maps an action-doc `type` string to a primitive Go
// family, or "" when the doc type is not a gated primitive (object,
// list-shaped, or unknown).
func docPrimitiveFamily(docType string) string {
	switch docType {
	case "integer":
		return "integer"
	case "string", "optional_string":
		return "string"
	case "bool":
		return "bool"
	}
	return ""
}

// goFamily reduces a rendered Go type string to a family. Pointers are
// transparent (a *string is the same family as string); slices are
// "list"; selector / map / interface / unrecognised idents fall to
// "object" (the "don't strictly compare" bucket).
func goFamily(typeStr string) string {
	typeStr = strings.TrimPrefix(typeStr, "*")
	if strings.HasPrefix(typeStr, "[]") {
		return "list"
	}
	switch typeStr {
	case "string":
		return "string"
	case "bool":
		return "bool"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		return "integer"
	}
	return "object"
}

// exprTypeString renders an ast type expression to a comparable string
// for goFamily. Only the shapes the families care about are rendered
// precisely; everything else collapses to a token goFamily treats as
// "object".
func exprTypeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprTypeString(t.X)
	case *ast.ArrayType:
		return "[]" + exprTypeString(t.Elt)
	case *ast.SelectorExpr:
		return exprTypeString(t.X) + "." + t.Sel.Name
	case *ast.MapType:
		return "map"
	case *ast.InterfaceType:
		return "interface{}"
	default:
		return "?"
	}
}

// mapBoundWorkActions are the work-surface actions whose param envelope is
// bound by map-indexing (rawStringParam / rawBoolParam / json.Unmarshal over a
// json.RawMessage map) rather than a typed param struct — the authored-type
// exceptions enumerated in docs/ACTION_DOC_CONTRACT.md §"Type derivation &
// reconciliation". They have no param struct, so the struct-kind gate
// (TestActionDocParamTypes_MatchHandlerStructFieldKinds, which excludes the
// work surface entirely) never covers them, and their authored doc types are
// pinned byte-exact by the T1 net WITHOUT ever being checked against what the
// handler actually READS. This is the gap closed below (suggestion
// extend-param-type-parity-to-strparam-int64param-binders; sibling to bug
// action-doc-descriptor-required-not-gated-against-handler-enforcement).
var mapBoundWorkActions = map[string]bool{
	"forge": true, "forge_edit": true, "forge_delete": true, "forge_schema": true,
	"task_edit": true, "roadmap_list": true,
}

// TestActionDocParamTypes_MapBoundWorkActionsBinderParity gates the authored
// param types of the map-bound work actions against per-key binder evidence,
// one tier deeper than the struct gate reaches. Evidence is the UNION of two
// sources:
//
//   - rawStringParam(_, "K") / rawBoolParam(_, "K") literal-key calls in the
//     handler source → K is string / bool (the forge family's binders).
//   - the typed-struct field families of sibling work actions (id / slug /
//     chain_slug / task_id carry typed definitions elsewhere on work, e.g.
//     taskBlockParams.TaskID int64) → reused via collectStructFieldFamilies.
//
// A documented PRIMITIVE type that contradicts the binder evidence for the same
// key is the bug-888 drift class (an int64-bound id documented `string`, etc.).
// CONSERVATIVE, matching the sibling gate: a param with no binder evidence is
// skipped (forge's `fields` object + `tasks` list are bound by extractFields /
// peelChainTasksFromParams, not the primitive binders — there is nothing to
// strictly compare, so they are not flagged).
//
// Scope boundary (documented): task_edit's int64 `id`/`task_id` are confirmed
// via their typed-struct siblings in the families map, NOT by resolving the
// json.Unmarshal target locals inside HandleTaskEdit — that intra-func dataflow
// is deliberately not walked (fragile); the families cross-check + the T1
// output net cover the residual.
func TestActionDocParamTypes_MapBoundWorkActionsBinderParity(t *testing.T) {
	reg, err := Load(actionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	internalDir := internalRoot(t)

	binderKind := collectRawParamBinderKinds(t, internalDir, []string{"work", "construct"})
	structFams := collectStructFieldFamilies(t, internalDir, []string{"work", "construct"})

	type miss struct{ action, name, docType, reason string }
	var misses []miss

	covered := 0
	for _, doc := range reg.List("work") {
		if !mapBoundWorkActions[doc.Action] {
			continue
		}
		for _, p := range doc.Params {
			df := docPrimitiveFamily(p.Type)
			if df == "" {
				continue // doc type is object/list/unknown — not a gated primitive
			}
			if bk, ok := binderKind[p.Name]; ok {
				covered++
				if bk != df {
					misses = append(misses, miss{doc.Action, p.Name, p.Type,
						fmt.Sprintf("the rawParam binder reads it as %s", bk)})
				}
				continue
			}
			if fams, ok := structFams[p.Name]; ok && len(fams) > 0 {
				allPrimitive, matched := true, false
				for f := range fams {
					if !primitiveFamily[f] {
						allPrimitive = false
					}
					if f == df {
						matched = true
					}
				}
				if allPrimitive {
					covered++
					if !matched {
						misses = append(misses, miss{doc.Action, p.Name, p.Type,
							fmt.Sprintf("sibling typed-struct field(s) are %s", famSetString(fams))})
					}
				}
			}
			// no binder evidence for this key → skip (can't confidently compare)
		}
	}

	if covered == 0 {
		t.Fatal("gate is vacuous: no map-bound work-action param matched any binder evidence — the scanner or action set drifted")
	}
	if len(misses) > 0 {
		t.Errorf("map-bound work-action doc param TYPES disagree with their binders (%d checked):", covered)
		for _, m := range misses {
			t.Errorf("  %s: param %q documented %q but %s — align the doc type to what the handler reads.",
				m.action, m.name, m.docType, m.reason)
		}
	}
}

// TestMapBoundBinderParity_GateCatchesSyntheticDrift proves the binder-parity
// gate fires rather than passing vacuously: forge's schema_name is read via
// rawStringParam, so documenting it `integer` must mismatch while `string` must
// match.
func TestMapBoundBinderParity_GateCatchesSyntheticDrift(t *testing.T) {
	binderKind := collectRawParamBinderKinds(t, internalRoot(t), []string{"work", "construct"})
	if binderKind["schema_name"] != "string" {
		t.Fatalf("scanner failed to find the rawStringParam binder for schema_name; got %q (binders found=%d)", binderKind["schema_name"], len(binderKind))
	}
	if docPrimitiveFamily("integer") == binderKind["schema_name"] {
		t.Error("gate would not catch an integer-documented string-bound param (the 888 shape)")
	}
	if docPrimitiveFamily("string") != binderKind["schema_name"] {
		t.Error("gate over-fires: schema_name documented `string` should match its rawStringParam binder")
	}
}

// mapBoundKnowledgeActions are the knowledge-surface actions whose param
// envelope is bound by map-indexing via the mcpparam helpers
// (mcpparam.String/Int64/Int64Opt over a json.RawMessage) rather than a typed
// param struct — the knowledge analog of mapBoundWorkActions. Their docs derive
// from the knowledge registry (chain migrate-knowledge-action-docs-to-derive-
// contract) with AUTHORED param types (ParamStruct == nil), so the struct-kind
// gate above (which no longer lists knowledge) never covers them. This set +
// the binder-parity gate below pin those authored types against what the handler
// actually reads, one tier deeper — exactly as the forge-family binder gate does
// for work. (The struct-backed knowledge actions — curation_* / parse_context /
// resolve_references — derive their types and are pinned by the knowledge net.)
var mapBoundKnowledgeActions = map[string]bool{
	"vault_search": true, "vault_read": true,
	"kiwix_search": true, "kiwix_fetch": true, "kiwix_list_books": true,
	"knowledge_search": true, "knowledge_report_miss": true,
	"library_add": true, "library_update": true, "library_get": true,
	"library_find": true, "library_retire": true, "library_cross_reference": true,
	"library_list_active": true, "library_list_dewey": true, "library_list_sections": true,
}

// TestActionDocParamTypes_KnowledgeMapBoundBinderParity gates the authored param
// types of the map-bound knowledge actions against per-key mcpparam binder
// evidence. A documented PRIMITIVE type that contradicts what mcpparam.String
// (string) / mcpparam.Int64 / mcpparam.Int64Opt (integer) reads for the same key
// is the bug-888 drift class (an int-bound id documented `string`, etc.).
// CONSERVATIVE, matching the work sibling: a param whose key has no mcpparam
// binder evidence is skipped (no-param actions, or a key bound some other way).
func TestActionDocParamTypes_KnowledgeMapBoundBinderParity(t *testing.T) {
	reg, err := Load(actionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	binderKind := collectMcpparamBinderKinds(t, internalRoot(t), []string{"knowledge", "refresolve"})

	type miss struct{ action, name, docType, reason string }
	var misses []miss
	covered := 0
	for _, doc := range reg.List("knowledge") {
		if !mapBoundKnowledgeActions[doc.Action] {
			continue
		}
		for _, p := range doc.Params {
			df := docPrimitiveFamily(p.Type)
			if df == "" {
				continue // doc type is object/list/unknown — not a gated primitive
			}
			bk, ok := binderKind[p.Name]
			if !ok {
				continue // no mcpparam binder evidence for this key → can't compare
			}
			covered++
			if bk != df {
				misses = append(misses, miss{doc.Action, p.Name, p.Type,
					fmt.Sprintf("the mcpparam binder reads it as %s", bk)})
			}
		}
	}
	if covered == 0 {
		t.Fatal("gate is vacuous: no map-bound knowledge-action param matched any mcpparam binder — the scanner or action set drifted")
	}
	if len(misses) > 0 {
		t.Errorf("map-bound knowledge-action doc param TYPES disagree with their mcpparam binders (%d checked):", covered)
		for _, m := range misses {
			t.Errorf("  %s: param %q documented %q but %s — align the descriptor type to what the handler reads.",
				m.action, m.name, m.docType, m.reason)
		}
	}
}

// TestKnowledgeMapBoundBinderParity_GateCatchesSyntheticDrift proves the gate
// fires rather than passing vacuously: vault_search reads "query" via
// mcpparam.String, so documenting it `integer` must mismatch while `string`
// must match.
func TestKnowledgeMapBoundBinderParity_GateCatchesSyntheticDrift(t *testing.T) {
	binderKind := collectMcpparamBinderKinds(t, internalRoot(t), []string{"knowledge", "refresolve"})
	if binderKind["query"] != "string" {
		t.Fatalf("scanner failed to find the mcpparam.String binder for query; got %q (binders found=%d)", binderKind["query"], len(binderKind))
	}
	if binderKind["top_k"] != "integer" {
		t.Fatalf("scanner failed to find the mcpparam.Int64 binder for top_k; got %q", binderKind["top_k"])
	}
	if docPrimitiveFamily("integer") == binderKind["query"] {
		t.Error("gate would not catch an integer-documented string-bound param (the 888 shape)")
	}
	if docPrimitiveFamily("string") != binderKind["query"] {
		t.Error("gate over-fires: query documented `string` should match its mcpparam.String binder")
	}
}

// mapBoundMeasureActions are the measure-surface actions whose param types are
// AUTHORED in the descriptor (ParamStruct == nil), not derived — the measure
// analog of mapBoundKnowledgeActions. measure is mostly map-bound: the classify_*
// family + benchmark_query bind via the mcpparam helpers (mcpparam.String /
// Int64Opt), and benchmark_record binds via the tolerant parseBenchmarkResult map
// (takeStr/optStrPtr → string, takeIntOrBool/optInt* → integer, optFloatPtr →
// float). bench_run AND benchmark_replay derive from structs (benchRunParams /
// benchmarkReplayParams) and are pinned by the measure net + the derived-empty-type
// gate, so they are NOT listed here. The binder-parity gate below pins the
// map-bound subset's documented PRIMITIVE types (string/integer) against what the
// handler reads — mcpparam evidence for the classify_* family + benchmark_query,
// parseBenchmarkResult evidence for benchmark_record (chain finalize-action-docs-
// epic T4, bug 940, which documented those params). float params (the *_score
// columns) are not a gated primitive family, so the gate skips them.
var mapBoundMeasureActions = map[string]bool{
	"benchmark_record": true, "benchmark_query": true,
	"classify_chain_task_proportionality": true, "classify_retirement_observation": true,
	"classify_artifact_tier": true, "classify_audit_finding_severity": true,
	"classify_artifact_review_criterion": true, "classify_session_routing_trigger": true,
	"classify_pre_commit_failure": true, "classify_docstring_drift": true,
	"classify_bug_severity": true,
}

// TestActionDocParamTypes_MeasureMapBoundBinderParity gates the authored param
// types of the map-bound measure actions against per-key mcpparam binder
// evidence — the analog of the knowledge + work forge-family binder gates. A
// documented PRIMITIVE that contradicts what mcpparam.String (string) /
// mcpparam.Int64Opt (integer) reads for the same key is the bug-888 drift class.
// CONSERVATIVE: a param whose key has no binder evidence is skipped. Evidence is
// the UNION of two scanners: mcpparam.String/Int64Opt (the classify_* family +
// benchmark_query's filters) and parseBenchmarkResult's take*/opt* helpers
// (benchmark_record's provenance bundle — added in chain finalize-action-docs-epic
// T4 so the newly-documented map-bound params are gated, bug 940). A key read as
// `float` (optFloatPtr — the *_score columns) is not a gated primitive, so the gate
// skips it on the doc side regardless.
func TestActionDocParamTypes_MeasureMapBoundBinderParity(t *testing.T) {
	reg, err := Load(actionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	binderKind := collectMcpparamBinderKinds(t, internalRoot(t), []string{"measure"})
	// Merge parseBenchmarkResult evidence (benchmark_record). Keys agree by
	// construction — a key is read as one kind — so only-add-if-absent is safe.
	for k, v := range collectBenchmarkResultBinderKinds(t, internalRoot(t), []string{"measure"}) {
		if _, ok := binderKind[k]; !ok {
			binderKind[k] = v
		}
	}

	type miss struct{ action, name, docType, reason string }
	var misses []miss
	covered := 0
	for _, doc := range reg.List("measure") {
		if !mapBoundMeasureActions[doc.Action] {
			continue
		}
		for _, p := range doc.Params {
			df := docPrimitiveFamily(p.Type)
			if df == "" {
				continue // doc type is object/list/unknown — not a gated primitive
			}
			bk, ok := binderKind[p.Name]
			if !ok {
				continue // no mcpparam binder evidence for this key → can't compare
			}
			covered++
			if bk != df {
				misses = append(misses, miss{doc.Action, p.Name, p.Type,
					fmt.Sprintf("the mcpparam binder reads it as %s", bk)})
			}
		}
	}
	if covered == 0 {
		t.Fatal("gate is vacuous: no map-bound measure-action param matched any mcpparam binder — the scanner or action set drifted")
	}
	if len(misses) > 0 {
		t.Errorf("map-bound measure-action doc param TYPES disagree with their mcpparam binders (%d checked):", covered)
		for _, m := range misses {
			t.Errorf("  %s: param %q documented %q but %s — align the descriptor type to what the handler reads.",
				m.action, m.name, m.docType, m.reason)
		}
	}
}

// TestMeasureMapBoundBinderParity_GateCatchesSyntheticDrift proves the gate fires
// rather than passing vacuously: classify_bug_severity reads "bug_report" via
// mcpparam.String, so documenting it `integer` must mismatch while `string` must
// match.
func TestMeasureMapBoundBinderParity_GateCatchesSyntheticDrift(t *testing.T) {
	binderKind := collectMcpparamBinderKinds(t, internalRoot(t), []string{"measure"})
	if binderKind["bug_report"] != "string" {
		t.Fatalf("scanner failed to find the mcpparam.String binder for bug_report; got %q (binders found=%d)", binderKind["bug_report"], len(binderKind))
	}
	if binderKind["since"] != "integer" {
		t.Fatalf("scanner failed to find the mcpparam.Int64Opt binder for since; got %q", binderKind["since"])
	}
	if docPrimitiveFamily("integer") == binderKind["bug_report"] {
		t.Error("gate would not catch an integer-documented string-bound param (the 888 shape)")
	}
	if docPrimitiveFamily("string") != binderKind["bug_report"] {
		t.Error("gate over-fires: bug_report documented `string` should match its mcpparam.String binder")
	}
}

// TestMeasureBenchmarkResultBinderParity_GateCatchesSyntheticDrift proves the
// parseBenchmarkResult scanner (the T4 coverage extension, bug 940) actually finds
// benchmark_record's map-bound keys — without it, benchmark_record's documented
// params would silently fall through as "no binder evidence" and never be gated.
// scenario_id is read via takeStr (→ string) and run_at via takeIntOrBool
// (→ integer), so documenting run_at `string` must mismatch while `integer` matches.
func TestMeasureBenchmarkResultBinderParity_GateCatchesSyntheticDrift(t *testing.T) {
	binderKind := collectBenchmarkResultBinderKinds(t, internalRoot(t), []string{"measure"})
	if binderKind["scenario_id"] != "string" {
		t.Fatalf("scanner failed to find the takeStr binder for scenario_id; got %q (binders found=%d)", binderKind["scenario_id"], len(binderKind))
	}
	if binderKind["run_at"] != "integer" {
		t.Fatalf("scanner failed to find the takeIntOrBool binder for run_at; got %q", binderKind["run_at"])
	}
	if binderKind["accuracy_score"] != "float" {
		t.Fatalf("scanner failed to find the optFloatPtr binder for accuracy_score; got %q", binderKind["accuracy_score"])
	}
	if docPrimitiveFamily("string") == binderKind["run_at"] {
		t.Error("gate would not catch a string-documented integer-bound param (the 888 shape) for run_at")
	}
	if docPrimitiveFamily("integer") != binderKind["run_at"] {
		t.Error("gate over-fires: run_at documented `integer` should match its takeIntOrBool binder")
	}
}

// collectMcpparamBinderKinds scans non-test .go under the given packages for
// mcpparam.String(_, "K") / mcpparam.Int64(_, "K", _) / mcpparam.Int64Opt(_, "K")
// calls carrying a string-literal key (args[1]), returning K → family (string /
// integer). The call shape is a selector (pkg.Func), unlike the work forge
// family's package-local rawStringParam idents. All binders for a given key
// agree by construction (a key is read as one kind), so a plain map suffices.
func collectMcpparamBinderKinds(t *testing.T, internalDir string, pkgs []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	selFam := map[string]string{"String": "string", "Int64": "integer", "Int64Opt": "integer"}
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
		dir := filepath.Join(internalDir, pkg)
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok || pkgIdent.Name != "mcpparam" {
					return true
				}
				fam, ok := selFam[sel.Sel.Name]
				if !ok || len(call.Args) < 2 {
					return true
				}
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if key, uerr := strconv.Unquote(lit.Value); uerr == nil && key != "" {
					out[key] = fam
				}
				return true
			})
			return nil
		})
	}
	return out
}

// collectRawParamBinderKinds scans non-test .go under the given packages for
// rawStringParam(_, "K") and rawBoolParam(_, "K") calls carrying a string-
// literal key, returning K → family (string / bool). A non-literal key can't be
// mapped to a documented param name, so it is skipped. All binders for a given
// key agree by construction (a key is read as one kind), so a plain map is
// sufficient.
func collectRawParamBinderKinds(t *testing.T, internalDir string, pkgs []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	binderFam := map[string]string{"rawStringParam": "string", "rawBoolParam": "bool"}
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
		dir := filepath.Join(internalDir, pkg)
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				fam, ok := binderFam[ident.Name]
				if !ok || len(call.Args) < 2 {
					return true
				}
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if key, uerr := strconv.Unquote(lit.Value); uerr == nil && key != "" {
					out[key] = fam
				}
				return true
			})
			return nil
		})
	}
	return out
}

// collectBenchmarkResultBinderKinds scans non-test .go under the given packages
// for measure's parseBenchmarkResult binder helpers — takeStr/optStr/optStrPtr
// (→ string), takeIntOrBool/optIntPtr/optIntOrBool/optIntOrBoolPtr (→ integer),
// and optFloatPtr (→ float) — carrying a string-literal key, returning K → family.
// Unlike the mcpparam scanner (key at args[1]) these helpers vary the key
// position (the local closures takeStr/takeIntOrBool take the key at args[0]; the
// package opt* helpers take it at args[1]), so the scanner takes the FIRST string
// literal among the call's args. This is the parseBenchmarkResult analog of
// collectMcpparamBinderKinds — added in chain finalize-action-docs-epic T4 so
// benchmark_record's now-documented map-bound params are gated (bug 940's
// "extend parseBenchmarkResult coverage"). All binders for a given key agree by
// construction, so a plain map suffices.
func collectBenchmarkResultBinderKinds(t *testing.T, internalDir string, pkgs []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	identFam := map[string]string{
		"takeStr": "string", "optStr": "string", "optStrPtr": "string",
		"takeIntOrBool": "integer", "optIntPtr": "integer",
		"optIntOrBool": "integer", "optIntOrBoolPtr": "integer",
		"optFloatPtr": "float",
	}
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
		dir := filepath.Join(internalDir, pkg)
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				fam, ok := identFam[ident.Name]
				if !ok {
					return true
				}
				// The key position varies (closures take it at args[0], package
				// helpers at args[1]); take the first string literal among the args.
				for _, a := range call.Args {
					lit, ok := a.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if key, uerr := strconv.Unquote(lit.Value); uerr == nil && key != "" {
						out[key] = fam
					}
					break
				}
				return true
			})
			return nil
		})
	}
	return out
}

// ── Required-vs-enforcement gate ─────────────────────────────────────
// Bug action-doc-descriptor-required-not-gated-against-handler-enforcement
// (sibling of the type-parity gates above). The action-doc contract authors
// each param's `Required` in the co-located Go descriptor, but the real
// id-OR-slug one-of is enforced in the HANDLER (IdentifierRequiredError),
// never derived from or cross-checked against the descriptor — a
// documented-vs-enforced drift surface. The type gates above pin param TYPE
// against the handler; nothing pinned param REQUIRED-ness. This closes that
// axis for the one verifiable, codified enforcement class: the id-OR-slug
// one-of every IdentifierRequiredError call site enforces.

// identifierOneOfViolations checks one action doc against the id-OR-slug
// one-of its handler enforces via IdentifierRequiredError: both `id` and
// `slug` must be documented, and NEITHER may be marked Required — the handler
// accepts either arm alone (with `id` the preferred form per the rejection
// message). Returns the list of violations; empty when consistent. Extracted
// so the synthetic-drift test exercises the same logic the live gate runs.
func identifierOneOfViolations(doc ActionDoc) []string {
	var idP, slugP *Param
	for i := range doc.Params {
		switch doc.Params[i].Name {
		case "id":
			idP = &doc.Params[i]
		case "slug":
			slugP = &doc.Params[i]
		}
	}
	var v []string
	switch {
	case idP == nil:
		v = append(v, "declares no `id` param, but the handler accepts `id` (the preferred identifier per IdentifierRequiredError)")
	case idP.Required:
		v = append(v, "marks `id` Required, but the handler accepts `slug` alone (one-of, not individually required)")
	}
	switch {
	case slugP == nil:
		v = append(v, "declares no `slug` param, but the handler accepts `slug`")
	case slugP.Required:
		v = append(v, "marks `slug` Required, but the handler accepts `id` alone (one-of, not individually required)")
	}
	return v
}

// TestActionDocRequired_IdentifierOneOfParity gates descriptor Required flags
// against handler enforcement. For every work action whose handler rejects
// "neither id nor slug" via IdentifierRequiredError("<action>"), the descriptor
// must document both arms and mark neither Required. A descriptor that omits
// `id` or marks `slug` Required tells the caller a false contract ("slug is
// required" when the handler accepts id alone) — the drift this gate pins.
func TestActionDocRequired_IdentifierOneOfParity(t *testing.T) {
	reg, err := Load(actionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	enforced := collectIdentifierEnforcedActions(t, internalRoot(t), []string{"work"})
	if len(enforced) == 0 {
		t.Fatal(`scanner found no IdentifierRequiredError("<action>") call sites — the scanner or the helper name drifted`)
	}

	covered := 0
	type miss struct {
		action   string
		problems []string
	}
	var misses []miss
	for _, doc := range reg.List("work") {
		if !enforced[doc.Action] {
			continue
		}
		covered++
		if v := identifierOneOfViolations(*doc); len(v) > 0 {
			misses = append(misses, miss{doc.Action, v})
		}
	}
	if covered == 0 {
		t.Fatal("gate vacuous: no IdentifierRequiredError-enforced action matched a work registry doc — the action set or surface mapping drifted")
	}
	if len(misses) > 0 {
		t.Errorf("action-doc Required flags disagree with handler id-OR-slug enforcement (%d enforced actions checked):", covered)
		for _, m := range misses {
			for _, p := range m.problems {
				t.Errorf("  %s: %s", m.action, p)
			}
		}
	}
}

// TestActionDocRequired_GateCatchesSyntheticDrift proves the gate fires on the
// exact drift shape (an enforced action marking `slug` Required and omitting
// `id` — the chain_close / trained_model_* shape) rather than passing
// vacuously, and does NOT over-fire on the correct one-of shape.
func TestActionDocRequired_GateCatchesSyntheticDrift(t *testing.T) {
	drift := ActionDoc{Action: "synthetic_enforced", Params: []Param{
		{Name: "slug", Required: true},
	}}
	if v := identifierOneOfViolations(drift); len(v) < 2 {
		t.Errorf("gate failed to flag the drift shape (slug Required + no id); got violations=%v", v)
	}
	ok := ActionDoc{Action: "synthetic_ok", Params: []Param{
		{Name: "id", Required: false},
		{Name: "slug", Required: false},
	}}
	if v := identifierOneOfViolations(ok); len(v) != 0 {
		t.Errorf("gate over-fires on a correct id-OR-slug doc: %v", v)
	}
}

// collectIdentifierEnforcedActions scans non-test .go under the given packages
// for IdentifierRequiredError("<action>") string-literal-key calls, returning
// the set of action names whose handler enforces the id-OR-slug one-of. Mirrors
// collectRawParamBinderKinds' AST literal-arg scan; the function's own
// definition is a FuncDecl (not a CallExpr) so it is not picked up.
func collectIdentifierEnforcedActions(t *testing.T, internalDir string, pkgs []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
		dir := filepath.Join(internalDir, pkg)
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "IdentifierRequiredError" || len(call.Args) < 1 {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if key, uerr := strconv.Unquote(lit.Value); uerr == nil && key != "" {
					out[key] = true
				}
				return true
			})
			return nil
		})
	}
	return out
}

// ── Required-vs-enforcement gate: ml resolveModel one-of ─────────────
// ml.inference's resolveModel enforces a model_id-OR-task one-of (model_id wins
// when both are set; task alone needs project at the envelope). The descriptor
// authors model_id + task as Required:false (one-of, enforced in the handler), so
// — exactly like work's id-OR-slug — nothing cross-checked those Required flags
// against resolveModel until this gate. (Chain finalize-action-docs-epic T2; the
// ml analog of TestActionDocRequired_IdentifierOneOfParity.)

// modelOneOfViolations checks one action doc against the model_id-OR-task one-of
// resolveModel enforces: both `model_id` and `task` must be documented, and
// NEITHER may be marked Required (resolveModel accepts either arm alone). Mirrors
// identifierOneOfViolations. The "project required when resolving by task" rule is
// a dispatch-ENVELOPE requirement (project sits next to action/params, not inside
// params), not a params `required` flag, so it is out of this gate's scope — it is
// carried in the task param's description.
func modelOneOfViolations(doc ActionDoc) []string {
	var midP, taskP *Param
	for i := range doc.Params {
		switch doc.Params[i].Name {
		case "model_id":
			midP = &doc.Params[i]
		case "task":
			taskP = &doc.Params[i]
		}
	}
	var v []string
	switch {
	case midP == nil:
		v = append(v, "declares no `model_id` param, but resolveModel accepts `model_id`")
	case midP.Required:
		v = append(v, "marks `model_id` Required, but resolveModel accepts `task` alone (one-of, not individually required)")
	}
	switch {
	case taskP == nil:
		v = append(v, "declares no `task` param, but resolveModel accepts `task`")
	case taskP.Required:
		v = append(v, "marks `task` Required, but resolveModel accepts `model_id` alone (one-of, not individually required)")
	}
	return v
}

// TestActionDocRequired_ModelOneOfParity gates ml descriptor Required flags against
// resolveModel's model_id-OR-task enforcement, for every ml action whose handler
// emits the "<action> requires either model_id or task" one-of error.
func TestActionDocRequired_ModelOneOfParity(t *testing.T) {
	reg, err := Load(actionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	enforced := collectModelOneOfEnforcedActions(t, internalRoot(t), []string{"ml"})
	if len(enforced) == 0 {
		t.Fatal(`scanner found no "<action> requires either model_id or task" enforcement — resolveModel's error text or the scanner drifted`)
	}
	covered := 0
	type miss struct {
		action   string
		problems []string
	}
	var misses []miss
	for _, doc := range reg.List("ml") {
		if !enforced[doc.Action] {
			continue
		}
		covered++
		if v := modelOneOfViolations(*doc); len(v) > 0 {
			misses = append(misses, miss{doc.Action, v})
		}
	}
	if covered == 0 {
		t.Fatal("gate vacuous: no resolveModel-enforced action matched an ml registry doc — the action set or surface mapping drifted")
	}
	if len(misses) > 0 {
		t.Errorf("ml action-doc Required flags disagree with resolveModel model_id-OR-task enforcement (%d enforced actions checked):", covered)
		for _, m := range misses {
			for _, p := range m.problems {
				t.Errorf("  %s: %s", m.action, p)
			}
		}
	}
}

// TestActionDocRequired_ModelOneOf_GateCatchesSyntheticDrift proves the model
// one-of gate fires on the drift shape (model_id marked Required + task omitted)
// and does NOT over-fire on the correct one-of shape.
func TestActionDocRequired_ModelOneOf_GateCatchesSyntheticDrift(t *testing.T) {
	drift := ActionDoc{Action: "synthetic_inference", Params: []Param{
		{Name: "model_id", Required: true},
	}}
	if v := modelOneOfViolations(drift); len(v) < 2 {
		t.Errorf("gate failed to flag the drift shape (model_id Required + no task); got violations=%v", v)
	}
	ok := ActionDoc{Action: "synthetic_ok", Params: []Param{
		{Name: "model_id", Required: false},
		{Name: "task", Required: false},
	}}
	if v := modelOneOfViolations(ok); len(v) != 0 {
		t.Errorf("gate over-fires on a correct model_id-OR-task doc: %v", v)
	}
}

// collectModelOneOfEnforcedActions scans non-test .go under the given packages for
// the resolveModel one-of error literal ("<surface>.<action> requires either
// model_id or task"), returning the set of action names whose handler enforces the
// model_id-OR-task one-of (the action name is the suffix after the last "." of the
// "<surface>.<action>" prefix). Mirrors collectIdentifierEnforcedActions' literal
// scan — robust to a future ml convenience action adopting the same one-of message.
func collectModelOneOfEnforcedActions(t *testing.T, internalDir string, pkgs []string) map[string]bool {
	t.Helper()
	const marker = " requires either model_id or task"
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
		dir := filepath.Join(internalDir, pkg)
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, uerr := strconv.Unquote(lit.Value)
				if uerr != nil || !strings.Contains(s, marker) {
					return true
				}
				prefix := strings.TrimSpace(strings.SplitN(s, marker, 2)[0]) // "ml.inference"
				if dot := strings.LastIndex(prefix, "."); dot >= 0 && dot+1 < len(prefix) {
					out[prefix[dot+1:]] = true
				}
				return true
			})
			return nil
		})
	}
	return out
}

// ── Required-vs-enforcement gate: admin plain top-level requireds ────
// admin validates required params via ad-hoc `if p.X == "" { return ...required }`
// guards rather than a declarative schema (bug 943), so the guard's own error
// message — which names its params as `params.<name>` tokens — is the most stable,
// least-dataflow signal of which params are enforced. The five params-bearing CRUD
// handlers (project_register/host_register/host_remove/remote_exec) emit those
// guards; T4 documented their params with Required flags meant to match, and this
// gate pins that match. (Chain finalize-action-docs-epic T2.)
//
// SCOPE (documented, conservative): the enforced set is collected surface-wide by
// param NAME (mirroring param_tag_gate's surface-wide name pooling), not per-action
// dataflow. This is sound on admin because no admin action documents any of the
// enforced names (id/name/hostname/ssh_user/host/cmd) as optional. A future admin
// action that documented an enforced name as legitimately optional would need a
// per-action refinement; that case does not exist today. The gate checks the
// agent-blocking direction (a handler-enforced name documented Required=false) plus
// the presence direction (an enforced name documented Required=true by no action).
// action_describe's required surface/action use a different ("X is required",
// no params. prefix) idiom and are pinned by the net, so they are out of scope here.

// TestActionDocRequired_PlainRequiredParity gates admin descriptor Required flags
// against the `if p.X == ""` handler guards (detected via their `params.X ...
// required` error literals).
func TestActionDocRequired_PlainRequiredParity(t *testing.T) {
	reg, err := Load(actionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	enforced := collectParamsRequiredEnforced(t, internalRoot(t), []string{"admin"})
	if len(enforced) == 0 {
		t.Fatal(`scanner found no "params.X ... required" handler guards in admin — the guard message shape or scanner drifted`)
	}

	type miss struct{ action, name string }
	var misses []miss
	covered := 0
	documentedRequired := map[string]bool{}
	for _, doc := range reg.List("admin") {
		for _, p := range doc.Params {
			if p.Required {
				documentedRequired[p.Name] = true
			}
			if !enforced[p.Name] {
				continue
			}
			covered++
			if !p.Required {
				misses = append(misses, miss{doc.Action, p.Name})
			}
		}
	}
	if covered == 0 {
		t.Fatal("gate vacuous: no admin doc param matched a handler-enforced required name — the scanner or action set drifted")
	}
	if len(misses) > 0 {
		t.Errorf("admin action-doc Required flags disagree with handler `if p.X == \"\"` enforcement (%d enforced params checked):", covered)
		for _, m := range misses {
			t.Errorf("  %s: param %q is documented Required=false but the handler rejects it when absent — mark it Required=true.", m.action, m.name)
		}
	}
	// Presence direction (bug 943): a param the handler rejects when absent must be
	// documented Required=true by at least one admin action.
	for name := range enforced {
		if !documentedRequired[name] {
			t.Errorf("admin handler enforces required param %q but no admin action documents it Required=true (handler→doc presence gap, bug 943)", name)
		}
	}
}

// TestActionDocRequired_PlainRequired_GateCatchesSyntheticDrift proves the plain-
// required gate fires: project_register's id is handler-enforced, so a doc marking
// it Required=false must be flagged while Required=true must not.
func TestActionDocRequired_PlainRequired_GateCatchesSyntheticDrift(t *testing.T) {
	enforced := collectParamsRequiredEnforced(t, internalRoot(t), []string{"admin"})
	if !enforced["id"] || !enforced["host"] || !enforced["cmd"] {
		t.Fatalf("scanner failed to find the admin required-guard params; got enforced=%v", enforced)
	}
	// The drift shape: an enforced name documented Required=false.
	driftDoc := ActionDoc{Action: "synthetic_register", Params: []Param{{Name: "id", Required: false}}}
	flagged := false
	for _, p := range driftDoc.Params {
		if enforced[p.Name] && !p.Required {
			flagged = true
		}
	}
	if !flagged {
		t.Error("gate would not catch an enforced param documented Required=false (the agent-blocking drift)")
	}
	// The correct shape must not be flagged.
	okDoc := ActionDoc{Action: "synthetic_register_ok", Params: []Param{{Name: "id", Required: true}}}
	for _, p := range okDoc.Params {
		if enforced[p.Name] && !p.Required {
			t.Error("gate over-fires: id documented Required=true should satisfy the handler guard")
		}
	}
}

// collectParamsRequiredEnforced scans non-test .go under the given packages for
// the plain-required handler guards' error literals — strings containing "required"
// that name their params as `params.<name>` tokens (e.g. "params.id and params.name
// are required") — returning the set of param NAMES the surface's handlers reject
// when absent. The admin analog of collectIdentifierEnforcedActions / the mcpparam
// binder scanners: a literal-key AST scan, not intra-func dataflow.
func collectParamsRequiredEnforced(t *testing.T, internalDir string, pkgs []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
		dir := filepath.Join(internalDir, pkg)
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, uerr := strconv.Unquote(lit.Value)
				if uerr != nil || !strings.Contains(s, "required") {
					return true
				}
				for _, name := range paramsDotTokens(s) {
					out[name] = true
				}
				return true
			})
			return nil
		})
	}
	return out
}

// paramsDotTokens extracts every `params.<name>` identifier token from a string.
// Used to read the param names a handler's required-guard error message lists.
func paramsDotTokens(s string) []string {
	const pfx = "params."
	var out []string
	for {
		i := strings.Index(s, pfx)
		if i < 0 {
			break
		}
		s = s[i+len(pfx):]
		j := 0
		for j < len(s) && isParamIdentByte(s[j]) {
			j++
		}
		if j > 0 {
			out = append(out, s[:j])
		}
		s = s[j:]
	}
	return out
}

func isParamIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// famSetString renders a family set deterministically for error output.
func famSetString(fams map[string]bool) string {
	out := make([]string, 0, len(fams))
	for f := range fams {
		out = append(out, f)
	}
	// stable order for readable test output
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return fmt.Sprintf("{%s}", strings.Join(out, ", "))
}
