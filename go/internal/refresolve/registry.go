package refresolve

import (
	"sort"
	"sync"
)

// Registry holds resolvers indexed by ShapeCategory. Construct via
// NewRegistry; populate via Register; consult via Get or All.
//
// Deviation from design doc §3.2: the design described a package-
// level Register / All / Get (mirroring the projections package).
// In this codebase resolvers carry concrete dependencies (DB pool,
// knowledge.Deps, inference router) that aren't safe to install at
// init() time — they require the server's startup wiring. The
// resolver registry is therefore a per-Registry struct,
// constructed once at startup via BuildProductionRegistry and
// stored on the handler's Deps. Tests construct empty Registries
// and register mock resolvers.
type Registry struct {
	mu        sync.RWMutex
	resolvers map[ShapeCategory]Resolver
}

// NewRegistry returns an empty Registry. Production code uses
// BuildProductionRegistry to populate it; tests register mocks.
func NewRegistry() *Registry {
	return &Registry{resolvers: make(map[ShapeCategory]Resolver)}
}

// Register installs a resolver for one shape. Calling Register
// with the same shape twice overwrites; intentional — production
// startup may re-register a resolver after a config reload, and
// tests may swap resolvers without rebuilding the registry.
func (r *Registry) Register(res Resolver) {
	if res == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvers[res.Shape()] = res
}

// Get returns the resolver registered for the supplied shape, or
// (nil, false) if none is registered.
func (r *Registry) Get(shape ShapeCategory) (Resolver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res, ok := r.resolvers[shape]
	return res, ok
}

// All returns the registered resolvers sorted by Shape priority
// (see shapePriority in detect.go). The dispatcher iterates this
// order when resolving multiple shapes for the same token.
func (r *Registry) All() []Resolver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Resolver, 0, len(r.resolvers))
	for _, res := range r.resolvers {
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool {
		return shapePriority(out[i].Shape()) < shapePriority(out[j].Shape())
	})
	return out
}

// CloneWith returns a shallow copy of the registry with the supplied
// resolvers overlaid (each overrides any resolver registered for the
// same shape). The receiver is left unmodified, so this is safe to
// call per request from concurrent handlers — unlike Register, which
// mutates shared state.
//
// Bug 884: the parse_context handler reloads the skill manifest every
// call (so newly-added skills are DETECTED immediately) but the
// skill_trigger / discipline_skill / skill_candidate resolvers held a
// manifest snapshot taken when the registry was built at startup, so
// the new skill never RESOLVED until a daemon restart. The handler now
// calls CloneWith per call to overlay those three resolvers rebuilt
// from the freshly-loaded manifest, closing the detection-vs-resolution
// asymmetry without touching the shared registry.
func (r *Registry) CloneWith(overrides ...Resolver) *Registry {
	r.mu.RLock()
	clone := &Registry{resolvers: make(map[ShapeCategory]Resolver, len(r.resolvers))}
	for shape, res := range r.resolvers {
		clone.resolvers[shape] = res
	}
	r.mu.RUnlock()
	for _, res := range overrides {
		if res == nil {
			continue
		}
		clone.resolvers[res.Shape()] = res
	}
	return clone
}

// Shapes returns the set of registered shapes, sorted by priority.
// Convenience for tests and debug surfaces.
func (r *Registry) Shapes() []ShapeCategory {
	all := r.All()
	out := make([]ShapeCategory, 0, len(all))
	for _, res := range all {
		out = append(out, res.Shape())
	}
	return out
}
