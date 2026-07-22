package jsonutil

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ScalarToString converts one JSON scalar (number, bool, null, or a
// quoted string the caller passed via a list branch) into its display
// string. Mirrors fmt.Sprint(v) on the prior any-based path: numbers
// emit their canonical decimal form, bools emit "true"/"false", null
// and empty input emit the empty string. Malformed scalars fall back
// to the raw-string-with-quotes-trimmed form (matching the original
// forge/values.go implementation).
func ScalarToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if err := d.Decode(&n); err == nil {
		return n.String()
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return fmt.Sprintf("%t", b)
	}
	return strings.Trim(string(raw), "\"")
}

// SumOptInt64 returns a pointer to the sum of two optional int64s.
// nil + nil = nil; nil + x = x; x + y = x+y. Matches the Rust
// optional-fold telemetry shape used by knowledge handlers for
// cumulative token counts across multi-pass inference calls.
func SumOptInt64(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	var sum int64
	if a != nil {
		sum += *a
	}
	if b != nil {
		sum += *b
	}
	return &sum
}
