package construct

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/forge/registry"
	"toolkit/internal/work"
)

// ── construct.Update — the agent-facing edit umbrella ──────────────────────
//
// One call per edit operation, schema-name dispatched, internally orchestrating
// the kit-of-parts forge_edit assembles for its callers (B-G1 placeholder
// guard → ValidatePartial → B-ED2 set-by reject → existence probe → build
// typed *Edited event → record submit → B-F3 index sync). Mirrors
// construct.Create's shape so a Stage-4 caller routing through the layer makes
// one call per CRUD op, schema-name + typed Input.
//
// Stage 3 Slice 1 covers bug + suggestion (the event-sourced edit arms with no
// file-write side effects). Slice 2 adds chain + task; Slices 3-4 add the file
// schemas (memory + retro + report-card — B-ED3 markdown-doc semantics).

// NotFoundError signals an edit targeted a (project, slug) that doesn't exist.
// It carries the parts forge_edit's `not_found` envelope needs so the T7 Stage-4
// adapter can render the identical {error:"not_found", schema_name, slug} shape
// from a construct.Update/UpdateFromForge failure. Error() preserves
// construct.Update's historical wording verbatim, so string-matching callers and
// tests are unaffected by the type introduction.
type NotFoundError struct {
	Schema  string
	Slug    string
	Project string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("construct.Update: %s %q not found in project %q", e.Schema, e.Slug, e.Project)
}

// UpdateInput is the discriminated-union payload for Update. Each schema's
// typed sparse-update input rides its own field; the dispatch reads the
// field matching the schema name. Exactly ONE field must be set per call.
//
// Edit-shape inputs (BugEditInput, SuggestionEditInput, …) differ from the
// Create inputs (BugInput, SuggestionInput, …) — the edit form uses *string /
// *[]string pointers so "" is a legal new value and nil means "leave
// unchanged." See event_sourced.go's edit section for the rationale.
type UpdateInput struct {
	Bug           *BugEditInput
	Suggestion    *SuggestionEditInput
	Chain         *ChainEditInput
	Task          *TaskEditInput
	Memory        *MemoryEditInput
	Retrospective *ChainAnchoredDocEditInput
	ReportCard    *ChainAnchoredDocEditInput
}

// UpdateResult is the umbrella's return shape. Schema + EntitySlug identify
// the row that was updated; UpdatedFields mirrors forge_edit's same-named
// envelope key (sorted by name for deterministic output); EventEmitted is
// the typed *Edited event submitted through record. FilePath is the post-
// edit on-disk path for file schemas (memory / retrospective / report-card),
// empty for db-only schemas. Relocated is true when a memory_kind change
// moved the file.
type UpdateResult struct {
	Schema        string
	EntitySlug    string
	UpdatedFields []string
	EventEmitted  work.RecordEvent
	FilePath      string
	Relocated     bool
}

// Update takes a schema name + typed UpdateInput + project, runs the full
// orchestration, and returns the result. Mismatched (schema, UpdateInput)
// returns a clear error.
//
// Orchestration (per arm):
//  1. Validate the UpdateInput has exactly the field matching the schema set.
//  2. Lift the typed input into a fieldvalue.FieldValue map (the shape forge's
//     existing validation + placeholder guard understand).
//  3. **B-G1 placeholder reject** via RejectPlaceholderShapedFields.
//  4. **B-ED1** partial validation via ValidatePartial (unknown field /
//     required-empty / enum / pattern violations rejected; coerced map
//     returned for downstream marshaling).
//  5. **B-ED2 set-by reject** via RejectSetByEditFields.
//  6. **Existence probe** — the schema's projection table must already
//     contain (project, slug); a fold-only path would silently no-op the
//     UPDATE, matching forge_edit's `not_found` envelope.
//  7. Dispatch to the per-schema builder (build the typed *EditedPayload
//     with UpdatedFields + UpdatedValues; marshal to a work.RecordEvent).
//  8. Submit through work.HandleRecord (single event, non-strict — partial
//     mode doesn't apply for one event but the API expects events[]).
//  9. **B-F3** knowledge-index sync via IndexSyncFromProjection (only
//     for Indexed DB schemas; bug yes, suggestion no — IndexSyncFromProjection
//     no-ops on non-Indexed schemas via the strategy's ReadCanonicalFields
//     returning ok=false).
func Update(ctx context.Context, deps Deps, schema, project string, in UpdateInput) (UpdateResult, error) {
	if deps.Pool == nil {
		return UpdateResult{}, fmt.Errorf("construct.Update: Deps.Pool is required")
	}
	if deps.Schemas == nil {
		return UpdateResult{}, fmt.Errorf("construct.Update: Deps.Schemas is required")
	}
	if err := validateUpdateInputMatchesSchema(schema, in); err != nil {
		return UpdateResult{}, err
	}

	s, ok := deps.Schemas.Get(schema)
	if !ok {
		return UpdateResult{}, fmt.Errorf("construct.Update: unknown schema %q (no registry entry)", schema)
	}

	slug := slugFromUpdateInput(schema, in)
	if slug == "" {
		return UpdateResult{}, fmt.Errorf("construct.Update: schema %q: slug is required on the edit input", schema)
	}

	fields := fieldMapFromUpdateInput(schema, in)
	if len(fields) == 0 {
		return UpdateResult{}, fmt.Errorf("construct.Update: schema %q: no field updates supplied", schema)
	}

	// (3) B-G1.
	if err := RejectPlaceholderShapedFields(fields); err != nil {
		return UpdateResult{}, err
	}
	// (4) B-ED1 partial-validate (unknown field / required-empty / enum / pattern).
	validated, err := ValidatePartial(s, fields)
	if err != nil {
		return UpdateResult{}, err
	}
	chainSlug := chainSlugFromUpdateInput(schema, in)
	return updateFromValidated(ctx, deps, s, schema, project, slug, chainSlug, validated)
}

// UpdateRoutesToConstruct reports whether forge_edit on the schema can be served
// by construct.Update. Post-archive (chain 311 T7 Stage 6 P2-C.2) this is EVERY
// live editable schema: the event-sourced edits (bug/suggestion/chain/task), the
// file-schema edits with B-ED3 markdown-doc semantics (memory/retrospective/
// report-card), and the no-event §15 delta survivors (vault-note markdown
// rewrite + bench/trained_model generic UPDATE) re-homed from forge's
// ExecutePreparedEdit path. There is no longer a forge fallback.
func UpdateRoutesToConstruct(schema string) bool {
	switch schema {
	case "bug", "suggestion", "chain", "task", "memory", "retrospective", "report-card",
		"vault-note", "bench", "trained_model":
		return true
	default:
		return false
	}
}

// UpdateFromForge runs construct.Update's orchestration tail on an
// already-parsed-and-partial-validated forge edit: PrepareForgeEdit has
// done the parse + B-G1 placeholder guard + ValidatePartial, so this does NOT
// repeat them. For the event-sourced + file-schema edits it runs B-ED2 set-by
// reject → existence probe → build the typed *Edited event → record submit →
// B-F3 index sync (Update's steps 5-9). For the no-event delta survivors
// (vault-note / bench / trained_model — chain 311 T7 Stage 6 P2-C.2) it routes
// to updateNoEvent (markdown rewrite or generic UPDATE, no event). Takes the
// whole ForgeEditPrep so the no-event arm can thread prep.DropExtras into the
// vault-note markdown rewrite (parity with forge's ExecutePreparedEdit, which
// built EditOpts{DropExtras: prep.DropExtras}). Mirrors the create-side
// InputFromForge → Create split — the validated forge field map feeds straight
// in, so there's no typed-edit-input completeness gap.
func UpdateFromForge(ctx context.Context, deps Deps, prep ForgeEditPrep, project string) (UpdateResult, error) {
	if deps.Pool == nil {
		return UpdateResult{}, fmt.Errorf("construct.UpdateFromForge: Deps.Pool is required")
	}
	if deps.Schemas == nil {
		return UpdateResult{}, fmt.Errorf("construct.UpdateFromForge: Deps.Schemas is required")
	}
	s, ok := deps.Schemas.Get(prep.SchemaName)
	if !ok {
		return UpdateResult{}, fmt.Errorf("construct.UpdateFromForge: unknown schema %q (no registry entry)", prep.SchemaName)
	}
	if isNoEventEdit(prep.SchemaName) {
		return updateNoEvent(ctx, deps, s, prep, project)
	}
	return updateFromValidated(ctx, deps, s, prep.SchemaName, project, prep.Slug, prep.ChainSlug, prep.Validated)
}

// updateFromValidated is construct.Update's shared orchestration tail (steps
// 5-9): B-ED2 set-by reject → existence probe → dispatch the typed *Edited
// event → record submit → B-F3 index sync. Called by Update (after it lifts +
// validates the typed UpdateInput) and by UpdateFromForge (with a forge-validated
// map). `validated` is the coerced/partial-validated fieldvalue.FieldValue map.
func updateFromValidated(ctx context.Context, deps Deps, s registry.Schema, schema, project, slug, chainSlug string, validated map[string]fieldvalue.FieldValue) (UpdateResult, error) {
	// (5) B-ED2 set-by reject.
	if err := RejectSetByEditFields(s, validated); err != nil {
		return UpdateResult{}, err
	}

	// (6) Existence probe — match forge_edit's `not_found` semantic. Skipped
	// for file schemas (memory / retrospective / report-card): their re-homed
	// seams probe the on-disk artifact directly, so the umbrella defers to
	// the seam's NotFound to avoid racing the projection.
	if !schemaUsesFileProbe(schema) {
		exists, err := projectionRowExists(ctx, deps.Pool.DB(), schema, project, slug, chainSlug)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("construct.Update: existence probe: %w", err)
		}
		if !exists {
			return UpdateResult{}, &NotFoundError{Schema: schema, Slug: slug, Project: project}
		}
	}

	// (7) Dispatch.
	event, editRes, err := dispatchEditBuild(ctx, deps, schema, project, slug, chainSlug, validated)
	if err != nil {
		return UpdateResult{}, err
	}

	// (8) Record submit.
	params, err := json.Marshal(work.RecordParams{Events: []work.RecordEvent{event}})
	if err != nil {
		return UpdateResult{}, fmt.Errorf("construct.Update: marshal record params: %w", err)
	}
	res, err := work.HandleRecord(ctx, work.TableDeps{Pool: deps.Pool}, project, params)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("construct.Update: record submit: %w", err)
	}
	if !res.OK || res.Recorded != 1 {
		// Find the per-event rejection reason for a clearer error.
		var reason string
		if len(res.Results) > 0 && res.Results[0].RejectedReason != nil {
			reason = ": " + *res.Results[0].RejectedReason
		}
		return UpdateResult{}, fmt.Errorf("construct.Update: record submit incomplete: ok=%v recorded=%d want=1%s", res.OK, res.Recorded, reason)
	}

	// (9) B-F3 index sync for indexed DB schemas.
	if needsIndexSync(schema) {
		if _, err := IndexSyncFromProjection(ctx, deps.Pool, deps.Schemas, schema, project, slug); err != nil {
			return UpdateResult{}, fmt.Errorf("construct.Update: index sync %s: %w", schema, err)
		}
	}

	updatedFields, _ := updatedFieldsAndValues(validated)
	return UpdateResult{
		Schema:        schema,
		EntitySlug:    slug,
		UpdatedFields: updatedFields,
		EventEmitted:  event,
		FilePath:      editRes.ArtifactPath,
		Relocated:     editRes.Relocated,
	}, nil
}

// validateUpdateInputMatchesSchema enforces the union discipline: exactly
// the UpdateInput field matching the schema name must be non-nil, with no
// other field set. Slice 2 widens this to chain + task; Slices 3-4 to the
// file schemas.
func validateUpdateInputMatchesSchema(schema string, in UpdateInput) error {
	switch schema {
	case "bug":
		return requireExactlyUpdate(in, "Bug", in.Bug != nil)
	case "suggestion":
		return requireExactlyUpdate(in, "Suggestion", in.Suggestion != nil)
	case "chain":
		return requireExactlyUpdate(in, "Chain", in.Chain != nil)
	case "task":
		return requireExactlyUpdate(in, "Task", in.Task != nil)
	case "memory":
		return requireExactlyUpdate(in, "Memory", in.Memory != nil)
	case "retrospective":
		return requireExactlyUpdate(in, "Retrospective", in.Retrospective != nil)
	case "report-card":
		return requireExactlyUpdate(in, "ReportCard", in.ReportCard != nil)
	default:
		return fmt.Errorf("construct.Update: unknown schema %q (supported: bug, suggestion, chain, task, memory, retrospective, report-card)", schema)
	}
}

func requireExactlyUpdate(in UpdateInput, expected string, isSet bool) error {
	if !isSet {
		return fmt.Errorf("construct.Update: missing UpdateInput.%s", expected)
	}
	if extras := setUpdateInputFields(in, expected); len(extras) > 0 {
		return fmt.Errorf("construct.Update: unexpected UpdateInput fields set alongside UpdateInput.%s: %v", expected, extras)
	}
	return nil
}

func setUpdateInputFields(in UpdateInput, allowed ...string) []string {
	allowSet := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowSet[a] = true
	}
	var extras []string
	check := func(name string, isSet bool) {
		if isSet && !allowSet[name] {
			extras = append(extras, name)
		}
	}
	check("Bug", in.Bug != nil)
	check("Suggestion", in.Suggestion != nil)
	check("Chain", in.Chain != nil)
	check("Task", in.Task != nil)
	check("Memory", in.Memory != nil)
	check("Retrospective", in.Retrospective != nil)
	check("ReportCard", in.ReportCard != nil)
	return extras
}

// slugFromUpdateInput pulls the Slug field from the typed edit input matching
// the schema name. Returns "" when no input is set (the validateUpdate guard
// runs first, so this is reached only for the matching arm).
func slugFromUpdateInput(schema string, in UpdateInput) string {
	switch schema {
	case "bug":
		if in.Bug != nil {
			return in.Bug.Slug
		}
	case "suggestion":
		if in.Suggestion != nil {
			return in.Suggestion.Slug
		}
	case "chain":
		if in.Chain != nil {
			return in.Chain.Slug
		}
	case "task":
		if in.Task != nil {
			return in.Task.Slug
		}
	case "memory":
		if in.Memory != nil {
			return in.Memory.Slug
		}
	case "retrospective":
		if in.Retrospective != nil {
			return in.Retrospective.Slug
		}
	case "report-card":
		if in.ReportCard != nil {
			return in.ReportCard.Slug
		}
	}
	return ""
}

// chainSlugFromUpdateInput pulls the ChainSlug field for the task arm — task
// edits are scoped to (chain_slug, slug) so the anti-fanout fold guard can
// disambiguate same-slug tasks in different chains. Empty for non-task arms.
func chainSlugFromUpdateInput(schema string, in UpdateInput) string {
	if schema == "task" && in.Task != nil {
		return in.Task.ChainSlug
	}
	return ""
}

// fieldMapFromUpdateInput converts the typed edit input into a
// fieldvalue.FieldValue map — the shape ValidatePartial + the placeholder
// guard already understand. Each set pointer becomes one entry; nil pointers
// drop out.
func fieldMapFromUpdateInput(schema string, in UpdateInput) map[string]fieldvalue.FieldValue {
	switch schema {
	case "bug":
		return in.Bug.fieldMap()
	case "suggestion":
		return in.Suggestion.fieldMap()
	case "chain":
		return in.Chain.fieldMap()
	case "task":
		return in.Task.fieldMap()
	case "memory":
		return in.Memory.fieldMap()
	case "retrospective":
		return in.Retrospective.fieldMap()
	case "report-card":
		return in.ReportCard.fieldMap()
	}
	return nil
}

// dispatchEditBuild routes a validated (schema, fields) pair to the right
// per-schema edit builder and returns the typed *Edited event + (for file
// schemas) an EditResult carrying the post-edit artifact path / relocate
// flag. chainSlug is the task-arm anti-fanout disambiguator. deps + the
// schema registry's per-schema entry are needed for the file-schema arms
// (memory, retrospective, report-card) — they re-home the existing
// Edit*Artifact seams.
func dispatchEditBuild(ctx context.Context, deps Deps, schema, project, slug, chainSlug string, validated map[string]fieldvalue.FieldValue) (work.RecordEvent, EditResult, error) {
	switch schema {
	case "bug":
		ev, err := buildEditBug(project, slug, validated)
		return ev, EditResult{}, err
	case "suggestion":
		ev, err := buildEditSuggestion(project, slug, validated)
		return ev, EditResult{}, err
	case "chain":
		ev, err := buildEditChain(project, slug, validated)
		return ev, EditResult{}, err
	case "task":
		ev, err := buildEditTask(project, slug, chainSlug, validated)
		return ev, EditResult{}, err
	case "memory":
		s, ok := deps.Schemas.Get("memory")
		if !ok {
			return work.RecordEvent{}, EditResult{}, fmt.Errorf("construct.Update: memory schema not in registry")
		}
		return buildEditMemory(ctx, deps.Pool.DB(), s, project, slug, validated)
	case "retrospective":
		s, ok := deps.Schemas.Get("retrospective")
		if !ok {
			return work.RecordEvent{}, EditResult{}, fmt.Errorf("construct.Update: retrospective schema not in registry")
		}
		return buildEditRetrospective(ctx, deps.Pool, s, project, slug, validated)
	case "report-card":
		s, ok := deps.Schemas.Get("report-card")
		if !ok {
			return work.RecordEvent{}, EditResult{}, fmt.Errorf("construct.Update: report-card schema not in registry")
		}
		return buildEditReportCard(ctx, deps.Pool, s, project, slug, validated)
	default:
		return work.RecordEvent{}, EditResult{}, fmt.Errorf("construct.Update: no edit builder for schema %q", schema)
	}
}

// schemaUsesFileProbe reports whether the umbrella should SKIP the
// projection existence probe and rely on the per-schema seam's own
// file-based not-found surfacing. memory's EditMemoryArtifact does
// findMarkdownPath; matching it inside the umbrella with a proj_memories
// probe would either race or diverge from the file (forge_edit on memory
// doesn't update proj_memories, so a stale row could mislead the probe).
// Same logic will apply to retrospective + report-card in Slice 4.
func schemaUsesFileProbe(schema string) bool {
	switch schema {
	case "memory", "retrospective", "report-card":
		return true
	}
	return false
}

// projectionRowExists reports whether the per-schema projection row for the
// (project, slug[, chainSlug]) target exists. Match forge_edit's `not_found`
// envelope: the fold path's UPDATE would silently no-op for an unknown slug,
// so the pre-emit probe is load-bearing — without it a typo-slug edit
// "succeeds" with zero rows affected and pollutes the event log.
//
// Task is keyed on (chain_id, slug) via proj_chain_status join — same shape
// as editTaskInTx's existence check. chainSlug is required for the task arm
// and ignored otherwise.
func projectionRowExists(ctx context.Context, q db.Queryer, schema, project, slug, chainSlug string) (bool, error) {
	if schema == "task" {
		var exists int
		err := q.QueryRowContext(ctx,
			`SELECT 1 FROM proj_current_tasks t JOIN proj_chain_status c ON c.id = t.chain_id
			 WHERE c.project_id = ? AND c.slug = ? AND t.slug = ?`,
			project, chainSlug, slug,
		).Scan(&exists)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, sql.ErrNoRows):
			return false, nil
		default:
			return false, err
		}
	}
	var table string
	switch schema {
	case "bug":
		table = "proj_current_bugs"
	case "suggestion":
		table = "proj_current_suggestions"
	case "chain":
		table = "proj_chain_status"
	default:
		return false, fmt.Errorf("projectionRowExists: no projection mapping for schema %q", schema)
	}
	var exists int
	err := q.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT 1 FROM %s WHERE project_id = ? AND slug = ?`, table),
		project, slug,
	).Scan(&exists)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}
