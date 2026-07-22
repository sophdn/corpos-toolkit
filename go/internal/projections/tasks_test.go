package projections_test

import (
	"testing"

	"toolkit/internal/testutil"
)

// TestCurrentTasks_RebuildFromEmpty seeds the tasks CRUD table directly
// (no events), captures the migration-backfilled checksum of
// proj_current_tasks, TRUNCATEs the projection, runs RebuildFromEmpty,
// and asserts the post-rebuild checksum matches byte-for-byte. This is
// the T2 acceptance criterion: the projection migration's snapshot
// INSERT and the fold module's RebuildFromEmpty SQL must produce
// byte-identical domain-column state.
//
// The tableChecksum helper deliberately skips last_event_id and
// last_event_ts (those legitimately differ on rows touched by live fold
// vs snapshot-seeded rows; see the helper's comment in
// projections_test.go). The remaining columns must match.
func TestCurrentTasks_RebuildFromEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Post-T6: rebuild replays events; the retired chains/tasks CRUD
	// tables are gone. Seed synthetic ChainCreated + TaskCreated events.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7e00-0000-7000-8000-000000000000', '2026-05-21 00:00:00', 'system', 'test', 'ChainCreated', 'chain', 'c1', 'p1',
		 '{"output":"","design_decisions":"","completion_condition":""}', '019e7e00-0000-7000-8000-000000000000', 1),
		('019e7e00-0001-7000-8000-000000000001', '2026-05-21 00:00:01', 'system', 'test', 'TaskCreated', 'task', 't1', 'p1',
		 '{"chain_slug":"c1","position":1,"problem_statement":"do first thing"}', '019e7e00-0001-7000-8000-000000000001', 1),
		('019e7e00-0002-7000-8000-000000000002', '2026-05-21 00:00:02', 'system', 'test', 'TaskCreated', 'task', 't2', 'p1',
		 '{"chain_slug":"c1","position":2,"problem_statement":"do second thing"}', '019e7e00-0002-7000-8000-000000000002', 1),
		('019e7e00-0003-7000-8000-000000000003', '2026-05-21 00:00:03', 'system', 'test', 'TaskCompleted', 'task', 't2', 'p1',
		 '{}', '019e7e00-0003-7000-8000-000000000003', 1),
		('019e7e00-0004-7000-8000-000000000004', '2026-05-21 00:00:04', 'system', 'test', 'TaskCreated', 'task', 't3', 'p1',
		 '{"chain_slug":"c1","position":3,"problem_statement":"do third thing"}', '019e7e00-0004-7000-8000-000000000004', 1),
		('019e7e00-0005-7000-8000-000000000005', '2026-05-21 00:00:05', 'system', 'test', 'TaskTransitioned', 'task', 't3', 'p1',
		 '{"from_status":"pending","to_status":"blocked"}', '019e7e00-0005-7000-8000-000000000005', 1)`)

	// proj_chain_status must hold c1 before TaskCreated fold can resolve
	// chain_slug → chain_id, so rebuild chain_status first then
	// current_tasks. Capture the resulting checksum as canonical reference.
	mustExec(t, pool, `DELETE FROM proj_chain_status`)
	mustExec(t, pool, `DELETE FROM proj_current_tasks`)
	mustRebuild(t, pool, []string{"chain_status", "current_tasks"})
	reference := tableChecksum(t, pool, "proj_current_tasks")

	// Now wipe and rebuild again; checksum must match.
	mustExec(t, pool, `DELETE FROM proj_chain_status`)
	mustExec(t, pool, `DELETE FROM proj_current_tasks`)
	mustRebuild(t, pool, []string{"chain_status", "current_tasks"})
	after := tableChecksum(t, pool, "proj_current_tasks")
	if reference != after {
		t.Fatalf("proj_current_tasks checksum drift: reference=%s after=%s", reference, after)
	}

	// Sanity: rebuild produced the expected row count.
	if got := tableCount(t, pool, "proj_current_tasks"); got != 3 {
		t.Errorf("proj_current_tasks rows = %d, want 3", got)
	}
}

// TestTaskBlockers_RebuildFromEmpty exercises the proj_task_blockers
// fold module's RebuildFromEmpty path against a seeded task_blockers
// join table.
func TestTaskBlockers_RebuildFromEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Post-T6: rebuild replays events; the retired chains/tasks/task_blockers
	// CRUD tables are gone. Seed ChainCreated + TaskCreated for both tasks +
	// TaskTransitioned with blocker_slug to create the edge.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7f00-0000-7000-8000-000000000000', '2026-05-21 00:00:00', 'system', 'test', 'ChainCreated', 'chain', 'c1', 'p1',
		 '{"output":"","design_decisions":"","completion_condition":""}', '019e7f00-0000-7000-8000-000000000000', 1),
		('019e7f00-0001-7000-8000-000000000001', '2026-05-21 00:00:01', 'system', 'test', 'TaskCreated', 'task', 't-blocker', 'p1',
		 '{"chain_slug":"c1","position":1,"problem_statement":""}', '019e7f00-0001-7000-8000-000000000001', 1),
		('019e7f00-0002-7000-8000-000000000002', '2026-05-21 00:00:02', 'system', 'test', 'TaskCreated', 'task', 't-blocked', 'p1',
		 '{"chain_slug":"c1","position":2,"problem_statement":""}', '019e7f00-0002-7000-8000-000000000002', 1),
		('019e7f00-0003-7000-8000-000000000003', '2026-05-21 00:00:03', 'system', 'test', 'TaskTransitioned', 'task', 't-blocked', 'p1',
		 '{"from_status":"pending","to_status":"blocked","blocker_slug":"t-blocker"}', '019e7f00-0003-7000-8000-000000000003', 1)`)

	// chain_status + current_tasks must be folded before task_blockers
	// (the blocker fold resolves blocker_slug → task_id via proj_current_tasks).
	mustExec(t, pool, `DELETE FROM proj_chain_status`)
	mustExec(t, pool, `DELETE FROM proj_current_tasks`)
	mustExec(t, pool, `DELETE FROM proj_task_blockers`)
	mustRebuild(t, pool, []string{"chain_status", "current_tasks", "task_blockers"})
	reference := tableChecksum(t, pool, "proj_task_blockers")

	mustExec(t, pool, `DELETE FROM proj_chain_status`)
	mustExec(t, pool, `DELETE FROM proj_current_tasks`)
	mustExec(t, pool, `DELETE FROM proj_task_blockers`)
	mustRebuild(t, pool, []string{"chain_status", "current_tasks", "task_blockers"})
	after := tableChecksum(t, pool, "proj_task_blockers")
	if reference != after {
		t.Fatalf("proj_task_blockers checksum drift: reference=%s after=%s", reference, after)
	}

	if got := tableCount(t, pool, "proj_task_blockers"); got != 1 {
		t.Errorf("proj_task_blockers rows = %d, want 1", got)
	}
}
