package work

import (
	"reflect"
	"testing"

	"toolkit/internal/actionspec"
)

// TestExtract_RealWorkParamStructs asserts the shape-extractor (T2) derives the
// expected ordered {json-name, spec-type} for representative real work param
// structs — covering every type the work surface uses: string, int64, bool,
// *string (taskCompleteParams.HandoffOutput, roadmapUpdateParams pointers),
// *int64 (roadmapInsertParams.Position), and []<struct> (object[]). This is the
// internal test (package work) because the param structs are unexported.
//
// These shapes are the structural half of the contract; T3 merges them with the
// authored descriptor and asserts the full equivalence against the T1 net.
func TestExtract_RealWorkParamStructs(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want []actionspec.Param
	}{
		{
			name: "chainSlugParams (chain_status/chain_state — note id/chain_id present)",
			typ:  reflect.TypeOf(chainSlugParams{}),
			want: []actionspec.Param{
				{JSONName: "slug", Type: "string"},
				{JSONName: "chain", Type: "string"},
				{JSONName: "chain_slug", Type: "string"},
				{JSONName: "id", Type: "int64"},
				{JSONName: "chain_id", Type: "int64"},
			},
		},
		{
			name: "taskBlockParams (the widest identify+blocker struct)",
			typ:  reflect.TypeOf(taskBlockParams{}),
			want: []actionspec.Param{
				{JSONName: "slug", Type: "string"},
				{JSONName: "task_slug", Type: "string"},
				{JSONName: "id", Type: "int64"},
				{JSONName: "task_id", Type: "int64"},
				{JSONName: "chain_slug", Type: "string"},
				{JSONName: "blocker_slug", Type: "string"},
				{JSONName: "blocked_by", Type: "string"},
				{JSONName: "blocker_id", Type: "int64"},
				{JSONName: "blocker_chain_slug", Type: "string"},
				{JSONName: "blocked_by_chain", Type: "string"},
				{JSONName: "reason", Type: "string"},
			},
		},
		{
			name: "taskCompleteParams (*string handoff_output deref'd to string)",
			typ:  reflect.TypeOf(taskCompleteParams{}),
			want: []actionspec.Param{
				{JSONName: "slug", Type: "string"},
				{JSONName: "task_slug", Type: "string"},
				{JSONName: "id", Type: "int64"},
				{JSONName: "task_id", Type: "int64"},
				{JSONName: "chain_slug", Type: "string"},
				{JSONName: "chain", Type: "string"},
				{JSONName: "handoff_output", Type: "string"},
				{JSONName: "commit_sha", Type: "string"},
				{JSONName: "sha", Type: "string"},
			},
		},
		{
			name: "roadmapInsertParams (*int64 position deref'd to int64)",
			typ:  reflect.TypeOf(roadmapInsertParams{}),
			want: []actionspec.Param{
				{JSONName: "ref_kind", Type: "string"},
				{JSONName: "ref_slug", Type: "string"},
				{JSONName: "note", Type: "string"},
				{JSONName: "position", Type: "int64"},
			},
		},
		{
			name: "roadmapUpdateParams (*string note/ref_kind/ref_slug + ,omitempty)",
			typ:  reflect.TypeOf(roadmapUpdateParams{}),
			want: []actionspec.Param{
				{JSONName: "position", Type: "int64"},
				{JSONName: "note", Type: "string"},
				{JSONName: "ref_kind", Type: "string"},
				{JSONName: "ref_slug", Type: "string"},
			},
		},
		{
			name: "roadmapSetParams ([]RoadmapSetInput → object[])",
			typ:  reflect.TypeOf(roadmapSetParams{}),
			want: []actionspec.Param{
				{JSONName: "items", Type: "object[]"},
			},
		},
		{
			name: "trainedModelListParams (mixed string/bool/int64)",
			typ:  reflect.TypeOf(trainedModelListParams{}),
			want: []actionspec.Param{
				{JSONName: "task", Type: "string"},
				{JSONName: "status", Type: "string"},
				{JSONName: "verbose", Type: "bool"},
				{JSONName: "limit", Type: "int64"},
				{JSONName: "offset", Type: "int64"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := actionspec.Extract(c.typ)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Extract mismatch:\n got=%+v\nwant=%+v", got, c.want)
			}
		})
	}
}

// TestExtract_BatchOpsLandedDelta pins the single enumerated type delta the
// contract introduced (docs/ACTION_DOC_CONTRACT.md): BatchParams.Ops is
// []BatchOp, so the extractor derives "object[]". The hand-authored actionSpecs
// catalog historically declared "object" for batch.ops; T4 flipped the
// consumers onto the registry-derived shape, LANDING the "object" → "object[]"
// correction (the one intended output change in the whole chain). Post-flip the
// derived catalog (specByName) and the extractor must agree on "object[]".
func TestExtract_BatchOpsLandedDelta(t *testing.T) {
	shape := actionspec.Extract(reflect.TypeOf(BatchParams{}))
	var opsType string
	for _, p := range shape {
		if p.JSONName == "ops" {
			opsType = p.Type
		}
	}
	if opsType != "object[]" {
		t.Fatalf("extractor derives batch.ops type %q, want object[]", opsType)
	}

	// Post-T4: the derived catalog reflects the landed correction.
	spec, ok := specByName("batch")
	if !ok {
		t.Fatal("no catalog entry for batch")
	}
	var got string
	for _, p := range spec.Params {
		if p.Name == "ops" {
			got = p.Type
		}
	}
	if got != "object[]" {
		t.Fatalf("derived batch.ops type = %q, want object[] (the landed T4 delta)", got)
	}
}
