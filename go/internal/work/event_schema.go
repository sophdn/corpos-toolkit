package work

import (
	"context"
	"encoding/json"
	"fmt"

	"toolkit/internal/dispatch"
	"toolkit/internal/events"
)

// EventSchemaParams is the wire shape for the work.event_schema action.
type EventSchemaParams struct {
	// Type is the event type to describe. Omit it to LIST the registered
	// type enum instead of describing one.
	Type string `json:"type"`
}

// EventSchemaResult is the work.event_schema response. With no `type` it
// carries the registered enum in Types; with a `type` it carries that type's
// payload JSON Schema (raw) in Schema.
type EventSchemaResult struct {
	Types  []string        `json:"types,omitempty"`
	Type   string          `json:"type,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// HandleEventSchema is the discovery surface for the record event-type enum
// (chain emit-surface-forge-v2 — closes suggestion
// record-needs-mcp-event-type-and-payload-schema-discovery). It answers the
// two questions an agent constructing a record(events[]) call needs and
// previously had to answer by reading blueprints off disk:
//
//   - no `type`  → the closed set of event types record accepts (the enum).
//   - type=<T>   → T's payload JSON Schema (required fields + properties +
//     descriptions), straight from the embedded catalog.
//
// Read-only: no DB, no mutation, no rationale gate.
func HandleEventSchema(_ context.Context, params json.RawMessage) (EventSchemaResult, error) {
	var p struct {
		Type      string `json:"type"`
		EventType string `json:"event_type"`
		Kind      string `json:"kind"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return EventSchemaResult{}, fmt.Errorf("parse event_schema params: %w", err)
		}
	}
	typ := p.Type
	if typ == "" {
		typ = p.EventType
	}
	if typ == "" {
		typ = p.Kind
	}

	if typ == "" {
		return EventSchemaResult{Types: events.RegisteredTypes()}, nil
	}
	if !events.IsRegisteredType(typ) {
		return EventSchemaResult{Error: fmt.Sprintf(
			"unknown event type %q — call event_schema with no `type` to list the %d registered types",
			typ, len(events.RegisteredTypes()))}, nil
	}
	raw, err := events.SchemaBytes(typ)
	if err != nil {
		return EventSchemaResult{Error: fmt.Sprintf("read schema for %q: %v", typ, err)}, nil
	}
	return EventSchemaResult{Type: typ, Schema: json.RawMessage(raw)}, nil
}

// dispatchAdaptEventSchema wires HandleEventSchema through dispatch.Adapt. It
// needs no deps (reads the embedded event-schema catalog), so it registers
// regardless of DB-pool presence.
func dispatchAdaptEventSchema() dispatch.Handler {
	return dispatch.Adapt(func(ctx context.Context, _ string, params json.RawMessage) (EventSchemaResult, error) {
		return HandleEventSchema(ctx, params)
	})
}

// ── Action-doc descriptor (registry, actions_discovery.go) ────────
var eventSchemaDoc = ActionDoc{
	Purpose: "Discover the record(events[]) event-type enum + per-type payload schemas. With NO params, returns the closed set of event type names record accepts. With `type=<T>`, returns T's payload JSON Schema (required fields, properties, descriptions) from the embedded catalog. This is the discovery entry point for constructing record calls — use it instead of reading blueprints/events/*.json. Read-only.",
	Params: []DocParam{
		{Name: "type", Required: false, Description: "Event type to describe (e.g. BugResolved). Omit to list the full registered enum. Aliases: event_type, kind."},
	},
	Example: `{"type":"BugResolved"}`,
	Notes:   "No-arg call lists every registered event type; a typed call returns that type's payload schema. Pair with `record`: event_schema(no arg) to find the type, event_schema(type=T) for its payload shape, then record(events=[{type:T, payload:{…}, entity_slug:…}]). entity_kind is inferred from the type by record for well-known types.",
	Returns: &ActionReturn{Shape: "EventSchemaResult", Description: "Either {types:[…]} (no arg) or {type, schema:<JSON Schema>} (typed). On an unknown type, {error} with a hint to list."},
}
