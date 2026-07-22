package construct

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/forge/registry"
)

// ── Cross-cutting create + edit guards ─────────────────────────────────────
//
// These reproduce forge's pre-emit policies the record substrate doesn't
// enforce on its own. Create-path guards:
//
//   - B-D1 once-only-create: forge rejects a duplicate-slug create (pointing
//     the caller at forge_edit) rather than silently overwriting via the fold's
//     ON CONFLICT DO UPDATE — which is the right idempotent re-fold semantic
//     for the ledger (B-F2) but NOT forge's once-only CREATE policy. Without
//     this guard, a duplicate-slug record(events[]) would overwrite content.
//
//   - B-G2 double-dated-slug: a slug carrying a YYYY-MM-DD prefix is rejected
//     when the schema's filename_pattern would adjacently re-prepend the date
//     (`{date}_{slug}`), since the rendered filename would double-date.
//
// Edit-path guards (Stage 3):
//
//   - B-G1 placeholder-shaped value: `{{NAME}}` whole-value rejection (re-homed
//     via FirstPlaceholderShapedField — same regex forge_edit uses).
//
//   - B-ED2 set-by-lifecycle-field reject: a field declaring `set_by` is owned
//     by the named lifecycle action; a plain update rejects pre-emit with that
//     action named.
//
// Today these are caller-orchestrated (run before submitting the event).
// construct.Create / construct.Update fold them into their orchestration so
// the caller stops assembling the kit-of-parts by hand.

// RejectDuplicateCreate enforces forge's once-only-create policy (B-D1) for
// the event-sourced create schemas, reading the projection the schema folds
// into: bug/suggestion/chain by (project_id, slug); task by (chain_id, slug)
// after resolving chain_id from chainSlug. Returns a non-nil rejection when a
// row already exists; nil when the slug is free. chainSlug is required only
// for task and ignored otherwise. Pass the same write tx (or pool.DB()) the
// caller will use for the record emit so the check-then-emit is atomic.
func RejectDuplicateCreate(ctx context.Context, q db.Queryer, schemaName, project, chainSlug, slug string) error {
	switch schemaName {
	case "bug":
		return RejectDuplicateBySlug(ctx, q, "bug", "proj_current_bugs", project, slug)
	case "suggestion":
		return RejectDuplicateBySlug(ctx, q, "suggestion", "proj_current_suggestions", project, slug)
	case "chain":
		return RejectDuplicateBySlug(ctx, q, "chain", "proj_chain_status", project, slug)
	case "task":
		// task keys on (chain_id, slug), so RejectDuplicateBySlug's
		// (project_id, slug) shape doesn't fit — reproduce createTaskInTx's
		// check: resolve the chain, then probe proj_current_tasks. An
		// unresolved chain is not a duplicate (the create will reject later
		// for the missing chain).
		var chainID int64
		switch err := q.QueryRowContext(ctx,
			`SELECT id FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
			project, chainSlug).Scan(&chainID); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("task duplicate-check chain lookup: %w", err)
		}
		var exists int
		switch err := q.QueryRowContext(ctx,
			`SELECT 1 FROM proj_current_tasks WHERE chain_id = ? AND slug = ? LIMIT 1`,
			chainID, slug).Scan(&exists); {
		case err == nil:
			return fmt.Errorf("task create on existing (chain %q, slug %q) rejected — create is once-only; use task_edit / the lifecycle actions to update it", chainSlug, slug)
		case errors.Is(err, sql.ErrNoRows):
			return nil
		default:
			return fmt.Errorf("task duplicate-check: %w", err)
		}
	default:
		return fmt.Errorf("RejectDuplicateCreate: schema %q has no once-only-create policy", schemaName)
	}
}

// RejectPlaceholderShapedFields enforces forge's B-G1 placeholder guard on
// an edit's field map: an AI-agent `{{NAME}}` whole-value placeholder is
// rejected before reaching record, since writing it would destructively
// overwrite the existing content. Mirrors HandleForgeEdit's wording exactly
// (parity tests pin both sides); re-homed via the exported
// FirstPlaceholderShapedField seam (no duplicated regex). Returns nil
// when no field looks like a placeholder.
//
// Callers that want the forge_edit `allow_placeholder=true` opt-out should
// skip this guard explicitly — the construct layer doesn't surface a flag
// because every current call site builds fields from typed Inputs (an
// agent placeholder makes no sense in a typed *string field; the guard is
// belt-and-braces for the future generic-edit affordance).
func RejectPlaceholderShapedFields(fields map[string]fieldvalue.FieldValue) error {
	if name, value, _ := FirstPlaceholderShapedField(fields); name != "" {
		return fmt.Errorf(
			"field %q value %q looks like an AI-agent placeholder (whole-value `{{NAME}}` shape); writing it would destructively overwrite the existing content",
			name, value,
		)
	}
	return nil
}

// RejectSetByEditFields enforces B-ED2: a field whose schema declares
// `set_by = "<lifecycle-action>"` is NOT directly editable via a plain
// update — that field's value rides on a typed lifecycle event payload,
// not the projection's column, and forge_edit's UPDATE would crash the
// fold ("unknown column"). Mirrors EditDBInTx's rejection wording exactly
// so construct.Update returns the same caller-visible message forge_edit
// has for years; tests pin both.
//
// Returns nil when no provided field is owned by a lifecycle action.
// fields is the partial-update map (after FieldsFromJSON / typed-input
// conversion); the schema's declared fields drive the lookup (an unknown
// field is reported earlier by ValidatePartial — B-ED1).
func RejectSetByEditFields(schema registry.Schema, fields map[string]fieldvalue.FieldValue) error {
	declared := make(map[string]registry.Field, len(schema.Fields))
	for _, f := range schema.Fields {
		declared[f.Name] = f
	}
	storage := schema.ResolvedStorage()
	table := storage.Table
	if storage.DB != nil {
		table = storage.DB.Table
	}
	for name := range fields {
		fd, ok := declared[name]
		if !ok {
			continue // unknown field is ValidatePartial's territory
		}
		if fd.SetBy == "" {
			continue
		}
		return fmt.Errorf(
			"forge_edit can't set %q on %s — that field is owned by the %q action (its value lives on an event payload, not the projection). Use %s to (re)set it.",
			name, table, fd.SetBy, fd.SetBy,
		)
	}
	return nil
}

// RejectDoubleDatedSlug enforces forge's double-dated-slug guard (B-G2) for
// the file schemas: a slug carrying a YYYY-MM-DD prefix is rejected when the
// schema's filename_pattern would adjacently re-prepend the date. Reuses
// CheckDoubleDatedSlug (the same regex + pattern check forge's create
// dispatch uses). nil when the slug is safe or the schema doesn't double-date.
// (Today only vault-note triggers it; vault-note itself is a documented
// Stage-2 delta — no event — so this guard is in place for any future
// {date}_{slug} file schema the layer covers.)
func RejectDoubleDatedSlug(schema registry.Schema, schemaName, slug string) error {
	if rejected, message, hint := CheckDoubleDatedSlug(schema, schemaName, slug); rejected {
		return fmt.Errorf("%s (%s)", message, hint)
	}
	return nil
}
