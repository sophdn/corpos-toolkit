package actiondocs

import "testing"

// TestTaskBlock_IntParamsDocumentedAsInteger pins bug 888
// (task-block-task-id-blocker-id-documented-string-but-typed-int64).
//
// taskBlockParams types both the blocked-task id (TaskID) and the
// blocker id (BlockerID) as int64, so the dispatcher's typed unmarshal
// rejects a stringified id ("2659") with
//
//	cannot unmarshal string into Go struct field taskBlockParams.task_id of type int64
//
// The action-doc previously rendered task_id as `optional_string` and
// did not surface blocker_id as a first-class param at all (it was
// mentioned only in blocked_by's prose). An agent following the
// documented type therefore passed a string and ate a parse error.
//
// Single-sourcing (chain single-source-action-describe) changed the
// task_id representation: it is now a param-NAME alias of the canonical
// `id` param (work.actionSpecs marks it AliasOf "id"), so the generated
// corpus lists it under [[param_aliases]] rather than [[params]]. The
// int-type guard therefore rides on the canonical `id` param — which the
// blocked-task value actually unmarshals into — plus the documented
// alias edge task_id→id. blocker_id has no alias and stays a first-class
// integer param. The param_type_parity gate (TestActionDocParamTypes_*)
// still pins id / blocker_id against their int64 struct fields.
func TestTaskBlock_IntParamsDocumentedAsInteger(t *testing.T) {
	reg, err := Load(actionDocsRoot(t))
	if err != nil {
		t.Fatalf("load action-docs: %v", err)
	}
	doc, ok := reg.Get("work", "task_block")
	if !ok {
		t.Fatal("work.task_block action-doc not found")
	}

	byName := map[string]Param{}
	for _, p := range doc.Params {
		byName[p.Name] = p
	}

	// The canonical `id` param backs the blocked-task value (int64) — it
	// must render integer so a stringified id is caught.
	if p, ok := byName["id"]; !ok {
		t.Error("work.task_block: canonical id param missing from action-doc")
	} else if p.Type != "integer" {
		t.Errorf("work.task_block: id type = %q, want \"integer\" (struct field ID is int64; a stringified id fails the typed unmarshal)", p.Type)
	}

	// task_id is a param-NAME alias of id (single-sourced). Its int-ness
	// is carried by the canonical id above; pin that the alias edge is
	// documented so a caller using the legacy key still resolves, and that
	// it is NOT re-documented as a first-class (untyped-in-the-alias) param.
	if _, isParam := byName["task_id"]; isParam {
		t.Error("work.task_block: task_id documented as a first-class param; single-sourcing makes it a param_alias of id")
	}
	taskIDAliased := false
	for _, a := range doc.ParamAliases {
		if a.From == "task_id" {
			taskIDAliased = true
			if a.To != "id" {
				t.Errorf("work.task_block: task_id alias To = %q, want \"id\"", a.To)
			}
		}
	}
	if !taskIDAliased {
		t.Error("work.task_block: task_id alias edge missing from param_aliases (struct field TaskID is int64; the legacy key must still resolve to id)")
	}

	// blocker_id maps to taskBlockParams.BlockerID (int64) — must be a
	// first-class param, typed integer. Before the fix it was undocumented
	// (only named in blocked_by's prose), so its int64 type was
	// undiscoverable from the contract.
	if p, ok := byName["blocker_id"]; !ok {
		t.Error("work.task_block: blocker_id missing as a first-class param (struct field BlockerID is int64; it was only mentioned in blocked_by prose)")
	} else if p.Type != "integer" {
		t.Errorf("work.task_block: blocker_id type = %q, want \"integer\"", p.Type)
	}
}
