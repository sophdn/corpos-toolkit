package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// tsLayout is the canonical event timestamp layout: RFC 3339, UTC, ms
// precision — the same render [Emit] uses.
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// RecordArgs is the raw-typed-event input to [EmitRecord]. Unlike [EmitArgs]
// (which takes a typed [Payload] whose Go type names the event type),
// RecordArgs carries the type name + payload as data — the shape the forge-v2
// `record(events[])` surface receives from a caller. Actor + span_id come
// from ctx, as with [Emit].
type RecordArgs struct {
	Type      string          // event type; must be in the closed enum (RegisteredTypes)
	Payload   json.RawMessage // type-specific payload, validated against the per-type schema
	Entity    EntityRef       // primary entity the event acts on
	Ts        string          // optional caller ts (RFC3339); "" → server-authoritative now
	Rationale *string         // optional; nil falls back to ctx (RationaleFromContext)
	Refs      Refs            // caused_by_event_id + related_entities
}

// EmitRecord is the substrate primitive behind the forge-v2 `record` surface:
// it assembles a caller-supplied raw typed event into the canonical envelope,
// validates it with the SHARED validator ([ValidateRecordJSON] — the same
// closed-enum + envelope + per-type-payload check the registry CI runs), and
// on success INSERTs it into the local events ledger and runs the fold hook,
// all in the caller's transaction.
//
// It is the raw-JSON-payload counterpart to [Emit] (which takes a typed
// [Payload]); both produce identical rows and both validate through the same
// schemas, so an event recorded via EmitRecord is indistinguishable from one
// emitted by a forge handler. [Emit] is left untouched (forge depends on it).
//
// ## ts authority (§7 invariant 4)
//
// ts is server-authoritative by default. A caller-supplied args.Ts is accepted
// only under clamp rules: it must parse as RFC3339, and a FUTURE timestamp is
// clamped down to now (a caller cannot post-date an event ahead of the server
// clock — ordering is load-bearing). A past caller ts is honored as-is
// (backfill / replay use this). On a parse failure EmitRecord returns
// *ErrInvalidInput rather than silently substituting now.
//
// Returns the generated UUIDv7 event_id on success. On validation failure
// returns *ErrInvalidInput before the INSERT, leaving the tx clean — the
// `record` surface turns that into a per-event rejection (a ghost, T4).
func EmitRecord(ctx context.Context, tx *sql.Tx, args RecordArgs) (string, error) {
	if tx == nil {
		return "", &ErrInvalidInput{Field: "tx", Reason: "transaction is nil"}
	}
	p, err := prepareRecord(ctx, args)
	if err != nil {
		return "", err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (
			event_id, ts, actor_kind, actor_id, type,
			entity_kind, entity_slug, entity_project_id,
			payload, rationale, caused_by_event_id, related_entities,
			span_id, schema_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.eventID, p.ts, p.actor.Kind, p.actor.ID, args.Type,
		args.Entity.Kind, args.Entity.Slug, args.Entity.ProjectID,
		string(p.payload), p.rationale, args.Refs.CausedByEventID, string(p.relatedJSON),
		p.spanID, SchemaVersion,
	); err != nil {
		return "", fmt.Errorf("insert event: %w", err)
	}

	if foldHook != nil {
		raw := RawEvent{
			EventID:         p.eventID,
			Ts:              p.ts,
			ActorKind:       p.actor.Kind,
			ActorID:         p.actor.ID,
			Type:            args.Type,
			EntityKind:      args.Entity.Kind,
			EntitySlug:      args.Entity.Slug,
			EntityProjectID: args.Entity.ProjectID,
			Payload:         p.payload,
			Rationale:       p.rationale,
			CausedByEventID: args.Refs.CausedByEventID,
			RelatedEntities: p.relatedJSON,
			SpanID:          p.spanID,
			SchemaVersion:   SchemaVersion,
		}
		if err := foldHook(ctx, tx, raw); err != nil {
			return "", fmt.Errorf("fold hook: %w", err)
		}
	}
	return p.eventID, nil
}

// ValidateRecordArgs runs the full thin-fast-local validation for ONE event
// without inserting it — the substrate primitive behind record's dry_run
// mode. It returns *ErrInvalidInput exactly as [EmitRecord] would (same
// closed-enum + envelope + per-type-payload check), but touches no DB and
// emits no event, so a caller can preview a heterogeneous batch before any of
// it lands.
func ValidateRecordArgs(ctx context.Context, args RecordArgs) error {
	_, err := prepareRecord(ctx, args)
	return err
}

// preparedRecord is the assembled, schema-validated event [EmitRecord] is
// ready to INSERT — the output of [prepareRecord], shared by the emit path
// and the dry-run validate path.
type preparedRecord struct {
	eventID     string
	ts          string
	spanID      string
	actor       Actor
	payload     json.RawMessage
	rationale   *string
	relatedJSON []byte
}

// prepareRecord performs every step before the INSERT: input checks,
// id/ts/span/actor derivation (ts under the §7 clamp), envelope assembly, and
// the SHARED schema validation ([ValidateRecordJSON] — the same path the
// registry CI tier runs). On any validation failure it returns
// *ErrInvalidInput with no side effects.
func prepareRecord(ctx context.Context, args RecordArgs) (preparedRecord, error) {
	if args.Type == "" {
		return preparedRecord{}, &ErrInvalidInput{Field: "type", Reason: "event type is required"}
	}
	if args.Entity.Kind == "" {
		return preparedRecord{}, &ErrInvalidInput{Field: "entity.kind", Reason: "entity kind is required"}
	}
	if args.Entity.Slug == "" {
		return preparedRecord{}, &ErrInvalidInput{Field: "entity.slug", Reason: "entity slug is required"}
	}
	payload := args.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	eventID, err := newUUIDv7()
	if err != nil {
		return preparedRecord{}, fmt.Errorf("generate event_id: %w", err)
	}
	spanID, err := SpanIDFromContext(ctx)
	if err != nil {
		return preparedRecord{}, fmt.Errorf("derive span_id: %w", err)
	}
	actor := ActorFromContext(ctx)
	ts, err := resolveRecordTs(args.Ts)
	if err != nil {
		return preparedRecord{}, err
	}
	rationale := args.Rationale
	if rationale == nil {
		if r := RationaleFromContext(ctx); r != "" {
			r := r
			rationale = &r
		}
	}
	env := recordEnvelope{
		EventID:       eventID,
		Ts:            ts,
		Actor:         actorWire{Kind: actor.Kind, ID: actor.ID},
		Type:          args.Type,
		Entity:        args.Entity,
		Payload:       payload,
		Rationale:     rationale,
		Refs:          refsWire{CausedByEventID: args.Refs.CausedByEventID, RelatedEntities: relatedSlice(args.Refs.RelatedEntities)},
		SpanID:        spanID,
		SchemaVersion: SchemaVersion,
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return preparedRecord{}, fmt.Errorf("marshal record envelope: %w", err)
	}
	if err := ValidateRecordJSON(envJSON); err != nil {
		return preparedRecord{}, err
	}
	// Provenance guard: a record may not launder an agent scope/deferral
	// decision as the user's (see provenance.go). Runs for both the write
	// (EmitRecord) and dry-run (ValidateRecordArgs) paths.
	if err := checkPayloadProvenance(payload); err != nil {
		return preparedRecord{}, err
	}
	relatedJSON, err := json.Marshal(env.Refs.RelatedEntities)
	if err != nil {
		return preparedRecord{}, fmt.Errorf("marshal related_entities: %w", err)
	}
	return preparedRecord{
		eventID:     eventID,
		ts:          ts,
		spanID:      spanID,
		actor:       actor,
		payload:     payload,
		rationale:   rationale,
		relatedJSON: relatedJSON,
	}, nil
}

// entityKindByType maps well-known event types to their canonical entity
// kind, so record can INFER entity_kind when a caller omits it — cutting the
// call ceremony (suggestion record-reduce-call-ceremony-...). Best-effort:
// types absent here still require an explicit entity_kind. Single-entity
// types only; ambiguous/cross-cutting types (benchmarks, batch) are left out.
var entityKindByType = map[string]string{
	"BugReported": "bug", "BugTriaged": "bug", "BugResolved": "bug",
	"BugReopened": "bug", "BugEdited": "bug", "BugStamped": "bug",
	"ChainCreated": "chain", "ChainClosed": "chain", "ChainEdited": "chain",
	"ChainAndTasksForged": "chain",
	"TaskCreated":         "task", "TaskCompleted": "task", "TaskCancelled": "task",
	"TaskEdited": "task", "TaskStamped": "task", "TaskAssignedToChain": "task",
	"TaskTransitioned": "task", "TaskHandoff": "task", "TaskRetired": "task",
	"SuggestionReported": "suggestion", "SuggestionResolved": "suggestion",
	"SuggestionReopened": "suggestion", "SuggestionEdited": "suggestion",
	"SuggestionStamped": "suggestion",
	"MemoryWritten":     "memory",
	"RoadmapUpdated":    "roadmap",
	"MigrationForged":   "migration",
}

// EntityKindForType returns the canonical entity kind for an event type when
// it is unambiguous (so record can fill in a caller-omitted entity_kind), plus
// false for types whose entity kind is not well-known (those still require an
// explicit kind).
func EntityKindForType(typ string) (string, bool) {
	k, ok := entityKindByType[typ]
	return k, ok
}

// resolveRecordTs implements the §7 ts-authority clamp: "" → server now; a
// caller ts must be RFC3339 and is clamped down if it is in the future.
func resolveRecordTs(callerTs string) (string, error) {
	now := time.Now().UTC()
	if callerTs == "" {
		return now.Format(tsLayout), nil
	}
	parsed, err := time.Parse(time.RFC3339, callerTs)
	if err != nil {
		return "", &ErrInvalidInput{Field: "ts", Reason: "ts must be RFC3339: " + err.Error()}
	}
	parsed = parsed.UTC()
	if parsed.After(now) {
		// No post-dating ahead of the server clock — clamp to now.
		return now.Format(tsLayout), nil
	}
	return parsed.Format(tsLayout), nil
}

// recordEnvelope is the raw-payload counterpart to [envelope] — identical
// wire shape, but Payload is json.RawMessage (caller-supplied) rather than a
// typed Payload. Used only to assemble + validate the envelope JSON inside
// [EmitRecord].
type recordEnvelope struct {
	EventID       string          `json:"event_id"`
	Ts            string          `json:"ts"`
	Actor         actorWire       `json:"actor"`
	Type          string          `json:"type"`
	Entity        EntityRef       `json:"entity"`
	Payload       json.RawMessage `json:"payload"`
	Rationale     *string         `json:"rationale"`
	Refs          refsWire        `json:"refs"`
	SpanID        string          `json:"span_id"`
	SchemaVersion int             `json:"schema_version"`
}
