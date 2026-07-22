package construct

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/forge/registry"
)

// forgeReserved is the strip-list HandleForge passes to fieldvalue.FieldsFromJSON on
// the sugar shape. Single-sourced from registry.ReservedTopLevelKeys so
// the load-time collision warning and the runtime strip-list never drift.
//
// `title` is intentionally NOT in this set: the bug schema declares
// `title` as a real field. Slug auto-derivation reads `title` before sugar
// collection; fieldvalue.FieldsFromJSON then picks it up as a schema field for
// shapes that declare it. Mirrors the Rust FORGE_RESERVED list verbatim.
var forgeReserved = registry.ReservedTopLevelKeys

// ForgePrep is the validated, ready-to-persist output of PrepareForge: the
// agent's create envelope decoded, the schema resolved, every field validated
// — but NO DB write has happened yet. forge's own persistence
// (ExecutePreparedCreate) consumes it; so does the Stage-4 adapter in
// cmd/toolkit-server, which routes the event-sourced covered schemas through
// construct.Create and the §15 delta schemas through ExecutePreparedCreate.
// The exported fields are exactly what an adapter needs to build a typed
// construct.Input; `envelope` stays unexported because only forge's own hint
// rendering (FinalizeForgeCreate) reads it.
type ForgePrep struct {
	Schema               registry.Schema
	SchemaName           string
	Slug                 string
	Validated            map[string]fieldvalue.FieldValue
	ChainTaskEntries     []ChainTaskEntry
	ChainTasksWerePeeled bool
	envelope             envelopeShape
}

// PrepareForge runs the parse → schema-lookup → project-gate → field-extract →
// validate → chain-tasks-peel pipeline that fronts every forge create, WITHOUT
// performing the write. It returns:
//   - (prep, nil, nil) when the envelope is well-formed and validated;
//   - (zero, &rejection, nil) when the call is malformed/invalid — the rejection
//     is the exact agent-facing ForgeCreateResult HandleForge would have returned;
//   - (zero, nil, err) on a genuine infrastructure error.
//
// Factored out of HandleForge for T7 Stage 4 (chain 311): an adapter calls
// PrepareForge once, then routes the covered event-sourced schemas through
// construct.Create and the delta schemas through ExecutePreparedCreate — both
// reusing FinalizeForgeCreate's notifier+nudge+envelope tail so the
// agent-visible behavior is identical regardless of the persistence path.
func PrepareForge(deps Deps, project string, rawParams json.RawMessage) (ForgePrep, *ForgeCreateResult, error) {
	if deps.Schemas == nil {
		return ForgePrep{}, nil, errors.New("forge: schemas registry not configured")
	}

	params, err := parseParamMap(rawParams)
	if err != nil {
		return ForgePrep{}, nil, fmt.Errorf("forge: parse params: %w", err)
	}

	schemaName := rawStringParam(params, "schema_name")
	if schemaName == "" {
		schemaName = rawStringParam(params, "kind") // alias used by some skills
	}
	if schemaName == "" {
		return ForgePrep{}, &ForgeCreateResult{Error: missingRequiredEnvelope("schema_name")}, nil
	}

	slug := rawStringParam(params, "slug")
	titleConsumedForSlug := false
	if slug == "" {
		if title := rawStringParam(params, "title"); title != "" {
			slug = slugifyTitle(title)
			titleConsumedForSlug = true
		}
	}
	if slug == "" {
		return ForgePrep{}, &ForgeCreateResult{Error: missingRequiredEnvelope("slug")}, nil
	}

	schema, ok := deps.Schemas.Get(schemaName)
	if !ok {
		return ForgePrep{}, &ForgeCreateResult{
			Error:      "schema_not_found",
			SchemaName: schemaName,
			Registered: deps.Schemas.Names(),
			Hint:       "If you recently added blueprints/forge-schemas/" + schemaName + ".toml, call admin.schema_reload to rescan; the server only loads schemas at startup.",
		}, nil
	}

	// Project-required gate. Cross-project schemas (e.g. vault-note,
	// marked via [schema].cross_project = true) are exempt — the top-level
	// `project` is DB-attribution-only for them and defaults to a
	// sentinel when empty.
	if project == "" && !schemaIsCrossProject(schema) {
		return ForgePrep{}, &ForgeCreateResult{
			Error: fmt.Sprintf(
				"forge requires a project for schema '%s' (project-scoped); "+
					"none could be resolved",
				schemaName,
			),
			Hint: projectResolutionHint("forge"),
		}, nil
	}

	if !supportsCreate(schema) {
		return ForgePrep{}, &ForgeCreateResult{
			Error:      "schema does not support create",
			SchemaName: schemaName,
		}, nil
	}

	if rejection, hit := rejectDoubleDatedSlug(schema, schemaName, slug); hit {
		return ForgePrep{}, &rejection, nil
	}

	// Strip `title` from the sugar-shape param map when it was consumed
	// only for slug derivation (i.e. the caller didn't pass an explicit
	// slug) AND the schema doesn't declare `title` as a real field.
	// Without this, schemas like `chain` reject the same call that just
	// successfully derived a slug, because `title` survives into the
	// unknown-keys check below. Schemas that DO declare `title` (bug,
	// vault-note) keep the value so it lands in the field map.
	if titleConsumedForSlug && !schemaDeclaresField(schema, "title") {
		delete(params, "title")
	}

	// T7 of work-batching-and-forge-templates: forge(chain) can accept
	// full-object task entries in `tasks`. parseFieldValue rejects
	// list-of-objects shape (bug 1398), so peel chain.tasks BEFORE
	// extractFields runs, parse via the typed entry parser, and synthesize
	// a pipe-delimited representation for the validator's eyes. The parsed
	// entries flow into createChain via the hook context's ChainTasks field;
	// the after_create hook chainInsertTaskSkeletonsFromField uses them
	// (rich payloads for full-object entries; existing skeleton path for
	// pipe-delimited entries).
	var chainTaskEntries []ChainTaskEntry
	chainTasksWerePeeled := false
	if schemaName == "chain" {
		entries, picked, peelErr := peelChainTasksFromParams(params)
		if peelErr != nil {
			return ForgePrep{}, &ForgeCreateResult{
				Error:      peelErr.Error(),
				SchemaName: schemaName,
				Field:      "tasks",
				Message:    peelErr.Error(),
				Hint:       "tasks entries are either pipe-delimited strings (slug|scope|status) OR full task objects ({slug, problem_statement, acceptance_criteria, context_required, constraints, rationale}); a single tasks list MAY mix both. Per-task rationale is required on every full-object entry.",
			}, nil
		}
		chainTaskEntries = entries
		chainTasksWerePeeled = picked
		// Pipe-delimited chain tasks are DEPRECATED + REJECTED (chain 311 T7
		// Stage 6 P2-C.2, user decision): construct.ChainWithTasksInput models
		// only full task objects (per-task rationale required), and the forge
		// pipe-skeleton fan-out archived with forge. A pipe entry now rejects
		// with a clear migration message rather than silently falling back.
		if picked {
			for _, e := range entries {
				if e.Mode == ChainTaskModePipe {
					return ForgePrep{}, &ForgeCreateResult{
						Error:      "pipe-mode chain tasks are no longer supported",
						SchemaName: schemaName,
						Field:      "tasks",
						Message:    fmt.Sprintf("chain task %q uses the deprecated pipe-delimited shape (slug|scope|status)", e.Slug),
						Hint:       "pass full task objects instead: {slug, problem_statement, acceptance_criteria, context_required, constraints, rationale}. The pipe-delimited tasks shape was removed when forge archived (chain 311 T7 Stage 6).",
					}, nil
				}
			}
		}
		if picked {
			// Synthesize a pipe-delimited string list for the validator.
			// Full-object entries get a placeholder (slug|<placeholder>|pending)
			// so the validator sees a homogeneous string list; the chain
			// hook ignores Fields["tasks"] when ChainTasks is populated.
			synthesized := make([]string, 0, len(entries))
			for _, e := range entries {
				if e.Mode == ChainTaskModeFull {
					synthesized = append(synthesized, e.Slug+"|"+truncateTo(e.ProblemStatement, 120)+"|pending")
				} else {
					synthesized = append(synthesized, e.Slug+"|"+e.Scope+"|"+e.Status)
				}
			}
			synthRaw, _ := json.Marshal(synthesized)
			placeKeyInParams(params, "tasks", synthRaw)
		}
	}

	// Two equivalent param shapes (mirror Rust):
	//   structured: {schema_name, slug, fields: {<name>: <value>, ...}}
	//   sugar:      {schema_name, slug, <field-name>: <value>, ...}
	// create op has no extra routing keys — chain_slug (when present)
	// is a real payload field on the task schema's create path.
	fields, unknown, malformed, envelope, err := extractFields(schema, params, nil)
	if err != nil {
		var mixedErr *mixedEnvelopeError
		if errors.As(err, &mixedErr) {
			return ForgePrep{}, &ForgeCreateResult{
				Error:            mixedErr.Error(),
				SchemaName:       schemaName,
				Kind:             fieldvalue.ViolationMixedEnvelope,
				Message:          mixedErr.Error(),
				DetectedEnvelope: envelope.String(),
				SeenAtTopLevel:   mixedErr.TopLevelKeys,
				SeenInFields:     mixedErr.FieldsKeys,
				Hint:             "Pick one envelope: either move all schema fields into fields:{}, or drop fields:{} and pass them all at top level. The two shapes are mutually exclusive per call.",
			}, nil
		}
		return ForgePrep{}, nil, err
	}
	// Malformed-shape rejection runs BEFORE the unknown-field gate and
	// the "fields nil" envelope so a list-of-objects on a real field
	// surfaces the specific reason rather than getting masked by a
	// generic "needs fields or sugar" reply (bug 1398).
	if len(malformed) > 0 {
		bad := malformed[0]
		msg := fmt.Sprintf("field %q has a shape forge can't store: %s", bad.Name, bad.Reason)
		return ForgePrep{}, &ForgeCreateResult{
			Error:      msg,
			SchemaName: schemaName,
			Field:      bad.Name,
			Kind:       fieldvalue.ViolationMalformedField,
			Message:    msg,
			Hint:       malformedFieldHint(schemaName, bad.Name),
		}, nil
	}
	if fields == nil {
		return ForgePrep{}, &ForgeCreateResult{
			Error:        "forge requires either a `fields` object or top-level schema-field-named params",
			SchemaName:   schemaName,
			SchemaFields: fieldNames(schema),
			Hint:         "Pass `fields: {<name>: <value>, ...}` OR put each field at top level (e.g. `output: \"...\", design_decisions: \"...\"`).",
		}, nil
	}

	// Silent-drop B10 gate: unknown fields rejected with the canonical
	// message shape. The Rust handler emits the offending key plus the
	// alphabetised accepted list — match the format verbatim.
	if len(unknown) > 0 {
		// Bug 933: a stray `op` is the diagnostic case — forge_schema advertises
		// create+update envelopes per schema, which reads as "forge takes
		// op=update", but the `forge` action only ever creates. Give it a
		// dedicated message pointing at the real update/delete actions instead of
		// the generic "unknown param; accepted: <fields>" reply (whose field list
		// is irrelevant to an op key).
		if containsKey(unknown, "op") {
			return ForgePrep{}, &ForgeCreateResult{
				Error:      "forge has no `op` parameter — the `forge` action always creates. To UPDATE an existing row use forge_edit; to DELETE one use forge_delete.",
				SchemaName: schemaName,
				Hint:       "forge_schema advertises both a create and an update call-envelope per schema, but `forge` itself only creates — the update/delete envelopes are served by the forge_edit / forge_delete actions.",
			}, nil
		}
		accepted := fieldNames(schema)
		sort.Strings(accepted)
		// Report only the first unknown key to match Rust's error message
		// shape; callers retrying with the suggested set see the rest on
		// the next round.
		return ForgePrep{}, &ForgeCreateResult{
			Error: fmt.Sprintf(
				"unknown param %q on action 'forge'; accepted: %s",
				unknown[0],
				strings.Join(accepted, ", "),
			),
		}, nil
	}

	// Auto-injection: if a schema literally declares a field named
	// `project`, inject the dispatcher's top-level project value into
	// it so callers using the sugar shape don't have to repeat
	// themselves. Top-level `project` is otherwise stripped by
	// forgeReserved on the sugar shape.
	//
	// Chain `forge-vault-note-schema-rework` (T2 decision file
	// `decisions/2026-05-20_vault-note-scope-vs-project-split.md`) removed
	// vault-note's `project` field — renamed to `scope` — so this
	// block no longer fires for vault-note. The cross-project semantics
	// are now carried by [schema].cross_project, not by the field name.
	// No live schema in the registry currently declares a `project`
	// field; the block is preserved for any future schema that
	// legitimately wants the same auto-injection pattern.
	if project != "" && schemaDeclaresField(schema, "project") {
		if _, ok := fields["project"]; !ok {
			fields["project"] = fieldvalue.SingleValue(project)
		}
	}

	validated, err := fieldvalue.Validate(schema, fields)
	if err != nil {
		if field, kind, msg, ok := firstValidationViolation(err); ok {
			res := ForgeCreateResult{
				Error:   msg,
				Field:   field,
				Kind:    kind,
				Message: msg,
			}
			if kind == fieldvalue.ViolationMissingRequired {
				res.DetectedEnvelope = envelope.String()
				res.Hint = missingRequiredHint(envelope, field)
			}
			return ForgePrep{}, &res, nil
		}
		return ForgePrep{}, nil, err
	}

	return ForgePrep{
		Schema:               schema,
		SchemaName:           schemaName,
		Slug:                 slug,
		Validated:            validated,
		ChainTaskEntries:     chainTaskEntries,
		ChainTasksWerePeeled: chainTasksWerePeeled,
		envelope:             envelope,
	}, nil, nil
}

// ForgeEditPrep carries the parsed + validated state shared between
// HandleForgeEdit, HandleForgeEditInTx, and the T7 Stage 4 adapter. Built
// once by PrepareForgeEdit, consumed by every edit entry point so the
// validation logic lives in one place. Exported fields are what an adapter
// needs to route the validated edit through construct.Update.
type ForgeEditPrep struct {
	SchemaName string
	Schema     registry.Schema
	Slug       string
	ChainSlug  string
	Validated  map[string]fieldvalue.FieldValue
	DropExtras []string
}

// PrepareForgeEdit runs every param-validation step common to
// HandleForgeEdit and HandleForgeEditInTx — parse, schema lookup, project
// gate, supportsUpdate, field extract, B-G1 placeholder guard, ValidatePartial.
// Returns (prep, nil, nil) on success, (zero, &rejection, nil) when validation
// surfaces a caller-visible error envelope, or (zero, nil, err) for internal
// failures the caller should propagate. Set-by (B-ED2) rejection happens later
// (forge's EditDBInTx / construct's RejectSetByEditFields), NOT here.
func PrepareForgeEdit(deps Deps, project string, rawParams json.RawMessage) (ForgeEditPrep, *ForgeEditResult, error) {
	if deps.Schemas == nil {
		return ForgeEditPrep{}, nil, errors.New("forge_edit: schemas registry not configured")
	}
	params, err := parseParamMap(rawParams)
	if err != nil {
		return ForgeEditPrep{}, nil, fmt.Errorf("forge_edit: parse params: %w", err)
	}

	schemaName := rawStringParam(params, "schema_name")
	if schemaName == "" {
		schemaName = rawStringParam(params, "kind")
	}
	if schemaName == "" {
		return ForgeEditPrep{}, &ForgeEditResult{Error: missingRequiredEnvelope("schema_name")}, nil
	}
	slug := rawStringParam(params, "slug")
	if slug == "" {
		return ForgeEditPrep{}, &ForgeEditResult{Error: missingRequiredEnvelope("slug")}, nil
	}
	chainSlug := rawStringParam(params, "chain_slug")
	dropExtras := rawStringListParam(params, "__drop_extras")

	schema, ok := deps.Schemas.Get(schemaName)
	if !ok {
		return ForgeEditPrep{}, &ForgeEditResult{
			Error:      "schema_not_found",
			SchemaName: schemaName,
			Registered: deps.Schemas.Names(),
			Hint:       "If you recently added blueprints/forge-schemas/" + schemaName + ".toml, call admin.schema_reload to rescan; the server only loads schemas at startup.",
		}, nil
	}
	if project == "" && !schemaIsCrossProject(schema) {
		return ForgeEditPrep{}, &ForgeEditResult{
			Error: fmt.Sprintf(
				"forge_edit requires a project for schema '%s' (project-scoped); "+
					"none could be resolved",
				schemaName,
			),
			Hint: projectResolutionHint("forge_edit"),
		}, nil
	}
	if !supportsUpdate(schema) {
		return ForgeEditPrep{}, &ForgeEditResult{
			Error:        "schema does not support update",
			SchemaName:   schemaName,
			SupportedOps: schema.SupportedOps,
		}, nil
	}

	extraReserved := routingKeysFor(schema, "update")
	extraReserved = append(extraReserved, "allow_placeholder", "__drop_extras")
	fields, unknown, malformed, envelope, err := extractFields(schema, params, extraReserved)
	if err != nil {
		var mixedErr *mixedEnvelopeError
		if errors.As(err, &mixedErr) {
			return ForgeEditPrep{}, &ForgeEditResult{
				Error:            mixedErr.Error(),
				SchemaName:       schemaName,
				Kind:             fieldvalue.ViolationMixedEnvelope,
				Message:          mixedErr.Error(),
				DetectedEnvelope: envelope.String(),
				SeenAtTopLevel:   mixedErr.TopLevelKeys,
				SeenInFields:     mixedErr.FieldsKeys,
				Hint:             "Pick one envelope: either move all schema fields into fields:{}, or drop fields:{} and pass them all at top level. The two shapes are mutually exclusive per call.",
			}, nil
		}
		return ForgeEditPrep{}, nil, err
	}
	if len(malformed) > 0 {
		bad := malformed[0]
		msg := fmt.Sprintf("field %q has a shape forge can't store: %s", bad.Name, bad.Reason)
		return ForgeEditPrep{}, &ForgeEditResult{
			Error:      msg,
			SchemaName: schemaName,
			Field:      bad.Name,
			Kind:       fieldvalue.ViolationMalformedField,
			Message:    msg,
			Hint:       malformedFieldHint(schemaName, bad.Name),
		}, nil
	}
	if fields == nil && len(dropExtras) == 0 {
		return ForgeEditPrep{}, &ForgeEditResult{
			Error:        "forge_edit requires either a `fields` object or top-level schema-field-named params",
			SchemaName:   schemaName,
			SchemaFields: fieldNames(schema),
		}, nil
	}
	if fields == nil {
		fields = map[string]fieldvalue.FieldValue{}
	}
	if len(unknown) > 0 {
		accepted := fieldNames(schema)
		sort.Strings(accepted)
		return ForgeEditPrep{}, &ForgeEditResult{
			Error: fmt.Sprintf(
				"unknown param %q on action 'forge_edit'; accepted: %s",
				unknown[0],
				strings.Join(accepted, ", "),
			),
		}, nil
	}
	if !rawBoolParam(params, "allow_placeholder") {
		if name, value, _ := FirstPlaceholderShapedField(fields); name != "" {
			msg := fmt.Sprintf(
				"field %q value %q looks like an AI-agent placeholder (whole-value `{{NAME}}` shape); writing it would destructively overwrite the existing content",
				name, value,
			)
			return ForgeEditPrep{}, &ForgeEditResult{
				Error:      msg,
				SchemaName: schemaName,
				Field:      name,
				Kind:       fieldvalue.ViolationPlaceholderShapedValue,
				Message:    msg,
				Hint:       "If you intended a dry-run, no forge_edit dry-run exists yet — read the current value first via the appropriate work-tool action (e.g., bug_read / chain_state). If the literal IS the intended new value, pass `allow_placeholder: true` to bypass this guard.",
			}, nil
		}
	}
	validated, err := ValidatePartial(schema, fields)
	if err != nil {
		if field, kind, msg, ok := firstValidationViolation(err); ok {
			res := &ForgeEditResult{
				Error:   msg,
				Field:   field,
				Kind:    kind,
				Message: msg,
			}
			if kind == fieldvalue.ViolationMissingRequired {
				res.DetectedEnvelope = envelope.String()
				res.Hint = missingRequiredHint(envelope, field)
			}
			return ForgeEditPrep{}, res, nil
		}
		return ForgeEditPrep{}, nil, err
	}

	return ForgeEditPrep{
		SchemaName: schemaName,
		Schema:     schema,
		Slug:       slug,
		ChainSlug:  chainSlug,
		Validated:  validated,
		DropExtras: dropExtras,
	}, nil, nil
}

// ForgeDeletePrep is the validated, ready-to-execute output of PrepareForgeDelete:
// the schema resolved, project gate + supportsDelete passed — but no row deleted
// yet. Exported fields are what an adapter needs to route the actual delete
// through construct.Delete (T7 Stage 4).
type ForgeDeletePrep struct {
	SchemaName string
	Schema     registry.Schema
	Slug       string
	ChainSlug  string
}

// PrepareForgeDelete runs forge_delete's parse → schema-lookup → project-gate →
// supportsDelete-reject pipeline WITHOUT performing the delete. Returns
// (prep, nil, nil) when the schema supports delete and the call is well-formed,
// (zero, &rejection, nil) for the canonical reject envelopes (missing
// schema_name/slug, id-not-slug hint, schema_not_found, project gate, and the
// dominant case: a lifecycle-owned schema naming its soft_delete_action), or
// (zero, nil, err) on a genuine infrastructure error. The Stage-4 adapter reuses
// this so the rejection envelope stays byte-identical when the delete itself
// routes through construct.Delete.
func PrepareForgeDelete(deps Deps, project string, rawParams json.RawMessage) (ForgeDeletePrep, *ForgeDeleteResult, error) {
	if deps.Schemas == nil {
		return ForgeDeletePrep{}, nil, errors.New("forge_delete: schemas registry not configured")
	}
	params, err := parseParamMap(rawParams)
	if err != nil {
		return ForgeDeletePrep{}, nil, fmt.Errorf("forge_delete: parse params: %w", err)
	}
	schemaName := rawStringParam(params, "schema_name")
	if schemaName == "" {
		schemaName = rawStringParam(params, "kind")
	}
	if schemaName == "" {
		return ForgeDeletePrep{}, &ForgeDeleteResult{Error: missingRequiredEnvelope("schema_name")}, nil
	}
	slug := rawStringParam(params, "slug")
	if slug == "" {
		// Bug 1399: a caller passing `id` (the unambiguous handle for
		// db-shaped rows) gets a generic "slug is required" error and
		// has no path forward. The canonical id-by-default flows are
		// task_cancel / bug_resolve / chain_close — name them so the
		// next agent doesn't have to discover this empirically. The
		// schema isn't loaded yet here (we don't know the lifecycle
		// soft_delete_action), so the hint enumerates the common ones.
		if rawParamPresent(params, "id") {
			return ForgeDeletePrep{}, &ForgeDeleteResult{
				Error: "forge_delete requires `slug` (not `id`). For chain/task/bug rows use the lifecycle action — task_cancel, bug_resolve, chain_close — each accepts `id`. For other schemas resolve id→slug first (e.g. via the parent forge_list / read action).",
				Hint:  "If you have an id you reached for forge_delete to clean up a malformed row: pass it to task_cancel/bug_resolve directly. forge_delete is the lower-level escape hatch for slug-keyed schemas that don't have a soft-delete lifecycle action.",
			}, nil
		}
		return ForgeDeletePrep{}, &ForgeDeleteResult{Error: missingRequiredEnvelope("slug")}, nil
	}
	chainSlug := rawStringParam(params, "chain_slug")
	schema, ok := deps.Schemas.Get(schemaName)
	if !ok {
		return ForgeDeletePrep{}, &ForgeDeleteResult{
			Error:      "schema_not_found",
			SchemaName: schemaName,
			Registered: deps.Schemas.Names(),
			Hint:       "If you recently added blueprints/forge-schemas/" + schemaName + ".toml, call admin.schema_reload to rescan; the server only loads schemas at startup.",
		}, nil
	}
	if project == "" && !schemaIsCrossProject(schema) {
		return ForgeDeletePrep{}, &ForgeDeleteResult{
			Error: fmt.Sprintf(
				"forge_delete requires a project for schema '%s' (project-scoped); "+
					"none could be resolved",
				schemaName,
			),
			Hint: projectResolutionHint("forge_delete"),
		}, nil
	}
	if !supportsDelete(schema) {
		msg := fmt.Sprintf("schema %q does not support deletion", schemaName)
		hint := ""
		if alt := schema.Lifecycle.SoftDeleteAction; alt != "" {
			msg = fmt.Sprintf("schema %q does not support deletion; use action=%q for soft-cancellation", schemaName, alt)
			hint = fmt.Sprintf("call %s instead — this schema's state transitions are owned by lifecycle actions, not forge_delete", alt)
		}
		return ForgeDeletePrep{}, &ForgeDeleteResult{
			Error:        msg,
			SchemaName:   schemaName,
			SupportedOps: schema.SupportedOps,
			Hint:         hint,
		}, nil
	}
	return ForgeDeletePrep{SchemaName: schemaName, Schema: schema, Slug: slug, ChainSlug: chainSlug}, nil, nil
}

// schemaIsCrossProject reports whether the schema is cross-project —
// i.e. exempt from the project-required gate and from any auto-injection
// of the dispatcher's top-level project into schema fields. Driven by
// [schema].cross_project = true in the TOML.
//
// Replaces the prior schemaDeclaresProject heuristic ("schema has a field
// named project"), which conflated two concerns: gate-exemption (still
// needed) and field-auto-injection (deliberately removed for vault-note;
// see chain `forge-vault-note-schema-rework` closure).
func schemaIsCrossProject(s registry.Schema) bool {
	return s.Meta.CrossProject
}

// schemaDeclaresField reports whether the schema declares a field named
// `name`. Used by the title-as-slug-derivation strip in HandleForge so
// schemas that don't have `title` as a real field don't reject the
// derived-slug case.
func schemaDeclaresField(s registry.Schema, name string) bool {
	for _, f := range s.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

func supportsCreate(s registry.Schema) bool {
	if len(s.SupportedOps) == 0 {
		return true // schemas with no declaration default to create-allowed
	}
	for _, op := range s.SupportedOps {
		if op == "create" {
			return true
		}
	}
	return false
}

func fieldNames(s registry.Schema) []string {
	out := make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		out = append(out, f.Name)
	}
	return out
}

// parseParamMap decodes a forge call's raw params into a flat field map
// keyed by top-level name with each value held as json.RawMessage so the
// field-parsing code can dispatch on JSON shape per declared FieldType
// rather than via a generic `any` switch. Empty or nil rawParams returns
// an empty (non-nil) map.
func parseParamMap(rawParams json.RawMessage) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(rawParams) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(rawParams, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// rawParamPresent reports whether the top-level params map has a
// non-null value under key, regardless of JSON type. Used by the
// forge_delete id-without-slug detector (bug 1399) so a caller passing
// `id` as either a JSON number or a JSON string hits the same routing
// hint — rawStringParam would silently miss the number form.
func rawParamPresent(params map[string]json.RawMessage, key string) bool {
	raw, ok := params[key]
	if !ok || len(raw) == 0 {
		return false
	}
	return string(raw) != "null"
}

// rawStringParam returns the string-typed top-level param under key, or
// "" if absent or non-string. Mirrors the prior stringParam helper that
// operated on map[string]any; this version operates on the typed
// RawMessage map.
func rawStringParam(params map[string]json.RawMessage, key string) string {
	raw, ok := params[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// rawBoolParam returns the bool-typed top-level param under key, or
// false if absent or non-bool. Accepts the JSON literals true/false
// AND the string forms "true"/"false"/"1"/"0"/"yes"/"no"/"on"/"off"
// (case-insensitive) so callers using a JSON-typed transport and
// callers using string-typed query params both work uniformly.
func rawBoolParam(params map[string]json.RawMessage, key string) bool {
	raw, ok := params[key]
	if !ok || len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on":
			return true
		}
	}
	return false
}

// rawStringListParam returns the string-list-typed top-level param under
// key, or nil if absent or unparseable. Accepts a JSON array of strings
// (`["a","b"]`) OR a single string (`"a"`) for one-element lists — the
// same permissive shape forge.fieldvalue.FieldsFromJSON parses for optional-string-
// or-list fields. Used by the `__drop_extras` meta-param on forge_edit.
func rawStringListParam(params map[string]json.RawMessage, key string) []string {
	raw, ok := params[key]
	if !ok || len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}

// envelopeShape is the inferred shape of a forge caller's params, used
// to disambiguate validation errors (bug 1378). Returned alongside the
// extracted fields so missing_required / mixed_envelope rejections can
// name which envelope the validator was inspecting.
type envelopeShape int

const (
	envelopeNone envelopeShape = iota
	envelopeFields
	envelopeSugar
	envelopeMixed
)

func (s envelopeShape) String() string {
	switch s {
	case envelopeFields:
		return "fields-object"
	case envelopeSugar:
		return "top-level-sugar"
	case envelopeMixed:
		return "mixed"
	default:
		return ""
	}
}

// mixedEnvelopeError carries the colliding key samples a caller needs to
// fix a mixed-shape call. Surfaced via the typed result envelope, not
// thrown — handler converts to the user-visible rejection.
type mixedEnvelopeError struct {
	TopLevelKeys []string
	FieldsKeys   []string
}

func (e *mixedEnvelopeError) Error() string {
	return fmt.Sprintf(
		"mixed envelope: schema fields at top level (%s) AND inside fields:{} (%s) — use one shape, not both",
		strings.Join(e.TopLevelKeys, ", "),
		strings.Join(e.FieldsKeys, ", "),
	)
}

// extractFields runs the two-shape forge param dispatch: prefer the
// structured `fields: {...}` block when present, otherwise treat the
// flat top-level params (minus forgeReserved) as schema fields. Returns
// (nil, nil, nil, envelopeNone, nil) when neither shape produced any
// fields and no unknown keys were seen — caller emits the "needs fields
// or sugar" envelope. Returns a *mixedEnvelopeError when schema-named
// fields appear in BOTH locations. The malformed slot carries fields
// whose raw JSON shape failed parseFieldValue's scalar/list constraints
// (bug 1398) — the caller turns those into a rejection envelope.
func extractFields(schema registry.Schema, params map[string]json.RawMessage, extraReserved []string) (map[string]fieldvalue.FieldValue, []string, []fieldvalue.MalformedField, envelopeShape, error) {
	if rawFields, ok := params["fields"]; ok {
		nested, err := decodeNestedFields(rawFields)
		if err != nil {
			return nil, nil, nil, envelopeFields, fmt.Errorf("parse `fields` block: %w", err)
		}
		// Mixed-envelope detection: any schema field also at top level
		// is a contract violation (the validator silently ignores it).
		// extraReserved widens the "ignore at top level" set with the
		// per-op routing keys from routingKeysFor — chain_slug for
		// forge_edit on a task-table schema, etc. (bug 1412).
		if mixed := mixedEnvelopeKeys(schema, params, nested, extraReserved); mixed != nil {
			return nil, nil, nil, envelopeMixed, mixed
		}
		fields, unknown, malformed := fieldvalue.FieldsFromJSON(schema, nested, nil)
		// Bug 933: on the fields-envelope path the unknown set above covers only
		// keys INSIDE fields:{}; unknown TOP-LEVEL keys (chiefly a stray `op`,
		// which `forge` has no routing for — update/delete are forge_edit/
		// forge_delete) were never inspected, so they fell through silently and
		// the call did a create. Surface them as unknown too (top-level first so
		// the op-specific hint in HandleForge fires) instead of dropping them.
		if unknownTop := unknownTopLevelKeys(schema, params, extraReserved); len(unknownTop) > 0 {
			unknown = append(unknownTop, unknown...)
		}
		return fields, unknown, malformed, envelopeFields, nil
	}
	fields, unknown, malformed := fieldvalue.FieldsFromJSON(schema, params, forgeReserved)
	if len(fields) == 0 && len(unknown) == 0 && len(malformed) == 0 {
		return nil, nil, nil, envelopeNone, nil
	}
	return fields, unknown, malformed, envelopeSugar, nil
}

// projectResolutionHint composes the hint shown when forge / forge_edit /
// forge_delete reach the project-required gate with an empty resolved
// project. Names the three resolution paths the dispatcher walks
// (top-level param → CWD match against registered project_paths →
// --default-project flag) so the caller can self-correct without
// re-reading docs. The action name is plumbed so the example matches
// the verb the caller actually invoked.
//
// Read handlers and the non-create writes (bug_resolve, task_complete,
// etc.) tolerate an empty resolved project by design; the forge
// create/edit/delete trio is the surface that genuinely needs one
// because the row carries project_id as a real column.
func projectResolutionHint(action string) string {
	return "project resolution paths tried, in order: " +
		"(1) top-level `project` parameter on the call envelope — supply one, e.g. {\"project\": \"mcp-servers\"}; " +
		"(2) CWD match against registered project_paths (admin.project_register {id: ..., path: ...} attaches the path to a project so future calls auto-resolve); " +
		"(3) the server's --default-project flag (set at startup). " +
		"All three returned empty for this call. " +
		"Note: the top-level `project` accompanies the action on the meta-tool envelope — sibling to `action` and `params`, NOT a key inside `params`. " +
		"Example: mcp__toolkit-server__work({action: \"" + action + "\", project: \"mcp-servers\", params: {...}})"
}

// malformedFieldHint composes a tailored "try this" line for the common
// malformed-field cases the dispatcher knows about — chiefly the chain
// schema's `tasks` field, which is a string-or-list of pipe-delimited
// skeleton entries (NOT inline task records). Bug 1398: this is the
// shape agents reach for first when authoring a chain+tasks bundle, and
// the generic "value is a JSON array" reason isn't actionable enough.
func malformedFieldHint(schemaName, fieldName string) string {
	if schemaName == "chain" && fieldName == "tasks" {
		return "chain forge accepts `tasks` as either a string or a list of strings — each entry is pipe-delimited: `slug-description | scope | pending`. To create a chain with fully-populated task rows, pass `tasks = []` (or omit it) and then forge each task individually: forge(kind=task, chain_slug=<chain-slug>, slug=…, problem_statement=…, acceptance_criteria=…)."
	}
	return ""
}

// missingRequiredHint composes a one-line "try this" message for a
// missing_required violation, naming the envelope the validator was
// inspecting so the caller knows where to put the field. Empty string
// when the envelope is indeterminate (envelopeNone) — happens only when
// no schema-named keys were passed at all, in which case the upstream
// "needs fields or sugar" envelope already fires.
func missingRequiredHint(envelope envelopeShape, field string) string {
	switch envelope {
	case envelopeFields:
		return fmt.Sprintf("validator inspected fields:{} (because you passed a `fields` block) — add %q to fields:{}, or drop fields:{} entirely and pass every field as top-level sugar instead", field)
	case envelopeSugar:
		return fmt.Sprintf("validator inspected top-level sugar keys (no `fields` block present) — add %q at top level, or switch to passing every field inside fields:{}", field)
	default:
		return ""
	}
}

// routingKeysFor returns the schema's per-op routing-key list in
// stable order. Routing keys are identity keys that must appear at
// the top level of the call envelope AND are excluded from the fields
// payload (when the op has one). For task-table schemas, chain_slug
// is the routing key on update + delete — it disambiguates the
// (chain_slug, slug) composite key. Returns nil when the op has no
// schema-specific routing keys; callers treat nil as "the base
// envelope [schema_name, slug] is complete on its own".
//
// Single source of truth: this is the ONLY place the per-op routing
// semantic lives. Both the runtime validator (mixedEnvelopeKeys via
// extractFields, bug 1412) and the introspection envelope (introspect.go's
// buildCallEnvelopes, which drives admin.action_describe and
// forge_schema responses) read from this helper, so the spec advertised
// to callers and the runtime gate that enforces it can't disagree —
// closes the cleanup follow-on filed after bug 1412.
//
// Adding a new routing-key shape (e.g. library_slug on a hypothetical
// chain-scoped library schema) is a single-site edit here.
func routingKeysFor(schema registry.Schema, op string) []string {
	storage := schema.ResolvedStorage()
	isTaskTable := storage.Target == registry.StorageTargetDB && storage.Table == "tasks"
	switch op {
	case "update", "delete":
		if isTaskTable {
			return []string{"chain_slug"}
		}
	}
	return nil
}

// mixedEnvelopeKeys returns a *mixedEnvelopeError naming sample keys
// from each envelope when the caller passed schema fields BOTH at top
// level AND inside fields:{}. Returns nil when only one shape carries
// schema fields. Reserved top-level keys (slug, schema_name, project,
// etc.) don't count even when they happen to match a schema field name
// — they're the forge dispatch layer's reserved namespace.
func mixedEnvelopeKeys(schema registry.Schema, topLevel map[string]json.RawMessage, nested map[string]json.RawMessage, extraReserved []string) *mixedEnvelopeError {
	declared := make(map[string]struct{}, len(schema.Fields))
	for _, f := range schema.Fields {
		declared[f.Name] = struct{}{}
	}
	extraSet := make(map[string]struct{}, len(extraReserved))
	for _, k := range extraReserved {
		extraSet[k] = struct{}{}
	}
	var topHits []string
	for k := range topLevel {
		if _, isReserved := forgeReserved[k]; isReserved {
			continue
		}
		if _, isExtraReserved := extraSet[k]; isExtraReserved {
			// Per-op routing key (e.g. chain_slug for forge_edit on a
			// task-table schema) — top-level by design and excluded
			// from the fields payload. Don't flag it as a schema field
			// at top level (bug 1412).
			continue
		}
		if _, isField := declared[k]; isField {
			topHits = append(topHits, k)
		}
	}
	if len(topHits) == 0 {
		return nil
	}
	var nestedHits []string
	for k := range nested {
		if _, isField := declared[k]; isField {
			nestedHits = append(nestedHits, k)
		}
	}
	if len(nestedHits) == 0 {
		return nil
	}
	sort.Strings(topHits)
	sort.Strings(nestedHits)
	return &mixedEnvelopeError{TopLevelKeys: topHits, FieldsKeys: nestedHits}
}

// containsKey reports whether keys contains target. Small local helper for the
// op-specific unknown-key diagnosis (bug 933).
func containsKey(keys []string, target string) bool {
	for _, k := range keys {
		if k == target {
			return true
		}
	}
	return false
}

// unknownTopLevelKeys returns the top-level param keys that are neither a forge
// dispatch key (forgeReserved — schema_name/slug/project/fields/…), nor a per-op
// routing key (extraReserved — chain_slug / __drop_extras / allow_placeholder for
// forge_edit), nor a declared schema field. On the fields-envelope path these are
// silently dropped today (only the fields:{} block is unknown-checked), which let
// a stray `op` fall through to a create (bug 933). Schema-field-named keys at top
// level are NOT reported here — when they collide with fields:{} the
// mixedEnvelopeKeys gate owns that diagnosis; this helper deliberately covers only
// the never-valid-at-top-level keys. The result is sorted for deterministic error
// output.
func unknownTopLevelKeys(schema registry.Schema, topLevel map[string]json.RawMessage, extraReserved []string) []string {
	declared := make(map[string]struct{}, len(schema.Fields))
	for _, f := range schema.Fields {
		declared[f.Name] = struct{}{}
	}
	extra := make(map[string]struct{}, len(extraReserved))
	for _, k := range extraReserved {
		extra[k] = struct{}{}
	}
	var unknown []string
	for k := range topLevel {
		if k == "fields" {
			continue
		}
		if _, isReserved := forgeReserved[k]; isReserved {
			continue
		}
		if _, isExtraReserved := extra[k]; isExtraReserved {
			continue
		}
		if _, isField := declared[k]; isField {
			continue // a schema field at top level is mixedEnvelopeKeys' concern
		}
		unknown = append(unknown, k)
	}
	sort.Strings(unknown)
	return unknown
}

// decodeNestedFields unwraps a `fields: {...}` RawMessage into the
// per-field RawMessage map the parser consumes. A null or absent value
// returns an empty map; any other top-level shape is rejected so a caller
// passing `fields: "string"` gets a parse error rather than silent drop.
func decodeNestedFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// datePrefixedSlug matches a leading YYYY-MM-DD followed by either `_`
// or `-` on a slug. Both separators are organic — humans + agents
// reading existing on-disk filenames produce both shapes. Used by the
// double-date guard for schemas whose filename pattern auto-prepends
// today's date adjacent to the slug.
var datePrefixedSlug = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[_-]`)

// rejectDoubleDatedSlug returns a populated ForgeCreateResult and hit=true
// when the slug carries a YYYY-MM-DD_ prefix AND the schema's filename
// pattern would adjacently render the date next to the slug. Mirrors the
// vault-note bug 1348 case where a slug like `2026-05-17_foo` rendered
// as `.../2026-05-17_2026-05-17_foo.md` because the pattern is
// `{subdir}/{date}_{slug}.md` — double-dating the on-disk filename.
//
// The rejection is the cheaper alternative to silently stripping the
// prefix (which would change the canonical slug behind the caller's
// back); the error names the corrected slug so callers retry once and
// move on.
func rejectDoubleDatedSlug(schema registry.Schema, schemaName, slug string) (ForgeCreateResult, bool) {
	if !datePrefixedSlug.MatchString(slug) {
		return ForgeCreateResult{}, false
	}
	if !filenamePatternAdjacentDateSlug(schema) {
		return ForgeCreateResult{}, false
	}
	cleaned := slug[len("YYYY-MM-DD_"):]
	return ForgeCreateResult{
		Error: fmt.Sprintf(
			"slug %q starts with a date prefix, but schema %q's filename_pattern auto-prepends today's date — the rendered filename would double-date",
			slug, schemaName,
		),
		SchemaName: schemaName,
		Hint:       fmt.Sprintf("retry with slug=%q; the date is added automatically by the filename_pattern", cleaned),
	}, true
}

// CheckDoubleDatedSlug is the exported, envelope-free form of
// rejectDoubleDatedSlug for the record construction layer (T7 §15 / chain
// record-layer-stage2-additive-remainder, B-G2): it returns rejected=true with
// a message + corrected-slug hint when slug carries a YYYY-MM-DD prefix AND the
// schema's filename_pattern would adjacently re-prepend the date (the only live
// such schema is vault-note's {subdir}/{date}_{slug}.md). Reuses the same
// datePrefixedSlug regex + filenamePatternAdjacentDateSlug check forge's create
// dispatch uses, so the layer can't drift from forge's guard.
func CheckDoubleDatedSlug(schema registry.Schema, schemaName, slug string) (rejected bool, message, hint string) {
	res, hit := rejectDoubleDatedSlug(schema, schemaName, slug)
	if !hit {
		return false, "", ""
	}
	return true, res.Error, res.Hint
}

// filenamePatternAdjacentDateSlug reports whether the schema's rendered
// filename pattern places `{date}` immediately before `{slug}` — the
// adjacency that produces double-dating when the caller's slug already
// starts with a date. Checks both the [storage] block and the legacy
// [schema] block (some schemas duplicate the pattern across both).
func filenamePatternAdjacentDateSlug(s registry.Schema) bool {
	if s.Storage != nil && strings.Contains(s.Storage.FilenamePattern, "{date}_{slug}") {
		return true
	}
	return strings.Contains(s.Meta.FilenamePattern, "{date}_{slug}")
}

var slugifySanitize = regexp.MustCompile(`[^a-z0-9]+`)

// slugifyTitle lowercases, collapses non-alphanumeric runs to `-`,
// strips leading/trailing dashes, and caps length at 80 chars. When
// the cap falls mid-word, the slug is truncated at the last `-`
// boundary within the window so it ends on a complete word.
// SlugifyTitle is the exported form of [slugifyTitle] — the canonical
// title→slug derivation. Exposed so the forge-v2 record construction layer
// (chain emit-surface-forge-v2 T7) derives slugs IDENTICALLY to forge by
// REUSING this rule, rather than re-implementing it and risking divergence the
// parity net would then have to chase. forge's own behavior is unchanged.
func SlugifyTitle(title string) string { return slugifyTitle(title) }

func slugifyTitle(title string) string {
	lower := strings.ToLower(title)
	s := slugifySanitize.ReplaceAllString(lower, "-")
	s = strings.Trim(s, "-")
	if len(s) > 80 {
		truncated := s[:80]
		if idx := strings.LastIndex(truncated, "-"); idx > 0 {
			truncated = truncated[:idx]
		}
		s = strings.TrimRight(truncated, "-")
	}
	return s
}
