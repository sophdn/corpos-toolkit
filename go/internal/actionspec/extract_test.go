package actionspec

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSpecType_Vocabulary(t *testing.T) {
	type inner struct {
		X int `json:"x"`
	}
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"string", "", "string"},
		{"ptr-string", (*string)(nil), "string"},
		{"int64", int64(0), "int64"},
		{"int", int(0), "int64"},
		{"int32", int32(0), "int64"},
		{"uint64", uint64(0), "int64"},
		{"ptr-int64", (*int64)(nil), "int64"},
		{"bool", false, "bool"},
		{"ptr-bool", (*bool)(nil), "bool"},
		{"string-slice", []string(nil), "string[]"},
		{"ptr-string-slice", []*string(nil), "string[]"},
		{"struct-slice", []inner(nil), "object[]"},
		{"ptr-struct-slice", []*inner(nil), "object[]"},
		{"struct", inner{}, "object"},
		{"map", map[string]any(nil), "object"},
		{"any-slice", []any(nil), "object[]"}, // slice of non-string → object[]
		{"raw-json", json.RawMessage(nil), "json"},
		{"byte-slice", []byte(nil), "json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SpecType(reflect.TypeOf(c.val))
			if got != c.want {
				t.Errorf("SpecType(%T) = %q, want %q", c.val, got, c.want)
			}
		})
	}
}

// TestExtract_FieldSelectionAndOrder pins the field-selection rules in one
// struct: declaration order preserved, ,omitempty stripped, pointer deref'd,
// json:"-" skipped, untagged field skipped.
func TestExtract_FieldSelectionAndOrder(t *testing.T) {
	type sample struct {
		First   string  `json:"first"`
		Second  int64   `json:"second,omitempty"` // option stripped → "second"
		Ptr     *string `json:"third"`            // pointer deref'd → "string"
		Skipped string  `json:"-"`                // explicit skip
		NoTag   string  // untagged → skipped
		Last    bool    `json:"last"`
	}
	got := Extract(reflect.TypeOf(sample{}))
	want := []Param{
		{JSONName: "first", Type: "string"},
		{JSONName: "second", Type: "int64"},
		{JSONName: "third", Type: "string"},
		{JSONName: "last", Type: "bool"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract order/selection:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestExtract_PointerToStruct(t *testing.T) {
	type p struct {
		A string `json:"a"`
	}
	got := Extract(reflect.TypeOf(&p{}))
	want := []Param{{JSONName: "a", Type: "string"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract(*p) = %+v, want %+v", got, want)
	}
}

func TestExtract_NonStructReturnsNil(t *testing.T) {
	if got := Extract(reflect.TypeOf("")); got != nil {
		t.Errorf("Extract(string) = %+v, want nil", got)
	}
}

func TestExtract_EmbeddedFieldPanicsLoudly(t *testing.T) {
	type base struct {
		B string `json:"b"`
	}
	type embeds struct {
		base
		C string `json:"c"`
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("Extract did not panic on an embedded field; the boundary must be loud")
		}
	}()
	Extract(reflect.TypeOf(embeds{}))
}
