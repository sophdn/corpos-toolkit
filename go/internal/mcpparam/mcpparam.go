package mcpparam

import "encoding/json"

// String extracts a string parameter from a JSON-encoded params blob.
// Returns "" when params is nil, malformed, missing the key, or the
// value is the wrong shape. Callers map the empty string to a typed
// error when the field is required.
func String(params json.RawMessage, key string) string {
	if params == nil {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// Int64 extracts an int64 parameter; returns def when params is nil,
// malformed, missing the key, or the value is the wrong shape. No
// coercion — a JSON string "42" returns def, not 42.
func Int64(params json.RawMessage, key string, def int64) int64 {
	if params == nil {
		return def
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return def
	}
	raw, ok := m[key]
	if !ok {
		return def
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return def
	}
	return n
}

// Int64Opt extracts an optional int64 parameter; returns nil when
// params is nil, malformed, missing the key, or the value is the
// wrong shape. Distinct from Int64 because zero is a meaningful
// value for some fields (e.g. an explicit since=0 timestamp).
func Int64Opt(params json.RawMessage, key string) *int64 {
	if params == nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return nil
	}
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil
	}
	return &n
}
