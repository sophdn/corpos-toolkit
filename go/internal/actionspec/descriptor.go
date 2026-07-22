// descriptor.go holds the surface-agnostic descriptor → spec machinery for the
// action-doc contract (chain establish-action-doc-contract-on-work, generalized
// to every surface by the per-surface migration chains). It was factored out of
// package work (which originally owned it as action_doc.go) so a second surface
// — knowledge, then measure / admin / ml — can reuse ONE implementation of the
// descriptor types + the derive-merge rather than re-declaring them per package.
//
// The model: each action's param TYPE is DERIVED from its handler's typed param
// struct (via Extract, same package); only the irreducible semantics (purpose,
// param name-list/order/required/description/alias-of, value-aliases, errors,
// notes, envelope-requirements, example, returns) are authored in a co-located
// ActionDoc descriptor. An ordered []ActionEntry registry binds each action name
// to its descriptor + param-struct reflect.Type; DeriveSpecs merges the two into
// the []ActionSpec shape the doc consumers read (work_actions / CallShape on the
// work surface; the corpus generator + admin.action_describe on every surface).
//
// The ActionSpec/ActionParam/… JSON tags here MUST stay byte-identical to the
// shapes work emitted before the move — the work characterization net
// (internal/actiondocs/contract_net_test.go) is the parity oracle. Package work
// now type-aliases these (see internal/work/actions_discovery.go +
// action_doc.go) so every co-located work descriptor + external `work.ActionSpec`
// reference keeps compiling unchanged.

package actionspec

import "reflect"

// ActionParam describes one parameter on an action. AliasOf, when set, marks
// this param as an alias of another param's canonical name (e.g. `sha` is
// AliasOf "commit_sha"). work_actions / CallShape list aliases flat alongside
// canonical params; the corpus generator and admin.action_describe use AliasOf
// to split canonical [[params]] from [[param_aliases]]. Empty AliasOf ==
// canonical param.
type ActionParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`     // "string", "int64", "bool", "string[]", "object", "json"
	Required    bool   `json:"required"` // true if the action errors without it
	Description string `json:"description,omitempty"`
	AliasOf     string `json:"alias_of,omitempty"`
}

// ActionValueAlias normalizes a value form for a specific param (e.g.
// `fix`→`fixed` on resolution_kind). Mirrors actiondocs.ValueAlias so the
// generated corpus round-trips. Param scopes which param the alias applies to.
type ActionValueAlias struct {
	Param string `json:"param"`
	From  string `json:"from"`
	To    string `json:"to"`
	Notes string `json:"notes,omitempty"`
}

// ActionError documents a caller-controlled error the action can return
// (runtime/infra failures are out of scope). Mirrors actiondocs.ErrorCondition.
type ActionError struct {
	Condition string `json:"condition"`
	Message   string `json:"message"`
}

// ActionEnvelopeReq documents a dispatcher-enforced field on the call envelope
// (alongside action/params/project, NOT inside params) — canonically
// `rationale`. Mirrors actiondocs.EnvelopeRequirement.
type ActionEnvelopeReq struct {
	Field               string   `json:"field"`
	Required            bool     `json:"required"`
	Reason              string   `json:"reason,omitempty"`
	AppliesToActorKinds []string `json:"applies_to_actor_kinds,omitempty"`
}

// ActionReturn documents the success-response shape. Mirrors actiondocs.ReturnSpec.
type ActionReturn struct {
	Shape       string `json:"shape,omitempty"`
	Description string `json:"description,omitempty"`
}

// ActionExample is one call example with an optional human description.
// Mirrors actiondocs.Example. The work surface authored at most one example per
// action via the scalar ActionDoc.Example (no description); surfaces that author
// MULTIPLE examples or per-example descriptions (knowledge's parse_context /
// resolve_references) use ActionDoc.Examples instead.
type ActionExample struct {
	Description string `json:"description,omitempty"`
	Call        string `json:"call"`
}

// ActionSpec describes one action's full doc: the param shape + example
// (consumed by work_actions + the param-error renderer's CallShape on the work
// surface) AND the surface documentation fields (consumed by
// admin.action_describe via the generated + embedded corpus on every surface).
// Description is the action's one-line purpose. It is the merged output of an
// ActionEntry — see DeriveSpec.
type ActionSpec struct {
	Name                 string              `json:"name"`
	Description          string              `json:"description"`
	Params               []ActionParam       `json:"params"`
	Example              string              `json:"example,omitempty"`
	Examples             []ActionExample     `json:"examples,omitempty"`
	SeeAlso              string              `json:"see_also,omitempty"`
	ValueAliases         []ActionValueAlias  `json:"value_aliases,omitempty"`
	Errors               []ActionError       `json:"errors,omitempty"`
	Notes                string              `json:"notes,omitempty"`
	EnvelopeRequirements []ActionEnvelopeReq `json:"envelope_requirements,omitempty"`
	Returns              *ActionReturn       `json:"returns,omitempty"`
}

// DocParam is the authored half of one documented param: its name, order
// (position in ActionDoc.Params), required-ness, description, and alias-of. Its
// TYPE is normally DERIVED from the bound param struct's field kind (see
// ActionEntry.ParamStruct) — so Type stays empty for struct-backed params and
// is gate-enforced empty (the per-surface param-tag gate). Type is AUTHORED only
// for a param with no backing struct field (map-bound actions, or a custom-
// UnmarshalJSON alias / a param read outside the struct).
type DocParam struct {
	Name        string
	Required    bool
	Description string
	AliasOf     string
	Type        string // usually "" (derived from the struct); authored only when no struct field backs the param
}

// ActionDoc is the co-located, hand-authored documentation for one action —
// everything the registry cannot read off a param struct. It mirrors ActionSpec
// minus the per-param Type (derived) and minus Name (carried by ActionEntry,
// which pins catalog order).
//
// Example vs Examples: Example is the single-call work_actions/CallShape hint
// (one string, no description); Examples carries MULTIPLE examples and/or per-
// example descriptions for the corpus [[examples]] blocks (knowledge's
// parse_context / resolve_references). When Examples is set, the corpus
// generator uses it; otherwise it falls back to the scalar Example.
type ActionDoc struct {
	Purpose              string // ActionSpec.Description — the action's one-line "what it does"
	Params               []DocParam
	Example              string
	Examples             []ActionExample
	SeeAlso              string
	ValueAliases         []ActionValueAlias
	Errors               []ActionError
	Notes                string
	EnvelopeRequirements []ActionEnvelopeReq
	Returns              *ActionReturn
}

// ActionEntry binds an action name to its co-located descriptor and the param
// struct whose exported json-tagged field kinds supply the derived param types.
// ParamStruct is nil for actions with no typed param struct (map-bound actions,
// no-param actions) — those author their param Types in the descriptor.
type ActionEntry struct {
	Name        string
	Doc         ActionDoc
	ParamStruct reflect.Type
}

// DeriveSpec merges one registry entry into the ActionSpec shape the consumers
// read. Param ORDER comes from the descriptor; param TYPE comes from the struct
// by json-tag lookup when a field backs the param, otherwise from the authored
// DocParam.Type. Struct field order is never load-bearing.
func (e ActionEntry) DeriveSpec() ActionSpec {
	spec := ActionSpec{
		Name:                 e.Name,
		Description:          e.Doc.Purpose,
		Params:               make([]ActionParam, 0, len(e.Doc.Params)),
		Example:              e.Doc.Example,
		Examples:             e.Doc.Examples,
		SeeAlso:              e.Doc.SeeAlso,
		ValueAliases:         e.Doc.ValueAliases,
		Errors:               e.Doc.Errors,
		Notes:                e.Doc.Notes,
		EnvelopeRequirements: e.Doc.EnvelopeRequirements,
		Returns:              e.Doc.Returns,
	}
	var derived map[string]string
	if e.ParamStruct != nil {
		derived = make(map[string]string)
		for _, p := range Extract(e.ParamStruct) {
			derived[p.JSONName] = p.Type
		}
	}
	for _, dp := range e.Doc.Params {
		ap := ActionParam{
			Name:        dp.Name,
			Required:    dp.Required,
			Description: dp.Description,
			AliasOf:     dp.AliasOf,
		}
		if t, ok := derived[dp.Name]; ok {
			ap.Type = t // derived from the bound struct field's kind
		} else {
			ap.Type = dp.Type // authored: no struct field backs this param
		}
		spec.Params = append(spec.Params, ap)
	}
	return spec
}

// DeriveSpecs walks an ordered registry and produces the full catalog the
// consumers read. Order is preserved.
func DeriveSpecs(registry []ActionEntry) []ActionSpec {
	out := make([]ActionSpec, 0, len(registry))
	for _, e := range registry {
		out = append(out, e.DeriveSpec())
	}
	return out
}
