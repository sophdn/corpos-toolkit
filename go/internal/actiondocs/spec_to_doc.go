package actiondocs

// spec_to_doc.go projects a derived actionspec.ActionSpec (the registry's merged
// output — see actionspec/descriptor.go) into the ActionDoc TOML/serve shape this
// package owns. It was factored out of cmd/action-docs-corpus-gen so the corpus
// generator AND the per-surface migration equivalence tests share one projection
// rather than each carrying a copy. The generator emits the result as embedded
// TOML; admin.action_describe serves the parsed-back chunk. Keeping the
// projection here (not in the generator's package main) is what lets the
// knowledge/measure/admin/ml migrations assert "registry-derived == current doc
// output" without reimplementing the bridge.

import "toolkit/internal/actionspec"

// SpecToDoc projects one ActionSpec into the ActionDoc shape for the given
// surface. Canonical params (AliasOf=="") become Params; alias params become
// ParamAliases (from=name, to=canonical). The single Example becomes a
// one-element Examples slice. SeeAlso is a work_actions/CallShape-only hint with
// no doc field, so it is intentionally dropped.
func SpecToDoc(surface string, s actionspec.ActionSpec) ActionDoc {
	doc := ActionDoc{
		Surface: surface,
		Action:  s.Name,
		Purpose: s.Description,
		Notes:   s.Notes,
	}
	for _, p := range s.Params {
		if p.AliasOf == "" {
			doc.Params = append(doc.Params, Param{
				Name:        p.Name,
				Type:        docType(p.Type, p.Required),
				Required:    p.Required,
				Description: p.Description,
			})
			continue
		}
		doc.ParamAliases = append(doc.ParamAliases, ParamAlias{
			From: p.Name,
			To:   p.AliasOf,
		})
	}
	for _, va := range s.ValueAliases {
		doc.ValueAliases = append(doc.ValueAliases, ValueAlias{
			Param: va.Param, From: va.From, To: va.To, Notes: va.Notes,
		})
	}
	for _, e := range s.Errors {
		doc.Errors = append(doc.Errors, ErrorCondition{
			Condition: e.Condition, Message: e.Message,
		})
	}
	// Examples (multi + per-example description) win over the single scalar
	// Example; the work surface authors the scalar, knowledge's parse_context /
	// resolve_references author the slice.
	switch {
	case len(s.Examples) > 0:
		for _, ex := range s.Examples {
			doc.Examples = append(doc.Examples, Example{Description: ex.Description, Call: ex.Call})
		}
	case s.Example != "":
		doc.Examples = []Example{{Call: s.Example}}
	}
	for _, er := range s.EnvelopeRequirements {
		doc.EnvelopeRequirements = append(doc.EnvelopeRequirements, EnvelopeRequirement{
			Field:               er.Field,
			Required:            er.Required,
			Reason:              er.Reason,
			AppliesToActorKinds: er.AppliesToActorKinds,
		})
	}
	if s.Returns != nil {
		doc.Returns = &ReturnSpec{Shape: s.Returns.Shape, Description: s.Returns.Description}
	}
	return doc
}

// docType maps an actionspec param type to the action-doc TOML type vocabulary.
// The doc vocabulary only splits strings into required (`string`) vs optional
// (`optional_string`); integer/bool/object carry their optionality in the
// separate `required` flag. int64 MUST document as `integer` so the
// param_type_parity gate (which compares the doc type family against the handler
// struct field kind) stays green. Slice types collapse to `list` (string[]) /
// `object[]` — both land in the parity gate's ungated object bucket.
func docType(specType string, required bool) string {
	switch specType {
	case "string":
		if required {
			return "string"
		}
		return "optional_string"
	case "int64":
		return "integer"
	case "bool":
		return "bool"
	case "string[]":
		return "list"
	case "object[]":
		return "object[]"
	case "object", "json":
		return "object"
	default:
		return specType
	}
}
