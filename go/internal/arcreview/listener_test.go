package arcreview

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"toolkit/internal/events"
)

// captureObserver records every trigger event for assertion in tests.
// Mutex-guarded because the fold hook runs inside an emit-time tx; in
// production the same goroutine writes, but tests may parallel-run.
type captureObserver struct {
	mu     sync.Mutex
	events []SubstrateTriggerEvent
}

func (c *captureObserver) Observe(_ context.Context, evt SubstrateTriggerEvent) {
	c.mu.Lock()
	c.events = append(c.events, evt)
	c.mu.Unlock()
}

func (c *captureObserver) snapshot() []SubstrateTriggerEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SubstrateTriggerEvent, len(c.events))
	copy(out, c.events)
	return out
}

// withRestoredFoldHook captures the current fold hook and restores it
// after the test, so installing a listener in one test doesn't leak
// state into siblings.
func withRestoredFoldHook(t *testing.T) {
	t.Helper()
	prev := events.CurrentFoldHook()
	t.Cleanup(func() { events.SetFoldHook(prev) })
}

func TestListener_DetectsBugResolvedTrigger(t *testing.T) {
	withRestoredFoldHook(t)
	pool := openTestPool(t)
	obs := &captureObserver{}
	InstallListenerFoldHook(obs)

	// Seed a bug row so the BugResolved event has a real entity to
	// hang off (the entity_slug is the row's slug).
	_, err := pool.DB().Exec(`
		INSERT INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	ctx := context.Background()
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("bug", "test-bug-slug", "mcp-servers"),
			Payload: events.BugResolvedPayload{
				Kind: "fixed",
			},
		})
		return emitErr
	})
	if err != nil {
		t.Fatalf("emit BugResolved: %v", err)
	}

	got := obs.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 trigger event captured, got %d (%+v)", len(got), got)
	}
	if got[0].EventType != "BugResolved" {
		t.Fatalf("expected EventType=BugResolved, got %q", got[0].EventType)
	}
	if got[0].TriggerSlug != "event_bug_resolved" {
		t.Fatalf("expected trigger_slug=event_bug_resolved, got %q", got[0].TriggerSlug)
	}
	if got[0].EntityKind != "bug" || got[0].EntitySlug != "test-bug-slug" {
		t.Fatalf("entity ref mismatch: %+v", got[0])
	}
}

func TestListener_IgnoresNonTriggerEvent(t *testing.T) {
	withRestoredFoldHook(t)
	pool := openTestPool(t)
	obs := &captureObserver{}
	InstallListenerFoldHook(obs)

	// BugReported is NOT in SubstrateTriggerEventTypes; emit one and
	// confirm the observer sees nothing.
	_, err := pool.DB().Exec(`
		INSERT INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	ctx := context.Background()
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("bug", "another-bug", "mcp-servers"),
			Payload: events.BugReportedPayload{
				Title:            "t",
				ProblemStatement: "p",
			},
		})
		return emitErr
	})
	if err != nil {
		t.Fatalf("emit BugReported: %v", err)
	}
	if got := obs.snapshot(); len(got) != 0 {
		t.Fatalf("BugReported should NOT trigger; got %d observations: %+v", len(got), got)
	}
}

func TestListener_FoldHookChainPreserved(t *testing.T) {
	withRestoredFoldHook(t)
	pool := openTestPool(t)

	// Install a leading fold hook that counts every event.
	var leadingCount int
	events.SetFoldHook(func(_ context.Context, _ *sql.Tx, _ events.RawEvent) error {
		leadingCount++
		return nil
	})

	obs := &captureObserver{}
	InstallListenerFoldHook(obs) // chains in front of the leading hook

	_, err := pool.DB().Exec(`
		INSERT INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	ctx := context.Background()
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("chain", "test-chain", "mcp-servers"),
			Payload: events.ChainClosedPayload{},
		})
		return emitErr
	})
	if err != nil {
		t.Fatalf("emit ChainClosed: %v", err)
	}

	if leadingCount != 1 {
		t.Fatalf("leading hook must still fire after chaining; count=%d", leadingCount)
	}
	if got := obs.snapshot(); len(got) != 1 || got[0].EventType != "ChainClosed" {
		t.Fatalf("listener must still see triggers when chained; got %+v", got)
	}
}
