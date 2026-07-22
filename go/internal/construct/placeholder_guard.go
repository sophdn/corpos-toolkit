package construct

import (
	"regexp"
	"sort"

	"toolkit/internal/forge/fieldvalue"
)

// placeholderShapeRE matches a whole-value AI-agent placeholder of the
// form `{{NAME}}` (with optional surrounding whitespace). Narrow by
// design: a real value containing a `{{X}}` literal as a substring
// (e.g. "see {{TEMPLATE}} in docs") is NOT a placeholder — it's
// content. Empty `{{}}` is also not matched (no inner identifier).
//
// Suggestion `forge-edit-reject-placeholder-shaped-values-by-default`
// commit message: I sent `{{EXISTING_PROBLEM_STATEMENT_PLACEHOLDER}}`
// as a dry-run probe; the substrate accepted it and wrote the literal
// into bug 1429's problem_statement, requiring manual restore.
var placeholderShapeRE = regexp.MustCompile(`^\s*\{\{[^{}]+\}\}\s*$`)

// LooksLikePlaceholder reports whether s is a whole-value placeholder
// shape per placeholderShapeRE. Used by HandleForgeEdit to short-circuit
// destructive writes from probe-shaped values; also exported as a seam
// for callers building their own field-iteration loops (the construct
// edit path operates on typed pointer-style inputs, not a fieldvalue.FieldValue
// map — `construct.RejectPlaceholderShapedFields` calls this per field
// rather than reconstructing a map just to call FirstPlaceholderShapedField).
func LooksLikePlaceholder(s string) bool {
	return placeholderShapeRE.MatchString(s)
}

// FirstPlaceholderShapedField walks the fields map and returns the
// first (name, offending value, isList) where the value matches the
// placeholder shape. Returns "" "" false if no placeholders found.
// For list-valued fields, returns the first list element that matches.
//
// Iteration order is alphabetic on field name so the same offending
// field surfaces deterministically across runs (Go map iteration is
// randomised; the test suite needs a stable "which field tripped"
// signal).
//
// Exported (chain 311 T7 Stage 3): the construct edit path re-homes
// B-G1 via this seam, matching the §15 plan ("re-home via
// forge.FirstPlaceholderShapedField"). Stage 6 inverts the dependency
// and moves it into construct/ alongside the other seams.
func FirstPlaceholderShapedField(fields map[string]fieldvalue.FieldValue) (name, value string, fromList bool) {
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	// Stable order for deterministic error reporting in tests.
	sort.Strings(names)
	for _, n := range names {
		v := fields[n]
		if !v.Set {
			continue
		}
		if v.IsList {
			for _, item := range v.List {
				if LooksLikePlaceholder(item) {
					return n, item, true
				}
			}
			continue
		}
		if LooksLikePlaceholder(v.Single) {
			return n, v.Single, false
		}
	}
	return "", "", false
}
