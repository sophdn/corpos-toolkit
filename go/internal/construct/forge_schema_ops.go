package construct

import (
	"fmt"
	"regexp"
	"strings"

	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/forge/registry"
)

// Schema-op gates + partial validation, relocated from forge/edit.go +
// forge/delete.go in chain 311 T7 Stage 6 P2-C.2. These front the
// record-sugar edit/delete dispatch (construct.PrepareForgeEdit /
// PrepareForgeDelete) and were forge-internal helpers before the archive.

// supportsUpdate reports whether the schema declares the update (or edit) op.
// Schemas with no supported_ops declaration default to update-allowed.
func supportsUpdate(s registry.Schema) bool {
	if len(s.SupportedOps) == 0 {
		return true
	}
	for _, op := range s.SupportedOps {
		if op == "update" || op == "edit" {
			return true
		}
	}
	return false
}

// supportsDelete reports whether the schema declares the delete op. Lifecycle-
// owned schemas (chain/task/bug/suggestion) omit it intentionally.
func supportsDelete(s registry.Schema) bool {
	for _, op := range s.SupportedOps {
		if op == "delete" {
			return true
		}
	}
	return false
}

// ValidatePartial validates a partial update payload. Unlike fieldvalue.Validate
// it does not require every required field to be present — only the PROVIDED
// fields are checked. A required field that IS provided must be non-empty;
// type, enum, and pattern checks still apply to whatever fields the caller
// passed. Unknown fields are reported the same way Validate reports them.
func ValidatePartial(schema registry.Schema, fields map[string]fieldvalue.FieldValue) (map[string]fieldvalue.FieldValue, error) {
	var violations []fieldvalue.Violation

	declared := make(map[string]registry.Field, len(schema.Fields))
	for _, f := range schema.Fields {
		declared[f.Name] = f
	}
	for name := range fields {
		if _, ok := declared[name]; !ok {
			violations = append(violations, fieldvalue.Violation{
				Kind:    fieldvalue.ViolationUnknownField,
				Field:   name,
				Message: fmt.Sprintf("unknown field %q", name),
			})
		}
	}

	coerced := fieldvalue.CoerceFields(schema, fields)
	for name, v := range coerced {
		fd, ok := declared[name]
		if !ok {
			continue // unknown — already reported above
		}
		if mismatch := fieldvalue.TypeMismatch(fd, v); mismatch != nil {
			violations = append(violations, *mismatch)
			continue
		}
		if fd.Type.IsRequired() && v.IsEmpty() {
			violations = append(violations, fieldvalue.Violation{
				Kind:    fieldvalue.ViolationEmptyRequired,
				Field:   fd.Name,
				Message: fmt.Sprintf("required field %q cannot be set to empty via forge_edit", fd.Name),
			})
			continue
		}
		if len(fd.EnumValues) > 0 {
			for _, c := range fieldvalue.FlattenEnumCandidates(v) {
				if c == "" {
					continue
				}
				if !fieldvalue.ContainsExact(fd.EnumValues, c) {
					violations = append(violations, fieldvalue.Violation{
						Kind:  fieldvalue.ViolationEnum,
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
			re := regexp.MustCompile(fd.Pattern)
			for _, c := range fieldvalue.FlattenEnumCandidates(v) {
				if c == "" {
					continue
				}
				if !re.MatchString(c) {
					violations = append(violations, fieldvalue.Violation{
						Kind:  fieldvalue.ViolationPattern,
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
		return nil, &fieldvalue.ValidationError{Violations: violations}
	}
	return coerced, nil
}
