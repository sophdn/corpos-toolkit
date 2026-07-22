// extract.go holds the shape-extractor (T2): given a handler's typed param
// struct, derive each documented-eligible param's name (json tag) + type (the
// Go field kind, mapped to the spec vocabulary). The package doc + the
// reflect-vs-AST rationale live in doc.go.
package actionspec

import (
	"reflect"
	"strings"
)

// Param is one derived param: its wire name (json tag) and its spec-vocabulary
// type. This is the structural half of a documented param; the authored half
// (required / description / alias-of / semantics) lives in the action
// descriptor (T3).
type Param struct {
	JSONName string
	Type     string
}

// Extract returns the ordered structural shape of a param struct: one Param per
// json-tagged exported field, in declaration order. It is the single source of
// each param's TYPE under the contract.
//
// Inclusion rules (a field is an extractable param iff all hold):
//   - exported (reflect can read it, and only exported fields carry json tags
//     that the dispatcher's Unmarshal binds);
//   - has a json tag whose name is neither "" nor "-".
//
// `,omitempty` and other tag options are stripped — only the name is kept.
//
// Boundaries (documented per the T1 contract):
//   - Flat structs only. Anonymous/embedded fields are NOT promoted — no work
//     param struct embeds today; a future surface migration that needs it must
//     extend Extract (and add coverage) deliberately rather than rely on silent
//     promotion. Extract panics on an embedded struct field so the gap is loud,
//     never silent.
//   - Nested structs are a param, not a recursion: a struct/slice-of-struct
//     field maps to object / object[] — its sub-shape is documented in the
//     param's description + example, not split into sibling params.
//
// Extract operates only on structs; a non-struct type returns nil.
func Extract(t reflect.Type) []Param {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []Param
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			panic("actionspec.Extract: embedded field " + f.Name + " in " + t.Name() +
				" — Extract handles flat param structs only; extend it (with coverage) before adding embedding")
		}
		if !f.IsExported() {
			continue
		}
		name, ok := jsonName(f.Tag.Get("json"))
		if !ok {
			continue
		}
		out = append(out, Param{JSONName: name, Type: SpecType(f.Type)})
	}
	return out
}

// jsonName parses a struct field's json tag value, returning the wire name and
// whether the field is an extractable param. ("" / "-" → not a param.)
func jsonName(tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	name := tag
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		name = tag[:comma]
	}
	if name == "" || name == "-" {
		return "", false
	}
	return name, true
}

// SpecType maps a Go field type to the spec type vocabulary that work_actions /
// CallShape render verbatim (and that the corpus generator's docType() bridges
// into the doc-TOML vocabulary). It MUST reproduce the strings the hand-authored
// actionSpecs.Type used, so the contract flip (T4) is byte-identical:
//
//	int* / uint*        → "int64"     (every current id field is int64)
//	string              → "string"
//	bool                → "bool"
//	[]byte / RawMessage → "json"      (a raw-JSON param payload)
//	[]string            → "string[]"
//	[]<anything else>   → "object[]"
//	struct / map / any  → "object"
//
// Pointers are transparent (*string is "string"); a pointer is just an optional
// presentation of its element. Slice-element pointers are likewise transparent
// (`[]*T` families with `[]T`).
func SpecType(ft reflect.Type) string {
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	switch ft.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int64"
	case reflect.Slice:
		elem := ft.Elem()
		if elem.Kind() == reflect.Uint8 {
			// []byte / json.RawMessage — a raw-JSON payload, not a list.
			return "json"
		}
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.String {
			return "string[]"
		}
		return "object[]"
	default:
		// struct / map / interface / anything non-primitive — an object blob
		// whose sub-shape is documented in the param description + example.
		return "object"
	}
}
