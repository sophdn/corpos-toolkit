package arcreview

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"toolkit/internal/db"
)

// openTestPool mirrors the work package's helper: temp-dir SQLite with
// the full embedded migration set applied. Migration 048 creates the
// arc_review_debouncer table the Debouncer relies on.
func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	pool, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func TestDebouncer_FirstFireAllowed(t *testing.T) {
	pool := openTestPool(t)
	d := NewDebouncer(pool)
	ctx := context.Background()
	check, err := d.CheckAndRecordAttempt(ctx, "sess-1")
	if err != nil {
		t.Fatalf("CheckAndRecordAttempt: %v", err)
	}
	if !check.Allowed {
		t.Fatalf("first fire for a new session must be allowed; got Allowed=false")
	}
}

func TestDebouncer_WithinBackoffSuppressed(t *testing.T) {
	pool := openTestPool(t)
	frozen := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	now := frozen
	clock := func() time.Time { return now }
	d := NewDebouncer(pool).WithBackoffSeconds(60).WithClock(clock)
	ctx := context.Background()

	// First call: allowed.
	c1, err := d.CheckAndRecordAttempt(ctx, "sess-1")
	if err != nil || !c1.Allowed {
		t.Fatalf("first call expected allowed; got %+v err=%v", c1, err)
	}
	if err := d.RecordFire(ctx, "sess-1"); err != nil {
		t.Fatalf("RecordFire: %v", err)
	}

	// 30s later — inside 60s backoff → suppressed.
	now = frozen.Add(30 * time.Second)
	c2, err := d.CheckAndRecordAttempt(ctx, "sess-1")
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if c2.Allowed {
		t.Fatalf("second call within 60s backoff must be suppressed")
	}
	if c2.LastFire.IsZero() {
		t.Fatalf("LastFire must surface when suppressed")
	}
	if !c2.LastFire.Equal(frozen) {
		t.Fatalf("LastFire mismatch: got %v want %v", c2.LastFire, frozen)
	}
}

func TestDebouncer_AfterBackoffAllowed(t *testing.T) {
	pool := openTestPool(t)
	frozen := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	now := frozen
	clock := func() time.Time { return now }
	d := NewDebouncer(pool).WithBackoffSeconds(60).WithClock(clock)
	ctx := context.Background()

	if _, err := d.CheckAndRecordAttempt(ctx, "sess-1"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := d.RecordFire(ctx, "sess-1"); err != nil {
		t.Fatalf("RecordFire: %v", err)
	}

	// 61s later — outside the backoff window.
	now = frozen.Add(61 * time.Second)
	c, err := d.CheckAndRecordAttempt(ctx, "sess-1")
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if !c.Allowed {
		t.Fatalf("second call after 60s backoff must be allowed")
	}
}

func TestDebouncer_ScopedPerSession(t *testing.T) {
	pool := openTestPool(t)
	frozen := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	now := frozen
	clock := func() time.Time { return now }
	d := NewDebouncer(pool).WithBackoffSeconds(60).WithClock(clock)
	ctx := context.Background()

	if _, err := d.CheckAndRecordAttempt(ctx, "sess-A"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := d.RecordFire(ctx, "sess-A"); err != nil {
		t.Fatalf("RecordFire A: %v", err)
	}

	// Same instant — sess-B is a different session; must be allowed.
	c, err := d.CheckAndRecordAttempt(ctx, "sess-B")
	if err != nil {
		t.Fatalf("sess-B: %v", err)
	}
	if !c.Allowed {
		t.Fatalf("sess-B suppressed by sess-A — debouncer must be per-session")
	}
}

func TestDebouncer_EmptySessionRejected(t *testing.T) {
	pool := openTestPool(t)
	d := NewDebouncer(pool)
	_, err := d.CheckAndRecordAttempt(context.Background(), "")
	if err == nil {
		t.Fatalf("expected error for empty session_id")
	}
}

func TestDebouncer_RecordFirePersistsAcrossInstances(t *testing.T) {
	pool := openTestPool(t)
	frozen := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	now := frozen
	clock := func() time.Time { return now }
	ctx := context.Background()

	d1 := NewDebouncer(pool).WithBackoffSeconds(60).WithClock(clock)
	if _, err := d1.CheckAndRecordAttempt(ctx, "sess-X"); err != nil {
		t.Fatalf("d1 first call: %v", err)
	}
	if err := d1.RecordFire(ctx, "sess-X"); err != nil {
		t.Fatalf("d1 RecordFire: %v", err)
	}

	// Fresh Debouncer instance against the same pool — must see the
	// persisted last_fire_at row from d1.
	now = frozen.Add(10 * time.Second)
	d2 := NewDebouncer(pool).WithBackoffSeconds(60).WithClock(clock)
	c, err := d2.CheckAndRecordAttempt(ctx, "sess-X")
	if err != nil {
		t.Fatalf("d2 call: %v", err)
	}
	if c.Allowed {
		t.Fatalf("d2 must read d1's fire; got Allowed=true")
	}
}

// TestDebouncer_InFlightAttemptSuppressesSecondAttempt is the bug 1476
// regression: two triggers land 16s apart, the first's RecordFire hasn't
// run yet (still inside Qwen), the second arrives and previously passed
// CheckAndRecordAttempt because last_fire_at was stale. The fix gates
// on MAX(last_fire_at, last_fire_attempt_at), so the first attempt's
// synchronously-stamped attempt timestamp now suppresses the second.
func TestDebouncer_InFlightAttemptSuppressesSecondAttempt(t *testing.T) {
	pool := openTestPool(t)
	frozen := time.Date(2026, 5, 20, 3, 2, 32, 0, time.UTC)
	now := frozen
	clock := func() time.Time { return now }
	d := NewDebouncer(pool).WithBackoffSeconds(60).WithClock(clock)
	ctx := context.Background()

	// First attempt: allowed. RecordFire DOES NOT run — simulates an
	// in-flight Qwen call.
	c1, err := d.CheckAndRecordAttempt(ctx, "sess-race")
	if err != nil || !c1.Allowed {
		t.Fatalf("first attempt expected allowed; got %+v err=%v", c1, err)
	}

	// 16s later — well inside the 60s window. last_fire_at is still
	// NULL (RecordFire hasn't fired), but last_fire_attempt_at = T+0.
	// Pre-fix this returned Allowed=true; post-fix the attempt gate
	// kicks in.
	now = frozen.Add(16 * time.Second)
	c2, err := d.CheckAndRecordAttempt(ctx, "sess-race")
	if err != nil {
		t.Fatalf("second attempt err: %v", err)
	}
	if c2.Allowed {
		t.Fatalf("second attempt within backoff must be suppressed by in-flight attempt gate (bug 1476)")
	}
	if !c2.LastFire.Equal(frozen) {
		t.Fatalf("LastFire should surface the prior attempt timestamp; got %v want %v", c2.LastFire, frozen)
	}
}
