package projections_test

import (
	"context"
	"database/sql"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/testutil"
)

// TestFold_ChainStatusReflectsTaskTransitionPostState is the
// regression pin for bug
// `proj-chain-status-counters-always-one-task-transition-behind`.
//
// Setup: one chain with two pending tasks. Move t1 pending → active,
// then close t2. The chain_status projection MUST reflect the
// post-event counts after each emit, not the pre-event counts.
//
// The bug: chain_status projection's Name() ("chain_status") sorts
// before current_tasks ("current_tasks"), so FoldAll's Name-sorted
// iteration ran chain_status FIRST and recomputed its counters from
// proj_current_tasks BEFORE current_tasks had applied the event. Net:
// chain_status was permanently one transition behind. Dashboard (via
// observe-http GET /chains, which reads the denormalised counters
// directly) showed stale totals until the next task event in the
// chain triggered another fold cycle.
//
// Fix: FoldAll + RebuildAll honour an explicit DependsOn() interface
// on Projection so chain_status reads post-event projection state.
func TestFold_ChainStatusReflectsTaskTransitionPostState(t *testing.T) {
	pool := testutil.NewTestDB(t)
	installProjectionsFoldHook(t)
	seedProject(t, pool, "p1")

	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test"})

	// Create chain c1 + two pending tasks t1, t2.
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		if _, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("chain", "c1", "p1"),
			Payload: events.ChainCreatedPayload{Output: "x", DesignDecisions: "x", CompletionCondition: "x"},
		}); err != nil {
			return err
		}
		pos1, pos2 := 1, 2
		if _, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("task", "t1", "p1"),
			Payload: events.TaskCreatedPayload{ChainSlug: "c1", Position: &pos1, ProblemStatement: "p1"},
		}); err != nil {
			return err
		}
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("task", "t2", "p1"),
			Payload: events.TaskCreatedPayload{ChainSlug: "c1", Position: &pos2, ProblemStatement: "p2"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed chain + tasks: %v", err)
	}
	assertChainCounts(t, pool, "c1", chainCounts{Total: 2, Pending: 2})

	// Move t1 pending → active.
	mustEmitOne(t, pool, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("task", "t1", "p1"),
		Payload: events.TaskTransitionedPayload{FromStatus: "pending", ToStatus: "active"},
	})
	assertChainCounts(t, pool, "c1", chainCounts{Total: 2, Pending: 1, Active: 1})

	// Close t2. proj_chain_status MUST show pending=0 active=1
	// closed=1 immediately; before the fix this saw pending=1 (one
	// behind).
	sha, summary := "abc1234", "done"
	mustEmitOne(t, pool, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("task", "t2", "p1"),
		Payload: events.TaskCompletedPayload{CommitSHA: &sha, ClosureSummary: &summary},
	})
	assertChainCounts(t, pool, "c1", chainCounts{Total: 2, Active: 1, Closed: 1})
}

// TestFold_ChainStatusCorrectUnderTaskSlugCollision pins the
// chain-id resolution fix: two chains in the same project each carry
// a task with slug "1". chain_status must refresh the RIGHT chain's
// counts on each TaskCreated event, not arbitrarily pick whichever
// row sqlite returns first from a slug-only lookup.
//
// Before the fix `refreshChainTaskCountsForTaskSlug` ran
// `SELECT chain_id FROM proj_current_tasks WHERE slug = ?` — that
// returned one chain non-deterministically when N chains in the
// project shared a slug. The wrong chain's counts got refreshed (a
// no-op from its own data) and the right chain's never moved off
// zero. Seen in production as 30 drifted chains in seed-packet where
// numeric slugs ("1", "2", "3", "4") collide across 30+ chains
// each. Fix uses payload.chain_slug for TaskCreated /
// TaskAssignedToChain to disambiguate.
func TestFold_ChainStatusCorrectUnderTaskSlugCollision(t *testing.T) {
	pool := testutil.NewTestDB(t)
	installProjectionsFoldHook(t)
	seedProject(t, pool, "p1")

	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test"})

	// Two chains in the same project, each with tasks slugged "1" + "2".
	for _, chainSlug := range []string{"chain-a", "chain-b"} {
		mustEmitOne(t, pool, ctx, events.EmitArgs{
			Entity:  events.NewEntityRef("chain", chainSlug, "p1"),
			Payload: events.ChainCreatedPayload{Output: "x", DesignDecisions: "x", CompletionCondition: "x"},
		})
		for _, taskSlug := range []string{"1", "2"} {
			pos := 1
			if taskSlug == "2" {
				pos = 2
			}
			mustEmitOne(t, pool, ctx, events.EmitArgs{
				Entity: events.NewEntityRef("task", taskSlug, "p1"),
				Payload: events.TaskCreatedPayload{
					ChainSlug: chainSlug, Position: &pos, ProblemStatement: taskSlug,
				},
			})
		}
	}

	// Both chains MUST have total_tasks=2, pending=2.  Pre-fix the
	// slug-only lookup routed every refresh to whichever chain
	// happened to be returned first by sqlite — net was one chain at
	// 2/2 and the other stuck at 0/0.
	assertChainCounts(t, pool, "chain-a", chainCounts{Total: 2, Pending: 2})
	assertChainCounts(t, pool, "chain-b", chainCounts{Total: 2, Pending: 2})

	// Transition chain-a's "1" to active. The task fold updates
	// proj_current_tasks (status='active') for chain-a's task "1"
	// specifically (its WHERE clause uses chain_id). chain_status's
	// fan-out refresh runs against EVERY chain in the project that
	// has a task slugged "1" — both chains. chain-a re-reads its own
	// state (1 active, 1 pending); chain-b re-reads its state (still
	// 2 pending). Both agree with proj_current_tasks afterward.
	mustEmitOne(t, pool, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("task", "1", "p1"),
		Payload: events.TaskTransitionedPayload{FromStatus: "pending", ToStatus: "active"},
	})
	assertChainStatusAgreesWithCurrentTasks(t, pool, "chain-a")
	assertChainStatusAgreesWithCurrentTasks(t, pool, "chain-b")
}

// TestFold_ChainStatusMatchesCurrentTasksOverManyTransitions is a
// stress variant of the pin: drive a sequence of transitions and
// assert proj_chain_status agrees with the live count over
// proj_current_tasks after every emit. Captures the
// agreement-invariant the bug spec listed in acceptance criteria.
func TestFold_ChainStatusMatchesCurrentTasksOverManyTransitions(t *testing.T) {
	pool := testutil.NewTestDB(t)
	installProjectionsFoldHook(t)
	seedProject(t, pool, "p1")

	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test"})

	// Chain + 4 pending tasks.
	mustEmitOne(t, pool, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("chain", "c1", "p1"),
		Payload: events.ChainCreatedPayload{Output: "x", DesignDecisions: "x", CompletionCondition: "x"},
	})
	for i, slug := range []string{"t1", "t2", "t3", "t4"} {
		pos := i + 1
		mustEmitOne(t, pool, ctx, events.EmitArgs{
			Entity: events.NewEntityRef("task", slug, "p1"),
			Payload: events.TaskCreatedPayload{
				ChainSlug: "c1", Position: &pos, ProblemStatement: slug,
			},
		})
		assertChainStatusAgreesWithCurrentTasks(t, pool, "c1")
	}

	// Walk t1 through pending→active→closed; t2 through
	// pending→blocked→pending→active→closed; t3 cancelled; t4 stays
	// pending. After every emit the denormalised counters MUST agree
	// with the live COUNT(*) over proj_current_tasks.
	steps := []struct {
		slug    string
		payload events.Payload
	}{
		{"t1", events.TaskTransitionedPayload{FromStatus: "pending", ToStatus: "active"}},
		{"t1", events.TaskCompletedPayload{}},
		{"t2", events.TaskTransitionedPayload{FromStatus: "pending", ToStatus: "blocked"}},
		{"t2", events.TaskTransitionedPayload{FromStatus: "blocked", ToStatus: "pending"}},
		{"t2", events.TaskTransitionedPayload{FromStatus: "pending", ToStatus: "active"}},
		{"t2", events.TaskCompletedPayload{}},
		{"t3", events.TaskCancelledPayload{}},
	}
	for _, step := range steps {
		mustEmitOne(t, pool, ctx, events.EmitArgs{
			Entity:  events.NewEntityRef("task", step.slug, "p1"),
			Payload: step.payload,
		})
		assertChainStatusAgreesWithCurrentTasks(t, pool, "c1")
	}

	assertChainCounts(t, pool, "c1", chainCounts{Total: 4, Pending: 1, Closed: 2, Cancelled: 1})
}

// TestFold_DuplicateChainCreatedPreservesIdAndChildTasks is the regression
// pin for bug chain-create-fold-reassigns-pk-on-conflict-orphaning-tasks.
//
// foldChainCreated computed id = MAX(id)+1 then ran
// `ON CONFLICT(project_id, slug) DO UPDATE SET id = excluded.id, …` — so a
// SECOND ChainCreated for an already-existing (project, slug) reassigned the
// chain's primary key to that fresh value. Every child task (proj_current_tasks
// resolves chain_id from proj_chain_status by slug AT FOLD TIME) kept pointing
// at the OLD id and was silently orphaned. Observed live 2026-05-25: an
// erroneous forge(chain) re-emitted ChainCreated for chain 264, flipping it to
// id 306 and orphaning tasks 2491-2496 + 2768 (chain_state(264)->not found,
// chain_find(slug)->id 306 with 0 tasks, task_read still showing chain_id=264).
//
// Fix: drop `id = excluded.id` from the DO UPDATE clause so the existing id is
// preserved on conflict; the freshly-computed MAX+1 id is used only on a
// genuine insert. A re-fired ChainCreated may refresh content (output /
// completion_condition), never the PK.
func TestFold_DuplicateChainCreatedPreservesIdAndChildTasks(t *testing.T) {
	pool := testutil.NewTestDB(t)
	installProjectionsFoldHook(t)
	seedProject(t, pool, "p1")

	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test"})

	// Chain c1 + one pending task t1.
	mustEmitOne(t, pool, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("chain", "c1", "p1"),
		Payload: events.ChainCreatedPayload{Output: "v1", DesignDecisions: "v1", CompletionCondition: "v1"},
	})
	pos := 1
	mustEmitOne(t, pool, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("task", "t1", "p1"),
		Payload: events.TaskCreatedPayload{ChainSlug: "c1", Position: &pos, ProblemStatement: "p"},
	})

	var idBefore int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_chain_status WHERE slug = ?`, "c1").Scan(&idBefore); err != nil {
		t.Fatalf("read c1 id: %v", err)
	}
	assertChainCounts(t, pool, "c1", chainCounts{Total: 1, Pending: 1})

	// A SECOND ChainCreated for the SAME (project, slug) — the shape an
	// erroneous duplicate forge(chain) create produces. It must NOT reassign
	// the chain's id (that orphans child tasks); refreshing content is fine.
	mustEmitOne(t, pool, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("chain", "c1", "p1"),
		Payload: events.ChainCreatedPayload{Output: "v2", DesignDecisions: "v2", CompletionCondition: "v2"},
	})

	var idAfter int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_chain_status WHERE slug = ?`, "c1").Scan(&idAfter); err != nil {
		t.Fatalf("read c1 id after duplicate: %v", err)
	}
	if idAfter != idBefore {
		t.Fatalf("chain PK reassigned on duplicate ChainCreated: before=%d after=%d — orphans every child task", idBefore, idAfter)
	}

	// The child task must still be linked + counted (not orphaned).
	assertChainStatusAgreesWithCurrentTasks(t, pool, "c1")
	var taskChainID int64
	if err := pool.DB().QueryRow(`SELECT chain_id FROM proj_current_tasks WHERE slug = ?`, "t1").Scan(&taskChainID); err != nil {
		t.Fatalf("read t1 chain_id: %v", err)
	}
	if taskChainID != idBefore {
		t.Fatalf("task t1 orphaned: its chain_id=%d but chain c1 id=%d", taskChainID, idBefore)
	}
}

type chainCounts struct {
	Total     int64
	Pending   int64
	Active    int64
	Blocked   int64
	Closed    int64
	Cancelled int64
}

func assertChainCounts(t *testing.T, pool *db.Pool, slug string, want chainCounts) {
	t.Helper()
	var got chainCounts
	if err := pool.DB().QueryRow(
		`SELECT total_tasks, pending, active, blocked, closed, cancelled
		 FROM proj_chain_status WHERE slug = ?`, slug,
	).Scan(&got.Total, &got.Pending, &got.Active, &got.Blocked, &got.Closed, &got.Cancelled); err != nil {
		t.Fatalf("read proj_chain_status %s: %v", slug, err)
	}
	if got != want {
		t.Errorf("proj_chain_status[%s] = %+v, want %+v", slug, got, want)
	}
}

// assertChainStatusAgreesWithCurrentTasks asserts the denormalised
// counters on proj_chain_status match a live COUNT(*) over
// proj_current_tasks — the agreement invariant from the bug spec.
func assertChainStatusAgreesWithCurrentTasks(t *testing.T, pool *db.Pool, slug string) {
	t.Helper()
	var chainID int64
	if err := pool.DB().QueryRow(
		`SELECT id FROM proj_chain_status WHERE slug = ?`, slug,
	).Scan(&chainID); err != nil {
		t.Fatalf("read chain id %s: %v", slug, err)
	}
	var stored chainCounts
	if err := pool.DB().QueryRow(
		`SELECT total_tasks, pending, active, blocked, closed, cancelled
		 FROM proj_chain_status WHERE id = ?`, chainID,
	).Scan(&stored.Total, &stored.Pending, &stored.Active, &stored.Blocked, &stored.Closed, &stored.Cancelled); err != nil {
		t.Fatalf("read stored counts: %v", err)
	}
	var live chainCounts
	if err := pool.DB().QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status='pending'   THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='active'    THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='blocked'   THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='closed'    THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='cancelled' THEN 1 ELSE 0 END), 0)
		FROM proj_current_tasks WHERE chain_id = ?`, chainID,
	).Scan(&live.Total, &live.Pending, &live.Active, &live.Blocked, &live.Closed, &live.Cancelled); err != nil {
		t.Fatalf("read live counts: %v", err)
	}
	if stored != live {
		t.Errorf("proj_chain_status[%s] stored=%+v drifted from live proj_current_tasks=%+v",
			slug, stored, live)
	}
}

// mustEmitOne runs a single Emit in its own write tx and fails the
// test on error. Keeps the per-step loop terse without each call
// site repeating the WithWrite boilerplate.
func mustEmitOne(t *testing.T, pool *db.Pool, ctx context.Context, args events.EmitArgs) {
	t.Helper()
	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := events.Emit(ctx, tx, args)
		return err
	}); err != nil {
		t.Fatalf("emit %T (%s): %v", args.Payload, args.Entity.Slug, err)
	}
}
