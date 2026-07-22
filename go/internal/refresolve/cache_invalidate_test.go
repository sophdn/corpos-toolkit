package refresolve

import (
	"context"
	"database/sql"
	"testing"

	"toolkit/internal/events"
)

// invalidateFromEvent drops the primary entity's cached slug when it's
// a chain/task/bug; non-cacheable entity kinds (and missing slugs) are
// no-ops.
func TestInvalidateFromEvent_DropsPrimarySlugForCacheableKinds(t *testing.T) {
	cases := []struct {
		name        string
		evt         events.RawEvent
		shouldDrop  bool
		seededShape ShapeCategory
	}{
		{
			name:        "task entity",
			evt:         events.RawEvent{Type: "TaskStarted", EntityKind: "task", EntitySlug: "T1-thing"},
			shouldDrop:  true,
			seededShape: ShapeTaskSlug,
		},
		{
			name:        "chain entity",
			evt:         events.RawEvent{Type: "ChainEdited", EntityKind: "chain", EntitySlug: "some-chain"},
			shouldDrop:  true,
			seededShape: ShapeChainSlug,
		},
		{
			name:        "bug entity",
			evt:         events.RawEvent{Type: "BugResolved", EntityKind: "bug", EntitySlug: "some-bug"},
			shouldDrop:  true,
			seededShape: ShapeBugSlug,
		},
		{
			name:        "unrelated entity kind is a no-op",
			evt:         events.RawEvent{Type: "RoadmapUpdated", EntityKind: "roadmap", EntitySlug: "some-chain"},
			shouldDrop:  false,
			seededShape: ShapeChainSlug,
		},
		{
			name:        "empty slug is a no-op",
			evt:         events.RawEvent{Type: "TaskStarted", EntityKind: "task", EntitySlug: ""},
			shouldDrop:  false,
			seededShape: ShapeTaskSlug,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewParseContextCache()
			token := tc.evt.EntitySlug
			if token == "" {
				token = "some-chain"
			}
			cache.Put("sess", token, tc.seededShape, HitSet{Candidates: []Candidate{{ID: token}}})
			invalidateFromEvent(cache, tc.evt)
			_, _, ok := cache.Get("sess", token, tc.seededShape)
			if tc.shouldDrop && ok {
				t.Errorf("expected cache miss after invalidation, got hit")
			}
			if !tc.shouldDrop && !ok {
				t.Errorf("expected cache hit (entry should survive), got miss")
			}
		})
	}
}

// related_entities sweep: a TaskCompleted event whose related_entities
// names the parent chain invalidates BOTH the task and the chain slugs.
func TestInvalidateFromEvent_SweepsRelatedEntities(t *testing.T) {
	cache := NewParseContextCache()
	cache.Put("sess", "T1-thing", ShapeTaskSlug, HitSet{Candidates: []Candidate{{ID: "T1-thing"}}})
	cache.Put("sess", "parent-chain", ShapeChainSlug, HitSet{Candidates: []Candidate{{ID: "parent-chain"}}})
	cache.Put("sess", "unrelated", ShapeChainSlug, HitSet{Candidates: []Candidate{{ID: "unrelated"}}})

	evt := events.RawEvent{
		Type:            "TaskCompleted",
		EntityKind:      "task",
		EntitySlug:      "T1-thing",
		RelatedEntities: []byte(`[{"kind":"chain","slug":"parent-chain","project_id":null}]`),
	}
	invalidateFromEvent(cache, evt)

	if _, _, ok := cache.Get("sess", "T1-thing", ShapeTaskSlug); ok {
		t.Error("primary entity should have been invalidated")
	}
	if _, _, ok := cache.Get("sess", "parent-chain", ShapeChainSlug); ok {
		t.Error("related chain entity should have been invalidated")
	}
	if _, _, ok := cache.Get("sess", "unrelated", ShapeChainSlug); !ok {
		t.Error("unrelated entity should survive")
	}
}

// Malformed related_entities JSON is fail-open: the primary entity is
// still invalidated; the related sweep silently skips. Mirror of the
// docstring contract — stale-cache friction can't take down an emit.
func TestInvalidateFromEvent_MalformedRelatedEntitiesFailOpen(t *testing.T) {
	cache := NewParseContextCache()
	cache.Put("sess", "T1-thing", ShapeTaskSlug, HitSet{Candidates: []Candidate{{ID: "T1-thing"}}})

	evt := events.RawEvent{
		Type:            "TaskCompleted",
		EntityKind:      "task",
		EntitySlug:      "T1-thing",
		RelatedEntities: []byte(`{not-valid-json`),
	}
	invalidateFromEvent(cache, evt)

	if _, _, ok := cache.Get("sess", "T1-thing", ShapeTaskSlug); ok {
		t.Error("primary entity should still be invalidated even when related_entities JSON is bad")
	}
}

// InstallCacheInvalidationFoldHook chains in front of an existing hook
// (projections.FoldAll in production); both must run. Registers a
// sentinel prev-hook, installs the cache invalidator on top, then
// drives one fake event through events.CurrentFoldHook() and
// confirms (a) the invalidator dropped the matching cache entry and
// (b) the prev-hook fired.
func TestInstallCacheInvalidationFoldHook_ChainsExistingHook(t *testing.T) {
	prevSaved := events.CurrentFoldHook()
	t.Cleanup(func() { events.SetFoldHook(prevSaved) })

	prevRan := false
	events.SetFoldHook(func(_ context.Context, _ *sql.Tx, _ events.RawEvent) error {
		prevRan = true
		return nil
	})

	cache := NewParseContextCache()
	cache.Put("sess", "some-bug", ShapeBugSlug, HitSet{Candidates: []Candidate{{ID: "some-bug"}}})

	InstallCacheInvalidationFoldHook(cache, nil)

	hook := events.CurrentFoldHook()
	if hook == nil {
		t.Fatal("expected fold hook to be installed")
	}
	if err := hook(context.Background(), nil, events.RawEvent{
		Type:       "BugResolved",
		EntityKind: "bug",
		EntitySlug: "some-bug",
	}); err != nil {
		t.Fatalf("fold hook returned error: %v", err)
	}
	if _, _, ok := cache.Get("sess", "some-bug", ShapeBugSlug); ok {
		t.Error("cache invalidator did not run: entry still present")
	}
	if !prevRan {
		t.Error("prev hook did not run after installing cache invalidator on top")
	}
}
