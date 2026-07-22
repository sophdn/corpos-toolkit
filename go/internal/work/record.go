package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"toolkit/internal/dispatch"
	"toolkit/internal/events"
)

// RecordEvent is one entry in a record(events[]) call — a raw typed event the
// caller submits. Type + Payload are validated against the closed event-type
// enum (the shared go/internal/events validators); Entity is the primary
// entity the event acts on.
type RecordEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	// Entity may be given either as the flat entity_kind / entity_slug /
	// entity_project_id fields OR as a nested `entity` object {kind, slug,
	// project_id} (the canonical envelope shape). entity_kind is OPTIONAL:
	// when omitted it is inferred from the event type for well-known types
	// (BugResolved→bug, ChainCreated→chain, …; see events.EntityKindForType).
	// When both flat and nested are present, the flat field wins.
	EntityKind      string        `json:"entity_kind,omitempty"`
	EntitySlug      string        `json:"entity_slug,omitempty"`
	EntityProjectID *string       `json:"entity_project_id,omitempty"`
	Entity          *RecordEntity `json:"entity,omitempty"`
	Ts              string        `json:"ts,omitempty"`
	Rationale       string        `json:"rationale,omitempty"`
	CausedByEventID *string       `json:"caused_by_event_id,omitempty"`
}

// RecordEntity is the nested entity shape — the canonical envelope's
// {kind, slug, project_id} object, accepted as an alternative to the flat
// entity_* fields so callers can mirror the event envelope directly.
type RecordEntity struct {
	Kind      string  `json:"kind,omitempty"`
	Slug      string  `json:"slug,omitempty"`
	ProjectID *string `json:"project_id,omitempty"`
}

// RecordParams is the wire shape for the work.record action.
//
// StrictAllOrNothing (default false) selects the partial-success mode. The
// forge-v2 default is partial: a per-event VALIDATION rejection becomes a
// ghost (T4) and does NOT roll back the valid events recorded alongside it —
// ghosting the fumble is the point (§5). Set true to demand all-or-nothing:
// any rejection rolls back the whole batch (the strict mode some callers want
// for an atomic multi-event primitive like chain+tasks). A genuine
// infrastructure error (DB failure, fold error) ALWAYS rolls back, in both
// modes.
type RecordParams struct {
	Events             []RecordEvent `json:"events"`
	StrictAllOrNothing bool          `json:"strict_all_or_nothing"`
	// DryRun runs the full thin-fast-local validation for every event and
	// returns the same per-event ok/reject results WITHOUT appending anything
	// (zero events, zero ghosts) — a preview before a partial-success batch
	// half-lands (suggestion record-dry-run-validate-only-mode).
	DryRun bool `json:"dry_run"`
}

// RecordEventResult is one event's outcome in the consolidated return.
type RecordEventResult struct {
	Position       int     `json:"position"`
	Type           string  `json:"type"`
	EntitySlug     string  `json:"entity_slug"`
	OK             bool    `json:"ok"`
	EventID        *string `json:"event_id,omitempty"`
	RejectedReason *string `json:"rejected_reason,omitempty"`
}

// RecordResult is the work.record response envelope. Mirrors the partial-
// success shape of [BatchResult]: per-event outcomes plus batch-level
// totals. RolledBack=true means the whole tx aborted (a hard error, or
// StrictAllOrNothing with a rejection) and NOTHING persisted — even ok=true
// entries lose their event_id (their INSERTs were on the rolled-back tx).
type RecordResult struct {
	OK         bool                `json:"ok"`
	EventCount int                 `json:"event_count"`
	Recorded   int                 `json:"recorded"`
	Rejected   int                 `json:"rejected"`
	RolledBack bool                `json:"rolled_back"`
	DryRun     bool                `json:"dry_run,omitempty"`
	Results    []RecordEventResult `json:"results"`
	Error      string              `json:"error,omitempty"`
}

// resolveRecordEntity resolves an event's entity (kind, slug, project) from
// the flat entity_* fields, the nested entity object, event-type inference,
// and the call-level project — in that precedence. Returns a descriptive
// error when the entity can't be resolved (no slug, or an un-inferable kind).
func resolveRecordEntity(ev RecordEvent, callProject string) (events.EntityRef, error) {
	kind := ev.EntityKind
	slug := ev.EntitySlug
	projectID := ev.EntityProjectID
	if ev.Entity != nil {
		if kind == "" {
			kind = ev.Entity.Kind
		}
		if slug == "" {
			slug = ev.Entity.Slug
		}
		if projectID == nil {
			projectID = ev.Entity.ProjectID
		}
	}
	if kind == "" {
		if inferred, ok := events.EntityKindForType(ev.Type); ok {
			kind = inferred
		}
	}
	if slug == "" {
		return events.EntityRef{}, fmt.Errorf("entity_slug is required (give entity_slug, or a nested entity.slug)")
	}
	if kind == "" {
		return events.EntityRef{}, fmt.Errorf("entity_kind is required and could not be inferred from type %q (give entity_kind or a nested entity.kind)", ev.Type)
	}
	if projectID == nil && callProject != "" {
		pid := callProject
		projectID = &pid
	}
	return events.EntityRef{Kind: kind, Slug: slug, ProjectID: projectID}, nil
}

// HandleRecord is the forge-v2 `record(events[])` surface: it submits a
// heterogeneous, timestamped array of typed events to the HOT LOCAL DRAFT
// (the events ledger), validating each against the closed enum (the
// thin-fast-local tier, built on the SHARED [events.ValidateRecordJSON]),
// appending the valid + folding projections in ONE transaction, and returning
// consolidated per-event results immediately. The expensive tiers (causal,
// projection-coherence, registry immutability) run later on the async CI tail
// (T2 registry + T5 mirror), never blocking this return.
//
// Partial-success (the work.batch lineage, generalized to typed events):
//   - A per-event validation REJECTION is reported as ok=false + reason and
//     becomes a ghost (T4); the valid events still commit (default mode).
//   - StrictAllOrNothing=true makes any rejection roll back the whole call.
//   - A genuine error (DB / fold) always rolls back; ok=true entries then
//     lose their event_id.
//
// one create = a single-element events array. ts is server-authoritative
// unless a clamped caller ts is supplied (see [events.EmitRecord]).
func HandleRecord(ctx context.Context, deps TableDeps, project string, params json.RawMessage) (RecordResult, error) {
	var p RecordParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RecordResult{}, fmt.Errorf("parse record params: %w", err)
		}
	}
	if len(p.Events) == 0 {
		return RecordResult{Error: "record requires a non-empty events list"}, nil
	}
	if deps.Pool == nil {
		return RecordResult{Error: "record requires a DB pool"}, nil
	}

	// Pre-flight (no DB writes): require a type + a RESOLVABLE entity for each
	// event. Resolution is shared by the dry-run preview + the write path.
	resolved, rej := resolveRecordEntities(p, project)
	if rej != nil {
		return *rej, nil
	}

	// Dry-run: validate every event against the thin-fast-local tier and
	// report would-record / would-reject WITHOUT a write tx — zero events,
	// zero ghosts (suggestion record-dry-run-validate-only-mode).
	if p.DryRun {
		results := make([]RecordEventResult, len(p.Events))
		recorded, rejected := 0, 0
		for i, ev := range p.Events {
			ref := resolved[i]
			res := RecordEventResult{Position: i, Type: ev.Type, EntitySlug: ref.Slug}
			emitCtx := ctx
			if r := strings.TrimSpace(ev.Rationale); r != "" {
				emitCtx = events.WithRationale(ctx, r)
			}
			err := events.ValidateRecordArgs(emitCtx, events.RecordArgs{
				Type: ev.Type, Payload: ev.Payload, Entity: ref, Ts: ev.Ts,
				Refs: events.Refs{CausedByEventID: ev.CausedByEventID},
			})
			if err != nil {
				reason := err.Error()
				res.OK = false
				res.RejectedReason = &reason
				rejected++
			} else {
				res.OK = true // would record
				recorded++
			}
			results[i] = res
		}
		return RecordResult{
			OK:         rejected == 0,
			EventCount: len(p.Events),
			Recorded:   recorded,
			Rejected:   rejected,
			DryRun:     true,
			Results:    results,
		}, nil
	}

	var out RecordResult
	txErr := deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = recordEventsOnResolvedTx(ctx, tx, p, resolved)
		return err
	})
	if txErr != nil && !isRecordStrictAbort(txErr) {
		// A genuine infrastructure error — surface it.
		return RecordResult{}, txErr
	}
	return out, nil
}

// resolveRecordEntities runs the pre-flight (no-DB) resolution for every event:
// a non-empty type + a RESOLVABLE entity (flat fields → nested entity →
// type-inferred kind → call project). Returns the resolved refs, or a non-nil
// *RecordResult carrying the caller-visible rejection envelope on the first bad
// event. Shared by HandleRecord's dry-run + write paths and by RecordEventsOnTx.
func resolveRecordEntities(p RecordParams, project string) ([]events.EntityRef, *RecordResult) {
	resolved := make([]events.EntityRef, len(p.Events))
	for i, ev := range p.Events {
		if strings.TrimSpace(ev.Type) == "" {
			return nil, &RecordResult{Error: fmt.Sprintf("events[%d]: missing type", i)}
		}
		ref, err := resolveRecordEntity(ev, project)
		if err != nil {
			return nil, &RecordResult{Error: fmt.Sprintf("events[%d] (%s): %v", i, ev.Type, err)}
		}
		resolved[i] = ref
	}
	return resolved, nil
}

// RecordEventsOnTx emits a parsed RecordParams' events on the CALLER's tx — no
// WithWrite of its own. Returns the RecordResult; a non-nil error means the
// caller MUST roll back its tx (the strict-all-or-nothing abort sentinel, or a
// genuine infra/fold error). This is HandleRecord's former WithWrite body,
// extracted so construct.CreateInTx / UpdateInTx can thread an outer batch tx
// through the same emit-fold-ghost-partial-success machinery (T7 P1-C) — the
// record substrate stays the single event-emit path for both the standalone and
// in-batch forge routes. Resolution runs internally (the in-tx callers don't
// have access to the work-private resolver).
func RecordEventsOnTx(ctx context.Context, tx *sql.Tx, project string, p RecordParams) (RecordResult, error) {
	resolved, rej := resolveRecordEntities(p, project)
	if rej != nil {
		return *rej, nil
	}
	return recordEventsOnResolvedTx(ctx, tx, p, resolved)
}

// recordEventsOnResolvedTx is the emit-fold-ghost loop on a caller-provided tx,
// with resolution already done. Returns the RecordResult plus an error the
// caller propagates to its WithWrite: errRecordStrictAbort (strict-mode rollback
// after a rejection) or a genuine infra/ghost-write error. On any rollback the
// recorded events' ids are stripped (they won't exist post-rollback).
func recordEventsOnResolvedTx(ctx context.Context, tx *sql.Tx, p RecordParams, resolved []events.EntityRef) (RecordResult, error) {
	results := make([]RecordEventResult, len(p.Events))
	recorded, rejected := 0, 0
	rolledBack := false

	loopErr := func() error {
		for i, ev := range p.Events {
			ref := resolved[i]
			res := RecordEventResult{Position: i, Type: ev.Type, EntitySlug: ref.Slug}
			emitCtx := ctx
			if r := strings.TrimSpace(ev.Rationale); r != "" {
				emitCtx = events.WithRationale(ctx, r)
			}

			eventID, emitErr := events.EmitRecord(emitCtx, tx, events.RecordArgs{
				Type:    ev.Type,
				Payload: ev.Payload,
				Entity:  ref,
				Ts:      ev.Ts,
				Refs:    events.Refs{CausedByEventID: ev.CausedByEventID},
			})
			if emitErr != nil {
				// Distinguish a VALIDATION rejection (caller fumble → ghost,
				// partial-success) from a genuine infrastructure error
				// (DB/fold failure → must roll the whole tx back).
				var invalid *events.ErrInvalidInput
				if errors.As(emitErr, &invalid) {
					reason := emitErr.Error()
					res.OK = false
					res.RejectedReason = &reason
					rejected++
					results[i] = res
					// Ghost the rejection: a persistent fumble record +
					// session-anchored Stop-hook surfacing (T4). Never folds
					// into entity projections — see migration 084. A ghost
					// write failure is a hard error (it would mean the DB is
					// unhealthy), so it aborts like any infra failure.
					spanID, _ := events.SpanIDFromContext(emitCtx)
					if gErr := insertGhost(emitCtx, tx, ghost{
						SpanID:         spanID,
						SessionID:      ghostSessionFromCtx(emitCtx),
						ProjectID:      ref.ProjectID,
						AttemptedType:  ev.Type,
						EntityKind:     ref.Kind,
						EntitySlug:     ref.Slug,
						Reason:         reason,
						RewritePayload: ev.Payload,
					}); gErr != nil {
						return fmt.Errorf("events[%d] (%s): ghosting rejected event: %w", i, ev.Type, gErr)
					}
					if p.StrictAllOrNothing {
						rolledBack = true
						return errRecordStrictAbort
					}
					continue
				}
				// Hard error — abort the whole tx.
				return fmt.Errorf("events[%d] (%s): %w", i, ev.Type, emitErr)
			}
			id := eventID
			res.OK = true
			res.EventID = &id
			recorded++
			results[i] = res
		}
		return nil
	}()

	// On any rollback, the recorded events' INSERTs are undone — strip their
	// event_ids so the return doesn't advertise ids that won't exist.
	if rolledBack {
		for i := range results {
			if results[i].OK {
				results[i].EventID = nil
			}
		}
	}

	out := RecordResult{
		OK:         !rolledBack && loopErr == nil,
		EventCount: len(p.Events),
		Recorded:   recorded,
		Rejected:   rejected,
		RolledBack: rolledBack,
		Results:    results,
	}
	return out, loopErr
}

// errRecordStrictAbort is the sentinel that triggers a rollback in
// StrictAllOrNothing mode after a per-event rejection. WithWrite treats any
// non-nil return as a rollback signal; this lets HandleRecord tell a
// deliberate strict-abort apart from a real tx-machinery failure.
var errRecordStrictAbort = fmt.Errorf("record: strict_all_or_nothing abort on a rejected event")

func isRecordStrictAbort(err error) bool { return errors.Is(err, errRecordStrictAbort) }

// dispatchAdaptRecord wires HandleRecord through dispatch.Adapt, mirroring
// dispatchAdaptBatch. Kept here so the deps capture is local and the table.go
// entry stays one line.
func dispatchAdaptRecord(deps TableDeps) dispatch.Handler {
	return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (RecordResult, error) {
		return HandleRecord(ctx, deps, project, params)
	})
}

// ── Action-doc descriptor (registry, actions_discovery.go) ────────
var recordDoc = ActionDoc{
	Purpose: "The canonical write surface (the v2 successor to forge; chain 311 T7 Stage 5). TWO input modes: (1) FORGE-SHAPED SUGAR — {schema_name|kind, slug, fields[, op]} where op ∈ create|update|delete (default create) — the one-call ergonomics of the (now-deprecated) forge / forge_edit / forge_delete actions, routed through the construct umbrella; this is the mode most callers want. (2) RAW EVENTS — {events:[{type, payload, entity}, …]} — a heterogeneous, timestamped array of TYPED events appended directly to the hot local draft (the events ledger), for lifecycle events + multi-event sequences the sugar doesn't cover. In both modes each event is validated against the closed event-type enum (the shared go/internal/events validators — the thin-fast-local tier); valid events append + fold projections in one tx; per-event validation rejections are reported (and become ghosts) without rolling back the valid events (partial-success, default). The expensive tiers (causal, projection-coherence, registry immutability) run on the async CI tail (the canonical registry), never blocking this return.",
	Params: []DocParam{
		{Name: "events", Required: true, Description: "Array of {type, payload, entity_slug, entity_kind?, entity_project_id?, entity?, ts?, rationale?, caused_by_event_id?}. DISCOVER type names + payload shapes with the `event_schema` action (no arg = list types; type=<T> = T's payload schema). entity_kind is OPTIONAL — inferred from the event type for well-known types (BugResolved→bug, ChainCreated→chain, TaskCreated→task, …); pass it explicitly only for types it can't infer. The entity may also be given as a nested object `entity:{kind,slug,project_id}` (the canonical envelope shape) instead of the flat entity_* keys. ts is server-authoritative unless a clamped (non-future) RFC3339 caller value is supplied."},
		{Name: "strict_all_or_nothing", Required: false, Description: "Default false (partial-success: a rejected event is ghosted, the valid events still commit). True demands all-or-nothing: any rejection rolls back the whole call. A genuine DB/fold error always rolls back regardless."},
		{Name: "dry_run", Required: false, Description: "Default false. True validates every event against the thin-fast-local tier and returns the same per-event ok/rejected_reason results WITHOUT appending anything (zero events, zero ghosts) — preview a heterogeneous batch before any of it lands."},
	},
	Example: `{"rationale":"filing observed friction","events":[{"type":"BugReported","entity_slug":"my-bug","payload":{"title":"x","problem_statement":"y"}}]}`,
	Notes:   "Discover event types + payloads via `event_schema` (no arg lists the closed type enum; type=<T> returns T's payload schema). entity_kind is inferred from the event type when omitted; the entity may be flat (entity_kind/entity_slug/entity_project_id) OR nested (entity:{kind,slug,project_id}). The top-level `rationale` (shown in the example) is REQUIRED for agent actors. Partial-success mirrors work.batch; on any rollback ok=true entries lose their event_id. dry_run=true previews without writing. ts is server-set by default; a future caller ts is clamped to now. record is the v2 successor to forge; during the migration both coexist (forge untouched until parity is proven).\n\nCreate a CHAIN WITH TASKS by emitting, in ONE record call and IN ORDER: a ChainCreated event, then one TaskCreated per task (chain comes first so each TaskCreated's fold resolves it). Those are the load-bearing events — the chain + tasks are complete from them alone. ChainAndTasksForged is an OPTIONAL grouping/analytics signal you MAY stack alongside (no projection folds it); emit it only if you want the summary, not because the chain needs it.",
	EnvelopeRequirements: []ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate. Envelope-level rationale is the 'why this MCP call' grain; each event may also carry its own per-event rationale (recorded on that event's envelope).",
		AppliesToActorKinds: []string{"agent"},
	}},
	Returns: &ActionReturn{Shape: "RecordResult", Description: "ok, event_count, recorded, rejected, rolled_back, and results[] (per-event: position, type, entity_slug, ok, event_id on success or rejected_reason on a validation rejection)."},
}
