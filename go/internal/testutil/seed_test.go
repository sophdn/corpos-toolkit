package testutil_test

import (
	"database/sql"
	"testing"

	"toolkit/internal/testutil"
)

// TestSeedBug_OpenWritesNullResolvedAt pins migration 066's biconditional
// invariant from the helper side: status='open' → resolved_at IS NULL.
func TestSeedBug_OpenWritesNullResolvedAt(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	id := testutil.SeedBug(t, pool, "p", "open-bug", "open", testutil.SeedBugOpts{})
	var resolvedAt sql.NullString
	if err := pool.DB().QueryRow(
		`SELECT resolved_at FROM proj_current_bugs WHERE id = ?`, id,
	).Scan(&resolvedAt); err != nil {
		t.Fatal(err)
	}
	if resolvedAt.Valid {
		t.Errorf("status='open' should give NULL resolved_at; got %q", resolvedAt.String)
	}
}

// TestSeedBug_TerminalWritesDefaultResolvedAt pins the other half of the
// biconditional: any non-'open' status gets the fixture default
// timestamp (CHECK fails otherwise).
func TestSeedBug_TerminalWritesDefaultResolvedAt(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	for _, status := range []string{"fixed", "wontfix", "upstream", "dup", "routed"} {
		id := testutil.SeedBug(t, pool, "p", "t-"+status, status, testutil.SeedBugOpts{})
		var resolvedAt sql.NullString
		if err := pool.DB().QueryRow(
			`SELECT resolved_at FROM proj_current_bugs WHERE id = ?`, id,
		).Scan(&resolvedAt); err != nil {
			t.Fatal(err)
		}
		if !resolvedAt.Valid {
			t.Errorf("status=%q should give non-NULL resolved_at", status)
		}
	}
}

// TestSeedBug_ResolvedAtOverride confirms opts.ResolvedAt wins over the
// fixture default for non-'open' statuses.
func TestSeedBug_ResolvedAtOverride(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	id := testutil.SeedBug(t, pool, "p", "ov", "fixed", testutil.SeedBugOpts{
		ResolvedAt: "2025-01-01T00:00:00Z",
	})
	var got string
	if err := pool.DB().QueryRow(
		`SELECT resolved_at FROM proj_current_bugs WHERE id = ?`, id,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "2025-01-01T00:00:00Z" {
		t.Errorf("override ignored: got %q", got)
	}
}

// TestSeedBug_DefaultTitle pins the "T:<slug>" fallback.
func TestSeedBug_DefaultTitle(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	id := testutil.SeedBug(t, pool, "p", "no-title", "open", testutil.SeedBugOpts{})
	var title string
	if err := pool.DB().QueryRow(
		`SELECT title FROM proj_current_bugs WHERE id = ?`, id,
	).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "T:no-title" {
		t.Errorf("default title: got %q, want T:no-title", title)
	}
}

// TestSeedTask_ClosedWritesDefaultCommitSHA pins migration 066:
// status='closed' implies commit_sha IS NOT NULL.
func TestSeedTask_ClosedWritesDefaultCommitSHA(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	chainID := testutil.SeedChain(t, pool, "p", "c", "open", testutil.SeedChainOpts{})
	id := testutil.SeedTask(t, pool, chainID, "t", "closed", testutil.SeedTaskOpts{})
	var sha sql.NullString
	if err := pool.DB().QueryRow(
		`SELECT commit_sha FROM proj_current_tasks WHERE id = ?`, id,
	).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	if !sha.Valid {
		t.Errorf("status='closed' should give non-NULL commit_sha")
	}
}

// TestSeedTask_NonClosedWritesNullCommitSHA pins the inverse.
func TestSeedTask_NonClosedWritesNullCommitSHA(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	chainID := testutil.SeedChain(t, pool, "p", "c", "open", testutil.SeedChainOpts{})
	for _, status := range []string{"pending", "active", "blocked"} {
		id := testutil.SeedTask(t, pool, chainID, "t-"+status, status, testutil.SeedTaskOpts{})
		var sha sql.NullString
		if err := pool.DB().QueryRow(
			`SELECT commit_sha FROM proj_current_tasks WHERE id = ?`, id,
		).Scan(&sha); err != nil {
			t.Fatal(err)
		}
		if sha.Valid {
			t.Errorf("status=%q should give NULL commit_sha; got %q", status, sha.String)
		}
	}
}

// TestSeedTask_PositionAutoIncrement pins that default position fills
// the next slot within a chain, monotonically.
func TestSeedTask_PositionAutoIncrement(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	chainID := testutil.SeedChain(t, pool, "p", "c", "open", testutil.SeedChainOpts{})
	testutil.SeedTask(t, pool, chainID, "t1", "pending", testutil.SeedTaskOpts{})
	testutil.SeedTask(t, pool, chainID, "t2", "pending", testutil.SeedTaskOpts{})
	testutil.SeedTask(t, pool, chainID, "t3", "pending", testutil.SeedTaskOpts{})
	rows, err := pool.DB().Query(
		`SELECT slug, position FROM proj_current_tasks WHERE chain_id = ? ORDER BY position`,
		chainID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []struct {
		slug string
		pos  int64
	}{{"t1", 1}, {"t2", 2}, {"t3", 3}}
	i := 0
	for rows.Next() {
		var slug string
		var pos int64
		if err := rows.Scan(&slug, &pos); err != nil {
			t.Fatal(err)
		}
		if i >= len(want) || slug != want[i].slug || pos != want[i].pos {
			t.Errorf("row %d = (%q, %d), want %+v", i, slug, pos, want[i])
		}
		i++
	}
}

// TestSeedChain_ClosedWritesEmptyClosureSummary pins migration 066's
// closure-summary invariant on chains: closed implies NOT NULL. The
// empty string (column default) satisfies the CHECK.
func TestSeedChain_ClosedWritesEmptyClosureSummary(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	id := testutil.SeedChain(t, pool, "p", "closed-chain", "closed", testutil.SeedChainOpts{})
	var summary sql.NullString
	if err := pool.DB().QueryRow(
		`SELECT closure_summary FROM proj_chain_status WHERE id = ?`, id,
	).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if !summary.Valid {
		t.Errorf("status='closed' should give non-NULL closure_summary")
	}
}

// TestRefreshChainCounters_RecomputesFromTaskRows pins the helper's
// arithmetic: total_tasks + per-status buckets match the live task
// distribution.
func TestRefreshChainCounters_RecomputesFromTaskRows(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedProject(t, pool, "p")
	c := testutil.SeedChain(t, pool, "p", "c", "open", testutil.SeedChainOpts{})
	testutil.SeedTask(t, pool, c, "p1", "pending", testutil.SeedTaskOpts{})
	testutil.SeedTask(t, pool, c, "p2", "pending", testutil.SeedTaskOpts{})
	testutil.SeedTask(t, pool, c, "a1", "active", testutil.SeedTaskOpts{})
	testutil.SeedTask(t, pool, c, "x1", "closed", testutil.SeedTaskOpts{})

	testutil.RefreshChainCounters(t, pool, c)

	var total, pending, active, blocked, closed, cancelled int64
	if err := pool.DB().QueryRow(
		`SELECT total_tasks, pending, active, blocked, closed, cancelled
		 FROM proj_chain_status WHERE id = ?`, c,
	).Scan(&total, &pending, &active, &blocked, &closed, &cancelled); err != nil {
		t.Fatal(err)
	}
	if total != 4 || pending != 2 || active != 1 || closed != 1 || blocked != 0 || cancelled != 0 {
		t.Errorf("counters: total=%d pending=%d active=%d blocked=%d closed=%d cancelled=%d; want 4/2/1/0/1/0",
			total, pending, active, blocked, closed, cancelled)
	}
}
