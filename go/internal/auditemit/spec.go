package auditemit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// Spec is a findings spec: the actor and entity an audit's events are
// emitted under, plus one-or-more typed events. Events is a slice because
// the bare audit-emit one-shot emitted two events (an Architecture audit
// and a Convention audit) under a single chain entity; the slice preserves
// that and keeps every other (single-event) audit a one-element case.
type Spec struct {
	ActorID string      `json:"actor_id"`
	Entity  SpecEntity  `json:"entity"`
	Events  []SpecEvent `json:"events"`
}

// SpecEntity is the primary entity the audit events act on. The historical
// emitters always used a chain entity (kind="chain"); the shape is general
// so a future audit keyed to another entity kind needs no code change.
type SpecEntity struct {
	Kind      string `json:"kind"`
	Slug      string `json:"slug"`
	ProjectID string `json:"project_id"`
}

// SpecEvent is one event in a spec: the registered event type, the
// rationale recorded on the row, and the type-specific payload as raw JSON
// (validated against the per-type schema at emit time, exactly as the typed
// emitters were validated against the same schema by events.Emit).
type SpecEvent struct {
	Type      string          `json:"type"`
	Rationale string          `json:"rationale"`
	Payload   json.RawMessage `json:"payload"`
}

// Emitted is one landed event: its type and the generated event_id.
type Emitted struct {
	Type    string
	EventID string
}

// Load reads, parses, and structurally validates a spec file. It does NOT
// schema-validate the payloads — that happens at emit time against the live
// per-type schema, so Load stays usable without the events schema set
// loaded. Returns a wrapped error naming the path on any failure.
func Load(path string) (Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("read spec %s: %w", path, err)
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return Spec{}, fmt.Errorf("parse spec %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, fmt.Errorf("spec %s: %w", path, err)
	}
	return s, nil
}

// Validate checks the spec's structural invariants (the ones EmitRecord
// would otherwise reject one-event-too-late): an actor id, an entity
// kind+slug, at least one event, and a type+payload on every event.
// Per-payload schema conformance is left to emit time.
func (s Spec) Validate() error {
	if s.ActorID == "" {
		return fmt.Errorf("actor_id is required")
	}
	if s.Entity.Kind == "" {
		return fmt.Errorf("entity.kind is required")
	}
	if s.Entity.Slug == "" {
		return fmt.Errorf("entity.slug is required")
	}
	if len(s.Events) == 0 {
		return fmt.Errorf("at least one event is required")
	}
	for i, e := range s.Events {
		if e.Type == "" {
			return fmt.Errorf("events[%d].type is required", i)
		}
		if len(e.Payload) == 0 {
			return fmt.Errorf("events[%d].payload is required", i)
		}
	}
	return nil
}

// CheckSchema validates every event in the spec against its per-type schema
// WITHOUT touching a DB — the dry-run path and the spec-rot guard. It runs
// exactly the closed-enum + envelope + payload check events.EmitRecord would
// run before its INSERT (events.ValidateRecordArgs), but emits nothing, so a
// committed spec can be proven still-valid in a unit test. Returns a wrapped
// error naming the failing event index + type.
func CheckSchema(ctx context.Context, s Spec) error {
	ctx = events.WithActor(ctx, events.Actor{Kind: "agent", ID: s.ActorID})
	entity := events.NewEntityRef(s.Entity.Kind, s.Entity.Slug, s.Entity.ProjectID)
	for i, e := range s.Events {
		rationale := e.Rationale
		if err := events.ValidateRecordArgs(ctx, events.RecordArgs{
			Type:      e.Type,
			Payload:   e.Payload,
			Entity:    entity,
			Rationale: &rationale,
		}); err != nil {
			return fmt.Errorf("events[%d] (%s): %w", i, e.Type, err)
		}
	}
	return nil
}

// Emit lands every event in the spec through events.EmitRecord — the
// raw-JSON-payload twin of events.Emit — each in its own write transaction,
// in spec order. The actor (kind "agent", matching every historical
// emitter) comes from the spec; the rationale is per-event. The emitted row
// is byte-for-byte what the typed emitter produced, because EmitRecord
// validates through the same schema and writes the same columns.
//
// On the first event that fails to emit, Emit stops and returns the events
// landed so far plus a wrapped error naming the failing index and type. A
// schema-invalid event is rejected before its INSERT (EmitRecord's
// contract), so a partial failure leaves earlier events committed and the
// failing one absent — never a half-written row.
func Emit(ctx context.Context, pool *db.Pool, s Spec) ([]Emitted, error) {
	ctx = events.WithActor(ctx, events.Actor{Kind: "agent", ID: s.ActorID})
	entity := events.NewEntityRef(s.Entity.Kind, s.Entity.Slug, s.Entity.ProjectID)

	out := make([]Emitted, 0, len(s.Events))
	for i, e := range s.Events {
		rationale := e.Rationale
		var id string
		err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
			var emitErr error
			id, emitErr = events.EmitRecord(ctx, tx, events.RecordArgs{
				Type:      e.Type,
				Payload:   e.Payload,
				Entity:    entity,
				Rationale: &rationale,
			})
			return emitErr
		})
		if err != nil {
			return out, fmt.Errorf("emit events[%d] (%s): %w", i, e.Type, err)
		}
		out = append(out, Emitted{Type: e.Type, EventID: id})
	}
	return out, nil
}
