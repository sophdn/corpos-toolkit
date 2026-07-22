// Package policy is the dispatch-layer enforcement registry.
//
// ## Intended use
//
// Workflow served: at server startup the dispatcher loads
// action-manifests/dispatch-policy.toml into a [Registry]; before invoking
// any handler the dispatcher calls [Registry.Gates] to fetch the policy
// for the (surface, action) pair and enforces it (rationale required when
// the actor is an agent, boilerplate rejection, min-length).
//
// Invocation pattern: policy.Load(path) → *Registry, threaded into
// [dispatch.DispatchWith] via [dispatch.Options]. Hot-reloadable via
// [Registry.Reload] for admin.schema_reload-style ergonomics.
//
// Success shape: typed *Gates per action, default {} for unregistered
// entries. The default is read-only-ergonomic — a typo in an action name
// fails open (no enforcement) rather than blocking every call; the
// pre-commit lint (todo: future chain) catches missing entries on
// mutating actions.
//
// Non-goals: this package does not perform the enforcement itself —
// [Registry.ValidateRationale] returns a typed error envelope, the
// dispatcher decides whether to act on it. Boilerplate stop-list and
// min-length are constants here; they're tight on purpose (T3 acceptance
// criteria) and should not be tuned without a chain-level decision.
package policy

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Gates carries the enforcement flags for one (surface, action) pair.
type Gates struct {
	// RequiresRationale set to true means an agent-actor call must
	// carry a non-empty, non-boilerplate, min-length rationale.
	// Human and system actors are not subject to the boilerplate /
	// min-length checks; their rationale (when supplied) is recorded
	// verbatim and an absent value is allowed.
	RequiresRationale bool `toml:"requires_rationale"`
}

// Registry is the loaded dispatch policy. The zero value is a usable
// "no gates anywhere" registry — useful for tests and for transports
// (e.g. unit-tested handlers) that don't pull in the TOML.
type Registry struct {
	// gates is keyed by "<surface>.<action>" so a missing key trivially
	// degrades to the zero-Gates default.
	gates map[string]Gates
}

// NewEmpty returns a registry with no policies. Useful for tests that
// want the no-enforcement default without loading a TOML.
func NewEmpty() *Registry {
	return &Registry{gates: map[string]Gates{}}
}

// Load reads dispatch-policy.toml at path and returns a populated
// registry. An empty path or non-existent file is NOT an error — the
// caller gets a zero-policy registry so the server can boot even when
// the policy file is mis-resolved. The startup log line indicates which
// happened.
func Load(path string) (*Registry, error) {
	r := NewEmpty()
	if path == "" {
		return r, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, fmt.Errorf("read dispatch policy %q: %w", path, err)
	}
	// TOML shape: top-level keys are surface names; each surface's value
	// is a map of action_name → Gates. BurntSushi/toml decodes nested
	// tables into map[string]map[string]Gates directly.
	var raw map[string]map[string]Gates
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse dispatch policy %q: %w", path, err)
	}
	for surface, actions := range raw {
		for action, gates := range actions {
			r.gates[surface+"."+action] = gates
		}
	}
	return r, nil
}

// Gates returns the policy for one (surface, action). Absent entries
// return the zero-Gates default (no enforcement).
func (r *Registry) Gates(surface, action string) Gates {
	if r == nil || r.gates == nil {
		return Gates{}
	}
	return r.gates[surface+"."+action]
}

// Len reports how many (surface, action) entries the registry holds.
// Used in startup logging so the operator can confirm the policy file
// loaded.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.gates)
}

// Actions returns the list of "<surface>.<action>" keys in the registry.
// Used by tests and by lint pre-commit checks that want to assert every
// mutating action in the dispatch table has a corresponding policy entry.
// Order is not stable across calls.
func (r *Registry) Actions() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.gates))
	for k := range r.gates {
		out = append(out, k)
	}
	return out
}

// --- rationale validation ---

// MinRationaleLen is the minimum non-whitespace length for an
// agent-actor rationale on a `requires_rationale = true` action.
// Conservative on purpose: substantive rationales are short ("merge
// conflict resolved", "fix typo in error", "test was wrong"), but
// six characters is past every single-word boilerplate ("ok", "done").
const MinRationaleLen = 6

// boilerplateStopList is the case-insensitive set of rationales that the
// dispatcher rejects even when long enough to clear MinRationaleLen.
// Kept tight per the T3 acceptance criterion that boilerplate rejection
// should make boilerplate uncomfortable, not gatekeep substantive short
// rationales. The list is closed; adding entries requires a chain-level
// decision (the design assumption is that quality enforcement happens
// downstream in query-telemetry-substrate, not here).
var boilerplateStopList = map[string]struct{}{
	"ok":           {},
	"okay":         {},
	"done":         {},
	"complete":     {},
	"completed":    {},
	"as requested": {},
	"as asked":     {},
	"see above":    {},
	"see below":    {},
	"lgtm":         {},
	"n/a":          {},
	"na":           {},
	"none":         {},
	"nope":         {},
	"todo":         {},
	"tbd":          {},
	"asdf":         {},
	"foo":          {},
	"bar":          {},
	"test":         {},
	"testing":      {},
	"trying":       {},
	"because":      {}, // bare "because" is the prototypical hand-wave
	"yes":          {},
	"no":           {},
}

// RationaleError is the structured rejection envelope returned by
// [Registry.ValidateRationale] when a call fails the gate. The
// dispatcher converts it into the wire-level InvalidInput JSON envelope.
type RationaleError struct {
	Field  string // always "rationale"
	Reason string // e.g. "empty", "boilerplate", "too short"
	Hint   string // human-readable suggestion for the agent
}

func (e *RationaleError) Error() string {
	return fmt.Sprintf("invalid rationale: %s", e.Reason)
}

// ValidateRationale returns nil when the rationale passes the gate for
// the supplied actor kind. Returns a *RationaleError on failure. Callers
// can rely on the typed error to render the structured envelope.
//
// Validation rules per T3 acceptance criteria:
//   - Non-agent actors (human, system) pass through regardless. The
//     dispatcher records whatever they supplied (or NULL if absent).
//   - Agent actors must supply a non-empty, non-whitespace rationale.
//   - Trimmed length must be >= MinRationaleLen (6 chars).
//   - Lowercase-trimmed rationale must not match the boilerplate stop
//     list.
//
// The check is short-circuit: empty before length before stop-list.
func (g Gates) ValidateRationale(actorKind, rationale string) error {
	if !g.RequiresRationale {
		return nil
	}
	if actorKind != "agent" {
		return nil
	}
	trimmed := strings.TrimSpace(rationale)
	if trimmed == "" {
		return &RationaleError{
			Field:  "rationale",
			Reason: "empty",
			Hint:   "supply a non-empty `rationale` field describing why you're making this change (one or two sentences, the agent's own words; not boilerplate).",
		}
	}
	if len([]rune(trimmed)) < MinRationaleLen {
		return &RationaleError{
			Field:  "rationale",
			Reason: "too short",
			Hint:   fmt.Sprintf("rationale must be at least %d non-whitespace characters; describe the why in a phrase, not an abbreviation.", MinRationaleLen),
		}
	}
	if _, ok := boilerplateStopList[strings.ToLower(trimmed)]; ok {
		return &RationaleError{
			Field:  "rationale",
			Reason: "boilerplate",
			Hint:   "rationale matches the boilerplate stop-list ('ok', 'done', 'as requested', etc.) — describe the actual reason for this state change in your own words.",
		}
	}
	return nil
}
