package work_test

import (
	"context"
	"testing"

	"toolkit/internal/work"
)

// Regression net for bug
// `task-unblock-noops-on-status-only-block-and-complete-sweep-doesnt-fire`.
//
// A status-only block is status='blocked' with NO structural
// proj_task_blockers edge — the shape forge(chain, tasks=[...]) produces
// for inline status=blocked entries (the chain's design_decisions prose
// records the dep; no task_block call ever made it structural).

// FAILURE 1 (root-caused): task_unblock on a status-only-blocked task
// returns {ok:true} but never flips it to pending. HandleTaskUnblock's
// status flip is gated on len(removedBlockerSlugs) > 0; a status-only
// block has zero edges, so the flip is skipped while success is reported.
// The task_blockers status_only diagnostic literally advises "Run
// task_unblock if the dep cleared" — so task_unblock MUST honor it.
func TestTaskUnblock_StatusOnlyBlockedFlipsToPending(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "blocked-no-edges", "blocked") // status-only: no edge

	resp, err := work.HandleTaskUnblock(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "blocked-no-edges"}))
	if err != nil {
		t.Fatalf("HandleTaskUnblock: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected ok=true; got %+v", resp)
	}

	var status string
	if err := pool.DB().QueryRow(
		`SELECT status FROM proj_current_tasks WHERE slug = 'blocked-no-edges'`).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "pending" {
		t.Errorf("status-only unblock must flip blocked -> pending; got %q (the {ok:true} no-op bug)", status)
	}

	// Ledger correctness: exactly one blocked->pending transition, carrying
	// the canonical chain slug so the fold targets this task and doesn't
	// fan out across same-slug tasks in other chains.
	var n int64
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*) FROM events
		WHERE type = 'TaskTransitioned'
		  AND entity_slug = 'blocked-no-edges'
		  AND json_extract(payload, '$.from_status') = 'blocked'
		  AND json_extract(payload, '$.to_status') = 'pending'
		  AND json_extract(payload, '$.chain_slug') = 'c'`).Scan(&n); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if n != 1 {
		t.Errorf("expected exactly 1 chain-scoped blocked->pending event; got %d", n)
	}
}

// Idempotency: unblocking a non-blocked task must not fabricate a
// spurious transition. A pending task is already actionable; task_unblock
// should be a clean no-op (ok, no event) rather than emitting pending->pending.
func TestTaskUnblock_NonBlockedTaskIsCleanNoop(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "already-pending", "pending")

	resp, err := work.HandleTaskUnblock(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "already-pending"}))
	if err != nil {
		t.Fatalf("HandleTaskUnblock: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected ok=true; got %+v", resp)
	}
	var status string
	if err := pool.DB().QueryRow(
		`SELECT status FROM proj_current_tasks WHERE slug = 'already-pending'`).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "pending" {
		t.Errorf("non-blocked task should stay pending; got %q", status)
	}
	var n int64
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*) FROM events
		WHERE type = 'TaskTransitioned' AND entity_slug = 'already-pending'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("non-blocked unblock should emit no transition; got %d", n)
	}
}

// FAILURE 2 (observed live, suspected environmental): the task_complete
// prose-only sweep on a chain whose blocker topology MIXES status-only
// blocks (positions with no edge) with a structural edge whose blocker is
// ITSELF a status-only-blocked task — the exact shape of the chain that
// surfaced this bug (refactor-handler-parse-context-core: positions 3/4/5
// status-only blocked, position 6 structurally blocked by position 5).
// Completing position 2 must sweep the status-only blocks (3/4/5) to
// pending while leaving the structurally-blocked position 6 blocked.
func TestTaskComplete_SweepWithMixedStatusOnlyAndStructuralBlockers(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t2", "active")  // completing
	seedTask(t, pool, "c", "t3", "blocked") // status-only
	seedTask(t, pool, "c", "t4", "blocked") // status-only
	seedTask(t, pool, "c", "t5", "blocked") // status-only AND the structural blocker of t6
	seedTask(t, pool, "c", "t6", "blocked") // structurally blocked by t5

	var t6id, t5id int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='t6'`).Scan(&t6id)
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='t5'`).Scan(&t5id)
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_task_blockers (blocked_task_id, blocker_task_id, created_at)
		 VALUES (?, ?, datetime('now'))`, t6id, t5id); err != nil {
		t.Fatalf("seed structural blocker: %v", err)
	}

	if _, err := work.HandleTaskComplete(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "t2", "handoff_output": "done"})); err != nil {
		t.Fatalf("HandleTaskComplete: %v", err)
	}

	expect := map[string]string{
		"t2": "closed",
		"t3": "pending", // status-only, swept
		"t4": "pending", // status-only, swept
		"t5": "pending", // status-only, swept (even though it blocks t6)
		"t6": "blocked", // structural blocker t5 still open (not closed) → stays blocked
	}
	for slug, want := range expect {
		var got string
		if err := pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = ?`, slug).Scan(&got); err != nil {
			t.Fatalf("lookup %s: %v", slug, err)
		}
		if got != want {
			t.Errorf("%s: want %q, got %q", slug, want, got)
		}
	}
}

// The prose-only sweep must NOT fan out across chains that share a task
// slug. The sweep emits TaskTransitioned events; if those carry an empty
// chain_slug the fold targets every same-slug task in the project (the
// `task-lifecycle-event-folds-fan-out-across-duplicate-task-slugs` class).
// Generic chain-step slugs (audit-eight-axes, triage-gate, …) recur across
// many chains, so this is the common case, not a corner. c1 and c2 both
// have a status-only-blocked "dup"; completing a task in c1 must leave
// c2's "dup" blocked.
func TestTaskComplete_SweepDoesNotFanOutAcrossDuplicateSlugChains(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")
	seedTask(t, pool, "c1", "done-task", "active")
	seedTask(t, pool, "c1", "dup", "blocked")
	seedTask(t, pool, "c2", "dup", "blocked")

	if _, err := work.HandleTaskComplete(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "done-task", "chain_slug": "c1", "handoff_output": "x"})); err != nil {
		t.Fatalf("HandleTaskComplete: %v", err)
	}

	get := func(chain string) string {
		var s string
		if err := pool.DB().QueryRow(`SELECT t.status FROM proj_current_tasks t
			JOIN proj_chain_status c ON t.chain_id = c.id
			WHERE t.slug = 'dup' AND c.slug = ?`, chain).Scan(&s); err != nil {
			t.Fatalf("lookup dup in %s: %v", chain, err)
		}
		return s
	}
	if got := get("c1"); got != "pending" {
		t.Errorf("c1.dup should be swept to pending; got %q", got)
	}
	if got := get("c2"); got != "blocked" {
		t.Errorf("FAN-OUT: c2.dup must stay blocked (different chain); got %q", got)
	}
}

// Bug prose-only-sweep-runs-before-edge-cleanup-fold-strands-structurally-
// blocked-task-on-blocker-close: completing a task whose dependent is
// structurally blocked ONLY by it must leave the dependent pending. The
// sweep ran BEFORE the close event's edge-cleanup fold, so the dependent
// still had its edge at sweep time and was missed; after the close folded
// the edge away, nothing re-swept → stranded blocked-with-no-edge.
func TestTaskComplete_UnblocksStructuralDependentWhenBlockerCloses(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "blocker", "active")    // will complete
	seedTask(t, pool, "c", "dependent", "blocked") // structurally blocked by blocker

	var depID, blkID int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='dependent'`).Scan(&depID)
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='blocker'`).Scan(&blkID)
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_task_blockers (blocked_task_id, blocker_task_id, created_at)
		 VALUES (?, ?, datetime('now'))`, depID, blkID); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	if _, err := work.HandleTaskComplete(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "blocker", "handoff_output": "done"})); err != nil {
		t.Fatalf("HandleTaskComplete: %v", err)
	}

	var status string
	if err := pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug='dependent'`).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "pending" {
		t.Errorf("dependent's only blocker closed → must flip to pending; got %q (stranded)", status)
	}
	var edges int
	pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_task_blockers WHERE blocked_task_id=?`, depID).Scan(&edges)
	if edges != 0 {
		t.Errorf("edge to closed blocker should be cleaned; got %d", edges)
	}
}

// Guard for the reorder: a dependent with TWO blockers must STAY blocked
// when only one closes (the sweep's NOT EXISTS(edge) still sees the other
// edge). Ensures running the sweep after the close-fold doesn't over-sweep.
func TestTaskComplete_DependentWithSecondBlockerStaysBlocked(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "blocker1", "active") // will complete
	seedTask(t, pool, "c", "blocker2", "active") // stays open
	seedTask(t, pool, "c", "dependent", "blocked")

	var depID, b1, b2 int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='dependent'`).Scan(&depID)
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='blocker1'`).Scan(&b1)
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='blocker2'`).Scan(&b2)
	pool.DB().Exec(`INSERT INTO proj_task_blockers (blocked_task_id, blocker_task_id, created_at) VALUES (?,?,datetime('now')),(?,?,datetime('now'))`, depID, b1, depID, b2)

	if _, err := work.HandleTaskComplete(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "blocker1", "handoff_output": "done"})); err != nil {
		t.Fatalf("HandleTaskComplete: %v", err)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug='dependent'`).Scan(&status)
	if status != "blocked" {
		t.Errorf("dependent still has blocker2 open → must stay blocked; got %q", status)
	}
}
