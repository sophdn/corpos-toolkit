package work_test

import (
	"context"
	"testing"

	"toolkit/internal/work"
)

// Regression net for bug
// `task-blocker-edge-fold-resolves-by-project-slug-ignoring-chain-fans-out`.
//
// foldTaskBlockersTransitioned / foldTaskBlockersCleanupOnClose resolved the
// blocked (and closing) task id by (project_id, slug) only — no chain scope,
// no ORDER BY. When a task slug recurs across chains (generic step slugs like
// "dup" / audit-eight-axes do), the edge INSERT/DELETE could land on an
// arbitrary same-slug task in the wrong chain — an UNSPECIFIED-ORDER latent
// bug. (It is non-deterministic: SQLite returns the correct row in these
// setups, so these tests pass on the pre-fix code too — they PIN the intended
// chain-scoped behavior rather than demonstrating red→green. The fix removes
// the dependence on query order by resolving via the chain the event carries.)

// Edge INSERT must attach to the blocked task's OWN chain.
func TestTaskBlock_EdgeTargetsCorrectChainWhenSlugDuplicated(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedTask(t, pool, "c1", "dup", "pending") // target, seeded first → lower id
	seedTask(t, pool, "c1", "c1blocker", "pending")
	seedChain(t, pool, "mcp-servers", "c2")
	seedTask(t, pool, "c2", "dup", "pending") // sibling, later id → returned by unordered query

	if _, err := work.HandleTaskBlock(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{
			"slug": "dup", "chain_slug": "c1",
			"blocker_slug": "c1blocker", "blocker_chain_slug": "c1",
		})); err != nil {
		t.Fatalf("HandleTaskBlock: %v", err)
	}

	edgeOn := func(chain string) int {
		var n int
		pool.DB().QueryRow(`
			SELECT COUNT(*) FROM proj_task_blockers tb
			JOIN proj_current_tasks t ON tb.blocked_task_id = t.id
			JOIN proj_chain_status c ON t.chain_id = c.id
			WHERE t.slug = 'dup' AND c.slug = ?`, chain).Scan(&n)
		return n
	}
	if edgeOn("c1") != 1 {
		t.Errorf("edge must attach to c1.dup (the blocked task's chain); got %d", edgeOn("c1"))
	}
	if edgeOn("c2") != 0 {
		t.Errorf("edge must NOT attach to c2.dup (different chain); got %d", edgeOn("c2"))
	}
}

// Edge DELETE on unblock must remove the edge from the blocked task's OWN
// chain, not a sibling's same-slug task.
func TestTaskUnblock_EdgeRemovalTargetsCorrectChainWhenSlugDuplicated(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedTask(t, pool, "c1", "dup", "blocked") // target, seeded first → lower id
	seedTask(t, pool, "c1", "c1blocker", "pending")
	seedChain(t, pool, "mcp-servers", "c2")
	seedTask(t, pool, "c2", "dup", "blocked") // sibling, later id → returned by unordered query
	seedTask(t, pool, "c2", "c2blocker", "pending")

	// Give BOTH dups a structural edge directly (bypassing block, to isolate
	// the unblock fold).
	for _, ch := range []struct{ chain, blk string }{{"c1", "c1blocker"}, {"c2", "c2blocker"}} {
		var depID, blkID int64
		pool.DB().QueryRow(`SELECT t.id FROM proj_current_tasks t JOIN proj_chain_status c ON c.id=t.chain_id WHERE t.slug='dup' AND c.slug=?`, ch.chain).Scan(&depID)
		pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug=?`, ch.blk).Scan(&blkID)
		pool.DB().Exec(`INSERT INTO proj_task_blockers (blocked_task_id, blocker_task_id, created_at) VALUES (?,?,datetime('now'))`, depID, blkID)
	}

	// Unblock c1.dup (unblock-all form).
	if _, err := work.HandleTaskUnblock(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "dup", "chain_slug": "c1"})); err != nil {
		t.Fatalf("HandleTaskUnblock: %v", err)
	}

	edgeOn := func(chain string) int {
		var n int
		pool.DB().QueryRow(`
			SELECT COUNT(*) FROM proj_task_blockers tb
			JOIN proj_current_tasks t ON tb.blocked_task_id = t.id
			JOIN proj_chain_status c ON t.chain_id = c.id
			WHERE t.slug = 'dup' AND c.slug = ?`, chain).Scan(&n)
		return n
	}
	if edgeOn("c1") != 0 {
		t.Errorf("c1.dup edge should be removed; got %d", edgeOn("c1"))
	}
	if edgeOn("c2") != 1 {
		t.Errorf("c2.dup edge must be untouched (different chain); got %d", edgeOn("c2"))
	}
}
