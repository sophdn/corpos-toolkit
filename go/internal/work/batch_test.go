package work_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toolkit/internal/construct"
	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
	"toolkit/internal/testutil"
	"toolkit/internal/work"
)

// forgeCreateInTxSeam / forgeEditInTxSeam build the construct in-tx seams the
// work.batch forge/forge_edit ops require post forge-archive (chain 311 T7 Stage
// 6 P2-C.2) — mirrors cmd/toolkit-server's batchForgeCreateInTx/batchForgeEditInTx
// (the forge in-tx fallback that tests used to rely on is gone, so a batch with
// forge ops MUST wire these).
func forgeCreateInTxSeam(pool *db.Pool, schemas *registry.Registry) func(context.Context, *sql.Tx, string, json.RawMessage) (string, error) {
	return func(ctx context.Context, tx *sql.Tx, project string, raw json.RawMessage) (string, error) {
		var peek struct {
			SchemaName string `json:"schema_name"`
			Kind       string `json:"kind"`
		}
		_ = json.Unmarshal(raw, &peek)
		name := peek.SchemaName
		if name == "" {
			name = peek.Kind
		}
		if name != "" && !construct.BatchEligible(name) {
			return "", errors.New("forge(" + name + ") is not batch-creatable — batch forge create is scoped to bug/suggestion/task")
		}
		cdeps := construct.Deps{Pool: pool, Schemas: schemas}
		prep, rej, err := construct.PrepareForge(cdeps, project, raw)
		if err != nil {
			return "", err
		}
		if rej != nil {
			return "", errors.New(rej.Error)
		}
		in, err := construct.InputFromForge(prep)
		if err != nil {
			return "", err
		}
		return construct.CreateInTx(ctx, tx, cdeps, prep.SchemaName, project, in, prep.Validated)
	}
}

func forgeEditInTxSeam(pool *db.Pool, schemas *registry.Registry) func(context.Context, *sql.Tx, string, json.RawMessage) (string, error) {
	return func(ctx context.Context, tx *sql.Tx, project string, raw json.RawMessage) (string, error) {
		cdeps := construct.Deps{Pool: pool, Schemas: schemas}
		prep, rej, err := construct.PrepareForgeEdit(cdeps, project, raw)
		if err != nil {
			return "", err
		}
		if rej != nil {
			return "", errors.New(rej.Error)
		}
		return construct.UpdateInTx(ctx, tx, cdeps, prep.SchemaName, project, prep.Slug, prep.ChainSlug, prep.Validated)
	}
}

// forgeBatchDeps returns TableDeps wired with both forge in-tx seams (+ Pool +
// Schemas), for batch tests that exercise forge/forge_edit ops.
func forgeBatchDeps(pool *db.Pool, schemas *registry.Registry) work.TableDeps {
	return work.TableDeps{
		Pool:            pool,
		Schemas:         schemas,
		ForgeCreateInTx: forgeCreateInTxSeam(pool, schemas),
		ForgeEditInTx:   forgeEditInTxSeam(pool, schemas),
	}
}

// loadForgeRegistry locates blueprints/forge-schemas relative to the
// test CWD and loads it into a registry. Used by batch tests that
// exercise forge_edit. Mirrors the helper in forge_test.go (the work
// package can't import test helpers from the forge package).
func loadForgeRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	wd, _ := os.Getwd()
	var dir string
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		candidate := filepath.Join(d, "blueprints", "forge-schemas")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			dir = candidate
			break
		}
	}
	if dir == "" {
		t.Skip("blueprints/forge-schemas not found relative to test CWD")
	}
	r, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	return r
}

// seedTaskWithChainSlug returns the (chain_slug) for the seeded task —
// the test helper seedTask seeds against a chain by slug.
//
// Smoke (a): 3-op happy path. task_complete + task_start + bug_resolve.
// All three commit atomically; each op's cascade event id lands in the
// per-op result; BatchExecuted emits after the outer tx commits.
func TestBatch_HappyPath_ThreeOps(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")
	seedTask(t, pool, "c", "t2", "pending")
	testutil.SeedBug(t, pool, "mcp-servers", "b1", "open", testutil.SeedBugOpts{})

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"batch_rationale": "T1 → T2 handoff plus a parallel bug resolution",
		"ops": []map[string]any{
			{
				"op":        "task_complete",
				"params":    map[string]any{"slug": "t1", "chain_slug": "c", "commit_sha": "deadbeef"},
				"rationale": "T1 work landed in deadbeef",
			},
			{
				"op":        "task_start",
				"params":    map[string]any{"slug": "t2", "chain_slug": "c"},
				"rationale": "next task in the chain picked up",
			},
			{
				"op":        "bug_resolve",
				"params":    map[string]any{"slug": "b1", "resolution_kind": "fixed", "commit_sha": "deadbeef"},
				"rationale": "same SHA closed the unrelated bug",
			},
		},
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.OK || res.RolledBack {
		t.Fatalf("expected ok=true rolled_back=false, got %+v", res)
	}
	if res.OpCount != 3 || res.Succeeded != 3 || res.Failed != 0 {
		t.Errorf("counts: %+v", res)
	}
	// Each op gets a non-empty cascade event id.
	for i, op := range res.Ops {
		if !op.OK {
			t.Errorf("op[%d] not ok: %+v", i, op)
		}
		if op.EventID == nil || *op.EventID == "" {
			t.Errorf("op[%d] missing cascade event id: %+v", i, op)
		}
	}
	if res.BatchEventID == "" {
		t.Errorf("BatchExecuted event id missing")
	}

	// Verify the writes landed in the projections.
	var t1Status, t2Status, bugStatus string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&t1Status)
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't2'`).Scan(&t2Status)
	pool.DB().QueryRow(`SELECT status FROM proj_current_bugs WHERE slug = 'b1'`).Scan(&bugStatus)
	if t1Status != "closed" || t2Status != "active" || bugStatus != "fixed" {
		t.Errorf("projection state after batch: t1=%q t2=%q bug=%q", t1Status, t2Status, bugStatus)
	}
}

// Smoke (b): 1-op trivial degenerate. A single task_complete via batch
// must produce the same projection state as a direct task_complete. The
// batch envelope is doing real work (outer tx + per-op rationale gate
// + BatchExecuted emit) but the underlying transition is unchanged.
func TestBatch_SingleOp_DegenerateMatchesDirectCall(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"ops": []map[string]any{
			{
				"op":        "task_complete",
				"params":    map[string]any{"slug": "t1", "chain_slug": "c"},
				"rationale": "single-op batch trivial case",
			},
		},
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.OK || res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("trivial batch outcome: %+v", res)
	}

	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "closed" {
		t.Errorf("post-batch projection: status=%q", status)
	}
}

// Smoke (c): failure mid-batch in default mode. Op[0] succeeds, op[1]
// fails on a non-existent task. The outer tx rolls back; the
// previously-stamped task_complete is UNDONE; the per-op result for
// op[0] retains ok=true but EventID is stripped (the cascade event was
// rolled back); RolledBack=true on the envelope.
func TestBatch_FailureMidBatch_DefaultRollsBack(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"ops": []map[string]any{
			{
				"op":        "task_complete",
				"params":    map[string]any{"slug": "t1", "chain_slug": "c"},
				"rationale": "first op should land then be rolled back",
			},
			{
				"op":        "task_start",
				"params":    map[string]any{"slug": "does-not-exist", "chain_slug": "c"},
				"rationale": "second op fails on unknown slug; batch aborts",
			},
		},
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if res.OK || !res.RolledBack {
		t.Fatalf("expected ok=false rolled_back=true, got %+v", res)
	}
	if res.Failed != 1 || res.Succeeded != 1 {
		t.Errorf("counts: %+v", res)
	}
	// Op[0] ran but its cascade event is rolled back; EventID stripped.
	if !res.Ops[0].OK || res.Ops[0].EventID != nil {
		t.Errorf("op[0] expected ok=true event_id=nil after rollback, got %+v", res.Ops[0])
	}
	if res.Ops[1].OK || res.Ops[1].ErrorKind == nil {
		t.Errorf("op[1] expected ok=false with error_kind, got %+v", res.Ops[1])
	}
	// The actual database state: t1 is STILL active (rollback worked).
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "active" {
		t.Errorf("rollback failed: t1 status=%q (expected unchanged from seeded 'active')", status)
	}
}

// Smoke (d): failure mid-batch with continue_on_error=true. The failing
// op records its error in per-op status; subsequent ops still execute;
// each successful op commits independently (outer tx still commits).
func TestBatch_ContinueOnError_KeepsRunningAfterFailure(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")
	seedTask(t, pool, "c", "t2", "pending")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"continue_on_error": true,
		"ops": []map[string]any{
			{
				"op":        "task_start",
				"params":    map[string]any{"slug": "missing", "chain_slug": "c"},
				"rationale": "this op fails; continue_on_error should keep going",
			},
			{
				"op":        "task_complete",
				"params":    map[string]any{"slug": "t1", "chain_slug": "c"},
				"rationale": "this op runs after op[0] failed",
			},
			{
				"op":        "task_start",
				"params":    map[string]any{"slug": "t2", "chain_slug": "c"},
				"rationale": "and so does this one",
			},
		},
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if res.RolledBack {
		t.Fatalf("continue_on_error must not roll back: %+v", res)
	}
	if res.Failed != 1 || res.Succeeded != 2 {
		t.Errorf("counts: %+v", res)
	}
	// Op[0] failed; op[1] and op[2] succeeded and persisted.
	if res.Ops[0].OK || res.Ops[1].OK == false || res.Ops[2].OK == false {
		t.Errorf("per-op status: %+v", res.Ops)
	}
	var t1, t2 string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&t1)
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't2'`).Scan(&t2)
	if t1 != "closed" || t2 != "active" {
		t.Errorf("post-batch projections: t1=%q t2=%q", t1, t2)
	}
}

// Smoke (e): missing rationale on any sub-op rejects the whole batch
// pre-execution. No DB writes happen; no tx opens.
func TestBatch_MissingPerOpRationale_RejectsPreExecution(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"ops": []map[string]any{
			{
				"op":        "task_complete",
				"params":    map[string]any{"slug": "t1", "chain_slug": "c"},
				"rationale": "ok rationale",
			},
			{
				"op":     "task_complete",
				"params": map[string]any{"slug": "t1", "chain_slug": "c"},
				// rationale omitted on purpose
			},
		},
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if res.Error == "" || !strings.Contains(res.Error, "ops[1]") {
		t.Errorf("expected rejection pointing at ops[1], got %+v", res)
	}
	// Op[0] must NOT have run.
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "active" {
		t.Errorf("pre-execution rejection leaked a write: t1 status=%q", status)
	}
}

// Smoke (f): 3-op forge_edit batch. Re-executes a sequence shaped like
// the actual T7/T8/T9 chain-forge follow-up that motivated this chain
// (three task_edit calls on different tasks in the same chain to add
// token-budget measurement anchors).
func TestBatch_ThreeForgeEdits_LandSameStateAsSequential(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "ta", "pending")
	seedTask(t, pool, "c", "tb", "pending")
	seedTask(t, pool, "c", "tc", "pending")

	deps := forgeBatchDeps(pool, loadForgeRegistry(t))
	params := map[string]any{
		"batch_rationale": "T7/T8/T9-shaped follow-up: three forge_edits in one round-trip",
		"ops": []map[string]any{
			{
				"op": "forge_edit",
				"params": map[string]any{
					"schema_name":       "task",
					"slug":              "ta",
					"chain_slug":        "c",
					"problem_statement": "edited via batch op 0",
				},
				"rationale": "edit ta",
			},
			{
				"op": "forge_edit",
				"params": map[string]any{
					"schema_name":       "task",
					"slug":              "tb",
					"chain_slug":        "c",
					"problem_statement": "edited via batch op 1",
				},
				"rationale": "edit tb",
			},
			{
				"op": "forge_edit",
				"params": map[string]any{
					"schema_name":       "task",
					"slug":              "tc",
					"chain_slug":        "c",
					"problem_statement": "edited via batch op 2",
				},
				"rationale": "edit tc",
			},
		},
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.OK || res.Succeeded != 3 {
		t.Fatalf("expected 3-op success, got %+v", res)
	}
	// Each task's problem_statement was updated.
	for i, slug := range []string{"ta", "tb", "tc"} {
		var ps string
		pool.DB().QueryRow(`SELECT problem_statement FROM proj_current_tasks WHERE slug = ?`, slug).Scan(&ps)
		want := "edited via batch op " + string(rune('0'+i))
		if ps != want {
			t.Errorf("task %s: got %q, want %q", slug, ps, want)
		}
	}
}

// Smoke (g): forge_edit in a batch with the index-upsert notifier wired
// (mirroring main.go's ForgeOnEdit) must NOT deadlock and must sync the
// knowledge_pointers index through the OUTER batch tx. Regression for
// bug `forge-edit-in-batch-deadlocks-via-nested-pool-withwrite-in-onedit-notifier`.
//
// The bug: HandleBatch holds db.Pool's non-reentrant write mutex across
// the whole batch via one outer WithWrite; the pool-based OnEdit notifier
// (IndexUpsertOnEditNotifier → pointers.Upsert → pool.WithWrite) re-entered
// that mutex on the SAME goroutine → permanent deadlock (observed 505s at
// 0% CPU in production). The other forge_edit batch tests construct deps
// WITHOUT ForgeOnEdit, so the notifier never fired — which is exactly the
// test gap that let the deadlock ship. This test wires it like main.go does.
//
// Post-fix the InTx path runs the index sync through the outer tx, so it
// both (a) completes and (b) reads the PENDING edit back (not stale state):
// the task pointer's question reflects the just-written problem_statement.
func TestBatch_ForgeEditTask_WithIndexNotifier_SyncsThroughOuterTx(t *testing.T) {
	pool := openTestPool(t)
	schemas := loadForgeRegistry(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "ta", "pending")

	deps := work.TableDeps{
		Pool:    pool,
		Schemas: schemas,
		// Post forge-archive (chain 311 T7 Stage 6 P2-C.2) batch routes forge_edit
		// through the construct in-tx seam (mirrors main.go's batchForgeEditInTx);
		// the index sync runs on the OUTER tx via construct.UpdateInTx →
		// IndexUpsertOnEditInTx. The pool-based notifier that used to deadlock here
		// is no longer wired into batch at all.
		ForgeEditInTx: func(ctx context.Context, tx *sql.Tx, project string, raw json.RawMessage) (string, error) {
			cdeps := construct.Deps{Pool: pool, Schemas: schemas}
			prep, rej, err := construct.PrepareForgeEdit(cdeps, project, raw)
			if err != nil {
				return "", err
			}
			if rej != nil {
				return "", errors.New(rej.Error)
			}
			return construct.UpdateInTx(ctx, tx, cdeps, prep.SchemaName, project, prep.Slug, prep.ChainSlug, prep.Validated)
		},
	}
	const newProblem = "edited in batch via forge_edit — index must reflect this"
	params := map[string]any{
		"batch_rationale": "regression: forge_edit + index notifier must not deadlock",
		"ops": []map[string]any{
			{
				"op": "forge_edit",
				"params": map[string]any{
					"schema_name":       "task",
					"slug":              "ta",
					"chain_slug":        "c",
					"problem_statement": newProblem,
				},
				"rationale": "edit ta; fires the index-upsert notifier in-tx",
			},
		},
	}
	raw, _ := json.Marshal(params)

	// Run in a goroutine with a hard timeout: pre-fix this deadlocks
	// forever (the nested WithWrite blocks on a mutex its own caller
	// holds), so a plain call would hang the whole test binary.
	done := make(chan struct{})
	var res work.BatchResult
	var err error
	go func() {
		res, err = work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("forge_edit-in-batch with the index notifier deadlocked (nested pool.WithWrite in OnEdit); did not complete in 15s")
	}
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.OK || res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("batch outcome: %+v", res)
	}
	// The edit committed via the outer tx.
	var ps string
	pool.DB().QueryRow(`SELECT problem_statement FROM proj_current_tasks WHERE slug = 'ta'`).Scan(&ps)
	if ps != newProblem {
		t.Errorf("task problem_statement = %q, want %q", ps, newProblem)
	}
	// The index synced through the OUTER tx and read the PENDING edit
	// (not stale pre-edit state): the task pointer's question reflects
	// the new problem_statement. Pointer shape per buildTaskPointer:
	// source_type='task', source_ref='<project>::<chain>::<slug>'.
	var question string
	if err := pool.DB().QueryRow(
		`SELECT question FROM knowledge_pointers WHERE source_type='task' AND source_ref='mcp-servers::c::ta'`,
	).Scan(&question); err != nil {
		t.Fatalf("knowledge_pointers row for task ta not found (in-tx index sync did not run?): %v", err)
	}
	if !strings.Contains(question, "edited in batch") {
		t.Errorf("index question = %q, want it to reflect the new problem_statement (proves in-tx read-back, not stale)", question)
	}
}

// TestBatch_ForgeBug_WithIndexNotifier_SyncsThroughOuterTx: a bug created
// through work.batch with ForgeOnCreate wired (mirroring main.go) must land
// its knowledge_pointers index entry through the OUTER batch tx — the same
// shape the non-batch HandleForge path produces via deps.OnCreate. Regression
// for bug `forge-create-in-batch-skips-knowledge-pointer-onecreate-not-wired`:
// the batch forge Deps was constructed WITHOUT OnCreate, so HandleForgeInTx
// emitted the BugReported event (→ proj_current_bugs + bugs_fts) but never
// synced knowledge_pointers, leaving batch-created bugs invisible to
// knowledge_search / resolve_references. Like the edit fix, the sync runs
// in-tx (NOT the pool-based notifier, which would re-enter the held write
// mutex and deadlock).
func TestBatch_ForgeBug_WithIndexNotifier_SyncsThroughOuterTx(t *testing.T) {
	pool := openTestPool(t)
	schemas := loadForgeRegistry(t)
	if _, err := pool.DB().Exec(`INSERT OR IGNORE INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	deps := work.TableDeps{
		Pool:    pool,
		Schemas: schemas,
		// Post forge-archive: batch routes forge create through the construct
		// in-tx seam (mirrors main.go's batchForgeCreateInTx); the knowledge_pointer
		// sync runs on the OUTER tx via construct.CreateInTx → IndexUpsertOnCreateInTx.
		ForgeCreateInTx: func(ctx context.Context, tx *sql.Tx, project string, raw json.RawMessage) (string, error) {
			cdeps := construct.Deps{Pool: pool, Schemas: schemas}
			prep, rej, err := construct.PrepareForge(cdeps, project, raw)
			if err != nil {
				return "", err
			}
			if rej != nil {
				return "", errors.New(rej.Error)
			}
			in, err := construct.InputFromForge(prep)
			if err != nil {
				return "", err
			}
			return construct.CreateInTx(ctx, tx, cdeps, prep.SchemaName, project, in, prep.Validated)
		},
	}
	params := map[string]any{
		"ops": []map[string]any{
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":       "bug",
					"slug":              "batch-pointer-test",
					"title":             "batch-created bug must land a knowledge pointer",
					"problem_statement": "verifying in-tx index sync on the batch forge create path",
				},
				"rationale": "create a bug in batch; index sync must run in-tx",
			},
		},
	}
	raw, _ := json.Marshal(params)

	done := make(chan struct{})
	var res work.BatchResult
	var err error
	go func() {
		res, err = work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("forge-in-batch with the index notifier deadlocked or hung; did not complete in 15s")
	}
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.OK || res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("batch outcome: %+v", res)
	}
	// The bug committed via the outer tx.
	var status string
	if err := pool.DB().QueryRow(`SELECT status FROM proj_current_bugs WHERE slug = 'batch-pointer-test'`).Scan(&status); err != nil {
		t.Fatalf("proj_current_bugs row not found: %v", err)
	}
	// The knowledge_pointers entry synced through the OUTER tx. Pointer
	// shape per buildBugPointer: source_type='bug', source_ref='<proj>::<slug>'.
	var question string
	if err := pool.DB().QueryRow(
		`SELECT question FROM knowledge_pointers WHERE source_type='bug' AND source_ref='mcp-servers::batch-pointer-test'`,
	).Scan(&question); err != nil {
		t.Fatalf("knowledge_pointers row for batch-created bug not found (in-tx index sync did not run?): %v", err)
	}
}

// Pre-execution gate: an unknown (or not-allowlisted) action rejects
// the whole batch with a clear error. Closes the silent-drop vault
// learning's prescription: every new action surface rejects unknown
// params/ops at dispatch with the accepted list named.
func TestBatch_UnknownAction_RejectsPreExecution(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t1", "active")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"ops": []map[string]any{
			{"op": "task_complete", "params": map[string]any{"slug": "t1", "chain_slug": "c"}, "rationale": "ok"},
			// `roadmap_set` is a real work-surface action that's NOT
			// allowlisted; use it as the canonical not-allowlisted op.
			// Pre-T1-of-batch-allowlist-widening this test used chain_close;
			// chain_close moved into the allowlist so the test needed a
			// different unknown op.
			{"op": "roadmap_set", "params": map[string]any{}, "rationale": "roadmap_set not allowlisted"},
		},
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if res.Error == "" || !strings.Contains(res.Error, "not batch-allowlisted") {
		t.Errorf("expected allowlist-rejection error, got %+v", res)
	}
	// Op[0] must NOT have run.
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't1'`).Scan(&status)
	if status != "active" {
		t.Errorf("pre-execution rejection leaked a write: t1 status=%q", status)
	}
}

// Sanity: AllowedBatchOps returns the current allowlist sorted. Pinning
// the public surface so a future allowlist expansion is intentional.
// Post-T1-of-batch-allowlist-widening: chain_close added.
func TestAllowedBatchOps_Sorted(t *testing.T) {
	got := work.AllowedBatchOps()
	want := []string{"bug_resolve", "chain_close", "forge", "forge_edit", "task_complete", "task_start"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want=%q", i, got[i], want[i])
		}
	}
}
