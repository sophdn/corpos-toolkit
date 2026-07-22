package work_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/work"
)

// openTaskTestPool is an alias for openTestPool now that db.Open
// (bug 1326) applies the full schema, including migration 015's
// task_blockers table and migration 026's tasks.commit_sha column.
// Retained as a separate name so callers reading the call sites can
// see at a glance that the test depends on task-level schema; both
// signatures are interchangeable.
func openTaskTestPool(t *testing.T) *db.Pool {
	t.Helper()
	return openTestPool(t)
}

func TestTaskStart_PendingToActive(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, err := work.HandleTaskStart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "t1"}))
	if err != nil {
		t.Fatalf("HandleTaskStart: %v", err)
	}
	if !resp.OK || resp.Status != "active" {
		t.Errorf("resp: %+v", resp)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "active" {
		t.Errorf("db status: %q", status)
	}
}

// Auto-clear status_only-blocked: a task created with status='blocked'
// but NO structural task_blockers edges (the prose-only chain-creation
// pattern that bit phase-4-legacy-field-deprecation) can now be
// started via task_start. The handler emits TaskTransitioned(blocked
// →pending) first, then TaskTransitioned(pending→active); the
// projection lands at status='active'. Pinned by
// suggestion `task-start-clears-status-only-blocked-when-no-structural-edges`.
// Closes bug `no-path-from-status-only-blocked-task-to-pending`.
func TestTaskStart_AutoClearsStatusOnlyBlocked(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "blocked-no-edges", "blocked")

	resp, err := work.HandleTaskStart(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "blocked-no-edges"}))
	if err != nil {
		t.Fatalf("HandleTaskStart: %v", err)
	}
	if !resp.OK || resp.Status != "active" {
		t.Fatalf("expected ok=true status=active for status_only-blocked auto-clear; got %+v", resp)
	}
	var status string
	if err := pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 'blocked-no-edges'`).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "active" {
		t.Errorf("projection status: got %q, want active", status)
	}
	// Event-ledger correctness: both transitions must land.
	var transitionedCount int64
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*) FROM events
		WHERE type = 'TaskTransitioned'
		  AND entity_slug = 'blocked-no-edges'
	`).Scan(&transitionedCount); err != nil {
		t.Fatalf("count TaskTransitioned events: %v", err)
	}
	if transitionedCount != 2 {
		t.Errorf("expected 2 TaskTransitioned events (blocked→pending, pending→active); got %d",
			transitionedCount)
	}
}

// Structural blocker still enforced: a task with status='blocked' AND
// a real task_blockers edge must NOT auto-clear. task_start should
// fall through to the natural blocked→active state-machine reject.
// The blocker stays intact; the task stays blocked.
func TestTaskStart_DoesNotAutoClearStructurallyBlocked(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "blocker", "pending")
	seedTask(t, pool, "c", "blocked-with-edge", "blocked")

	// Seed a structural edge: blocker → blocked-with-edge.
	var blockerID, blockedID int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug = 'blocker'`).Scan(&blockerID); err != nil {
		t.Fatalf("lookup blocker id: %v", err)
	}
	if err := pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug = 'blocked-with-edge'`).Scan(&blockedID); err != nil {
		t.Fatalf("lookup blocked id: %v", err)
	}
	if _, err := pool.DB().Exec(`
		INSERT INTO proj_task_blockers (blocked_task_id, blocker_task_id, reason, created_at)
		VALUES (?, ?, 'test edge', datetime('now'))`,
		blockedID, blockerID); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	resp, err := work.HandleTaskStart(context.Background(), pool, "mcp-servers",
		mustJSON(t, map[string]any{"slug": "blocked-with-edge"}))
	if err != nil {
		t.Fatalf("HandleTaskStart: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected start to fail for structurally-blocked task; got ok response: %+v", resp)
	}
	if resp.Error == "" {
		t.Errorf("expected non-empty error explaining the blocker; got empty")
	}
	// Task must still be 'blocked' — auto-clear did NOT engage.
	var status string
	if err := pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 'blocked-with-edge'`).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "blocked" {
		t.Errorf("structurally-blocked task auto-cleared (regression of guard); got status %q, want blocked", status)
	}
}

func TestTaskUnstart_ActiveToPending(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	resp, err := work.HandleTaskUnstart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "t1"}))
	if err != nil {
		t.Fatalf("HandleTaskUnstart: %v", err)
	}
	if !resp.OK || resp.Status != "pending" {
		t.Errorf("resp: %+v", resp)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "pending" {
		t.Errorf("db status: %q (want pending)", status)
	}
}

func TestTaskUnstart_FromPendingRejected(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskUnstart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "t1"}))
	if resp.Error == "" {
		t.Fatalf("expected error on pending → pending (not in transition table); got OK %+v", resp)
	}
}

func TestTaskUnstart_RequiresIdentifier(t *testing.T) {
	pool := openTaskTestPool(t)
	resp, _ := work.HandleTaskUnstart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if resp.Error == "" || !contains(resp.Error, `{"id":6326}`) {
		t.Errorf("expected self-describing identifier error; got %q", resp.Error)
	}
}

func TestTaskComplete_ActiveToClosedWithSHA(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	resp, err := work.HandleTaskComplete(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t1",
		"commit_sha": "abc1234",
	}))
	if err != nil {
		t.Fatalf("HandleTaskComplete: %v", err)
	}
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var status, sha string
	pool.DB().QueryRow(`SELECT status, commit_sha FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status, &sha)
	if status != "closed" || sha != "abc1234" {
		t.Errorf("post-complete row: status=%q sha=%q", status, sha)
	}
}

func TestTaskComplete_AcceptsShaAlias(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	resp, _ := work.HandleTaskComplete(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1",
		"sha":  "deadbeef",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var sha string
	pool.DB().QueryRow(`SELECT commit_sha FROM proj_current_tasks WHERE slug = 't1'`).Scan(&sha)
	if sha != "deadbeef" {
		t.Errorf("sha column: %q", sha)
	}
}

func TestTaskComplete_PendingRequiresHandoff(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskComplete(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1",
	}))
	if resp.Error == "" {
		t.Fatal("empty error message")
	}
	// Pending → closed without handoff_output is gated by NonEmptyField.
	// Rust error mentions handoff_output and the hint about task_start.
	for _, s := range []string{"handoff_output", "task_start"} {
		if !contains(resp.Error, s) {
			t.Errorf("error %q missing %q", resp.Error, s)
		}
	}
}

func TestTaskComplete_PendingWithHandoffAllowed(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskComplete(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":           "t1",
		"handoff_output": "shipped it",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
}

// TestTaskComplete_SweepsProseOnlyBlockedTasksInChain covers the bug
// `task-complete-doesnt-auto-unblock-intra-chain-prose-deps`: chain
// authors who record "X blocks Y" in design_decisions prose without
// creating structural task_blockers edges leave Y stuck in blocked
// after X closes. The post-close sweep should flip prose-only-blocked
// tasks in the same chain to pending; tasks with structural blockers
// pointing elsewhere should stay blocked.
func TestTaskComplete_SweepsProseOnlyBlockedTasksInChain(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")  // will complete
	seedTask(t, pool, "c", "t2", "blocked") // prose-only blocked
	seedTask(t, pool, "c", "t3", "blocked") // prose-only blocked
	seedTask(t, pool, "c", "tOther", "pending")
	seedTask(t, pool, "c", "t4", "blocked") // structural-blocked by tOther
	// Create a real proj_task_blockers row from t4 → tOther so t4 has
	// a structural blocker that should survive the sweep.
	var t4ID, tOtherID int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug = 't4'`).Scan(&t4ID)
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug = 'tOther'`).Scan(&tOtherID)
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_task_blockers (blocked_task_id, blocker_task_id, created_at)
		 VALUES (?, ?, datetime('now'))`,
		t4ID, tOtherID); err != nil {
		t.Fatalf("seed structural blocker: %v", err)
	}

	resp, err := work.HandleTaskComplete(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":           "t1",
		"handoff_output": "shipped",
	}))
	if err != nil {
		t.Fatalf("HandleTaskComplete: %v", err)
	}
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}

	expect := map[string]string{
		"t1":     "closed",  // the completing task
		"t2":     "pending", // prose-only blocked, swept
		"t3":     "pending", // prose-only blocked, swept
		"t4":     "blocked", // structural blocker (tOther) still open
		"tOther": "pending", // never blocked, unchanged
	}
	for slug, want := range expect {
		var got string
		if err := pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = ?`, slug).Scan(&got); err != nil {
			t.Fatalf("lookup %s: %v", slug, err)
		}
		if got != want {
			t.Errorf("%s status: want %q, got %q", slug, want, got)
		}
	}

	// Audit ledger: the closing TaskCompleted event for t1 plus two
	// TaskTransitioned events for the swept t2 + t3 should be present.
	var transitionedCount int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM events
		 WHERE type = 'TaskTransitioned'
		   AND entity_slug IN ('t2', 't3')
		   AND json_extract(payload, '$.from_status') = 'blocked'
		   AND json_extract(payload, '$.to_status') = 'pending'`).Scan(&transitionedCount); err != nil {
		t.Fatalf("event count: %v", err)
	}
	if transitionedCount != 2 {
		t.Errorf("expected 2 sweep TaskTransitioned events, got %d", transitionedCount)
	}
}

// TestTaskCancel_DoesNotSweepProseOnlyBlocked verifies the sweep is
// gated on closed (not cancelled). A cancelled task wasn't "completed"
// in the satisfies-prose-dep sense — downstream waiters should stay
// blocked because the work they were waiting on won't happen.
func TestTaskCancel_DoesNotSweepProseOnlyBlocked(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")
	seedTask(t, pool, "c", "t2", "blocked") // prose-only blocked

	resp, _ := work.HandleTaskCancel(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "t1"}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}

	var t2Status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't2'`).Scan(&t2Status)
	if t2Status != "blocked" {
		t.Errorf("t2 should stay blocked after t1 cancel; got %q", t2Status)
	}
}

func TestTaskCancel_AnyOpenStateAllowed(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	resp, _ := work.HandleTaskCancel(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "t1"}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "cancelled" {
		t.Errorf("status: %q", status)
	}
}

// Bug task-reopen-lands-in-active-not-documented-pending: reopen must
// land a closed task in 'pending' (the documented contract + the backlog
// state a lowest-position-pending pickup heuristic sees), NOT 'active'.
func TestTaskReopen_ClosedToPending(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "closed")

	resp, _ := work.HandleTaskReopen(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "t1"}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	if resp.Status != "pending" {
		t.Errorf("result status: got %q, want pending", resp.Status)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "pending" {
		t.Errorf("stored status: got %q, want pending", status)
	}
}

func TestTaskReopen_CancelledToPending(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "cancelled")

	resp, _ := work.HandleTaskReopen(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "t1"}))
	if !resp.OK {
		t.Fatalf("cancelled → pending should be allowed (TASK_TRANSITIONS row): %+v", resp)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "pending" {
		t.Errorf("stored status: got %q, want pending", status)
	}
}

// Bug 1402: task_stamp_sha on an active task atomically completes
// and stamps it in a single call — the verb-shape says "this SHA
// captures the closure" and the caller shouldn't have to round-trip
// through task_complete.
func TestTaskStampSHA_OnActiveTaskCompletesAndStamps(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t1",
		"commit_sha": "abc1234",
	}))
	if !resp.OK {
		t.Fatalf("active stamp: %+v", resp)
	}
	var status, sha string
	pool.DB().QueryRow(`SELECT status, commit_sha FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status, &sha)
	if status != "closed" {
		t.Errorf("status: want closed, got %q", status)
	}
	if sha != "abc1234" {
		t.Errorf("sha: %q", sha)
	}
}

// Bug 1402: task_stamp_sha on a pending task also atomically closes
// and stamps. The handoff_output gate that task_complete normally
// enforces on pending→closed is bypassed by design — calling
// task_stamp_sha is the explicit signal that the SHA captures the
// closure (callers who want prose should use task_complete instead).
func TestTaskStampSHA_OnPendingTaskCompletesAndStamps(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t1",
		"commit_sha": "abc1234",
	}))
	if !resp.OK {
		t.Fatalf("pending stamp: %+v", resp)
	}
	var status, sha string
	pool.DB().QueryRow(`SELECT status, commit_sha FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status, &sha)
	if status != "closed" {
		t.Errorf("status: want closed, got %q", status)
	}
	if sha != "abc1234" {
		t.Errorf("sha: %q", sha)
	}
}

// Bug 1402: task_stamp_sha still errors on cancelled tasks — those
// states require explicit reopen first.
func TestTaskStampSHA_OnCancelledTaskErrors(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "cancelled")

	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t1",
		"commit_sha": "abc1234",
	}))
	if resp.Error == "" || !contains(resp.Error, "task_reopen") {
		t.Errorf("want reopen hint for cancelled task, got %q", resp.Error)
	}
}

func TestTaskStampSHA_OnClosedTask(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "closed")

	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t1",
		"commit_sha": "abc1234",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var sha string
	pool.DB().QueryRow(`SELECT commit_sha FROM proj_current_tasks WHERE slug = 't1'`).Scan(&sha)
	if sha != "abc1234" {
		t.Errorf("sha: %q", sha)
	}
}

// Bug 975: task_stamp_sha on an active task auto-closes it, so a
// follow-up task_complete(handoff_output=…) used to error closed→closed
// and the handoff was silently dropped. The additive fix lets the single
// stamp call carry handoff_output straight into the closure — no recovery
// task_edit, no closed→closed error.
func TestTaskStampSHA_OnActiveTaskCarriesHandoff(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":           "t1",
		"commit_sha":     "abc1234",
		"handoff_output": "shipped the distroless image",
	}))
	if !resp.OK {
		t.Fatalf("active stamp with handoff: %+v", resp)
	}
	var status, sha, handoff string
	pool.DB().QueryRow(`SELECT status, commit_sha, handoff_output FROM proj_current_tasks WHERE slug = 't1'`).
		Scan(&status, &sha, &handoff)
	if status != "closed" {
		t.Errorf("status: want closed, got %q", status)
	}
	if sha != "abc1234" {
		t.Errorf("sha: %q", sha)
	}
	if handoff != "shipped the distroless image" {
		t.Errorf("handoff_output: want it to land, got %q", handoff)
	}
}

// Bug 975: stamping a pending task with a handoff likewise carries it
// into the closure (the pending → closed handoff gate is bypassed by the
// stamp verb, same as the no-handoff case).
func TestTaskStampSHA_OnPendingTaskCarriesHandoff(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":           "t1",
		"commit_sha":     "abc1234",
		"handoff_output": "done",
	}))
	if !resp.OK {
		t.Fatalf("pending stamp with handoff: %+v", resp)
	}
	var handoff string
	pool.DB().QueryRow(`SELECT handoff_output FROM proj_current_tasks WHERE slug = 't1'`).Scan(&handoff)
	if handoff != "done" {
		t.Errorf("handoff_output: want it to land, got %q", handoff)
	}
}

// Bug 975: passing handoff_output when stamping an ALREADY-closed task is
// a no-op for the close (the task is closed already) — rather than
// silently dropping the prose, the call rejects with a hint pointing at
// task_edit, the canonical post-closure handoff-attach path.
func TestTaskStampSHA_HandoffOnClosedTaskRejected(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "closed")

	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":           "t1",
		"commit_sha":     "abc1234",
		"handoff_output": "late note",
	}))
	if resp.Error == "" || !contains(resp.Error, "task_edit") {
		t.Errorf("want task_edit hint when handoff supplied on already-closed task, got %q", resp.Error)
	}
}

func TestTaskBlock_FlipsToBlocked(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	seedTask(t, pool, "c", "blocker", "active")

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":         "t1",
		"blocker_slug": "blocker",
		"reason":       "waiting on X",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "blocked" {
		t.Errorf("status: %q", status)
	}
}

// Regression for bug `task-lifecycle-event-folds-fan-out-across-duplicate-
// task-slugs`. Two chains each have a task slug='retro'. A lifecycle op on
// ONE chain's task previously fanned out to the same-slug task in every
// other chain (the events carried no chain disambiguation; the fold matched
// slug+project). Mirrors the production incident: a completion + a reopen
// each fanned out across 8 'retrospective' tasks. Post-fix, the chain_slug
// stamped on the event scopes the fold to one task.
func TestTaskLifecycle_DoesNotFanOutAcrossDuplicateSlugs(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "chain-a")
	seedChain(t, pool, "mcp-servers", "chain-b")
	seedTask(t, pool, "chain-a", "retro", "active")
	seedTask(t, pool, "chain-b", "retro", "pending")

	var idA int64
	if err := pool.DB().QueryRow(
		`SELECT t.id FROM proj_current_tasks t JOIN proj_chain_status c ON c.id=t.chain_id
		 WHERE c.slug='chain-a' AND t.slug='retro'`).Scan(&idA); err != nil {
		t.Fatalf("lookup chain-a/retro: %v", err)
	}

	statusOf := func(chain string) (string, string) {
		var status, sha string
		pool.DB().QueryRow(
			`SELECT t.status, COALESCE(t.commit_sha,'') FROM proj_current_tasks t JOIN proj_chain_status c ON c.id=t.chain_id
			 WHERE c.slug=? AND t.slug='retro'`, chain).Scan(&status, &sha)
		return status, sha
	}

	// 1) Complete chain-a's retro (by id) with a SHA.
	resp, _ := work.HandleTaskComplete(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": idA, "commit_sha": "abc1234",
	}))
	if !resp.OK {
		t.Fatalf("complete chain-a/retro: %+v", resp)
	}
	if s, _ := statusOf("chain-a"); s != "closed" {
		t.Errorf("chain-a/retro status = %q, want closed", s)
	}
	if s, _ := statusOf("chain-b"); s != "pending" {
		t.Errorf("FANOUT: chain-b/retro = %q after completing chain-a/retro, want pending", s)
	}

	// 2) Reopen chain-a's retro — must not flip chain-b's, and must clear
	// chain-a's commit_sha (non-closed task carries no commit).
	resp2, _ := work.HandleTaskReopen(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": idA,
	}))
	if !resp2.OK {
		t.Fatalf("reopen chain-a/retro: %+v", resp2)
	}
	// Reopen lands in pending (the backlog), not active — bug
	// task-reopen-lands-in-active-not-documented-pending. The sha must
	// still clear (a non-closed task carries no commit).
	if s, sha := statusOf("chain-a"); s != "pending" || sha != "" {
		t.Errorf("chain-a/retro after reopen = (%q, sha=%q), want (pending, empty)", s, sha)
	}
	if s, _ := statusOf("chain-b"); s != "pending" {
		t.Errorf("FANOUT: chain-b/retro = %q after reopening chain-a/retro, want pending", s)
	}
}

func TestTaskUnblock_RemovesBlockerAndFlipsToPending(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	seedTask(t, pool, "c", "blocker", "active")

	work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1", "blocker_slug": "blocker",
	}))
	resp, _ := work.HandleTaskUnblock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1", "blocker_slug": "blocker",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "pending" {
		t.Errorf("post-unblock status: %q", status)
	}
}

// Bug 1318 regression: task_block + task_unblock accept (task_id, blocker_id)
// as alternatives to (slug, blocker_slug). task_search returns id as the
// primary identifier, so an agent pattern-matching that output naturally
// reaches for the numeric form; this used to fail with an unhelpful
// "task_block requires slug" error.
func TestTaskBlock_AcceptsTaskIDAndBlockerID(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	seedTask(t, pool, "c", "blocker", "active")

	var tID, bID int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug = 't1'`).Scan(&tID)
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug = 'blocker'`).Scan(&bID)

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"task_id":    tID,
		"blocker_id": bID,
		"reason":     "id-shape call",
	}))
	if !resp.OK || resp.Status != "blocked" {
		t.Fatalf("id-shape block resp: %+v", resp)
	}
	if resp.Slug != "t1" {
		t.Errorf("response should echo recovered slug; got %q", resp.Slug)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE id = ?`, tID).Scan(&status)
	if status != "blocked" {
		t.Errorf("status after id-shape block: %q", status)
	}

	// And the unblock side accepts ids too.
	uresp, _ := work.HandleTaskUnblock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"task_id":    tID,
		"blocker_id": bID,
	}))
	if !uresp.OK {
		t.Fatalf("id-shape unblock resp: %+v", uresp)
	}
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE id = ?`, tID).Scan(&status)
	if status != "pending" {
		t.Errorf("status after id-shape unblock: %q", status)
	}
}

// Bug 1318 regression: the legacy `task_slug` alias from the previous
// block-task.md instructions must still resolve, so stale callers don't
// hit a "requires slug" error on an arg they were told to pass.
func TestTaskBlock_AcceptsTaskSlugAlias(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	seedTask(t, pool, "c", "blocker", "active")

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"task_slug":    "t1",
		"blocker_slug": "blocker",
	}))
	if !resp.OK || resp.Slug != "t1" {
		t.Fatalf("task_slug alias resp: %+v", resp)
	}
}

// Bug task-block-action-doc-advertises-blocked_by-canonical-but-handler-only-accepts-blocker_slug
// regression: the action-doc at go/internal/actiondocs/corpus/work/task_block.toml
// declares 'blocked_by' as the canonical blocker-param name (with
// 'blocker_slug' listed as the FROM-side alias). Before the fix, the
// Go struct only carried a json:"blocker_slug" tag, so a caller using
// the documented canonical name dropped its intent silently — JSON
// unmarshal left BlockerSlug empty, the INSERT branch was skipped, and
// the handler returned OK with status='blocked' but no task_blockers
// row. The fix adds BlockedBy / BlockedByChain fields with their own
// JSON tags plus a normalizeAliases() helper that copies them into the
// FROM-side fields before any logic runs. Both name shapes now produce
// the same edge. This test pins both halves: (a) the row is written,
// (b) task_blockers introspection surfaces it as a structural edge
// rather than the status_only synthetic.
func TestTaskBlock_AcceptsActionDocCanonicalBlockedByName(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	seedTask(t, pool, "c", "blocker", "active")

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t1",
		"blocked_by": "blocker",
		"reason":     "canonical-name round-trip",
	}))
	if !resp.OK || resp.Status != "blocked" {
		t.Fatalf("blocked_by canonical resp: %+v", resp)
	}

	// Structural edge must exist — this is the half that silently failed
	// before the fix. A status-flag-only update would pass the response-
	// shape assertion above but fail this row check.
	var rowCount int
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*) FROM proj_task_blockers
		WHERE blocked_task_id = (SELECT id FROM proj_current_tasks WHERE slug = 't1')
		  AND blocker_task_id = (SELECT id FROM proj_current_tasks WHERE slug = 'blocker')
	`).Scan(&rowCount); err != nil {
		t.Fatalf("count task_blockers edge: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("structural edge missing: blocked_by call returned OK but task_blockers has %d rows", rowCount)
	}

	// And task_blockers introspection should surface the edge as a real
	// structural row, not as the status_only synthetic.
	bResp, err := work.HandleTaskBlockers(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1",
	}))
	if err != nil {
		t.Fatalf("task_blockers introspection: %v", err)
	}
	if len(bResp.List) != 1 {
		t.Fatalf("expected 1 blocker entry, got %d: %+v", len(bResp.List), bResp.List)
	}
	if bResp.List[0].Kind == "status_only" {
		t.Errorf("blocker entry should be structural, got status_only: %+v", bResp.List[0])
	}
	if bResp.List[0].Slug != "blocker" {
		t.Errorf("blocker entry slug: got %q, want 'blocker'", bResp.List[0].Slug)
	}
}

// Same regression, cross-chain variant. The action-doc advertises
// 'blocked_by_chain' as the canonical alias for 'blocker_chain_slug';
// the fix lands both. This test verifies the cross-chain edge is
// written when the canonical name is used for both fields.
func TestTaskBlock_AcceptsActionDocCanonicalBlockedByChainName(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "chain-a")
	seedChain(t, pool, "mcp-servers", "chain-b")
	seedTask(t, pool, "chain-a", "blocked-task", "pending")
	seedTask(t, pool, "chain-b", "blocker-task", "active")

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":             "blocked-task",
		"chain_slug":       "chain-a",
		"blocked_by":       "blocker-task",
		"blocked_by_chain": "chain-b",
		"reason":           "cross-chain canonical-name round-trip",
	}))
	if !resp.OK || resp.Status != "blocked" {
		t.Fatalf("cross-chain canonical resp: %+v", resp)
	}

	var rowCount int
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*) FROM proj_task_blockers
		WHERE blocked_task_id = (SELECT id FROM proj_current_tasks WHERE slug = 'blocked-task')
		  AND blocker_task_id = (SELECT id FROM proj_current_tasks WHERE slug = 'blocker-task')
	`).Scan(&rowCount); err != nil {
		t.Fatalf("count cross-chain edge: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("cross-chain structural edge missing: rows=%d", rowCount)
	}
}

// Bug 1318 regression: the missing-identifier error names the actual
// accepted params (slug/task_id + blocker_slug/blocker_id), not the
// ambiguous "task_block requires slug". The previous message cost agents
// repeat retries because "slug" alone didn't say which slug.
func TestTaskBlock_ErrorNamesAcceptedParams(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"chain_slug": "c",
		// Notably no slug / task_id / task_slug.
	}))
	if resp.Error == "" {
		t.Fatal("expected error for missing task identifier")
	}
	for _, want := range []string{"slug", "task_id", "blocker_slug", "blocker_id", "chain_slug"} {
		if !contains(resp.Error, want) {
			t.Errorf("error should name %q; got %q", want, resp.Error)
		}
	}
}

func TestTaskBlockers_ListsBlockers(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	seedTask(t, pool, "c", "b1", "active")
	seedTask(t, pool, "c", "b2", "active")

	work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1", "blocker_slug": "b1", "reason": "first",
	}))
	work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1", "blocker_slug": "b2", "reason": "second",
	}))

	resp, _ := work.HandleTaskBlockers(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1",
	}))
	if len(resp.List) != 2 {
		t.Errorf("blocker count: want 2, got %d", len(resp.List))
	}
	// Structural blockers must not carry kind="status_only" — that's the
	// disambiguation marker reserved for the synthetic status-only entry.
	for i, e := range resp.List {
		if e.Kind != "" {
			t.Errorf("structural blocker %d should not set kind; got %q", i, e.Kind)
		}
	}
}

// Bug 1379: a task with status='blocked' but zero structural edges
// (the cross-chain-wait shape) must surface ONE synthetic blocker
// entry with kind="status_only" so callers can disambiguate.
func TestTaskBlockers_StatusOnlySyntheticWhenBlockedWithoutEdges(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "blocked")

	resp, _ := work.HandleTaskBlockers(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1",
	}))
	if len(resp.List) != 1 {
		t.Fatalf("expected 1 synthetic entry, got %d (%+v)", len(resp.List), resp.List)
	}
	if resp.List[0].Kind != "status_only" {
		t.Errorf("synthetic entry kind: want %q, got %q", "status_only", resp.List[0].Kind)
	}
	if resp.List[0].Reason == "" {
		t.Errorf("synthetic entry must carry a Reason explaining the disambiguation")
	}
}

// Bug 1413: task_blockers DOES return cross-chain edges (edges created
// via task_block's blocker_chain_slug parameter where the blocker lives
// in a different chain than the blocked task). The pre-fix docs and
// synthetic-entry message both claimed otherwise; the SQL has always
// JOIN'd to the blocker's chain without filtering. This test pins the
// behavior so the docs and code never drift back out of sync.
func TestTaskBlockers_ReturnsCrossChainEdges(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "blocked-chain")
	seedChain(t, pool, "mcp-servers", "blocker-chain")
	seedTask(t, pool, "blocked-chain", "blocked-task", "pending")
	seedTask(t, pool, "blocker-chain", "blocker-task", "active")

	// Cross-chain block: blocked-task in blocked-chain, blocker-task
	// in blocker-chain.
	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":               "blocked-task",
		"chain_slug":         "blocked-chain",
		"blocker_slug":       "blocker-task",
		"blocker_chain_slug": "blocker-chain",
		"reason":             "cross-chain dep test",
	}))
	if !resp.OK {
		t.Fatalf("task_block cross-chain: %+v", resp)
	}

	out, _ := work.HandleTaskBlockers(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "blocked-task",
		"chain_slug": "blocked-chain",
	}))
	if len(out.List) != 1 {
		t.Fatalf("want 1 structural cross-chain entry, got %d (%+v)", len(out.List), out.List)
	}
	e := out.List[0]
	if e.Kind != "" {
		t.Errorf("cross-chain entry should be structural (kind empty), got %q", e.Kind)
	}
	if e.Slug != "blocker-task" {
		t.Errorf("entry.Slug: got %q, want blocker-task", e.Slug)
	}
	if e.ChainSlug != "blocker-chain" {
		t.Errorf("entry.ChainSlug: got %q, want blocker-chain (the blocker's chain)", e.ChainSlug)
	}
}

// Bug 1387: task_blockers accepts {id} the same way task_read does.
// The id-by-default parity work (bug 1329) reached task_read / bug_read /
// bug_resolve / bug_stamp_sha; this test pins the same for task_blockers.
func TestTaskBlockers_AcceptsIDFormParity(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	seedTask(t, pool, "c", "b1", "active")

	work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1", "blocker_slug": "b1", "reason": "x",
	}))

	// Look up t1's id so the call carries it like a chain_state →
	// task_blockers flow would.
	var id int64
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT id FROM proj_current_tasks WHERE slug = 't1'`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	// id-form
	bySlug, _ := work.HandleTaskBlockers(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1",
	}))
	byID, _ := work.HandleTaskBlockers(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": id,
	}))
	if len(byID.List) != len(bySlug.List) {
		t.Fatalf("id-form list len = %d, slug-form list len = %d", len(byID.List), len(bySlug.List))
	}
	for i := range bySlug.List {
		if byID.List[i].Slug != bySlug.List[i].Slug {
			t.Errorf("entry %d: id-form slug = %q, slug-form slug = %q",
				i, byID.List[i].Slug, bySlug.List[i].Slug)
		}
	}
}

func TestTaskBlockers_MissingIdentifierError(t *testing.T) {
	pool := openTaskTestPool(t)
	resp, _ := work.HandleTaskBlockers(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if resp.Err == nil {
		t.Fatal("expected error envelope")
	}
	if !contains(resp.Err.Error, `{"id":6326}`) {
		t.Errorf("error should self-describe the identifier shape; got %q", resp.Err.Error)
	}
}

// Bug 1379: a pending task with no structural blockers must continue
// to return the empty list — the synthetic entry only fires when
// task.status='blocked'.
func TestTaskBlockers_EmptyListWhenPendingNoEdges(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskBlockers(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1",
	}))
	if len(resp.List) != 0 {
		t.Errorf("expected empty list for pending task; got %+v", resp.List)
	}
}

func TestTaskRead_BySlug(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, err := work.HandleTaskRead(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1", "chain_slug": "c",
	}))
	if err != nil {
		t.Fatalf("HandleTaskRead: %v", err)
	}
	if resp.Task == nil {
		t.Fatalf("expected Task, got err=%+v", resp.Err)
	}
	if resp.Task.Slug != "t1" || resp.Task.Status != "pending" {
		t.Errorf("task: %+v", resp.Task)
	}
}

func TestTaskSearch_PatternMatchesAcrossChains(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")
	pool.DB().Exec(`UPDATE proj_current_tasks SET problem_statement = 'fixing the parser bug' WHERE 1 = 1`)
	seedTask(t, pool, "c1", "parse-fix", "pending")
	seedTask(t, pool, "c2", "other-task", "pending")

	pool.DB().Exec(`UPDATE proj_current_tasks SET problem_statement = 'fixing the parser bug' WHERE slug = 'parse-fix'`)
	pool.DB().Exec(`UPDATE proj_current_tasks SET problem_statement = 'unrelated' WHERE slug = 'other-task'`)

	resp, _ := work.HandleTaskSearch(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"pattern": "parser",
	}))
	if len(resp.List) != 1 || resp.List[0].Slug != "parse-fix" {
		t.Errorf("search hits: %+v", resp.List)
	}
}

func TestTaskSearch_ChainScopedOrdersByPosition(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "a", "pending")
	seedTask(t, pool, "c", "b", "pending")
	seedTask(t, pool, "c", "c-task", "pending")

	resp, _ := work.HandleTaskSearch(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"chain": "c",
	}))
	if len(resp.List) != 3 {
		t.Fatalf("count: %d", len(resp.List))
	}
	for i, want := range []string{"a", "b", "c-task"} {
		if resp.List[i].Slug != want {
			t.Errorf("position %d: want %q, got %q", i, want, resp.List[i].Slug)
		}
	}
}

// Bug work-chain-identifier-handling-inconsistent-across-actions: task_list
// accepts a chain by numeric chain_id (resolved to slug), and tolerates a slug
// passed in chain_id — so the same chain handle works here as on chain_state.
func TestTaskSearch_AcceptsChainID(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "by-id")
	seedTask(t, pool, "by-id", "t1", "pending")
	seedTask(t, pool, "by-id", "t2", "pending")
	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_chain_status WHERE slug = ?`, "by-id").Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}

	// numeric chain_id
	resp, _ := work.HandleTaskSearch(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"chain_id": id}))
	if resp.Err != nil {
		t.Fatalf("chain_id numeric returned error: %+v", resp.Err)
	}
	if len(resp.List) != 2 {
		t.Errorf("want 2 tasks via numeric chain_id, got %d (%+v)", len(resp.List), resp.List)
	}

	// slug mistakenly passed in chain_id (string) routes to the chain slug
	respSlug, _ := work.HandleTaskSearch(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"chain_id": "by-id"}))
	if respSlug.Err != nil {
		t.Fatalf("slug-in-chain_id returned error: %+v", respSlug.Err)
	}
	if len(respSlug.List) != 2 {
		t.Errorf("want 2 tasks via slug-in-chain_id, got %d", len(respSlug.List))
	}

	// unknown numeric chain_id → typed not-found envelope
	respMiss, _ := work.HandleTaskSearch(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"chain_id": int64(999999)}))
	if respMiss.Err == nil {
		t.Errorf("want typed not-found envelope for unknown chain_id, got %+v", respMiss)
	}
}

func TestTaskEdit_UpdatesFlatFields(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":              "t1",
		"chain_slug":        "c",
		"problem_statement": "new statement",
		"constraints":       "new constraints",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var ps, con string
	pool.DB().QueryRow(`SELECT problem_statement, constraints FROM proj_current_tasks WHERE slug = 't1'`).Scan(&ps, &con)
	if ps != "new statement" || con != "new constraints" {
		t.Errorf("post-edit: ps=%q con=%q", ps, con)
	}
}

// Bug 1441 alias parity for task_edit: task_id / task_slug / chain
// resolve to the canonical (slug, chain_slug) inside the handler.
func TestTaskEdit_TaskIDAlias(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	var id int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='t1'`).Scan(&id)
	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"task_id":           id,
		"problem_statement": "edited via task_id alias",
	}))
	if !resp.OK {
		t.Fatalf("with task_id= alias, task_edit should resolve and write; got %+v", resp)
	}
	var ps string
	pool.DB().QueryRow(`SELECT problem_statement FROM proj_current_tasks WHERE slug='t1'`).Scan(&ps)
	if ps != "edited via task_id alias" {
		t.Errorf("post-edit problem_statement: %q", ps)
	}
}

func TestTaskEdit_ChainAndTaskSlugAliases(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "ca")
	seedChain(t, pool, "mcp-servers", "cb")
	seedTask(t, pool, "ca", "shared", "pending")
	seedTask(t, pool, "cb", "shared", "pending")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"task_slug":         "shared",
		"chain":             "ca",
		"problem_statement": "edited via slug+chain aliases",
	}))
	if !resp.OK {
		t.Fatalf("with task_slug+chain= aliases, task_edit should resolve and write; got %+v", resp)
	}
}

func TestTaskEdit_NoFieldsErrors(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t1", "chain_slug": "c",
	}))
	if resp.Error == "" {
		t.Errorf("expected error, got %+v", resp)
	}
}

// Bug 1319 regression: acceptance_criteria and context_required accept
// JSON arrays — the previous behaviour rejected them at the json.Unmarshal
// stage with "cannot unmarshal array into Go struct field ... type string",
// forcing callers to pre-flatten lists into newline-bulleted strings.
//
// Also asserts the storage form matches: a list passed to task_edit
// produces the same stored value as the equivalent "\n- "-joined string,
// which is the same shape forge.AsJoined writes on the create path.
func TestTaskEdit_AcceptsListForOptionalStringOrList(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	seedTask(t, pool, "c", "t2", "pending")

	// Caller A: pass a list.
	respA, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":                "t1",
		"chain_slug":          "c",
		"acceptance_criteria": []string{"item one", "item two", "item three"},
		"context_required":    []string{"file a", "file b"},
	}))
	if !respA.OK {
		t.Fatalf("list-shape edit: %+v", respA)
	}
	// fields_written must name both fields in spec order
	// (acceptance_criteria before context_required per taskEditFieldSpec).
	if len(respA.FieldsWritten) != 2 ||
		respA.FieldsWritten[0] != "acceptance_criteria" ||
		respA.FieldsWritten[1] != "context_required" {
		t.Errorf("fields_written: %v", respA.FieldsWritten)
	}

	// Caller B: pre-flatten to "\n- "-joined string (the old required shape).
	respB, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":                "t2",
		"chain_slug":          "c",
		"acceptance_criteria": "item one\n- item two\n- item three",
		"context_required":    "file a\n- file b",
	}))
	if !respB.OK {
		t.Fatalf("string-shape edit: %+v", respB)
	}

	// Storage parity: both forms must produce identical stored rows.
	var acA, crA, acB, crB string
	pool.DB().QueryRow(`SELECT acceptance_criteria, context_required FROM proj_current_tasks WHERE slug = 't1'`).Scan(&acA, &crA)
	pool.DB().QueryRow(`SELECT acceptance_criteria, context_required FROM proj_current_tasks WHERE slug = 't2'`).Scan(&acB, &crB)
	if acA != acB {
		t.Errorf("acceptance_criteria storage form drift:\n  list  → %q\n  joined → %q", acA, acB)
	}
	if crA != crB {
		t.Errorf("context_required storage form drift:\n  list  → %q\n  joined → %q", crA, crB)
	}
}

// Bug 1319 regression: task_edit accepts a single string for
// optional_string_or_list fields too — symmetric with forge's
// coerceFields branch that lifts a single string to a 1-element list.
func TestTaskEdit_AcceptsStringForOptionalStringOrList(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":                "t1",
		"chain_slug":          "c",
		"acceptance_criteria": "single prose paragraph",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var ac string
	pool.DB().QueryRow(`SELECT acceptance_criteria FROM proj_current_tasks WHERE slug = 't1'`).Scan(&ac)
	if ac != "single prose paragraph" {
		t.Errorf("single-string store: %q", ac)
	}
}

// Bug 1319 regression: partial-update preserved when one field validates
// and another doesn't. The validating fields write; the bad one surfaces
// in field_errors. Old behavior: the entire call aborted with a parse
// error, even fields that were structurally valid.
func TestTaskEdit_PartialUpdateOnFieldError(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	// problem_statement is a string-shape field; passing a list must fail
	// for that field while acceptance_criteria (list-shape) still writes.
	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":                "t1",
		"chain_slug":          "c",
		"problem_statement":   []string{"bad", "shape"},
		"acceptance_criteria": []string{"good", "shape"},
	}))
	if !resp.OK {
		t.Fatalf("partial update should still report OK; got %+v", resp)
	}
	if len(resp.FieldsWritten) != 1 || resp.FieldsWritten[0] != "acceptance_criteria" {
		t.Errorf("fields_written should be [acceptance_criteria]; got %v", resp.FieldsWritten)
	}
	if resp.FieldErrors["problem_statement"] == "" {
		t.Errorf("field_errors[problem_statement] should be set; got %v", resp.FieldErrors)
	}
	// The good field actually wrote.
	var ac, ps string
	pool.DB().QueryRow(`SELECT acceptance_criteria, problem_statement FROM proj_current_tasks WHERE slug = 't1'`).Scan(&ac, &ps)
	if ac != "good\n- shape" {
		t.Errorf("acceptance_criteria did not write; got %q", ac)
	}
	// problem_statement was 'p' from seedTask; must NOT have been corrupted.
	if ps != "p" {
		t.Errorf("problem_statement was unexpectedly touched: %q", ps)
	}
}

// Bug 1319 regression: when every supplied field fails validation,
// task_edit returns a field_errors envelope and writes nothing — the
// db transaction is not opened.
func TestTaskEdit_AllFieldsFailNoWrite(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":              "t1",
		"chain_slug":        "c",
		"problem_statement": []string{"a", "b"},
		"constraints":       []string{"a", "b"},
	}))
	if resp.OK {
		t.Fatalf("expected failure response when no fields validate; got %+v", resp)
	}
	if resp.FieldErrors["problem_statement"] == "" || resp.FieldErrors["constraints"] == "" {
		t.Errorf("both failing fields should be reported; got %v", resp.FieldErrors)
	}
	if !contains(resp.Error, "field_errors") {
		t.Errorf("error should point to field_errors envelope; got %q", resp.Error)
	}
}

// ── bug 1423: handoff_output_append ────────────────────────────────────────

// task_edit's handoff_output_append concatenates onto the existing
// stored handoff_output via SQLite `||` — atomic, no read-then-write.
// The caller controls the separator (leading newline etc.); this test
// pins the byte-exact concatenation.
func TestTaskEdit_HandoffOutputAppend_Concatenates(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")
	// Seed an existing handoff_output so we can prove the append is a
	// concatenation rather than a replace.
	if _, err := pool.DB().Exec(`UPDATE proj_current_tasks SET handoff_output = ? WHERE slug = 't1'`, "shipped it"); err != nil {
		t.Fatalf("seed handoff_output: %v", err)
	}

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":                  "t1",
		"chain_slug":            "c",
		"handoff_output_append": "\n\n## Post-closure audit\nfound residue X",
	}))
	if !resp.OK {
		t.Fatalf("append edit failed: %+v", resp)
	}
	if len(resp.FieldsWritten) != 1 || resp.FieldsWritten[0] != "handoff_output_append" {
		t.Errorf("fields_written should name the input key; got %v", resp.FieldsWritten)
	}

	var got string
	pool.DB().QueryRow(`SELECT handoff_output FROM proj_current_tasks WHERE slug = 't1'`).Scan(&got)
	want := "shipped it\n\n## Post-closure audit\nfound residue X"
	if got != want {
		t.Errorf("post-append handoff_output:\n  want: %q\n  got:  %q", want, got)
	}
}

// task_edit rejects callers that supply BOTH handoff_output and
// handoff_output_append in the same call — composition order is
// ambiguous (apply set then append? append then set?), so the safer
// affordance is to surface the conflict and let the caller pick.
func TestTaskEdit_HandoffOutputAppend_RejectsCombinedWithSet(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":                  "t1",
		"chain_slug":            "c",
		"handoff_output":        "full rewrite",
		"handoff_output_append": "addendum",
	}))
	if resp.OK {
		t.Fatalf("expected envelope error, got OK: %+v", resp)
	}
	if !contains(resp.Error, "validation failed") {
		t.Errorf("error should name validation failure; got %q", resp.Error)
	}
	var foundConflict bool
	for _, e := range resp.Errors {
		if contains(e, "mutually exclusive") {
			foundConflict = true
		}
	}
	if !foundConflict {
		t.Errorf("errors should surface the mutually-exclusive conflict; got %v", resp.Errors)
	}
	// Nothing must have written.
	var stored string
	pool.DB().QueryRow(`SELECT handoff_output FROM proj_current_tasks WHERE slug = 't1'`).Scan(&stored)
	if stored != "" {
		t.Errorf("rejected call should leave handoff_output untouched; got %q", stored)
	}
}

// ── bug 1422: aggregate envelope-level validation errors ──────────────────

// task_edit aggregates multiple structural problems (missing slug,
// unknown field names) into one envelope so callers can fix the call
// shape in a single revision rather than iterating one error at a
// time. Pre-fix behaviour returned slug-missing first; on retry with
// an unknown field name, returned "no field updates supplied" with no
// hint that the typo'd key was the actual problem.
func TestTaskEdit_AggregatesEnvelopeErrors(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	// No slug; a typo'd field name (`handoff_outpt`). Both must
	// appear in the same envelope.
	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"handoff_outpt": "shipped",
	}))
	if resp.OK {
		t.Fatalf("expected envelope error, got OK: %+v", resp)
	}
	if resp.Error == "" {
		t.Errorf("error summary should be set; got %+v", resp)
	}
	var sawSlug, sawUnknown bool
	for _, e := range resp.Errors {
		if contains(e, "requires an identifier") {
			sawSlug = true
		}
		if contains(e, "unknown field: handoff_outpt") {
			sawUnknown = true
		}
	}
	if !sawSlug || !sawUnknown {
		t.Errorf("expected both slug-missing and unknown-field in errors; got %v", resp.Errors)
	}
	if len(resp.EditableFields) == 0 {
		t.Errorf("editable_fields should populate on the unknown-field path; got %v", resp.EditableFields)
	}
}

// Unknown-field detection: a single typo'd key with slug present must
// still surface the unknown field by name rather than collapsing to
// the legacy "no field updates supplied" envelope.
func TestTaskEdit_UnknownFieldNamedExplicitly(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":          "t1",
		"chain_slug":    "c",
		"problem_stmt":  "typo'd key",
		"constraintsZZ": "another typo",
	}))
	if resp.OK {
		t.Fatalf("expected envelope error, got OK: %+v", resp)
	}
	if len(resp.Errors) < 2 {
		t.Fatalf("expected both unknown fields surfaced; got %v", resp.Errors)
	}
	var sawA, sawB bool
	for _, e := range resp.Errors {
		if contains(e, "unknown field: problem_stmt") {
			sawA = true
		}
		if contains(e, "unknown field: constraintsZZ") {
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Errorf("both unknown fields should be named; got %v", resp.Errors)
	}
}

// Routing keys (slug, chain_slug, id, rationale) must NOT be flagged
// as unknown fields even though they aren't in the editable spec.
// rationale is a dispatch-layer envelope field; agents nest it into
// params often enough that the dispatch layer detects the mistake.
// We allowlist it here so a legitimate top-level rationale doesn't
// trip the unknown-field check.
func TestTaskEdit_RoutingKeysAreNotUnknownFields(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":              "t1",
		"chain_slug":        "c",
		"id":                int64(0),
		"rationale":         "rationale that somehow leaked here",
		"problem_statement": "ok",
	}))
	if !resp.OK {
		t.Fatalf("routing keys should not trip unknown-field check; got %+v", resp)
	}
}

// ── bug task-start-id-rejection-and-unhelpful-disambiguation ───────────────

// task_start (and siblings) must accept {id} alongside {slug}, matching
// the bug 1329 parity discipline task_read already follows. Before this
// fix task_start returned "task_start requires slug" when called with
// just {id}.
func TestTaskStart_ByID(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='t1'`).Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}
	resp, err := work.HandleTaskStart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"id": id}))
	if err != nil {
		t.Fatalf("HandleTaskStart by id: %v", err)
	}
	if !resp.OK || resp.Status != "active" {
		t.Fatalf("resp: %+v", resp)
	}
	if resp.Slug != "t1" {
		t.Errorf("slug should be resolved from id; got %q", resp.Slug)
	}
}

func TestTaskComplete_ByID(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	var id int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='t1'`).Scan(&id)

	handoff := "done"
	resp, err := work.HandleTaskComplete(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id":             id,
		"handoff_output": handoff,
	}))
	if err != nil {
		t.Fatalf("HandleTaskComplete by id: %v", err)
	}
	if !resp.OK || resp.Status != "closed" || resp.Slug != "t1" {
		t.Fatalf("resp: %+v", resp)
	}
}

func TestTaskCancel_ByID(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	var id int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='t1'`).Scan(&id)
	resp, _ := work.HandleTaskCancel(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"id": id}))
	if !resp.OK || resp.Status != "cancelled" {
		t.Fatalf("resp: %+v", resp)
	}
}

// Calling task_start with neither slug nor id surfaces the self-
// describing identifier error (chain quiet-and-instrument-operator-
// surface T4): it names the {"id":<int>} form, the slug+chain_slug
// alternative, and the catalog-sourced accepted-params + example, so the
// caller corrects in one retry instead of round-tripping to work_actions.
func TestTaskStart_RequiresSlugOrIdError(t *testing.T) {
	pool := openTaskTestPool(t)
	resp, _ := work.HandleTaskStart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	for _, want := range []string{"task_start", `{"id":6326}`, "integer", "chain_slug", "Accepted params"} {
		if !strings.Contains(resp.Error, want) {
			t.Errorf("error %q missing self-describing fragment %q", resp.Error, want)
		}
	}
}

// The `chain` shorthand is silently mapped to chain_slug (mirroring the
// alias task_search already accepts). Before this fix passing `chain`
// was ignored and the slug-resolution path saw an empty chain_slug.
func TestTaskStart_ChainAliasAcceptedForChainSlug(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "ca")
	seedChain(t, pool, "mcp-servers", "cb")
	seedTask(t, pool, "ca", "shared", "pending")
	seedTask(t, pool, "cb", "shared", "pending")

	resp, _ := work.HandleTaskStart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":  "shared",
		"chain": "ca",
	}))
	if !resp.OK || resp.Status != "active" {
		t.Fatalf("with chain= alias, task_start should resolve to chain ca; got %+v", resp)
	}
}

// Bug 1441: task_id is accepted as an alias for id on the task_*
// surface, so callers reaching for the schema-aligned spelling don't
// hit a "requires slug or id" round-trip per call.
func TestTaskStart_TaskIDAlias(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	var id int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='t1'`).Scan(&id)
	resp, _ := work.HandleTaskStart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"task_id": id}))
	if !resp.OK || resp.Status != "active" || resp.Slug != "t1" {
		t.Fatalf("with task_id= alias, task_start should resolve; got %+v", resp)
	}
}

func TestTaskRead_TaskIDAlias(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "pending")

	var id int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_tasks WHERE slug='t1'`).Scan(&id)
	resp, err := work.HandleTaskRead(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"task_id": id}))
	if err != nil {
		t.Fatalf("HandleTaskRead: %v", err)
	}
	if resp.Err != nil {
		t.Fatalf("with task_id= alias, task_read should succeed; got %+v", resp.Err)
	}
	if resp.Task == nil || resp.Task.Slug != "t1" {
		t.Fatalf("task slug mismatch: %+v", resp.Task)
	}
}

// Ambiguous slugs surface a message that names the disambiguation
// parameter (`chain_slug`) and lists each candidate inline. Before
// this fix the message named the colliding chains but not the
// parameter to pass.
func TestTaskStart_AmbiguousSlugErrorNamesDisambiguator(t *testing.T) {
	pool := openTaskTestPool(t)
	seedChain(t, pool, "mcp-servers", "alpha")
	seedChain(t, pool, "mcp-servers", "beta")
	seedTask(t, pool, "alpha", "duo", "pending")
	seedTask(t, pool, "beta", "duo", "pending")

	resp, _ := work.HandleTaskStart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "duo"}))
	if resp.OK {
		t.Fatalf("expected ambiguous-slug failure, got OK: %+v", resp)
	}
	for _, fragment := range []string{
		`is ambiguous`,
		`chain_slug="alpha"`,
		`chain_slug="beta"`,
		"chain", // the shorthand-alias call-out
	} {
		if !contains(resp.Error, fragment) {
			t.Errorf("ambiguous-slug error missing %q; got %q", fragment, resp.Error)
		}
	}
}

func contains(haystack, needle string) bool {
	return needle == "" || (len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0))
}
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
