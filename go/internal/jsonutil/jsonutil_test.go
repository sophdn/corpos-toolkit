package jsonutil

import (
	"encoding/json"
	"testing"
)

func TestScalarToString(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"null literal", "null", ""},
		{"quoted string", `"hello"`, "hello"},
		{"empty string", `""`, ""},
		{"int", "42", "42"},
		{"negative int", "-7", "-7"},
		{"float", "3.14", "3.14"},
		{"true", "true", "true"},
		{"false", "false", "false"},
		{"malformed", "not-json", "not-json"},
		{"quoted-malformed-trim", `"unterminated`, "unterminated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScalarToString(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("ScalarToString(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func ptr(v int64) *int64 { return &v }

func TestSumOptInt64(t *testing.T) {
	if got := SumOptInt64(nil, nil); got != nil {
		t.Errorf("nil+nil: got %v, want nil", got)
	}
	if got := SumOptInt64(ptr(3), nil); got == nil || *got != 3 {
		t.Errorf("3+nil: got %v, want *3", got)
	}
	if got := SumOptInt64(nil, ptr(5)); got == nil || *got != 5 {
		t.Errorf("nil+5: got %v, want *5", got)
	}
	if got := SumOptInt64(ptr(7), ptr(11)); got == nil || *got != 18 {
		t.Errorf("7+11: got %v, want *18", got)
	}
	if got := SumOptInt64(ptr(0), ptr(0)); got == nil || *got != 0 {
		t.Errorf("0+0: got %v, want *0", got)
	}
}
