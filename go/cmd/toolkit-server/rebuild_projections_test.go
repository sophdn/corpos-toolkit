package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/projections"
	"toolkit/internal/testutil"
)

// seedClosedTask wraps [testutil.SeedTask] with position=1 default
// and the closed-status invariant the fixture-commit_sha satisfies.
func seedClosedTask(t *testing.T, pool *db.Pool, chainID int64, slug string) {
	t.Helper()
	testutil.SeedTask(t, pool, chainID, slug, "closed", testutil.SeedTaskOpts{Position: 1})
}

// seedOpenChain wraps [testutil.SeedChain] for the mcp-servers project
// in status='open' and returns the assigned id.
func seedOpenChain(t *testing.T, pool *db.Pool, slug string) int64 {
	t.Helper()
	return testutil.SeedChain(t, pool, "mcp-servers", slug, "open", testutil.SeedChainOpts{})
}

// TestWriteAutoSnapshot_WritesParseableSqliteFile asserts the snapshot
// path returned by writeAutoSnapshot points at a valid SQLite database
// with the canonical schema (proj_* tables and watermark visible).
func TestWriteAutoSnapshot_WritesParseableSqliteFile(t *testing.T) {
	pool, dbPath := openFreshPool(t)
	defer pool.Close()

	ctx := context.Background()
	snapPath, err := writeAutoSnapshot(ctx, pool, dbPath)
	if err != nil {
		t.Fatalf("writeAutoSnapshot: %v", err)
	}
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}
	if !strings.Contains(snapPath, snapshotPrefix) {
		t.Errorf("snapshot path missing convention prefix: %s", snapPath)
	}

	// Open the snapshot and verify the schema landed.
	snap, err := sql.Open("sqlite", snapPath)
	if err != nil {
		t.Fatalf("open snap: %v", err)
	}
	defer snap.Close()
	var n int
	if err := snap.QueryRow(`SELECT COUNT(*) FROM proj_current_bugs`).Scan(&n); err != nil {
		t.Errorf("query snap: %v", err)
	}
}

// TestRotateSnapshots_KeepsLastN seeds 12 placeholder snapshot files
// + asserts only the 10 lexically-newest survive a rotation.
func TestRotateSnapshots_KeepsLastN(t *testing.T) {
	dir := t.TempDir()
	dbBase := "toolkit.db"
	// Seed 12 placeholder snapshots with monotonic ISO 8601 suffixes.
	for i := 0; i < snapshotKeepCount+2; i++ {
		ts := time.Date(2026, 5, 22, i, 0, 0, 0, time.UTC).Format("2006-01-02T15-04-05Z")
		path := filepath.Join(dir, dbBase+snapshotPrefix+ts+".db")
		if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := rotateSnapshots(dir, dbBase); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, dbBase+snapshotPrefix+"*.db"))
	if len(matches) != snapshotKeepCount {
		t.Errorf("after rotation: want %d files, got %d (%v)", snapshotKeepCount, len(matches), matches)
	}
	// Verify the OLDEST two timestamps are the ones deleted (hour=0,1).
	for _, p := range matches {
		if strings.Contains(p, "T00-") || strings.Contains(p, "T01-") {
			t.Errorf("oldest snapshot should have been deleted but found %s", p)
		}
	}
}

// TestStateCounts_DetectsRegressionDirections covers the three axes
// the post-rebuild guard watches. Pre has fewer rows in each axis;
// post has more; regression should fire on each.
func TestStateCounts_DetectsRegressionDirections(t *testing.T) {
	pre := stateCounts{OpenBugs: 5, PendingTasks: 100, OpenChains: 10}
	post := stateCounts{OpenBugs: 6, PendingTasks: 100, OpenChains: 10}
	regs := pre.regressionsVs(post)
	if len(regs) != 1 || !strings.Contains(regs[0], "open bugs") {
		t.Errorf("open-bugs only: want 1 reg, got %v", regs)
	}

	post = stateCounts{OpenBugs: 5, PendingTasks: 200, OpenChains: 11}
	regs = pre.regressionsVs(post)
	if len(regs) != 2 {
		t.Errorf("pending+chains: want 2 regs, got %v", regs)
	}

	post = stateCounts{OpenBugs: 5, PendingTasks: 100, OpenChains: 10}
	regs = pre.regressionsVs(post)
	if len(regs) != 0 {
		t.Errorf("identical: want 0 regs, got %v", regs)
	}

	// Post WITH FEWER rows is NOT a regression — entities legitimately
	// closing should never trigger the guard.
	post = stateCounts{OpenBugs: 3, PendingTasks: 50, OpenChains: 5}
	regs = pre.regressionsVs(post)
	if len(regs) != 0 {
		t.Errorf("post fewer: want 0 regs (no regression direction), got %v", regs)
	}
}

// TestRestoreFromSnapshot_RoundTrip verifies the restore path:
// seed → snapshot → mutate (DELETE rows) → restore → verify rows
// match the pre-mutation state.
func TestRestoreFromSnapshot_RoundTrip(t *testing.T) {
	pool, dbPath := openFreshPool(t)
	defer pool.Close()
	ctx := context.Background()

	chainID := seedOpenChain(t, pool, "test-chain")
	seedClosedTask(t, pool, chainID, "t1")
	seedClosedTask(t, pool, chainID, "t2")

	preCount := countTasks(t, pool)
	if preCount != 2 {
		t.Fatalf("pre-snapshot task count: want 2, got %d", preCount)
	}

	snapPath, err := writeAutoSnapshot(ctx, pool, dbPath)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Mutate after the snapshot: delete all tasks.
	if _, err := pool.DB().Exec(`DELETE FROM proj_current_tasks`); err != nil {
		t.Fatal(err)
	}
	if countTasks(t, pool) != 0 {
		t.Fatal("post-mutate: want 0 tasks")
	}

	// Restore.
	if err := restoreFromSnapshot(ctx, pool, snapPath); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if countTasks(t, pool) != preCount {
		t.Errorf("post-restore: want %d, got %d", preCount, countTasks(t, pool))
	}
}

// TestRebuildFromSnapshot_SeedPlusZeroEvents tests the
// "snapshot + 0 events" path: rebuilding from a snapshot that's
// already current should produce the snapshot's exact state.
func TestRebuildFromSnapshot_SeedPlusZeroEvents(t *testing.T) {
	pool, dbPath := openFreshPool(t)
	defer pool.Close()
	ctx := context.Background()

	chainID := seedOpenChain(t, pool, "test-chain")
	seedClosedTask(t, pool, chainID, "t1")

	snapPath, err := writeAutoSnapshot(ctx, pool, dbPath)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Truncate proj_* so we can verify rebuild repopulates from snap.
	if _, err := pool.DB().Exec(`DELETE FROM proj_current_tasks`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.DB().Exec(`DELETE FROM proj_chain_status`); err != nil {
		t.Fatal(err)
	}

	if rc := rebuildFromSnapshot(ctx, pool, snapPath); rc != 0 {
		t.Fatalf("rebuildFromSnapshot returned %d", rc)
	}

	if countTasks(t, pool) != 1 {
		t.Errorf("post-rebuild tasks: want 1, got %d", countTasks(t, pool))
	}
	if countChains(t, pool) != 1 {
		t.Errorf("post-rebuild chains: want 1, got %d", countChains(t, pool))
	}
}

// TestRebuildFromSnapshot_MissingFileSurfacesError asserts a clean
// error for a non-existent --from-snapshot path (rather than an opaque
// ATTACH failure).
func TestRebuildFromSnapshot_MissingFileSurfacesError(t *testing.T) {
	pool, _ := openFreshPool(t)
	defer pool.Close()
	ctx := context.Background()
	rc := rebuildFromSnapshot(ctx, pool, "/tmp/does-not-exist-snapshot.db")
	if rc != 1 {
		t.Errorf("want rc=1 for missing snapshot, got %d", rc)
	}
}

// openFreshPool returns a Pool + the on-disk path of a temp DB. The
// migration set is auto-applied by db.Open.
func openFreshPool(t *testing.T) (*db.Pool, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	pool, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	// Seed the mcp-servers project row so FK-style fixture inserts
	// don't trip on missing-project guards.
	if _, err := pool.DB().Exec(
		`INSERT OR IGNORE INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return pool, path
}

func countTasks(t *testing.T, pool *db.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_current_tasks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countChains(t *testing.T, pool *db.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_chain_status`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// _ keeps the testutil + projections imports used by side-effect.
var _ = testutil.NewTestDB
var _ = projections.All
