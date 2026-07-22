package fieldvalue

import (
	"fmt"
	"regexp"
	"strings"

	"toolkit/internal/forge/registry"
)

// Violation describes one field-level validation failure. Mirrors the
// Rust FieldViolation enum but flattened: Kind classifies the failure
// and Message carries the agent-facing text.
type Violation struct {
	Kind    string
	Field   string
	Message string
}

func (v Violation) Error() string {
	if v.Field != "" {
		return fmt.Sprintf("%s: %s", v.Field, v.Message)
	}
	return v.Message
}

// ValidationError aggregates all field violations from one validate call.
// Implements error; the wrapped Violations slice is exposed so handlers
// can render structured envelopes.
type ValidationError struct {
	Violations []Violation
}

func (v *ValidationError) Error() string {
	if len(v.Violations) == 0 {
		return "validation failed"
	}
	parts := make([]string, 0, len(v.Violations))
	for _, vio := range v.Violations {
		parts = append(parts, vio.Error())
	}
	return strings.Join(parts, "; ")
}

const (
	ViolationUnknownField    = "unknown_field"
	ViolationMissingRequired = "missing_required"
	ViolationEmptyRequired   = "empty_required"
	ViolationTypeMismatch    = "type_mismatch"
	ViolationEnum            = "enum"
	ViolationPattern         = "pattern"
	ViolationMixedEnvelope   = "mixed_envelope"
	// ViolationMalformedField fires when a field's raw JSON shape can't
	// coerce to a scalar or homogeneous string list — e.g. bug 1398's
	// `tasks: [{slug:…,problem_statement:…}, …]` collapsing each object
	// into a JSON-stringified blob. Detected at parse time so the
	// rejection envelope can name the field and the bad shape rather
	// than letting downstream skeleton-task / column writes silently
	// produce corrupted rows.
	ViolationMalformedField = "malformed_field"
	// ViolationPlaceholderShapedValue fires when a forge_edit field's
	// value matches the AI-agent placeholder shape `{{SOMETHING}}` —
	// the literal a caller might use as a dry-run probe. Without this
	// guard, the substrate accepts the placeholder and writes it
	// destructively into the projection (suggestion
	// `forge-edit-reject-placeholder-shaped-values-by-default`).
	// Caller can opt out via `allow_placeholder=true` for tests or
	// edge cases where the literal IS intended.
	ViolationPlaceholderShapedValue = "placeholder_shaped_value"
)

// Validate checks caller-supplied fields against the schema. Mirrors the
// Rust validate_fields contract: every violation is collected (not
// short-circuited) and returned together. On success the returned map is
// the coerced canonical view (string_or_list singles already lifted to
// 1-element lists, optional_string lists joined on ",").
//
// Validations applied:
//   - Unknown field — caller passed a field not declared on the schema.
//   - Missing required field — required field absent.
//   - Empty required field — required field present but empty.
//   - Type mismatch — list/single shape disagrees with FieldType.
//   - Enum violation — value not in EnumValues when EnumValues is non-empty.
//   - Pattern violation — value does not match the declared regex.
//
// Enum and pattern checks extend beyond validate_fields' Rust scope —
// the Rust path leaves enum/pattern enforcement to ad-hoc handler code.
// The chain spec calls them out explicitly under PARITY_STANDARD §1c so
// they live here.
func Validate(schema registry.Schema, fields map[string]FieldValue) (map[string]FieldValue, error) {
	var violations []Violation

	declared := make(map[string]registry.Field, len(schema.Fields))
	for _, f := range schema.Fields {
		declared[f.Name] = f
	}

	// Unknown fields first — flagged before coercion so the caller sees
	// the typed field they actually passed.
	for name := range fields {
		if _, ok := declared[name]; !ok {
			violations = append(violations, Violation{
				Kind:    ViolationUnknownField,
				Field:   name,
				Message: fmt.Sprintf("unknown field %q", name),
			})
		}
	}

	coerced := CoerceFields(schema, fields)

	for _, fd := range schema.Fields {
		v, present := coerced[fd.Name]
		if !present {
			if fd.Type.IsRequired() {
				violations = append(violations, Violation{
					Kind:    ViolationMissingRequired,
					Field:   fd.Name,
					Message: fmt.Sprintf("required field %q is missing", fd.Name),
				})
			}
			continue
		}
		if mismatch := TypeMismatch(fd, v); mismatch != nil {
			violations = append(violations, *mismatch)
			continue
		}
		if fd.Type.IsRequired() && v.IsEmpty() {
			violations = append(violations, Violation{
				Kind:    ViolationEmptyRequired,
				Field:   fd.Name,
				Message: fmt.Sprintf("required field %q is empty", fd.Name),
			})
			continue
		}
		if len(fd.EnumValues) > 0 {
			candidates := FlattenEnumCandidates(v)
			for _, c := range candidates {
				if c == "" {
					continue
				}
				if !ContainsExact(fd.EnumValues, c) {
					violations = append(violations, Violation{
						Kind:  ViolationEnum,
						Field: fd.Name,
						Message: fmt.Sprintf(
							"field %q value %q is not in accepted set: %s",
							fd.Name, c, strings.Join(fd.EnumValues, ", "),
						),
					})
					break
				}
			}
		}
		if fd.Pattern != "" {
			// validate() at load time already proved the regex compiles.
			re := regexp.MustCompile(fd.Pattern)
			for _, c := range FlattenEnumCandidates(v) {
				if c == "" {
					continue
				}
				if !re.MatchString(c) {
					violations = append(violations, Violation{
						Kind:  ViolationPattern,
						Field: fd.Name,
						Message: fmt.Sprintf(
							"field %q value %q does not match pattern %s",
							fd.Name, c, fd.Pattern,
						),
					})
					break
				}
			}
		}
	}

	if len(violations) > 0 {
		return nil, &ValidationError{Violations: violations}
	}
	return coerced, nil
}

func TypeMismatch(fd registry.Field, v FieldValue) *Violation {
	wantList := fd.Type.IsList()
	switch fd.Type {
	case registry.FieldTypeStringOrList, registry.FieldTypeOptionalStringOrList:
		// CoerceFields already lifted singles to lists for these types,
		// so a Single here is a coercion bug — treat as a mismatch.
		if !v.IsList {
			return &Violation{
				Kind:  ViolationTypeMismatch,
				Field: fd.Name,
				Message: fmt.Sprintf(
					"field %q: expected list (single strings auto-coerced), got string",
					fd.Name,
				),
			}
		}
		return nil
	}
	if wantList && !v.IsList {
		return &Violation{
			Kind:    ViolationTypeMismatch,
			Field:   fd.Name,
			Message: fmt.Sprintf("field %q: expected list, got string", fd.Name),
		}
	}
	if !wantList && v.IsList {
		return &Violation{
			Kind:    ViolationTypeMismatch,
			Field:   fd.Name,
			Message: fmt.Sprintf("field %q: expected string, got list", fd.Name),
		}
	}
	return nil
}

func FlattenEnumCandidates(v FieldValue) []string {
	if v.IsList {
		return v.List
	}
	return []string{v.Single}
}

func ContainsExact(items []string, x string) bool {
	for _, it := range items {
		if it == x {
			return true
		}
	}
	return false
}
