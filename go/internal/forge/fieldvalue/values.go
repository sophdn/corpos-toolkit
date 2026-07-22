// Package fieldvalue holds forge's leaf shared types: FieldValue (the
// schema-call value container) plus its accessors, caller-input coercion,
// JSON parsing, and the field-level validators. Extracted from package forge
// in chain 311 T7 Stage 6 P2-B (2026-05-29) so the construct layer can depend
// on these types WITHOUT importing forge — the prerequisite for severing the
// construct→forge edge when forge archives at P2-C.
//
// It depends only on forge/registry (schema definitions) + jsonutil, both
// leaves, so it introduces no import cycle. Package forge re-exports every
// symbol here via type aliases + thin forwarders (forge/values.go,
// forge/validate.go), so forge's own code and its ~33-file characterization
// net compile unchanged through the transition.
package fieldvalue

import (
	"encoding/json"
	"fmt"
	"strings"

	"toolkit/internal/forge/registry"
	"toolkit/internal/jsonutil"
)

// FieldValue holds a forge-call value for a single field. Exactly one of
// the two value fields is populated; both empty means "field absent". This
// mirrors the Rust FieldValue enum (Single | List).
type FieldValue struct {
	// Set is true when the caller supplied this field (regardless of
	// whether the value is empty). Distinguishes "absent" from
	// "explicitly empty string" — required for the right MissingRequired
	// vs EmptyRequired error.
	Set    bool
	Single string
	List   []string
	// IsList records whether the caller's encoding was list-shaped. The
	// FieldType validation rules in Rust pattern-match on (FieldType,
	// Single|List); preserving the caller's encoding lets us emit the
	// same TypeMismatch errors.
	IsList bool
}

// SingleValue returns a FieldValue holding a single string.
func SingleValue(s string) FieldValue {
	return FieldValue{Set: true, Single: s}
}

// ListValue returns a FieldValue holding a list of strings.
func ListValue(items []string) FieldValue {
	return FieldValue{Set: true, List: append([]string(nil), items...), IsList: true}
}

// IsEmpty reports whether the value is empty in its native shape.
func (v FieldValue) IsEmpty() bool {
	if v.IsList {
		return len(v.List) == 0
	}
	return v.Single == ""
}

// AsJoined returns the DB-storage form for this field value: list items
// joined on "\n- "; single values pass through unchanged. Used by the
// DB-create path and by string-typed column writes.
func (v FieldValue) AsJoined() string {
	if v.IsList {
		return strings.Join(v.List, "\n- ")
	}
	return v.Single
}

// StringField returns the joined string for the named field, or "" if
// absent. Mirrors Rust forge-lib::values::string_field. (Was the private
// stringField + an exported StringField wrapper in package forge; collapsed
// to one exported impl here — forge keeps both names as forwarders.)
func StringField(fields map[string]FieldValue, name string) string {
	v, ok := fields[name]
	if !ok {
		return ""
	}
	return v.AsJoined()
}

// ListField returns the []string list for the named field, preserving
// the caller's original list shape when the field is `*_or_list`-typed
// + the caller passed a JSON array. When the caller passed a single
// string, CoerceFields will have lifted it to a 1-element list already;
// ListField surfaces that. Returns nil when the field is absent OR the
// list is empty.
//
// Use this for event-payload []string fields (BugReportedPayload.
// AcceptanceCriteria, TaskCreatedPayload.AcceptanceCriteria, etc.) so
// the list-shape preservation that bug
// `forge-time-list-shape-silently-lost-on-bug-acceptance-criteria`
// observed is the default — NOT a per-handler discipline thing.
func ListField(fields map[string]FieldValue, name string) []string {
	v, ok := fields[name]
	if !ok || v.IsEmpty() {
		return nil
	}
	if v.IsList {
		// Copy so the caller can't mutate the registered fields map.
		out := make([]string, len(v.List))
		copy(out, v.List)
		return out
	}
	return []string{v.Single}
}

// CoerceFields applies the same caller-input normalization Rust does
// inside validate_fields before any structural check runs:
//
//   - string_or_list / optional_string_or_list fields supplied as a single
//     string become a 1-element list (or empty list for the empty string).
//   - optional_string fields supplied as a list are joined on ",". The
//     comma form matches the bug surface multi-tag convention so callers
//     can pass either ["a", "b"] or "a,b".
//
// The returned map is a fresh copy; the input is not mutated. (Was the
// private coerceFields in package forge.)
func CoerceFields(schema registry.Schema, fields map[string]FieldValue) map[string]FieldValue {
	out := make(map[string]FieldValue, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	for _, fd := range schema.Fields {
		v, ok := out[fd.Name]
		if !ok {
			continue
		}
		switch fd.Type {
		case registry.FieldTypeStringOrList, registry.FieldTypeOptionalStringOrList:
			if !v.IsList {
				if v.Single == "" {
					out[fd.Name] = FieldValue{Set: true, IsList: true, List: nil}
				} else {
					out[fd.Name] = FieldValue{Set: true, IsList: true, List: []string{v.Single}}
				}
			}
		case registry.FieldTypeOptionalString:
			if v.IsList {
				out[fd.Name] = FieldValue{Set: true, Single: strings.Join(v.List, ",")}
			}
		}
	}
	return out
}

// MalformedField records a field whose raw JSON shape can't coerce to a
// scalar or homogeneous string list. Bug 1398: a chain forge call with
// `tasks: [{slug:…, problem_statement:…}, …]` previously fell through
// ParseFieldValue's mixed-list branch and produced one corrupted row
// instead of a clean rejection. The handler turns Malformed entries
// into a rejection envelope keyed by field name + reason.
type MalformedField struct {
	Name   string
	Reason string
}

// FieldsFromJSON walks a decoded params map and constructs the forge
// fields map. Acceptable JSON shapes per field:
//   - "string"        → SingleValue("string")
//   - ["a","b"]       → ListValue(["a","b"])
//   - other JSON kinds (number, bool, null) are coerced to their string
//     form via ParseFieldValue's number/bool/null branches to match the
//     Rust handler's permissive shape (an agent passing severity=2 should
//     not crash the create).
//
// Reserved keys (schema_name, slug, project, etc.) are skipped — only
// schema-declared field names are extracted. Returns the fields map,
// any unknown-top-level keys, and any malformed shapes (nested
// objects/arrays where a scalar or string-list is expected).
//
// The raw input is keyed by json.RawMessage so the parser can branch on
// JSON shape per field without going through map[string]any. The field
// vocabulary is bounded by registry.FieldType (six variants, all of
// which resolve to string or []string at the value level), so the parse
// shape is closed — see vault
// `reference/2026-05-15_go-mcp-dispatch-typed-returns-pattern.md` for the
// rule against type-aliasing this map to any.
func FieldsFromJSON(schema registry.Schema, raw map[string]json.RawMessage, reserved map[string]struct{}) (map[string]FieldValue, []string, []MalformedField) {
	declared := make(map[string]struct{}, len(schema.Fields))
	for _, f := range schema.Fields {
		declared[f.Name] = struct{}{}
	}
	fields := make(map[string]FieldValue)
	var unknown []string
	var malformed []MalformedField
	for k, v := range raw {
		if _, isReserved := reserved[k]; isReserved {
			continue
		}
		if _, isField := declared[k]; !isField {
			unknown = append(unknown, k)
			continue
		}
		fv, badShape := ParseFieldValue(v)
		if badShape != "" {
			malformed = append(malformed, MalformedField{Name: k, Reason: badShape})
			continue
		}
		fields[k] = fv
	}
	return fields, unknown, malformed
}

// ParseFieldValue resolves one field's raw JSON into a FieldValue. Tries
// string first (the common case for required/optional_string columns),
// then the list shapes, then permissive number/bool coercion (matches
// Rust handler behavior where severity=2 is silently fmt.Sprint'd). A
// null or absent raw decodes to an empty SingleValue with Set=true.
//
// Returns a non-empty `badShape` string (and an empty FieldValue) when
// the JSON is shaped in a way forge can't represent: a top-level object,
// or a list whose elements are themselves objects/arrays. Bug 1398: a
// list-of-objects like `[{...}, {...}]` previously parsed via the
// mixed-list branch and each object got JSON-stringified into one list
// element — silently producing corrupted skeleton rows. The badShape
// path lets the dispatcher reject the call instead. (Was the private
// parseFieldValue in package forge.)
func ParseFieldValue(raw json.RawMessage) (FieldValue, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return SingleValue(""), ""
	}
	// String first — covers the most common path.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return SingleValue(s), ""
	}
	// Homogeneous string list — the schema-correct list shape.
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return ListValue(list), ""
	}
	// Heterogeneous list — fmt.Sprint each item to match the Rust
	// handler's permissive coercion (`["a", 2]` → `["a", "2"]`).
	// Reject when an element is itself a JSON object/array (bug 1398):
	// scalarToString would otherwise return a JSON-stringified blob and
	// downstream skeleton-task / column writes would silently produce
	// corrupted rows.
	var mixed []json.RawMessage
	if err := json.Unmarshal(raw, &mixed); err == nil {
		items := make([]string, 0, len(mixed))
		for i, item := range mixed {
			if shape := DescribeJSONShape(item); shape == "object" || shape == "array" {
				return FieldValue{}, fmt.Sprintf(
					"list element at index %d is a JSON %s; forge fields accept scalars (string/number/bool) or homogeneous string lists, not nested objects/arrays — create child rows individually via a separate forge call",
					i, shape,
				)
			}
			items = append(items, jsonutil.ScalarToString(item))
		}
		return ListValue(items), ""
	}
	// Top-level value isn't string, list, or RawMessage-list — must be
	// an object (or another non-coercible shape). Reject if it's an
	// object/array; fall back to permissive scalar coercion otherwise
	// (numbers, bools, nulls all land here when the leading json.Unmarshal
	// attempts above failed because of e.g. quoting weirdness).
	if shape := DescribeJSONShape(raw); shape == "object" || shape == "array" {
		return FieldValue{}, fmt.Sprintf(
			"value is a JSON %s; forge fields accept scalars or homogeneous string lists, not nested objects/arrays",
			shape,
		)
	}
	return SingleValue(jsonutil.ScalarToString(raw)), ""
}

// DescribeJSONShape returns "object" / "array" / "scalar" / "" based on
// the first non-whitespace byte of raw. Used by ParseFieldValue's
// reject-nested-shapes branches (bug 1398) to name the offending shape
// in the error message without round-tripping through json.Unmarshal.
func DescribeJSONShape(raw json.RawMessage) string {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return "object"
		case '[':
			return "array"
		default:
			return "scalar"
		}
	}
	return ""
}
