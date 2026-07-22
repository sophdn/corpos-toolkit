package curation

import (
	"context"
	"errors"
	"fmt"

	"toolkit/internal/db"
)

// SourceMaterialBuilder reconstructs the text payload that should be
// scored, given a candidate row. One impl per origin. See
// docs/CURATION_GO_MIGRATION.md §4-§5.
//
// Implementations should be stateless (or hold only read-only config) —
// the registry shares one instance across all passes.
type SourceMaterialBuilder interface {
	// Origin returns the curation_candidates.origin value this builder
	// handles. The registry uses this to dispatch.
	Origin() string

	// Build returns the source material text for cand. Returns an error
	// if the underlying source (task row, JSONL file, vault note) can't
	// be read — callers decide whether to skip the candidate or abort.
	Build(ctx context.Context, pool *db.Pool, cand Candidate) (string, error)
}

// ErrUnknownOrigin is returned by BuilderRegistry.ForOrigin when no
// builder is registered for the requested origin. Distinct error so
// callers can pattern-match (errors.Is) and decide whether to log-and-
// skip (acceptable: new origin in DB the binary hasn't been updated for)
// vs abort (unacceptable: registry wasn't populated at startup).
var ErrUnknownOrigin = errors.New("curation: no builder registered for origin")

// BuilderRegistry dispatches by curation_candidates.origin. Construct
// at startup; Register all builders BEFORE any pass runs (registration
// is not concurrency-safe with lookups).
type BuilderRegistry struct {
	byOrigin map[string]SourceMaterialBuilder
}

// NewBuilderRegistry returns an empty registry. Call Register for each
// builder before exposing the registry to passes.
func NewBuilderRegistry() *BuilderRegistry {
	return &BuilderRegistry{byOrigin: make(map[string]SourceMaterialBuilder)}
}

// Register adds b to the registry. Panics if b's Origin() is already
// registered — duplicate registration is a programmer error, not a
// runtime condition (would silently shadow one of the two builders).
func (r *BuilderRegistry) Register(b SourceMaterialBuilder) {
	origin := b.Origin()
	if _, exists := r.byOrigin[origin]; exists {
		panic(fmt.Sprintf("curation: duplicate builder for origin %q", origin))
	}
	r.byOrigin[origin] = b
}

// ForOrigin returns the registered builder or ErrUnknownOrigin.
func (r *BuilderRegistry) ForOrigin(origin string) (SourceMaterialBuilder, error) {
	b, ok := r.byOrigin[origin]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownOrigin, origin)
	}
	return b, nil
}

// Origins returns the set of origin strings the registry knows about,
// in unspecified order. Useful for diagnostics and tests.
func (r *BuilderRegistry) Origins() []string {
	out := make([]string, 0, len(r.byOrigin))
	for o := range r.byOrigin {
		out = append(out, o)
	}
	return out
}
