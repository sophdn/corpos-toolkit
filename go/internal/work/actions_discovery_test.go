package work_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"toolkit/internal/actiondocs"
	"toolkit/internal/admin"
	"toolkit/internal/work"
)

// TestWorkActions_ReturnsCatalog covers the happy path: the discovery
// action returns a non-empty list with the expected core entries.
func TestWorkActions_ReturnsCatalog(t *testing.T) {
	resp, err := work.HandleWorkActions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("HandleWorkActions: %v", err)
	}
	if len(resp) == 0 {
		t.Fatal("expected non-empty catalog")
	}
	want := map[string]bool{
		"task_block":    false,
		"task_search":   false,
		"task_blockers": false,
		"roadmap_set":   false,
		"bug_resolve":   false,
		"work_actions":  false, // self-referential entry
	}
	for _, s := range resp {
		if _, named := want[s.Name]; named {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected catalog entry for %q", name)
		}
	}
}

// TestWorkActions_EmptyMarshalsAsArray ensures the empty case wire
// shape stays [] not null (consumers distinguish "no data" from "error"
// by shape).
func TestWorkActions_EmptyMarshalsAsArray(t *testing.T) {
	var empty work.WorkActionsResult
	b, _ := json.Marshal(empty)
	if string(b) != "[]" {
		t.Errorf("empty result marshalled as %s, want []", b)
	}
}

// TestWorkActions_TaskBlockSpecMentionsCrossChainParam covers the
// specific friction this bug exists to retire: cold callers couldn't
// find the cross-chain blocker_chain_slug parameter. The spec must
// name it.
func TestWorkActions_TaskBlockSpecMentionsCrossChainParam(t *testing.T) {
	resp, _ := work.HandleWorkActions(context.Background(), "", nil)
	var taskBlock *work.ActionSpec
	for i, s := range resp {
		if s.Name == "task_block" {
			taskBlock = &resp[i]
			break
		}
	}
	if taskBlock == nil {
		t.Fatal("task_block not in catalog")
	}
	hasBlockerChainSlug := false
	for _, p := range taskBlock.Params {
		if p.Name == "blocker_chain_slug" {
			hasBlockerChainSlug = true
		}
	}
	if !hasBlockerChainSlug {
		t.Errorf("task_block spec missing blocker_chain_slug param — cross-chain blockers won't be discoverable")
	}
}

// TestWorkActions_ChainStateSpecAdvertisesIDParams is the regression guard
// for bug `chain-state-action-doc-omits-id-chain-id-params-handler-accepts`.
// HandleChainState resolves a chain by id/chain_id OR slug (p.resolvedID()
// over getChainByID), but the action-doc historically advertised only the
// slug aliases — so an agent reading action_describe(work, chain_state)
// couldn't discover that {"id":295} is a valid call even though chain_find
// returns ids. The id form must be on the spec. (chain_status, by contrast,
// resolves by slug ONLY, so it correctly omits id — not asserted here.)
func TestWorkActions_ChainStateSpecAdvertisesIDParams(t *testing.T) {
	resp, _ := work.HandleWorkActions(context.Background(), "", nil)
	var chainState *work.ActionSpec
	for i, s := range resp {
		if s.Name == "chain_state" {
			chainState = &resp[i]
			break
		}
	}
	if chainState == nil {
		t.Fatal("chain_state not in catalog")
	}
	found := map[string]bool{"id": false, "chain_id": false}
	for _, p := range chainState.Params {
		if _, ok := found[p.Name]; ok {
			found[p.Name] = true
		}
	}
	for name, ok := range found {
		if !ok {
			t.Errorf("chain_state spec missing %q param — the id form HandleChainState accepts isn't discoverable via action_describe", name)
		}
	}
}

// TestWorkActions_CoversEveryRegisteredAction enforces parity between
// the actionSpecs catalog and the actual dispatch.Table built by
// BuildTable. A new action that lands in BuildTable without an entry
// in actionSpecs is a regression: the discovery surface goes stale,
// re-introducing the bug-1335 condition.
func TestWorkActions_CoversEveryRegisteredAction(t *testing.T) {
	resp, _ := work.HandleWorkActions(context.Background(), "", nil)
	specNames := make(map[string]bool, len(resp))
	for _, s := range resp {
		specNames[s.Name] = true
	}

	// Build a minimal table — Schemas + Pool nil paths are fine; we
	// only care about the set of registered action names. Pool=nil
	// returns early before the bulk of actions register, so we mock a
	// real-ish pool via the test helper.
	pool := openTestPool(t)
	tableDeps := work.TableDeps{Pool: pool, Schemas: nil, Bus: nil}
	table := work.BuildTable(tableDeps)

	missing := []string{}
	for name := range table {
		if !specNames[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("actions registered in BuildTable but missing from actionSpecs catalog: %v\n  Add an ActionSpec entry to actions_discovery.go.", missing)
	}
}

// ── actiondocs(action_describe) ↔ actionSpecs param-shape parity ─────────
// Chain single-source-action-describe T2. The two hand-maintained doc
// systems — admin.action_describe (sourced from the embedded action-docs
// TOML corpus) and actionSpecs (sourced from this package, consumed by
// work_actions + the param-error renderer's CallShape) — must agree on the
// param SHAPE they advertise for every work action. They drifted: task_reopen
// (and ~37 other actions) declared a single required `slug` in TOML while the
// catalog declares id/slug/chain_slug all optional. See
// docs/SINGLE_SOURCE_ACTION_DESCRIBE.md for the T1 inventory + decision.
//
// This test pins the agreement on the OBSERVABLE describe output (it drives
// the real HandleActionDescribe), so it survives T3 single-sourcing the
// overlap fields regardless of where the derivation lives. Pre-T3 it FAILS
// on the current drift; T3 makes it pass by construction.

// paramFamily reduces a param `type` from EITHER vocabulary (actionSpecs'
// int64/object[]/string[] or actiondocs' integer/optional_string/json object)
// to a comparable family, so the gate tolerates vocabulary differences while
// still catching real disagreements (e.g. list documented as string).
func paramFamily(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "string", "optional_string":
		return "string"
	case "integer", "int", "int64":
		return "integer"
	case "bool", "boolean":
		return "bool"
	}
	if strings.HasSuffix(t, "[]") || strings.Contains(t, "list") || strings.Contains(t, "array") {
		return "list"
	}
	if strings.Contains(t, "object") || strings.Contains(t, "json") {
		return "object"
	}
	return "other:" + t
}

// workParamParityMismatches returns the disagreements between the param shape
// admin.action_describe serves (doc) and the actionSpecs catalog entry (spec)
// on the overlap fields. Empty slice == agreement.
//
// The accepted-key SET comparison normalizes the two systems' alias modelling:
// actionSpecs lists every accepted key (canonical + alias) as a flat param;
// actiondocs separates canonical [[params]] from [[param_aliases]]. So the
// describe key set is (canonical param names ∪ param-alias `from` names), and
// it must equal the actionSpecs param-name set. required/type are compared on
// the canonical params only (aliases carry no independent required/type).
func workParamParityMismatches(spec work.ActionSpec, doc *actiondocs.ActionDoc) []string {
	var out []string

	specByName := map[string]work.ActionParam{}
	specKeys := map[string]bool{}
	for _, p := range spec.Params {
		specByName[p.Name] = p
		specKeys[p.Name] = true
	}

	docCanonical := map[string]actiondocs.Param{}
	docKeys := map[string]bool{}
	for _, p := range doc.Params {
		docCanonical[p.Name] = p
		docKeys[p.Name] = true
	}
	for _, a := range doc.ParamAliases {
		docKeys[a.From] = true
	}

	// Accepted-key set agreement.
	var specOnly, docOnly []string
	for k := range specKeys {
		if !docKeys[k] {
			specOnly = append(specOnly, k)
		}
	}
	for k := range docKeys {
		if !specKeys[k] {
			docOnly = append(docOnly, k)
		}
	}
	sort.Strings(specOnly)
	sort.Strings(docOnly)
	if len(specOnly) > 0 {
		out = append(out, fmt.Sprintf("keys in actionSpecs but not served by describe (neither canonical nor alias): %v", specOnly))
	}
	if len(docOnly) > 0 {
		out = append(out, fmt.Sprintf("keys served by describe but absent from actionSpecs: %v", docOnly))
	}

	// required + type family on canonical params present in both.
	var shared []string
	for name := range docCanonical {
		if _, ok := specByName[name]; ok {
			shared = append(shared, name)
		}
	}
	sort.Strings(shared)
	for _, name := range shared {
		sp := specByName[name]
		dp := docCanonical[name]
		if sp.Required != dp.Required {
			out = append(out, fmt.Sprintf("param %q required differs (actionSpecs=%v describe=%v)", name, sp.Required, dp.Required))
		}
		if paramFamily(sp.Type) != paramFamily(dp.Type) {
			out = append(out, fmt.Sprintf("param %q type-family differs (actionSpecs=%q[%s] describe=%q[%s])", name, sp.Type, paramFamily(sp.Type), dp.Type, paramFamily(dp.Type)))
		}
	}

	// Example presence + value.
	specHas := spec.Example != ""
	docExample := ""
	if len(doc.Examples) > 0 {
		docExample = doc.Examples[0].Call
	}
	docHas := docExample != ""
	switch {
	case specHas != docHas:
		out = append(out, fmt.Sprintf("example presence differs (actionSpecs=%v describe=%v)", specHas, docHas))
	case specHas && docHas && spec.Example != docExample:
		out = append(out, fmt.Sprintf("example value differs (actionSpecs=%q describe=%q)", spec.Example, docExample))
	}

	return out
}

// realActionDocs loads the live action-docs corpus embedded in the binary
// (go/internal/actiondocs/corpus) — the same source admin.action_describe
// serves in production, so this parity test exercises the real served docs
// rather than a disk re-read.
func realActionDocs(t *testing.T) *actiondocs.Registry {
	t.Helper()
	reg, err := actiondocs.LoadEmbedded()
	if err != nil {
		t.Fatalf("actiondocs.LoadEmbedded(): %v", err)
	}
	if reg.Len() == 0 {
		t.Fatal("action-docs corpus empty — expected the work chunks")
	}
	return reg
}

// describeServedWorkDoc drives the real admin.action_describe handler for a
// work action and returns the served doc (nil when the corpus has no chunk).
func describeServedWorkDoc(t *testing.T, reg *actiondocs.Registry, action string) *actiondocs.ActionDoc {
	t.Helper()
	deps := admin.Deps{ActionDocs: reg}
	raw := json.RawMessage(fmt.Sprintf(`{"surface":"work","action":%q}`, action))
	res, err := admin.HandleActionDescribe(context.Background(), deps, raw)
	if err != nil {
		t.Fatalf("HandleActionDescribe(%s): %v", action, err)
	}
	return res.Doc
}

// TestActionDescribe_WorkParamsMatchActionSpecs is the parity gate. For every
// actionSpecs entry that also has a TOML chunk, the param shape served by
// admin.action_describe must agree with the catalog on the overlap fields.
func TestActionDescribe_WorkParamsMatchActionSpecs(t *testing.T) {
	// Single-sourcing has landed (chain single-source-action-describe T5/T6):
	// admin.action_describe(work,X) serves the corpus generated from actionSpecs
	// and embedded in the binary, so this gate now passes by construction. It
	// guards against a future hand-edit of the generated corpus or an
	// actionSpecs change shipped without regeneration. See
	// docs/SINGLE_SOURCE_ACTION_DESCRIBE.md.
	reg := realActionDocs(t)
	specs, err := work.HandleWorkActions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("HandleWorkActions: %v", err)
	}

	var failures, specOnlyNoChunk []string
	for _, spec := range specs {
		doc := describeServedWorkDoc(t, reg, spec.Name)
		if doc == nil {
			// Registered action with no action-doc chunk: action_describe
			// can't describe it. Out of scope for the OVERLAP parity (this
			// gate compares in-both actions); surfaced separately below.
			specOnlyNoChunk = append(specOnlyNoChunk, spec.Name)
			continue
		}
		for _, m := range workParamParityMismatches(spec, doc) {
			failures = append(failures, spec.Name+": "+m)
		}
	}
	sort.Strings(failures)
	sort.Strings(specOnlyNoChunk)

	if len(specOnlyNoChunk) > 0 {
		t.Logf("actionSpecs entries with NO action-doc chunk (describe can't serve them): %v", specOnlyNoChunk)
	}
	if len(failures) > 0 {
		t.Errorf("admin.action_describe(work,X) disagrees with actionSpecs on %d point(s):", len(failures))
		for _, f := range failures {
			t.Errorf("  %s", f)
		}
		t.Errorf("Fix: single-source the overlap fields (param shape + example) per docs/SINGLE_SOURCE_ACTION_DESCRIBE.md so describe derives them from actionSpecs.")
	}
}

// TestActionDescribe_WorkParamsParity_CatchesSyntheticDrift proves the gate
// flags the task_reopen-shaped drift rather than passing vacuously, and does
// NOT over-fire on an agreeing shape.
func TestActionDescribe_WorkParamsParity_CatchesSyntheticDrift(t *testing.T) {
	// actionSpecs: id/slug both optional, with an example (the correct shape).
	spec := work.ActionSpec{
		Name: "synthetic_reopen",
		Params: []work.ActionParam{
			{Name: "id", Type: "int64", Required: false},
			{Name: "slug", Type: "string", Required: false},
		},
		Example: `{"id":6326}`,
	}

	// describe-served doc: the task_reopen TOML shape — single required slug,
	// no id, no example.
	drifted := &actiondocs.ActionDoc{
		Action: "synthetic_reopen",
		Params: []actiondocs.Param{{Name: "slug", Type: "string", Required: true}},
	}
	if got := workParamParityMismatches(spec, drifted); len(got) == 0 {
		t.Error("parity gate failed to flag the task_reopen-shaped drift")
	}

	// Agreeing doc: id/slug optional (id documented in the actiondocs `integer`
	// vocabulary), example present + equal. Must produce zero mismatches.
	agree := &actiondocs.ActionDoc{
		Action: "synthetic_reopen",
		Params: []actiondocs.Param{
			{Name: "id", Type: "integer", Required: false},
			{Name: "slug", Type: "string", Required: false},
		},
		Examples: []actiondocs.Example{{Call: `{"id":6326}`}},
	}
	if got := workParamParityMismatches(spec, agree); len(got) != 0 {
		t.Errorf("parity gate over-fires on an agreeing shape: %v", got)
	}
}

// TestSmoke_DescribeWorkActionsCallShapeAgree is the chain
// single-source-action-describe T8 end-to-end smoke. For representative work
// actions, the three consumers of the single source advertise the same param
// set: admin.action_describe (the embedded corpus), work_actions (the
// actionSpecs catalog), and CallShape (the param-error renderer). The
// representatives exercise the drift classes the chain set out to fix:
// task_reopen (the original case — TOML once declared a lone required slug
// while the catalog declares id/slug/chain_slug optional), bug_resolve
// (alias-heavy), and roadmap_set (a list-typed `items` param).
//
// describeServedWorkDoc drives the real handler against realActionDocs =
// actiondocs.LoadEmbedded() — the flagless embedded corpus, no
// --action-docs-dir — so a non-nil served doc is also the flagless-serving
// confirmation at the handler+embed boundary.
func TestSmoke_DescribeWorkActionsCallShapeAgree(t *testing.T) {
	reg := realActionDocs(t)
	specs, err := work.HandleWorkActions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("HandleWorkActions: %v", err)
	}
	byName := map[string]work.ActionSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}

	for _, action := range []string{"task_reopen", "bug_resolve", "roadmap_set"} {
		t.Run(action, func(t *testing.T) {
			spec, ok := byName[action]
			if !ok {
				t.Fatalf("%s absent from the work_actions catalog", action)
			}

			// Flagless describe (embedded corpus) serves full docs.
			doc := describeServedWorkDoc(t, reg, action)
			if doc == nil {
				t.Fatalf("admin.action_describe(work,%s) served no doc from the embedded corpus (flagless serving broken)", action)
			}

			// describe == work_actions on the accepted-key set + required/type/example.
			if ms := workParamParityMismatches(spec, doc); len(ms) > 0 {
				for _, m := range ms {
					t.Errorf("%s: describe vs work_actions disagree: %s", action, m)
				}
			}

			// CallShape == work_actions: CallShape renders from actionSpecs, so
			// every catalog param name must appear in the rendered shape. Guards
			// against a future CallShape refactor silently dropping a param.
			shape := work.CallShape(action)
			for _, p := range spec.Params {
				if !strings.Contains(shape, p.Name) {
					t.Errorf("%s: CallShape omits catalog param %q; shape=%q", action, p.Name, shape)
				}
			}
		})
	}
}

// TestSmoke_TaskReopenIdSlugSingleSourced pins the ORIGINAL drift case
// explicitly: admin.action_describe(work,task_reopen) accepts `id` (integer,
// optional — preferred) alongside `slug`, matching the catalog, rather than
// the pre-chain TOML shape of a single required `slug` with no `id`.
func TestSmoke_TaskReopenIdSlugSingleSourced(t *testing.T) {
	reg := realActionDocs(t)
	doc := describeServedWorkDoc(t, reg, "task_reopen")
	if doc == nil {
		t.Fatal("task_reopen served no doc from the embedded corpus")
	}

	canonical := map[string]actiondocs.Param{}
	for _, p := range doc.Params {
		canonical[p.Name] = p
	}
	accepted := func(name string) bool {
		if _, ok := canonical[name]; ok {
			return true
		}
		for _, a := range doc.ParamAliases {
			if a.From == name {
				return true
			}
		}
		return false
	}

	idP, hasID := canonical["id"]
	if !hasID {
		t.Error("task_reopen describe missing canonical `id` — the original drift was a lone required `slug` with no id")
	} else {
		if idP.Required {
			t.Error("task_reopen `id` should be optional (preferred, not required); describe marks it required")
		}
		if got := paramFamily(idP.Type); got != "integer" {
			t.Errorf("task_reopen `id` should be integer-family; describe type=%q[%s]", idP.Type, got)
		}
	}
	if !accepted("slug") {
		t.Error("task_reopen describe no longer accepts `slug` (canonical or alias)")
	}
}
