package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// FoldHook is the signature an external package (typically the
// projections package) implements to refresh derived state inside the
// same tx as the event INSERT. Receives the just-INSERTed event in
// RawEvent shape. Called from [Emit] post-INSERT; a non-nil error
// propagates out and the caller's WithWrite tx rolls back. See
// docs/PROJECTIONS.md and T4 of agent-first-substrate for the full
// contract.
type FoldHook func(ctx context.Context, tx *sql.Tx, evt RawEvent) error

// RawEvent mirrors one row of the events table — the shape FoldHook
// implementations receive. Pointer fields are nullable; JSON columns
// (payload, related_entities) arrive as raw bytes so consumers can
// decode against their own typed schemas.
type RawEvent struct {
	EventID         string
	Ts              string
	ActorKind       string
	ActorID         string
	Type            string
	EntityKind      string
	EntitySlug      string
	EntityProjectID *string
	Payload         []byte
	Rationale       *string
	CausedByEventID *string
	RelatedEntities []byte
	SpanID          string
	SchemaVersion   int
}

// foldHook is the registered post-INSERT hook; nil by default so the
// events package is usable in isolation (and so test runs without the
// projections package wired don't blow up). Set via [SetFoldHook] at
// server startup.
var foldHook FoldHook

// SetFoldHook installs the global fold hook. Calling twice replaces
// the prior hook; tests use this to register a poison hook then
// restore. The events package never imports projections — the hook
// seam is the dependency reversal.
func SetFoldHook(fn FoldHook) { foldHook = fn }

// CurrentFoldHook returns the currently-installed hook (nil when
// unset). Callers that want to chain a new behavior in front of the
// existing hook capture this, set their wrapper via SetFoldHook, and
// delegate to the captured one. Used by internal/arcreview's
// substrate trigger listener so projections-fold-then-arcreview-detect
// runs in one tx without coupling the two packages.
func CurrentFoldHook() FoldHook { return foldHook }

// EmitArgs is what handlers construct to record a state change. The
// Payload's concrete type identifies the event type (via its
// EventType() method) and supplies the type-specific fields the
// validator will cross-check against the embedded JSON Schema. Actor,
// span_id, and rationale come from ctx, not from this struct —
// that's the seam T3's dispatch middleware fills via [WithActor],
// [WithSpanID], and [WithRationale]. Handlers may override the
// ctx-supplied rationale by setting Rationale here, but the canonical
// flow is dispatch-stamps-ctx-then-handler-emits.
type EmitArgs struct {
	Entity    EntityRef
	Payload   Payload
	Rationale *string
	Refs      Refs
}

// Emit constructs an event from args + ctx-derived Actor/SpanID, validates
// it against the envelope and per-type payload schemas, and INSERTs it
// into the events table via the caller-supplied transaction. Returns the
// generated UUIDv7 event_id on success.
//
// CALLER CONTRACT:
//   - tx must be live (inside pool.WithWrite or equivalent). Emit does
//     not begin or commit — the dual-write discipline (event INSERT first,
//     CRUD UPDATE second, both in one tx) is the caller's responsibility.
//   - Validation runs BEFORE the INSERT; on validation failure, no row
//     lands and the enclosing transaction stays clean.
//   - schema_version is fixed to [SchemaVersion]; callers do not set it.
//
// The args.Rationale field is recorded verbatim. This package does NOT
// enforce that rationale is non-empty for agent actors — that's the
// dispatch-boundary middleware's job (T3). Emit is the substrate
// primitive; the policy lives one layer up.
func Emit(ctx context.Context, tx *sql.Tx, args EmitArgs) (string, error) {
	if tx == nil {
		return "", &ErrInvalidInput{Field: "tx", Reason: "transaction is nil"}
	}
	if args.Payload == nil {
		return "", &ErrInvalidInput{Field: "payload", Reason: "payload is nil"}
	}
	if args.Entity.Kind == "" {
		return "", &ErrInvalidInput{Field: "entity.kind", Reason: "entity kind is required"}
	}
	if args.Entity.Slug == "" {
		return "", &ErrInvalidInput{Field: "entity.slug", Reason: "entity slug is required"}
	}

	eventID, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("generate event_id: %w", err)
	}
	spanID, err := SpanIDFromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("derive span_id: %w", err)
	}
	actor := ActorFromContext(ctx)
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	typ := args.Payload.EventType()

	// Rationale resolution: an explicit args.Rationale wins (handlers
	// that need to override the request-level rationale do this); when
	// nil, fall back to the ctx-supplied value stamped by T3's dispatch
	// middleware; when both absent, the column lands NULL — acceptable
	// for non-agent actors (the policy gate rejects the empty case for
	// agents before the handler ever runs).
	rationale := args.Rationale
	if rationale == nil {
		if r := RationaleFromContext(ctx); r != "" {
			r := r
			rationale = &r
		}
	}

	env := envelope{
		EventID:       eventID,
		Ts:            ts,
		Actor:         actorWire{Kind: actor.Kind, ID: actor.ID},
		Type:          typ,
		Entity:        args.Entity,
		Payload:       args.Payload,
		Rationale:     rationale,
		Refs:          refsWire{CausedByEventID: args.Refs.CausedByEventID, RelatedEntities: relatedSlice(args.Refs.RelatedEntities)},
		SpanID:        spanID,
		SchemaVersion: SchemaVersion,
	}

	if err := validateEnvelope(env); err != nil {
		return "", err
	}

	payloadJSON, err := json.Marshal(args.Payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	// Provenance guard — same rule as the record path (see provenance.go); the
	// legacy typed-forge route emits through Emit, so it is covered here too.
	if err := checkPayloadProvenance(payloadJSON); err != nil {
		return "", err
	}
	relatedJSON, err := json.Marshal(env.Refs.RelatedEntities)
	if err != nil {
		return "", fmt.Errorf("marshal related_entities: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO events (
			event_id, ts, actor_kind, actor_id, type,
			entity_kind, entity_slug, entity_project_id,
			payload, rationale, caused_by_event_id, related_entities,
			span_id, schema_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, ts, actor.Kind, actor.ID, typ,
		args.Entity.Kind, args.Entity.Slug, args.Entity.ProjectID,
		string(payloadJSON), rationale, args.Refs.CausedByEventID, string(relatedJSON),
		spanID, SchemaVersion,
	)
	if err != nil {
		return "", fmt.Errorf("insert event: %w", err)
	}

	// Post-INSERT fold hook — projections refresh derived state inside
	// the same tx. Nil hook = events package running in isolation
	// (tests that don't exercise projections, or pre-T4 callers).
	if foldHook != nil {
		raw := RawEvent{
			EventID:         eventID,
			Ts:              ts,
			ActorKind:       actor.Kind,
			ActorID:         actor.ID,
			Type:            typ,
			EntityKind:      args.Entity.Kind,
			EntitySlug:      args.Entity.Slug,
			EntityProjectID: args.Entity.ProjectID,
			Payload:         payloadJSON,
			Rationale:       rationale,
			CausedByEventID: args.Refs.CausedByEventID,
			RelatedEntities: relatedJSON,
			SpanID:          spanID,
			SchemaVersion:   SchemaVersion,
		}
		if err := foldHook(ctx, tx, raw); err != nil {
			return "", fmt.Errorf("fold hook: %w", err)
		}
	}
	return eventID, nil
}

// envelope mirrors blueprints/events/_envelope.json as a typed Go shape.
// The struct tags drive json.Marshal output, which is what
// jsonschema-go's validator inspects via reflection. Keeping this typed
// (rather than a map[string]any) means a typo in a top-level field name
// is a compile error, not a runtime validation failure.
type envelope struct {
	EventID       string    `json:"event_id"`
	Ts            string    `json:"ts"`
	Actor         actorWire `json:"actor"`
	Type          string    `json:"type"`
	Entity        EntityRef `json:"entity"`
	Payload       Payload   `json:"payload"`
	Rationale     *string   `json:"rationale"`
	Refs          refsWire  `json:"refs"`
	SpanID        string    `json:"span_id"`
	SchemaVersion int       `json:"schema_version"`
}

// actorWire is the JSON shape of the actor field. Distinct from
// [Actor] only because the lowercase JSON tags are an output detail —
// the Actor struct lives at the package API surface with Go-style
// field names.
type actorWire struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// refsWire is the JSON shape of the refs field. RelatedEntities is
// always a non-nil slice (possibly empty) so the JSON output is `[]`
// rather than `null`, matching the schema's `"type": "array"`.
type refsWire struct {
	CausedByEventID *string     `json:"caused_by_event_id"`
	RelatedEntities []EntityRef `json:"related_entities"`
}

// relatedSlice returns a non-nil [EntityRef] slice so the wire-level
// `related_entities` field is `[]` (not `null`) when the caller passed
// no refs. Cheaper than a custom MarshalJSON.
func relatedSlice(refs []EntityRef) []EntityRef {
	if refs == nil {
		return []EntityRef{}
	}
	return refs
}
