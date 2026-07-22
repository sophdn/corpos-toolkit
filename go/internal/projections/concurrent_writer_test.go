package projections_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/testutil"
)

// TestFold_DuplicateCreatePreservesPK is the regression pin for the
// task/bug/suggestion half of the PK-stability invariant (chain
// worktree-multi-agent-orchestration-support T1). Bug 932 fixed the chains
// fold (`id = excluded.id` reassigned the PK on a re-fired Created event,
// orphaning every child/referrer); the sibling task/bug/suggestion folds
// carried the same `id = excluded.id` and are fixed alongside. A second
// Created event for an existing (project/chain, slug) must refresh content
// only — never reassign the primary key.
func TestFold_DuplicateCreatePreservesPK(t *testing.T) {
	pool := testutil.NewTestDB(t)
	installProjectionsFoldHook(t)
	seedProject(t, pool, "p1")
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test"})

	// Chain to anchor the task.
	mustEmitOne(t, pool, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("chain", "c1", "p1"),
		Payload: events.ChainCreatedPayload{Output: "o", DesignDecisions: "d", CompletionCondition: "c"},
	})

	cases := []struct {
		name       string
		table      string
		whereSlug  string
		first, dup events.EmitArgs
	}{
		{
			name:      "task",
			table:     "proj_current_tasks",
			whereSlug: "t1",
			first: events.EmitArgs{
				Entity:  events.NewEntityRef("task", "t1", "p1"),
				Payload: events.TaskCreatedPayload{ChainSlug: "c1", Position: intPtr(1), ProblemStatement: "v1"},
			},
			dup: events.EmitArgs{
				Entity:  events.NewEntityRef("task", "t1", "p1"),
				Payload: events.TaskCreatedPayload{ChainSlug: "c1", Position: intPtr(1), ProblemStatement: "v2"},
			},
		},
		{
			name:      "bug",
			table:     "proj_current_bugs",
			whereSlug: "b1",
			first: events.EmitArgs{
				Entity:  events.NewEntityRef("bug", "b1", "p1"),
				Payload: events.BugReportedPayload{Title: "t", ProblemStatement: "v1"},
			},
			dup: events.EmitArgs{
				Entity:  events.NewEntityRef("bug", "b1", "p1"),
				Payload: events.BugReportedPayload{Title: "t", ProblemStatement: "v2"},
			},
		},
		{
			name:      "suggestion",
			table:     "proj_current_suggestions",
			whereSlug: "s1",
			first: events.EmitArgs{
				Entity:  events.NewEntityRef("suggestion", "s1", "p1"),
				Payload: events.SuggestionReportedPayload{Title: "t", ProblemStatement: "v1"},
			},
			dup: events.EmitArgs{
				Entity:  events.NewEntityRef("suggestion", "s1", "p1"),
				Payload: events.SuggestionReportedPayload{Title: "t", ProblemStatement: "v2"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustEmitOne(t, pool, ctx, tc.first)
			var idBefore int64
			q := fmt.Sprintf(`SELECT id FROM %s WHERE slug = ?`, tc.table)
			if err := pool.DB().QueryRow(q, tc.whereSlug).Scan(&idBefore); err != nil {
				t.Fatalf("read %s id: %v", tc.name, err)
			}

			// Re-fire the Created event for the same slug — the shape an
			// erroneous duplicate forge produces. Must NOT reassign the PK.
			mustEmitOne(t, pool, ctx, tc.dup)

			var idAfter int64
			if err := pool.DB().QueryRow(q, tc.whereSlug).Scan(&idAfter); err != nil {
				t.Fatalf("read %s id after duplicate: %v", tc.name, err)
			}
			if idAfter != idBefore {
				t.Fatalf("%s PK reassigned on duplicate Created: before=%d after=%d", tc.name, idBefore, idAfter)
			}
		})
	}
}

// TestConcurrentWriters_SharedDB is the stress pin for the shared-DB
// concurrency model (chain worktree-multi-agent-orchestration-support T1).
//
// It simulates N parallel worktree agents, each running its own
// toolkit-server *process* against ONE shared data/toolkit.db: every agent
// gets an INDEPENDENT *db.Pool (a separate *sql.DB, hence a separate
// connection pool, which SQLite treats as a distinct process for file
// locking). All agents forge tasks AND complete them concurrently against
// the same chain — exercising both the MAX(id)+1 PK assignment (collision
// surface) and the chain_status counter recompute (fold-race surface).
//
// The model — WAL + _busy_timeout + _txlock=immediate (db.Open), plus the
// PK-stable folds — must yield:
//   - zero write errors (cross-process contention waits, it does not fail);
//   - every forged task present (no lost writes);
//   - unique primary keys (no MAX+1 collision);
//   - chain_status counters consistent with the rows (no lost counter update).
func TestConcurrentWriters_SharedDB(t *testing.T) {
	installProjectionsFoldHook(t)
	path := filepath.Join(t.TempDir(), "shared.db")

	// The base pool migrates the schema and seeds the project + chain BEFORE
	// any agent opens — so the agents' db.Open calls find every migration
	// already applied (a no-op) and never race on schema creation.
	base, err := db.Open(path)
	if err != nil {
		t.Fatalf("open base pool: %v", err)
	}
	defer base.Close()
	seedProject(t, base, "p1")
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "seed"})
	mustEmitOne(t, base, ctx, events.EmitArgs{
		Entity:  events.NewEntityRef("chain", "c1", "p1"),
		Payload: events.ChainCreatedPayload{Output: "o", DesignDecisions: "d", CompletionCondition: "c"},
	})

	const (
		agents   = 8
		perAgent = 12
	)
	var wg sync.WaitGroup
	errs := make(chan error, agents*perAgent*2)

	for a := 0; a < agents; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			// Independent pool == independent process from SQLite's lock view.
			pool, err := db.Open(path)
			if err != nil {
				errs <- fmt.Errorf("agent %d open: %w", a, err)
				return
			}
			defer pool.Close()
			actorCtx := events.WithActor(context.Background(),
				events.Actor{Kind: "agent", ID: fmt.Sprintf("agent-%d", a)})

			slugs := make([]string, perAgent)
			for i := range slugs {
				slug := fmt.Sprintf("t-%d-%d", a, i)
				slugs[i] = slug
				pos := a*perAgent + i + 1
				if err := pool.WithWrite(actorCtx, func(tx *sql.Tx) error {
					_, e := events.Emit(actorCtx, tx, events.EmitArgs{
						Entity:  events.NewEntityRef("task", slug, "p1"),
						Payload: events.TaskCreatedPayload{ChainSlug: "c1", Position: &pos, ProblemStatement: "p"},
					})
					return e
				}); err != nil {
					errs <- fmt.Errorf("agent %d create %s: %w", a, slug, err)
				}
			}
			// Close phase: complete every task this agent created. Runs
			// concurrently with other agents' creates + completes, hammering
			// the chain_status counter recompute.
			for _, slug := range slugs {
				if err := pool.WithWrite(actorCtx, func(tx *sql.Tx) error {
					_, e := events.Emit(actorCtx, tx, events.EmitArgs{
						Entity:  events.NewEntityRef("task", slug, "p1"),
						Payload: events.TaskCompletedPayload{ChainSlug: "c1"},
					})
					return e
				}); err != nil {
					errs <- fmt.Errorf("agent %d complete %s: %w", a, slug, err)
				}
			}
		}(a)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent write error (the model should serialize, not fail): %v", e)
	}

	want := int64(agents * perAgent)

	// No lost writes: every task row present.
	if got := tableCount(t, base, "proj_current_tasks"); got != want {
		t.Errorf("task count = %d, want %d (lost writes)", got, want)
	}

	// Unique PKs: no two tasks share an id (no MAX+1 collision).
	var distinct int64
	if err := base.DB().QueryRow(`SELECT COUNT(DISTINCT id) FROM proj_current_tasks`).Scan(&distinct); err != nil {
		t.Fatalf("count distinct ids: %v", err)
	}
	if distinct != want {
		t.Errorf("distinct task ids = %d, want %d (PK collision)", distinct, want)
	}

	// Every task folded to closed; chain_status counters agree with the rows
	// (no lost counter update under the concurrent fold).
	var total, pending, closed int64
	if err := base.DB().QueryRow(
		`SELECT total_tasks, pending, closed FROM proj_chain_status WHERE slug = ?`, "c1",
	).Scan(&total, &pending, &closed); err != nil {
		t.Fatalf("read chain_status: %v", err)
	}
	if total != want || closed != want || pending != 0 {
		t.Errorf("chain_status counters = {total:%d pending:%d closed:%d}, want {total:%d pending:0 closed:%d} (fold race)",
			total, pending, closed, want, want)
	}
}

func intPtr(i int) *int { return &i }
