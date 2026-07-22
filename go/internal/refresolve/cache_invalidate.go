package refresolve

import (
	"context"
	"database/sql"
	"encoding/json"

	"toolkit/internal/events"
)

// InstallCacheInvalidationFoldHook chains a parse_context-cache
// invalidator in front of the currently-installed events FoldHook.
// Call once at toolkit-server startup AFTER projections has installed
// its hook (the chained-prev pattern preserves projections' refresh).
//
// On every emitted event whose entity is a chain/task/bug, the hook
// drops every cache entry that keyed on the entity's slug (across all
// sessions). related_entities slugs of the same kinds are invalidated
// too — a TaskCompleted event whose related_entities[0] is its chain
// invalidates the chain's cached candidates so a subsequent
// parse_context call on the chain slug re-resolves with the new
// pending/closed/blocked counts.
//
// The chain/task/bug shape filter mirrors the per-shape cache policy
// table in cache.go: those three shapes have PolicyShortFiveTurns
// precisely because their underlying state mutates over the chain's
// lifetime — adding event-bus invalidation upgrades the TTL fallback
// to immediate freshness on state changes. Other shapes
// (filesystem, vault, kiwix, etc.) cache indefinitely; they don't
// mutate from event emits.
//
// Fail-open on related_entities JSON parse failure: the primary
// entity is still invalidated, the related sweep just skips. We never
// fail the surrounding emit tx because of an invalidator hiccup —
// stale-cache friction is a UX papercut; an unwritable events table
// is a correctness disaster.
//
// Work-state cache (chain parse-context-lean-orienting T6) is also
// invalidated by the same hook — the work-state surface bundles
// open bugs / active tasks / recent chains, so ANY chain/task/bug
// state change potentially shifts the answer. Coarse-but-correct:
// the hook drops every WorkStateCache entry on the same event types.
//
// Chain parse-context-lean-orienting T1 + T6 — event-bus invalidation
// upgrades the cache's freshness guarantee from "TTL-bounded" to
// "immediate on mutation."
func InstallCacheInvalidationFoldHook(cache *ParseContextCache, workState *WorkStateCache) {
	if cache == nil && workState == nil {
		return
	}
	prev := events.CurrentFoldHook()
	events.SetFoldHook(func(ctx context.Context, tx *sql.Tx, evt events.RawEvent) error {
		invalidateFromEvent(cache, evt)
		if workState != nil && isCacheableEntityKind(evt.EntityKind) {
			workState.InvalidateAll()
		}
		if prev != nil {
			return prev(ctx, tx, evt)
		}
		return nil
	})
}

// invalidateFromEvent applies the per-event invalidation policy.
// Exported in test-only form via the package's _test.go siblings;
// production callers go through the fold hook.
func invalidateFromEvent(cache *ParseContextCache, evt events.RawEvent) {
	if cache == nil {
		return
	}
	if isCacheableEntityKind(evt.EntityKind) && evt.EntitySlug != "" {
		cache.InvalidateToken(evt.EntitySlug)
	}
	if len(evt.RelatedEntities) == 0 {
		return
	}
	var related []events.EntityRef
	if err := json.Unmarshal(evt.RelatedEntities, &related); err != nil {
		return
	}
	for _, r := range related {
		if isCacheableEntityKind(r.Kind) && r.Slug != "" {
			cache.InvalidateToken(r.Slug)
		}
	}
}

// isCacheableEntityKind reports whether the entity kind is one whose
// resolved candidates are cached with a state-sensitive policy
// (PolicyShortFiveTurns). Must stay in sync with the entries in
// PolicyForShape that return that policy.
func isCacheableEntityKind(kind string) bool {
	switch kind {
	case "chain", "task", "bug":
		return true
	default:
		return false
	}
}
