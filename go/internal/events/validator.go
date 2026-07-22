package events

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemasFS is the embedded copy of blueprints/events/. Kept in sync
// with the canonical source by scripts/sync-event-schemas.sh, invoked
// by scripts/precommit.sh before the build stages — same shape as the
// migrations-mirror discipline in scripts/sync-migrations.sh. The
// embed package rejects symlinks, so this is a real on-disk copy.
//
//go:embed schemas/*.json
var schemasFS embed.FS

var (
	loadOnce       sync.Once
	loadErr        error
	envelopeSchema *jsonschema.Resolved
	payloadSchemas map[string]*jsonschema.Resolved
)

// loadSchemas reads and resolves every JSON Schema file from schemasFS.
// _envelope.json becomes envelopeSchema; every other file becomes a
// payload schema keyed by its filename-without-extension. Idempotent
// via sync.Once.
func loadSchemas() error {
	loadOnce.Do(func() {
		payloadSchemas = make(map[string]*jsonschema.Resolved)
		entries, err := fs.ReadDir(schemasFS, "schemas")
		if err != nil {
			loadErr = fmt.Errorf("read embedded schemas: %w", err)
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := fs.ReadFile(schemasFS, "schemas/"+e.Name())
			if err != nil {
				loadErr = fmt.Errorf("read schema %s: %w", e.Name(), err)
				return
			}
			var s jsonschema.Schema
			if err := json.Unmarshal(data, &s); err != nil {
				loadErr = fmt.Errorf("parse schema %s: %w", e.Name(), err)
				return
			}
			resolved, err := s.Resolve(nil)
			if err != nil {
				loadErr = fmt.Errorf("resolve schema %s: %w", e.Name(), err)
				return
			}
			name := strings.TrimSuffix(e.Name(), ".json")
			if name == "_envelope" {
				envelopeSchema = resolved
			} else {
				payloadSchemas[name] = resolved
			}
		}
		if envelopeSchema == nil {
			loadErr = fmt.Errorf("envelope schema (_envelope.json) missing from embedded schemas")
		}
	})
	return loadErr
}

// SchemaBytes returns the raw JSON schema bytes for the given event
// type name (e.g. "BugResolved"), reading from the embedded schemas/
// directory. Returns an error if the type is not registered or the
// file cannot be read. Used by the schema-evolution guard test to
// count constraint occurrences without parsing into a resolved schema.
func SchemaBytes(typ string) ([]byte, error) {
	return fs.ReadFile(schemasFS, "schemas/"+typ+".json")
}

// IsRegisteredType reports whether typ has a payload schema in the
// embedded catalog. Used by tests + by callers that want to check
// before Emit (Emit itself rejects unregistered types with
// *ErrInvalidInput, so calling this first is optional).
func IsRegisteredType(typ string) bool {
	if err := loadSchemas(); err != nil {
		return false
	}
	_, ok := payloadSchemas[typ]
	return ok
}

// RegisteredTypes returns the sorted set of event type names that have a
// payload schema in the embedded catalog — exactly the names IsRegisteredType
// reports true for. It is the single, runtime-derived source of truth for
// "which event types exist": the guard test iterates it instead of a
// hand-maintained literal list, so adding an event type means adding a
// schemas/<Type>.json file (a new file — no shared list to merge-conflict on
// when two agents each add a type in parallel; chain
// worktree-multi-agent-orchestration-support T7). Returns nil if the catalog
// fails to load.
func RegisteredTypes() []string {
	if err := loadSchemas(); err != nil {
		return nil
	}
	out := make([]string, 0, len(payloadSchemas))
	for name := range payloadSchemas {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// validateEnvelope runs the two-pass check Emit performs before INSERT:
//  1. The fully-built envelope against _envelope.json.
//  2. The typed payload against blueprints/events/<type>.json.
//
// jsonschema-go's Validate(any) requires a dynamic-typed JSON value (the
// result of json.Unmarshal into an `any`); it explicitly rejects Go
// structs (see google/jsonschema-go issue #23). The bytes-roundtrip
// here is therefore unavoidable, and is the concentrated stdlib
// boundary the package-level lint exemption in go/.golangci.yml
// documents. Callers outside the package never see `any`.
//
// Returns *ErrInvalidInput on failure so the dispatch layer can surface
// a structured error to the caller.
func validateEnvelope(env envelope) error {
	if err := loadSchemas(); err != nil {
		return fmt.Errorf("load schemas: %w", err)
	}

	typ := env.Type
	payloadSchema, ok := payloadSchemas[typ]
	if !ok {
		return &ErrInvalidInput{Field: "type", Reason: "unknown event type: " + typ}
	}

	envValue, err := toDynamicJSON(env)
	if err != nil {
		return fmt.Errorf("encode envelope for validation: %w", err)
	}
	if err := envelopeSchema.Validate(envValue); err != nil {
		return &ErrInvalidInput{Field: "envelope", Reason: err.Error()}
	}

	payloadValue, err := toDynamicJSON(env.Payload)
	if err != nil {
		return fmt.Errorf("encode payload for validation: %w", err)
	}
	if err := payloadSchema.Validate(payloadValue); err != nil {
		return &ErrInvalidInput{Field: "payload", Reason: err.Error()}
	}
	return nil
}

// toDynamicJSON marshals a typed Go value through encoding/json then
// unmarshals to the dynamic JSON value shape that jsonschema-go's
// validator requires. This is the single point where the typed Go
// world meets jsonschema-go's `any` API surface — same pattern as
// internal/db's Scanner concentrating database/sql.Scan's variadic
// `any`. See validateEnvelope's doc for the boundary rationale.
func toDynamicJSON(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
