package construct

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"toolkit/internal/events"
	"toolkit/internal/forge/fieldvalue"
)

// Finalize tail + result types relocated from forge/handler.go + forge/types.go
// + forge/edit.go + forge/strategy.go in chain 311 T7 Stage 6 P2-C.2 (forge
// archive). FinalizeForgeCreate / FinalizeForgeEdit are the post-persist tail the
// record-sugar dispatch (cmd/toolkit-server handleAgentForge/Edit) shares: the
// work.batch burst nudge, the deferral-capture nudge, the OnCreate/OnEdit
// notifier (SSE bus publish + FTS5/knowledge index upsert), and the success
// envelope. Sharing this tail is what keeps the agent-visible response identical
// regardless of which layer persisted the row.

// CreatePersistResult carries the persistence-layer artifact metadata the
// finalize tail + after-create notifier read (relocated from forge.CreateResult;
// renamed to avoid colliding with construct.CreateResult, which is the
// event-sourced umbrella's richer return shape). For the construct-routed
// create the dispatch builds this from construct.CreateResult's FilePath +
// RoutingNote; the event-sourced schemas leave both empty.
type CreatePersistResult struct {
	ArtifactPath    string
	DBMirrorError   string
	PostCommitError string
	RoutingNote     string
}

// EditResult is the persistence-layer edit result the finalize tail consumes
// (relocated from forge/edit.go). The construct file-schema + db edit builders
// return it; FinalizeForgeEdit converts it into the public ForgeEditResult.
type EditResult struct {
	UpdatedFields []string
	NotFound      bool
	ArtifactPath  string
	Relocated     bool
	Action        string
	RoutingNote   string
	DroppedExtras []string
}

// AfterCreateNotifier returns the action verb describing what happened on the
// create path ("created" / "updated" for vault-note same-slug re-forges). Empty
// string means the notifier has no opinion — the tail falls back to "created".
// Invoked after a successful create when the dispatch host wires an event bus or
// FTS5 indexer.
type AfterCreateNotifier func(ctx context.Context, schemaName, project, slug string, result CreatePersistResult, fields map[string]fieldvalue.FieldValue) (string, error)

// AfterEditNotifier is invoked after a successful edit. The fields map carries
// only the fields the caller modified; index-sync notifiers re-derive from these
// PLUS the existing row.
type AfterEditNotifier func(ctx context.Context, schemaName, project, slug string, result EditResult, fields map[string]fieldvalue.FieldValue) error

// duplicateCreateError marks a once-only-create (project, slug) collision so the
// dispatch can render the rich forge_edit hint rather than a bare string (bug
// 934). Relocated from forge/strategy.go; the record construction layer's
// B-D1 dup-check (work.RejectDuplicateBySlug) returns the same sentinel.
type duplicateCreateError struct {
	schemaName string
	slug       string
}

func (e *duplicateCreateError) Error() string {
	return fmt.Sprintf("forge(%s): a %s with slug %q already exists in this project — create is once-only",
		e.schemaName, e.schemaName, e.slug)
}

// isBatchEligible reports whether the schema is batch-creatable — the capability
// the retired Strategy.BatchEligible() exposed. The set is the work.batch
// allowlist: bug, suggestion, task. Used by the finalize tail to scope the
// work.batch burst nudge (a burst of forge(memory)/forge(vault-note)/forge(chain)
// can't be collapsed into a batch, so it must neither nudge nor count).
func isBatchEligible(schemaName string) bool {
	switch schemaName {
	case "bug", "suggestion", "task":
		return true
	default:
		return false
	}
}

// BatchEligible reports whether the schema may be CREATED inside a work.batch
// envelope (the bug/suggestion/task allowlist). The batch forge-create seam
// rejects non-eligible schemas pre-dispatch — a chain-with-tasks is served by
// forge(chain, tasks=[...]) directly, not by batching forge(chain)+forge(task).
// Exported so the composition root's batch seam can enforce the scope gate the
// archived forge.HandleForgeInTx used to own.
func BatchEligible(schemaName string) bool { return isBatchEligible(schemaName) }

// DuplicateCreateEnvelope renders the canonical once-only-create rejection
// envelope (pointing at forge_edit) when err is the duplicate-create sentinel.
// Covers both the native create rejection AND construct.Create's (it delegates
// B-D1 to work.RejectDuplicateBySlug, which returns the same *duplicateCreateError).
// Returns ok=false for any other error so the caller surfaces it generically.
func DuplicateCreateEnvelope(err error) (ForgeCreateResult, bool) {
	var dup *duplicateCreateError
	if !errors.As(err, &dup) {
		return ForgeCreateResult{}, false
	}
	return ForgeCreateResult{
		Error:      dup.Error(),
		SchemaName: dup.schemaName,
		Slug:       dup.slug,
		Hint: fmt.Sprintf(
			"a %s with slug %q already exists — use forge_edit to update it (forge create is once-only per (project, slug)). If you intend to replace it, forge_delete first, then re-create.",
			dup.schemaName, dup.slug),
	}, true
}

// FinalizeForgeCreate runs the post-create tail SHARED by the construct-routed
// path and (pre-archive) forge's native persistence: the work.batch burst nudge,
// the deferral-capture nudge, the OnCreate notifier (SSE bus publish + FTS5/
// knowledge index upsert), and the success envelope. `result` carries the
// persistence layer's artifact path / routing note (empty for the event-sourced
// schemas; populated for the file schemas).
func FinalizeForgeCreate(ctx context.Context, deps Deps, project string, prep ForgePrep, result CreatePersistResult) ForgeCreateResult {
	schemaName := prep.SchemaName
	slug := prep.Slug

	// Bug 887 / 925: nudge toward work.batch only on a successful create of a
	// batch-creatable schema (bug/suggestion/task). Non-batchable forges must
	// neither nudge NOR count toward a burst (the short-circuit skips Record).
	burstHint := ""
	if isBatchEligible(schemaName) && deps.BurstTracker != nil && deps.BurstTracker.Record(events.MCPSessionIDFromContext(ctx)) {
		burstHint = ForgeBatchNudge
	}

	// Bug decide-per-recommendation-task-strands-the-recommendation-in-transcript:
	// a forged task/chain-task that DEFERS to an external recommendation but leaves
	// context_required empty risks stranding that recommendation in the transcript.
	hint := burstHint
	switch schemaName {
	case "task":
		if TaskDefersWithoutCapturedContext(fieldvalue.StringField(prep.Validated, "problem_statement"), fieldvalue.StringField(prep.Validated, "context_required")) {
			hint = joinHints(hint, DeferralCaptureNudge)
		}
	case "chain":
		if deferring := deferringChainTaskSlugs(prep.ChainTaskEntries); len(deferring) > 0 {
			hint = joinHints(hint, chainDeferralNudge(deferring))
		}
	}

	action := "created"
	if deps.OnCreate != nil {
		notifierAction, cbErr := deps.OnCreate(ctx, schemaName, project, slug, result, prep.Validated)
		if cbErr != nil {
			if notifierAction != "" {
				action = notifierAction
			}
			return ForgeCreateResult{
				Ok:               true,
				SchemaName:       schemaName,
				Slug:             slug,
				Action:           action,
				ArtifactPath:     result.ArtifactPath,
				RoutingNote:      result.RoutingNote,
				PostCommitError:  result.PostCommitError,
				AfterCreateError: cbErr.Error(),
				Hint:             hint,
			}
		}
		if notifierAction != "" {
			action = notifierAction
		}
	}

	return ForgeCreateResult{
		Ok:              true,
		SchemaName:      schemaName,
		Slug:            slug,
		Action:          action,
		ArtifactPath:    result.ArtifactPath,
		RoutingNote:     result.RoutingNote,
		DBMirrorError:   result.DBMirrorError,
		PostCommitError: result.PostCommitError,
		Hint:            hint,
	}
}

// buildForgeEditResult converts an EditResult into the public ForgeEditResult
// envelope with NO side effects (the after-edit index sync is fired separately
// by FinalizeForgeEdit's notifier).
func buildForgeEditResult(prep ForgeEditPrep, result EditResult) ForgeEditResult {
	if result.NotFound {
		return ForgeEditResult{
			Error:      "not_found",
			SchemaName: prep.SchemaName,
			Slug:       prep.Slug,
		}
	}
	sort.Strings(result.UpdatedFields)
	sort.Strings(result.DroppedExtras)
	action := result.Action
	if action == "" {
		action = "updated"
	}
	return ForgeEditResult{
		Ok:            true,
		SchemaName:    prep.SchemaName,
		Slug:          prep.Slug,
		Action:        action,
		UpdatedFields: result.UpdatedFields,
		ArtifactPath:  result.ArtifactPath,
		Relocated:     result.Relocated,
		RoutingNote:   result.RoutingNote,
		DroppedExtras: result.DroppedExtras,
	}
}

// FinalizeForgeEdit builds the response envelope and fires the pool-based
// after-edit notifier (FTS5 / knowledge_pointers sync). Used by the non-batch
// edit path, where deps.OnEdit runs AFTER the edit's own write committed.
func FinalizeForgeEdit(ctx context.Context, deps Deps, project string, prep ForgeEditPrep, result EditResult) ForgeEditResult {
	out := buildForgeEditResult(prep, result)
	if out.Error == "" && deps.OnEdit != nil {
		if cbErr := deps.OnEdit(ctx, prep.SchemaName, project, prep.Slug, result, prep.Validated); cbErr != nil {
			out.AfterEditError = cbErr.Error()
		}
	}
	return out
}
