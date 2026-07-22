package work

import "strings"

// param_errors.go renders self-describing param-rejection messages from
// the actions_discovery catalog — the single source of truth for every
// action's call shape. Chain quiet-and-instrument-operator-surface T4:
// param-shape rejections used to be hand-written strings ("<action>
// requires slug or id") that forced a separate work_actions / forge_schema
// round-trip to recover the shape — a per-operation tax. Sourcing the
// shape from the descriptor registry here means the rejection self-describes AND there
// is no second param list to drift from the catalog (the AC's "reuse the
// metadata, don't hand-maintain per-error lists").
//
// Ships on the current per-handler structs: each rejection site calls a
// shared helper. The deferred param-resolution-consolidation refactor
// (ideas-to-process) later collapses the call sites themselves; this task
// makes the rendering uniform without waiting on it.

// specByName returns the catalog entry for an action, derived from the
// co-located descriptor registry (action_doc.go).
func specByName(name string) (ActionSpec, bool) {
	for _, s := range deriveActionSpecs() {
		if s.Name == name {
			return s, true
		}
	}
	return ActionSpec{}, false
}

// hasParam reports whether the spec lists a param by name.
func hasParam(spec ActionSpec, name string) bool {
	for _, p := range spec.Params {
		if p.Name == name {
			return true
		}
	}
	return false
}

// CallShape renders an action's accepted params + minimal example from the
// catalog: "Accepted params: a (int64), b (string)* [*=required]. Example:
// {...}". Returns "" for an unknown action. Exported so any handler's
// param-rejection path can append the shape uniformly (identifier-required
// today; missing-required / unknown-param sites can reuse it).
func CallShape(action string) string {
	spec, ok := specByName(action)
	if !ok {
		return ""
	}
	if len(spec.Params) == 0 {
		if spec.Example != "" {
			return "Takes no params. Example: " + spec.Example + "."
		}
		return "Takes no params."
	}
	parts := make([]string, 0, len(spec.Params))
	anyRequired := false
	for _, p := range spec.Params {
		s := p.Name + " (" + p.Type + ")"
		if p.Required {
			s += "*"
			anyRequired = true
		}
		parts = append(parts, s)
	}
	out := "Accepted params: " + strings.Join(parts, ", ")
	if anyRequired {
		out += " [*=required]"
	}
	if spec.Example != "" {
		out += ". Example: " + spec.Example
	}
	return out + "."
}

// IdentifierRequiredError is the shared rejection for the handlers that
// identify a row by id (preferred, an integer) or slug — replacing the
// hand-written "<action> requires slug or id" strings. The id-as-integer
// emphasis + the concrete {"id":6326} form targets the recurring fumble
// where callers pass {"id":"6326"} (string), {"slug":"6326"} (numeric id
// as a slug), or an unrecognised key like {"task":6326} — all of which
// leave both fields empty and trip this rejection with no shape to recover
// from. Appends the catalog CallShape so the slug+chain_slug form and the
// per-action example are visible in the same message. Falls back to a
// static message for actions absent from the catalog.
func IdentifierRequiredError(action string) string {
	spec, ok := specByName(action)
	msg := action + " requires an identifier: pass `id` as an integer (preferred, globally unique), e.g. {\"id\":6326}; or `slug`"
	if ok && hasParam(spec, "chain_slug") {
		msg += " with `chain_slug`"
	}
	msg += "."
	if shape := CallShape(action); shape != "" {
		msg += " " + shape
	}
	return msg
}
