package construct

import (
	"context"
	"encoding/json"
	"path/filepath"

	"toolkit/internal/forge/registry"
)

// introspect.go is the forge_schema / forge_schemas introspection surface,
// relocated from forge/introspect.go + forge/types.go in chain 311 T7 Stage 6
// P2-C.2 (forge archive). These read-only actions describe the loaded schema
// registry; they have no forge-persistence dependency, so they re-home onto
// construct (which already owns parseParamMap / rawStringParam / routingKeysFor
// in prepare.go). The dispatch host (cmd/toolkit-server) registers them on the
// work table — the surviving introspection affordance once forge archives.

// SchemaSummary is the per-schema row returned by forge_schemas: a stable name
// + the declared supported_ops + a relative source_file path so callers can
// trace which TOML defined the schema.
type SchemaSummary struct {
	Name         string   `json:"name"`
	SupportedOps []string `json:"supported_ops"`
	SourceFile   string   `json:"source_file"`
}

// FieldDetail is one field of a Schema as rendered by forge_schema. Only the
// public introspection-relevant fields are exposed; storage-internal hints
// (RenderAs, TableColumns) are omitted so the wire shape stays stable across
// storage-backend changes.
type FieldDetail struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Description string   `json:"description"`
	EnumValues  []string `json:"enum_values,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
}

// CallEnvelope describes how to construct a call to one supported_op on this
// schema (closes bug 1334: which keys go top-level vs inside `fields`).
type CallEnvelope struct {
	TopLevelRequired []string `json:"top_level_required"`
	TopLevelOptional []string `json:"top_level_optional,omitempty"`
	FieldsLocation   string   `json:"fields_location"`
	FieldsExclusions []string `json:"fields_exclusions,omitempty"`
	Notes            []string `json:"notes,omitempty"`
}

// SchemaDetail is the full forge_schema response: the schema's metadata plus
// its field list plus the per-supported-op call envelopes.
type SchemaDetail struct {
	Name          string                  `json:"name"`
	SupportedOps  []string                `json:"supported_ops"`
	SourceFile    string                  `json:"source_file"`
	Fields        []FieldDetail           `json:"fields"`
	CallEnvelopes map[string]CallEnvelope `json:"call_envelopes,omitempty"`
}

// ForgeSchemaResult is the response shape for HandleForgeSchema — success →
// SchemaDetail object; error → error envelope. The branches are mutually
// exclusive and the custom MarshalJSON picks the right inline struct.
type ForgeSchemaResult struct {
	Detail *SchemaDetail

	Error      string   `json:"-"`
	Name       string   `json:"-"`
	Registered []string `json:"-"`
	Hint       string   `json:"-"`
}

// MarshalJSON emits the success SchemaDetail directly when Error is empty and
// Detail is set; otherwise emits the error envelope (prior wire shape: success
// returns the SchemaDetail fields at the top level, not nested under `detail`).
func (r ForgeSchemaResult) MarshalJSON() ([]byte, error) {
	if r.Error == "" && r.Detail != nil {
		return json.Marshal(r.Detail)
	}
	envelope := struct {
		Error      string   `json:"error,omitempty"`
		Name       string   `json:"name,omitempty"`
		Registered []string `json:"registered,omitempty"`
		Hint       string   `json:"hint,omitempty"`
	}{
		Error:      r.Error,
		Name:       r.Name,
		Registered: r.Registered,
		Hint:       r.Hint,
	}
	return json.Marshal(envelope)
}

// ForgeSchemasResult is the typed slice returned by HandleForgeSchemas. Always
// emits as a JSON array (never an error envelope — degrades to an empty list
// when the registry is absent). MarshalJSON guarantees `[]` not `null`.
type ForgeSchemasResult []SchemaSummary

func (r ForgeSchemasResult) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]SchemaSummary(r))
}

// HandleForgeSchemas implements work.forge_schemas. Returns every loaded
// schema's summary in stable name-order so the response is reproducible.
func HandleForgeSchemas(_ context.Context, deps Deps, _ string, _ json.RawMessage) (ForgeSchemasResult, error) {
	if deps.Schemas == nil {
		return ForgeSchemasResult{}, nil
	}
	entries := deps.Schemas.All()
	out := make(ForgeSchemasResult, 0, len(entries))
	for _, e := range entries {
		out = append(out, SchemaSummary{
			Name:         e.Schema.Meta.Name,
			SupportedOps: copyOps(e.Schema.SupportedOps),
			SourceFile:   relSourceFile(e),
		})
	}
	return out, nil
}

// HandleForgeSchema implements work.forge_schema. Returns the full field list
// for a single named schema, or an error envelope if no such schema exists.
func HandleForgeSchema(_ context.Context, deps Deps, _ string, rawParams json.RawMessage) (ForgeSchemaResult, error) {
	params, err := parseParamMap(rawParams)
	if err != nil {
		return ForgeSchemaResult{}, err
	}
	name := rawStringParam(params, "schema_name")
	if name == "" {
		name = rawStringParam(params, "name")
	}
	if name == "" {
		name = rawStringParam(params, "kind")
	}
	if name == "" {
		return ForgeSchemaResult{Error: "forge_schema requires schema_name (kind / name accepted as aliases)"}, nil
	}
	if deps.Schemas == nil {
		return ForgeSchemaResult{Error: "forge schema registry not loaded (no --blueprints-dir resolved)"}, nil
	}
	entry, ok := deps.Schemas.Entry(name)
	if !ok {
		return ForgeSchemaResult{
			Error:      "schema_not_found",
			Name:       name,
			Registered: deps.Schemas.Names(),
			Hint:       "If you recently added blueprints/forge-schemas/" + name + ".toml, call admin.schema_reload to rescan; the server only loads schemas at startup.",
		}, nil
	}
	fields := make([]FieldDetail, 0, len(entry.Schema.Fields))
	for _, f := range entry.Schema.Fields {
		fields = append(fields, FieldDetail{
			Name:        f.Name,
			Type:        string(f.Type),
			Required:    f.Type.IsRequired(),
			Description: f.Description,
			EnumValues:  copyOps(f.EnumValues),
			Pattern:     f.Pattern,
		})
	}
	return ForgeSchemaResult{
		Detail: &SchemaDetail{
			Name:          entry.Schema.Meta.Name,
			SupportedOps:  copyOps(entry.Schema.SupportedOps),
			SourceFile:    relSourceFile(entry),
			Fields:        fields,
			CallEnvelopes: buildCallEnvelopes(entry.Schema),
		},
	}, nil
}

// buildCallEnvelopes computes the per-supported-op envelopes for a schema (the
// bug-1334 discoverability surface). Routing keys come from the single shared
// routingKeysFor predicate (prepare.go) so the validator + the advertised
// envelope stay in lockstep.
func buildCallEnvelopes(schema registry.Schema) map[string]CallEnvelope {
	envelopes := make(map[string]CallEnvelope, len(schema.SupportedOps))
	for _, op := range schema.SupportedOps {
		env := CallEnvelope{
			TopLevelRequired: []string{"schema_name", "slug"},
			TopLevelOptional: []string{"project"},
		}
		routing := routingKeysFor(schema, op)
		switch op {
		case "create":
			env.FieldsLocation = "object_or_sugar"
			env.Notes = []string{
				"`schema_name` accepts `kind` as an alias",
				"`slug` may be omitted when `title` is present at top level — the slug is derived from the title via slugifyTitle",
				"all schema fields appear in the `fields: {...}` object OR as top-level sugar keys; mutually exclusive shapes (use one or the other per call, not both)",
			}
		case "update":
			env.FieldsLocation = "object_or_sugar"
			if len(routing) > 0 {
				env.TopLevelRequired = append(env.TopLevelRequired, routing...)
				env.FieldsExclusions = append([]string(nil), routing...)
				env.Notes = []string{
					"`chain_slug` lives at the top level for update because slug is unique only within a chain — it disambiguates the target row",
					"`chain_slug` is in the schema field list but excluded from the update fields payload (it's the key, not content)",
					"only the fields being changed need to appear in the fields payload; omitted fields keep their existing values",
				}
			} else {
				env.Notes = []string{
					"only the fields being changed need to appear in the fields payload; omitted fields keep their existing values",
				}
			}
		case "delete":
			env.FieldsLocation = "none"
			if len(routing) > 0 {
				env.TopLevelRequired = append(env.TopLevelRequired, routing...)
				env.Notes = []string{
					"`chain_slug` lives at the top level for delete (same key-disambiguation reason as update)",
				}
			}
		default:
			env.FieldsLocation = "object_or_sugar"
		}
		envelopes[op] = env
	}
	return envelopes
}

// relSourceFile returns the source-file path as `<base(SourceDir)>/<SourceFile>`.
func relSourceFile(e registry.Entry) string {
	if e.SourceDir == "" {
		return e.SourceFile
	}
	return filepath.Join(filepath.Base(e.SourceDir), e.SourceFile)
}

// copyOps returns a fresh slice (callers can't mutate registry-owned data);
// empty input maps to empty output (never nil) to keep wire shape stable.
func copyOps(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
