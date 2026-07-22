package construct

import (
	"context"
	"encoding/json"
	"errors"

	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/work"
)

// handle.go is the agent-facing forge-shaped create/edit/delete orchestration —
// the parse front (PrepareForge*) → persistence (Create / UpdateFromForge /
// Delete, or the trained_model work-handler) → finalize tail (FinalizeForge*)
// glued into one call per op. Relocated from cmd/toolkit-server's
// handleAgentForge/Edit/Delete in chain 311 T7 Stage 6 P2-C.2 (forge archive) so
// (a) the binary's wiring stays pure assembly and (b) the forge characterization
// net — moved onto construct in the same commit — has a stable construct
// entrypoint to call (it used to call forge.HandleForge/Edit/Delete). The MCP
// dispatch host (cmd/toolkit-server) curries the deps and registers these on the
// work table; the record-sugar surface routes through them too.

// HandleForgeCreate runs the full forge-shaped create. Every live schema routes
// through construct: PrepareForge → (trained_model → work.HandleTrainedModelCreate
// | else construct.Create) → FinalizeForgeCreate.
//
// Two finalize-deps variants: sseFinalize (SSE-only OnCreate — for the covered
// creates whose construct.Create already synced the knowledge_pointer) and
// fullFinalize (SSE + IndexUpsertNotifier — for vault-note, whose construct.Create
// writes only the file, so the notifier upserts the pointer + returns the
// "created"/"updated" verb + does the scope-change orphan cleanup). Picking
// per-schema keeps the agent-visible action verb + pointer behavior identical to
// the pre-archive forge path.
func HandleForgeCreate(ctx context.Context, deps, sseFinalize, fullFinalize Deps, project string, params json.RawMessage) (ForgeCreateResult, error) {
	prep, rejection, err := PrepareForge(deps, project, params)
	if err != nil {
		return ForgeCreateResult{}, err
	}
	if rejection != nil {
		return *rejection, nil
	}
	// trained_model minimal forge-dep sever (chain 311 T7 Stage 6 P2-C.1): create
	// routes to work.HandleTrainedModelCreate (a direct INSERT parallel to
	// trained_model_promote/retire). PrepareForge already validated + slugged;
	// FinalizeForgeCreate runs the tail. The sse finalize deps suffice —
	// trained_model is not indexed (no pointer) and not batch-eligible (no burst
	// nudge), so the SSE publish is the only notifier effect either way.
	if prep.SchemaName == "trained_model" {
		v := prep.Validated
		if tmErr := work.HandleTrainedModelCreate(ctx, deps.Pool, project, prep.Slug,
			fieldvalue.StringField(v, "task"),
			fieldvalue.StringField(v, "version"),
			fieldvalue.StringField(v, "training_dataset_signature"),
			fieldvalue.StringField(v, "eval_metrics"),
			fieldvalue.StringField(v, "status"),
			fieldvalue.StringField(v, "artifact_path"),
		); tmErr != nil {
			return ForgeCreateResult{Error: tmErr.Error()}, nil
		}
		return FinalizeForgeCreate(ctx, sseFinalize, project, prep, CreatePersistResult{}), nil
	}
	in, convErr := InputFromForge(prep)
	if convErr != nil {
		// Every live create schema is in InputFromForge + CreateRoutesToConstruct;
		// pipe-mode chains reject at PrepareForge. A convErr here means an
		// unroutable schema reached the handler — surface it (no forge fallback).
		return ForgeCreateResult{Error: convErr.Error()}, nil
	}
	cres, cErr := Create(ctx, deps, prep.SchemaName, project, in)
	if cErr != nil {
		// Map the B-D1 duplicate rejection to the canonical once-only-create
		// envelope; surface anything else generically.
		if env, ok := DuplicateCreateEnvelope(cErr); ok {
			return env, nil
		}
		return ForgeCreateResult{Error: cErr.Error()}, nil
	}
	// vault-note: Create wrote only the file → the full notifier upserts the
	// pointer + returns the "created"/"updated" verb + does orphan cleanup. Every
	// other covered create pre-synced its pointer → the SSE-only notifier keeps
	// the verb "created" and avoids a double-write.
	finalize := sseFinalize
	if prep.SchemaName == "vault-note" {
		finalize = fullFinalize
	}
	return FinalizeForgeCreate(ctx, finalize, project, prep, CreatePersistResult{ArtifactPath: cres.FilePath, RoutingNote: cres.RoutingNote}), nil
}

// HandleForgeEdit runs the full forge-shaped edit: PrepareForgeEdit →
// UpdateFromForge (event-sourced + file-schema edits AND the no-event delta
// survivors vault-note/bench/trained_model) → FinalizeForgeEdit (response
// envelope + the OnEdit index notifier). A NotFoundError maps to the
// {error:"not_found", …} envelope; a set-by (B-ED2) rejection carries forge's
// exact wording.
func HandleForgeEdit(ctx context.Context, deps, editFinalize Deps, project string, params json.RawMessage) (ForgeEditResult, error) {
	prep, rejection, err := PrepareForgeEdit(deps, project, params)
	if err != nil {
		return ForgeEditResult{}, err
	}
	if rejection != nil {
		return *rejection, nil
	}
	ures, uErr := UpdateFromForge(ctx, deps, prep, project)
	if uErr != nil {
		var nf *NotFoundError
		if errors.As(uErr, &nf) {
			return ForgeEditResult{Error: "not_found", SchemaName: prep.SchemaName, Slug: prep.Slug}, nil
		}
		return ForgeEditResult{Error: uErr.Error()}, nil
	}
	return FinalizeForgeEdit(ctx, editFinalize, project, prep, EditResult{
		UpdatedFields: ures.UpdatedFields,
		ArtifactPath:  ures.FilePath,
		Relocated:     ures.Relocated,
	}), nil
}

// HandleForgeDelete runs the full forge-shaped delete: PrepareForgeDelete
// (rejection-only in practice — no live schema declares delete) → Delete
// (generic (project_id, slug) DELETE + knowledge_pointers cleanup for the
// test/extensibility synthetic schemas).
func HandleForgeDelete(ctx context.Context, deps Deps, project string, params json.RawMessage) (ForgeDeleteResult, error) {
	prep, rejection, err := PrepareForgeDelete(deps, project, params)
	if err != nil {
		return ForgeDeleteResult{}, err
	}
	if rejection != nil {
		return *rejection, nil
	}
	dres, dErr := Delete(ctx, deps, prep.SchemaName, project, prep.Slug)
	if dErr != nil {
		return ForgeDeleteResult{Error: dErr.Error()}, nil
	}
	if dres.NotFound {
		return ForgeDeleteResult{Error: "not_found", SchemaName: prep.SchemaName, Slug: prep.Slug}, nil
	}
	return ForgeDeleteResult{Ok: true, SchemaName: prep.SchemaName, Slug: prep.Slug}, nil
}
