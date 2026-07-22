package mcpresult

import (
	"encoding/json"
	"testing"
)

// wire-shape pins for MarshalOkOrError + MarshalOkOrErrorList. These
// snapshots represent the byte-exact JSON that the work package's
// hand-rolled MarshalJSON methods emitted before the migration to
// the helpers in this package. If any snapshot changes, the
// migration is no longer behaviour-preserving.

type sampleBug struct {
	Slug string `json:"slug"`
}

func check(t *testing.T, name string, got []byte, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: marshal failed: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: got %s, want %s", name, got, want)
	}
}

func TestMarshalOkOrError_PointerHappyPath(t *testing.T) {
	got, err := MarshalOkOrError(&sampleBug{Slug: "x"}, nil)
	check(t, "happy", got, err, `{"slug":"x"}`)
}

func TestMarshalOkOrError_PointerNilOK(t *testing.T) {
	var nilBug *sampleBug
	got, err := MarshalOkOrError(nilBug, nil)
	check(t, "nil ok", got, err, `null`)
}

func TestMarshalOkOrError_ErrorPath(t *testing.T) {
	env := &ErrorEnvelope{Error: "bug not found"}
	got, err := MarshalOkOrError[*sampleBug](nil, env)
	check(t, "error", got, err, `{"error":"bug not found"}`)
}

func TestMarshalOkOrError_ErrorWithHints(t *testing.T) {
	env := &ErrorEnvelope{
		Error:  "ambiguous slug",
		Hint:   "supply chain_slug to disambiguate",
		Chains: []string{"chain-a", "chain-b"},
	}
	got, err := MarshalOkOrError[*sampleBug](nil, env)
	check(t, "error+hints", got, err,
		`{"error":"ambiguous slug","hint":"supply chain_slug to disambiguate","chains":["chain-a","chain-b"]}`)
}

func TestMarshalOkOrError_ErrorOmitemptyKeysAbsent(t *testing.T) {
	env := &ErrorEnvelope{Error: "x"}
	got, err := MarshalOkOrError[*sampleBug](nil, env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"hint"`, `"action"`, `"editable_fields"`, `"chains"`} {
		if contains(got, key) {
			t.Errorf("expected key %s absent from envelope; got %s", key, got)
		}
	}
}

type sampleItem struct {
	ID int64 `json:"id"`
}

func TestMarshalOkOrErrorList_HappyPath(t *testing.T) {
	got, err := MarshalOkOrErrorList([]sampleItem{{ID: 1}, {ID: 2}}, nil)
	check(t, "list happy", got, err, `[{"id":1},{"id":2}]`)
}

func TestMarshalOkOrErrorList_NilCoercesToEmptyArray(t *testing.T) {
	got, err := MarshalOkOrErrorList[sampleItem](nil, nil)
	check(t, "nil list", got, err, `[]`)
}

func TestMarshalOkOrErrorList_EmptyCoercesToEmptyArray(t *testing.T) {
	got, err := MarshalOkOrErrorList([]sampleItem{}, nil)
	check(t, "empty list", got, err, `[]`)
}

func TestMarshalOkOrErrorList_ErrorPath(t *testing.T) {
	env := &ErrorEnvelope{Error: "bad params"}
	got, err := MarshalOkOrErrorList[sampleItem]([]sampleItem{{ID: 1}}, env)
	check(t, "list error wins over populated", got, err, `{"error":"bad params"}`)
}

func TestErrorEnvelope_DirectMarshal(t *testing.T) {
	env := ErrorEnvelope{
		Error:          "bad",
		Hint:           "h",
		Action:         "a",
		EditableFields: []string{"f1"},
		Chains:         []string{"c1"},
	}
	got, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"error":"bad","hint":"h","action":"a","editable_fields":["f1"],"chains":["c1"]}`
	if string(got) != want {
		t.Errorf("envelope: got %s, want %s", got, want)
	}
}

func contains(haystack []byte, needle string) bool {
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		if string(haystack[i:i+n]) == needle {
			return true
		}
	}
	return false
}
