package construct

// compat_fieldvalue.go is the in-package shim that lets the markdown / index /
// bench seams relocated from forge (chain 311 T7 Stage 6 P2-C.2) compile with
// the bare FieldValue vocabulary they were authored against — exactly the shape
// forge's own values.go shim provides over the fieldvalue leaf package (P2-B).
// construct's hand-written code uses fieldvalue.X explicitly; these aliases are
// additive (the underlying type is identical, so the two spellings interoperate)
// and exist only so the byte-identical relocated bodies don't need a mechanical
// FieldValue→fieldvalue.FieldValue rewrite that would obscure their provenance.

import "toolkit/internal/forge/fieldvalue"

// FieldValue aliases the leaf type so relocated bodies keep their bare spelling.
type FieldValue = fieldvalue.FieldValue

// SingleValue / ListValue forward to the leaf constructors.
func SingleValue(s string) FieldValue     { return fieldvalue.SingleValue(s) }
func ListValue(items []string) FieldValue { return fieldvalue.ListValue(items) }

// stringField forwards to the leaf accessor (formerly forge's private
// forwarder over fieldvalue.StringField).
func stringField(fields map[string]FieldValue, name string) string {
	return fieldvalue.StringField(fields, name)
}
