package mcpparam

import (
	"encoding/json"
	"testing"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestString(t *testing.T) {
	cases := []struct {
		name   string
		params json.RawMessage
		key    string
		want   string
	}{
		{"nil params", nil, "k", ""},
		{"empty object", raw(`{}`), "k", ""},
		{"key absent", raw(`{"other": "v"}`), "k", ""},
		{"key present string", raw(`{"k": "hello"}`), "k", "hello"},
		{"key present empty string", raw(`{"k": ""}`), "k", ""},
		{"key wrong type int", raw(`{"k": 42}`), "k", ""},
		{"key wrong type bool", raw(`{"k": true}`), "k", ""},
		{"key null", raw(`{"k": null}`), "k", ""},
		{"params malformed", raw(`{`), "k", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.params, tc.key)
			if got != tc.want {
				t.Errorf("String(%s, %q) = %q, want %q", tc.params, tc.key, got, tc.want)
			}
		})
	}
}

func TestInt64(t *testing.T) {
	cases := []struct {
		name   string
		params json.RawMessage
		key    string
		def    int64
		want   int64
	}{
		{"nil params returns def", nil, "k", 7, 7},
		{"empty object returns def", raw(`{}`), "k", 7, 7},
		{"key absent returns def", raw(`{"other": 99}`), "k", 7, 7},
		{"key present int", raw(`{"k": 42}`), "k", 7, 42},
		{"key zero is explicit", raw(`{"k": 0}`), "k", 7, 0},
		{"key wrong type string", raw(`{"k": "42"}`), "k", 7, 7},
		{"key wrong type bool", raw(`{"k": true}`), "k", 7, 7},
		// Preserved quirk from the duplicated originals: json.Unmarshal("null", &int64)
		// succeeds with value 0, so explicit null returns 0 (not the supplied default).
		{"key null preserves zero", raw(`{"k": null}`), "k", 7, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Int64(tc.params, tc.key, tc.def)
			if got != tc.want {
				t.Errorf("Int64(%s, %q, %d) = %d, want %d", tc.params, tc.key, tc.def, got, tc.want)
			}
		})
	}
}

func TestInt64Opt(t *testing.T) {
	if got := Int64Opt(nil, "k"); got != nil {
		t.Errorf("nil params: got %v, want nil", got)
	}
	if got := Int64Opt(raw(`{}`), "k"); got != nil {
		t.Errorf("absent key: got %v, want nil", got)
	}
	if got := Int64Opt(raw(`{"k": 42}`), "k"); got == nil || *got != 42 {
		t.Errorf("present int: got %v, want *42", got)
	}
	if got := Int64Opt(raw(`{"k": 0}`), "k"); got == nil || *got != 0 {
		t.Errorf("explicit zero: got %v, want *0", got)
	}
	if got := Int64Opt(raw(`{"k": "42"}`), "k"); got != nil {
		t.Errorf("wrong type string: got %v, want nil", got)
	}
	// Preserved quirk from the duplicated originals: json.Unmarshal("null", &int64)
	// succeeds with value 0, so explicit null returns &0 (not nil).
	if got := Int64Opt(raw(`{"k": null}`), "k"); got == nil || *got != 0 {
		t.Errorf("null preserves zero: got %v, want *0", got)
	}
}
