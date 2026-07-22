package refresolve

import (
	"context"
	"sort"
	"strings"
)

// memoryEntryResolver maps a hyphenated identifier from MEMORY.md
// to the entries that mention it (in title, description, or slug).
// Reference-resolution-migration T10 ships the real implementation;
// memoryIndex is parsed at LoadCatalogs time and shared across
// handler calls.
//
// Cache policy: PolicyIndefiniteWithinSession (the auto-memory
// rarely changes mid-conversation, and changes that DO happen are
// session-local writes the agent itself made).
type memoryEntryResolver struct {
	index *MemoryIndex
}

// NewMemoryEntryResolver constructs the memory_entry resolver with
// a parsed MEMORY.md index. Pass nil to ship the shell-only no-op
// behavior (tests / sandbox setups). Exported for tests.
func NewMemoryEntryResolver(index *MemoryIndex) Resolver {
	return memoryEntryResolver{index: index}
}

func (memoryEntryResolver) Shape() ShapeCategory   { return ShapeMemoryEntry }
func (memoryEntryResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 50} }

func (r memoryEntryResolver) Resolve(_ context.Context, ref Reference) (HitSet, error) {
	out := HitSet{ResolverName: "memory_entry"}
	if r.index == nil {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	entries := r.index.Lookup(ref.Token)
	if len(entries) == 0 {
		out.ConfidenceTier = TierNoHit
		return out, nil
	}
	// Deterministic order: alphabetical by slug.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	for _, e := range entries {
		debug := []string{"slug=" + e.Slug}
		if e.Description != "" {
			snippet := e.Description
			if len(snippet) > 80 {
				snippet = snippet[:77] + "..."
			}
			debug = append(debug, "desc="+strings.ReplaceAll(snippet, "\n", " "))
		}
		out.Candidates = append(out.Candidates, Candidate{
			ID:         e.Slug,
			Title:      e.Title,
			Score:      1.0,
			SourceRef:  "memory:" + e.BodyPath,
			DebugNotes: strings.Join(debug, " "),
		})
	}
	if len(out.Candidates) == 1 {
		out.ConfidenceTier = TierSingleExact
	} else {
		out.ConfidenceTier = TierFuzzyMulti
	}
	return out, nil
}
