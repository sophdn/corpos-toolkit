package events

import (
	"encoding/json"
	"fmt"
)

// ValidateRecordJSON validates one event's canonical envelope JSON against
// the shared envelope schema (_envelope.json) plus the per-type payload
// schema — exactly the two-pass check [Emit] runs before INSERT, but from
// raw bytes rather than a typed [EmitArgs].
//
// This is the single shared validation entrypoint for forge-v2: the
// canonical event-registry CI (the thorough validity-stamp tier, chain
// emit-surface-forge-v2 T2) and the local `record` surface (the
// thin-fast-local tier, T3) both call it, so "is this a structurally +
// schema-valid event" has ONE source of truth and the two tiers cannot
// drift. The closed event-type enum is the set of payload schemas embedded
// in this package ([RegisteredTypes]); ValidateRecordJSON rejects any type
// outside it.
//
// raw must be the full 11-field envelope object (_envelope.json shape:
// event_id, ts, actor, type, entity, payload, rationale, refs, span_id,
// schema_version). Returns *ErrInvalidInput naming the failing layer
// (type / envelope / payload) so callers can surface a structured rejection
// — the ghost reason on the local tier, the CI failure line on the registry
// tier.
func ValidateRecordJSON(raw []byte) error {
	if err := loadSchemas(); err != nil {
		return fmt.Errorf("load schemas: %w", err)
	}

	// Pull the type discriminator + the payload sub-object. A second
	// unmarshal into a dynamic value feeds the envelope validator the
	// shape jsonschema-go requires (json.Unmarshal into `any`).
	var fields struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return &ErrInvalidInput{Field: "envelope", Reason: "not a JSON object: " + err.Error()}
	}
	if fields.Type == "" {
		return &ErrInvalidInput{Field: "type", Reason: "missing or empty event type"}
	}
	payloadSchema, ok := payloadSchemas[fields.Type]
	if !ok {
		return &ErrInvalidInput{Field: "type", Reason: "unknown event type: " + fields.Type}
	}

	var envValue any
	if err := json.Unmarshal(raw, &envValue); err != nil {
		return &ErrInvalidInput{Field: "envelope", Reason: err.Error()}
	}
	if err := envelopeSchema.Validate(envValue); err != nil {
		return &ErrInvalidInput{Field: "envelope", Reason: err.Error()}
	}

	if len(fields.Payload) == 0 {
		return &ErrInvalidInput{Field: "payload", Reason: "missing payload"}
	}
	var payloadValue any
	if err := json.Unmarshal(fields.Payload, &payloadValue); err != nil {
		return &ErrInvalidInput{Field: "payload", Reason: err.Error()}
	}
	if err := payloadSchema.Validate(payloadValue); err != nil {
		return &ErrInvalidInput{Field: "payload", Reason: err.Error()}
	}
	return nil
}
