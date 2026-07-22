package construct

import (
	"context"
	"database/sql"
	"fmt"

	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/work"
)

// ── In-tx variants for the work.batch path (T7 P1-C) ────────────────────────
//
// work.batch wraps an outer write tx around N allowlisted sub-ops so a single
// MCP round-trip can collapse the most-frequent multi-call patterns. Its forge
// create/edit ops historically ran through HandleForgeInTx /
// HandleForgeEditInTx. CreateInTx / UpdateInTx route those through the construct
// umbrella on the OUTER tx instead, so the record substrate is the single
// event-emit path whether a forge create is standalone or batched.
//
// The non-tx Create/Update open their own write tx (via work.HandleRecord +
// pool-based index sync) — those would re-enter db.Pool's non-reentrant write
// mutex inside batch's outer tx and DEADLOCK. The in-tx variants therefore (a)
// emit through work.RecordEventsOnTx (the caller's tx, no nested WithWrite) and
// (b) index-sync through forge's tx seams (IndexUpsertOnCreateInTx /
// OnEditInTx) — the exact seams HandleForgeInTx already used, so the
// batch path's knowledge-pointer behavior is unchanged.
//
// Scope: batch allows only the event-sourced schemas (bug/suggestion/task — and
// chain via the dedicated fan-out, not batch); the file/delta schemas never
// reach here. `validated` is the forge-validated field map (from the adapter's
// PrepareForge) — needed by the tx index seams, which build the pointer from
// the input fields rather than a projection read.

// CreateInTx runs a covered create on the caller's tx: build the typed event(s),
// B-D1 dup-reject, emit through work.RecordEventsOnTx, then B-F3 index sync via
// forge's tx seam. Returns the head event id (the cascade event the batch result
// stamps). A duplicate/validation rejection or a strict-abort/infra error is
// returned as a Go error so batch's outer tx rolls back.
func CreateInTx(ctx context.Context, tx *sql.Tx, deps Deps, schema, project string, in Input, validated map[string]fieldvalue.FieldValue) (string, error) {
	if deps.Schemas == nil {
		return "", fmt.Errorf("construct.CreateInTx: Deps.Schemas is required")
	}
	if err := validateInputMatchesSchema(schema, in); err != nil {
		return "", err
	}
	events, _, _, err := dispatchBuild(ctx, deps, schema, project, in)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", fmt.Errorf("construct.CreateInTx: dispatch produced no events for schema %q", schema)
	}
	if shouldDupCheck(schema) {
		if err := RejectDuplicateCreate(ctx, tx, schema, project, chainSlugFromInput(in), events[0].EntitySlug); err != nil {
			return "", err
		}
	}
	res, err := work.RecordEventsOnTx(ctx, tx, project, work.RecordParams{
		Events:             events,
		StrictAllOrNothing: len(events) > 1,
	})
	if err != nil {
		return "", err // strict-abort / infra → caller rolls back
	}
	if !res.OK || res.Recorded != len(events) {
		return "", fmt.Errorf("construct.CreateInTx: record incomplete for %q: %s", schema, recordRejectReason(res))
	}
	if needsIndexSync(schema) {
		if err := IndexUpsertOnCreateInTx(ctx, tx, schema, project, events[0].EntitySlug, validated); err != nil {
			return "", fmt.Errorf("construct.CreateInTx: index sync %s: %w", schema, err)
		}
	}
	return headEventID(res), nil
}

// UpdateInTx runs a covered edit on the caller's tx: B-ED2 set-by reject →
// existence probe → build the typed *Edited event → emit via RecordEventsOnTx →
// B-F3 index sync via forge's tx edit seam. Returns the *Edited event id. A
// not-found / set-by / infra failure returns a Go error so batch rolls back.
// File-schema edits (markdown) are NOT batch-aware and never reach here (batch's
// dispatch rejects markdown-target schemas, matching HandleForgeEditInTx).
func UpdateInTx(ctx context.Context, tx *sql.Tx, deps Deps, schema, project, slug, chainSlug string, validated map[string]fieldvalue.FieldValue) (string, error) {
	if deps.Schemas == nil {
		return "", fmt.Errorf("construct.UpdateInTx: Deps.Schemas is required")
	}
	s, ok := deps.Schemas.Get(schema)
	if !ok {
		return "", fmt.Errorf("construct.UpdateInTx: unknown schema %q", schema)
	}
	if err := RejectSetByEditFields(s, validated); err != nil {
		return "", err
	}
	exists, err := projectionRowExists(ctx, tx, schema, project, slug, chainSlug)
	if err != nil {
		return "", fmt.Errorf("construct.UpdateInTx: existence probe: %w", err)
	}
	if !exists {
		return "", &NotFoundError{Schema: schema, Slug: slug, Project: project}
	}
	event, _, err := dispatchEditBuild(ctx, deps, schema, project, slug, chainSlug, validated)
	if err != nil {
		return "", err
	}
	res, err := work.RecordEventsOnTx(ctx, tx, project, work.RecordParams{Events: []work.RecordEvent{event}})
	if err != nil {
		return "", err
	}
	if !res.OK || res.Recorded != 1 {
		return "", fmt.Errorf("construct.UpdateInTx: record incomplete for %q: %s", schema, recordRejectReason(res))
	}
	if needsIndexSync(schema) {
		if err := IndexUpsertOnEditInTx(ctx, tx, deps.Schemas, schema, project, slug); err != nil {
			return "", fmt.Errorf("construct.UpdateInTx: index sync %s: %w", schema, err)
		}
	}
	return headEventID(res), nil
}

// headEventID returns the first event's id from a record result, or "" if absent.
func headEventID(res work.RecordResult) string {
	if len(res.Results) > 0 && res.Results[0].EventID != nil {
		return *res.Results[0].EventID
	}
	return ""
}

// recordRejectReason surfaces the first per-event rejection reason for a clearer
// in-tx error (mirrors construct.Update's reason extraction).
func recordRejectReason(res work.RecordResult) string {
	if len(res.Results) > 0 && res.Results[0].RejectedReason != nil {
		return *res.Results[0].RejectedReason
	}
	return fmt.Sprintf("ok=%v recorded=%d", res.OK, res.Recorded)
}
