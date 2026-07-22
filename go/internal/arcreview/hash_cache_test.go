package arcreview

import (
	"context"
	"testing"
	"time"
)

func TestArcHashFromSnapshot_Deterministic(t *testing.T) {
	snap := Snapshot{
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}
	h1 := arcHashFromSnapshot(snap)
	h2 := arcHashFromSnapshot(snap)
	if h1 != h2 {
		t.Fatalf("hash must be deterministic; got %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("expected sha256 hex (64 chars); got %d", len(h1))
	}
}

func TestArcHashFromSnapshot_DifferentContentDifferentHash(t *testing.T) {
	a := arcHashFromSnapshot(Snapshot{Messages: []Message{{Role: "user", Content: "a"}}})
	b := arcHashFromSnapshot(Snapshot{Messages: []Message{{Role: "user", Content: "b"}}})
	if a == b {
		t.Fatalf("distinct content must produce distinct hashes")
	}
}

func TestArcHashCache_MissOnEmptyTable(t *testing.T) {
	pool := openTestPool(t)
	cache := NewArcHashCache(pool)
	_, hit, err := cache.Lookup(context.Background(), "sess-1", "hash-1")
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if hit {
		t.Fatalf("expected miss on empty table")
	}
}

func TestArcHashCache_HitWithinTTL(t *testing.T) {
	pool := openTestPool(t)
	cache := NewArcHashCache(pool).WithTTLSeconds(600)
	ctx := context.Background()
	if err := cache.Record(ctx, "sess-1", "hash-1", "evt-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	entry, hit, err := cache.Lookup(ctx, "sess-1", "hash-1")
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if !hit {
		t.Fatalf("expected hit after Record within TTL")
	}
	if entry.PriorEventID != "evt-1" {
		t.Errorf("PriorEventID = %q; want evt-1", entry.PriorEventID)
	}
}

func TestArcHashCache_MissBeyondTTL(t *testing.T) {
	pool := openTestPool(t)
	frozen := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	now := frozen
	clk := func() time.Time { return now }
	cache := NewArcHashCache(pool).WithTTLSeconds(60).WithClock(clk)
	ctx := context.Background()
	if err := cache.Record(ctx, "sess-1", "hash-1", "evt-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Advance the clock past TTL.
	now = frozen.Add(2 * time.Minute)
	_, hit, err := cache.Lookup(ctx, "sess-1", "hash-1")
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if hit {
		t.Fatalf("expected miss beyond TTL")
	}
}

func TestArcHashCache_DifferentSessionsIndependent(t *testing.T) {
	pool := openTestPool(t)
	cache := NewArcHashCache(pool)
	ctx := context.Background()
	if err := cache.Record(ctx, "sess-1", "shared-hash", "evt-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	_, hit, err := cache.Lookup(ctx, "sess-2", "shared-hash")
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if hit {
		t.Fatalf("different session must not hit on the same hash")
	}
}

func TestArcHashCache_DifferentHashesIndependent(t *testing.T) {
	pool := openTestPool(t)
	cache := NewArcHashCache(pool)
	ctx := context.Background()
	if err := cache.Record(ctx, "sess-1", "hash-1", "evt-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	_, hit, err := cache.Lookup(ctx, "sess-1", "hash-2")
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if hit {
		t.Fatalf("different hash on same session must not hit")
	}
}

func TestArcHashCache_RecordReplacesOnConflict(t *testing.T) {
	pool := openTestPool(t)
	cache := NewArcHashCache(pool)
	ctx := context.Background()
	if err := cache.Record(ctx, "sess-1", "hash-1", "evt-old"); err != nil {
		t.Fatalf("Record first: %v", err)
	}
	if err := cache.Record(ctx, "sess-1", "hash-1", "evt-new"); err != nil {
		t.Fatalf("Record second: %v", err)
	}
	entry, hit, _ := cache.Lookup(ctx, "sess-1", "hash-1")
	if !hit || entry.PriorEventID != "evt-new" {
		t.Fatalf("ON CONFLICT should overwrite prior_event_id; got hit=%v entry=%+v", hit, entry)
	}
}
