package work_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/work"
)

// Smoke tests for forge (create) inside work.batch. Scoped to
// schema_name in {bug, suggestion, task}; chain creation with tasks is
// served by forge(chain, tasks=[…]) directly, so chain rejects in batch.

// (a) Happy path: batch of 3 forge(bug) ops land 3 bug rows + 3
// BugReported cascade events + 1 BatchExecuted.
func TestBatch_ForgeCreate_ThreeBugsHappyPath(t *testing.T) {
	pool := openTestPool(t)
	deps := forgeBatchDeps(pool, loadForgeRegistry(t))
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "file three bugs surfaced in one diagnostic session",
		"ops": []map[string]any{
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":       "bug",
					"slug":              "batch-bug-alpha",
					"title":             "alpha shape",
					"problem_statement": "alpha problem statement",
				},
				"rationale": "file alpha",
			},
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":       "bug",
					"slug":              "batch-bug-beta",
					"title":             "beta shape",
					"problem_statement": "beta problem statement",
				},
				"rationale": "file beta",
			},
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":       "bug",
					"slug":              "batch-bug-gamma",
					"title":             "gamma shape",
					"problem_statement": "gamma problem statement",
				},
				"rationale": "file gamma",
			},
		},
	})
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
	for i, op := range res.Ops {
		if !op.OK {
			t.Errorf("op[%d] not ok: %+v", i, op)
		}
		if op.EventID == nil || *op.EventID == "" {
			t.Errorf("op[%d] missing event id", i)
		}
	}
	// All three bug rows landed.
	for _, slug := range []string{"batch-bug-alpha", "batch-bug-beta", "batch-bug-gamma"} {
		var status string
		if err := pool.DB().QueryRow(`SELECT status FROM proj_current_bugs WHERE slug = ?`, slug).Scan(&status); err != nil {
			t.Errorf("bug %s missing from proj_current_bugs: %v", slug, err)
		}
		if status != "open" {
			t.Errorf("bug %s status = %q, want open", slug, status)
		}
	}
}

// (b) Mixed bug + suggestion in one batch — both arms route through
// their respective InTx variants in the same outer tx.
func TestBatch_ForgeCreate_MixedBugAndSuggestion(t *testing.T) {
	pool := openTestPool(t)
	deps := forgeBatchDeps(pool, loadForgeRegistry(t))
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "file one bug + one suggestion in one round-trip",
		"ops": []map[string]any{
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":       "bug",
					"slug":              "mixed-bug",
					"title":             "bug in mixed batch",
					"problem_statement": "bug content",
				},
				"rationale": "file bug",
			},
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":       "suggestion",
					"slug":              "mixed-suggestion",
					"title":             "suggestion in mixed batch",
					"problem_statement": "suggestion content",
				},
				"rationale": "file suggestion",
			},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.OK || res.RolledBack {
		t.Fatalf("expected ok=true rolled_back=false, got %+v", res)
	}
	var bugStatus, sugStatus string
	pool.DB().QueryRow(`SELECT status FROM proj_current_bugs WHERE slug = 'mixed-bug'`).Scan(&bugStatus)
	pool.DB().QueryRow(`SELECT status FROM proj_current_suggestions WHERE slug = 'mixed-suggestion'`).Scan(&sugStatus)
	if bugStatus != "open" {
		t.Errorf("bug status = %q, want open", bugStatus)
	}
	if sugStatus != "open" {
		t.Errorf("suggestion status = %q, want open", sugStatus)
	}
}

// (c) Per-op validation failure mid-batch rolls back the outer tx —
// the prior successful forge is undone.
func TestBatch_ForgeCreate_ValidationFailureRollsBack(t *testing.T) {
	pool := openTestPool(t)
	deps := forgeBatchDeps(pool, loadForgeRegistry(t))
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "test rollback when op 2 fails validation",
		"ops": []map[string]any{
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":       "bug",
					"slug":              "rollback-bug-one",
					"title":             "ok one",
					"problem_statement": "first bug, should be undone when op 2 fails",
				},
				"rationale": "valid op",
			},
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name": "bug",
					"slug":        "rollback-bug-two",
					// title + problem_statement missing → validation error
				},
				"rationale": "invalid op",
			},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.RolledBack {
		t.Errorf("expected rolled_back=true, got %+v", res)
	}
	// The valid prior op must have been rolled back.
	var n int
	pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_current_bugs WHERE slug = 'rollback-bug-one'`).Scan(&n)
	if n != 0 {
		t.Errorf("rollback-bug-one persisted despite rollback (count=%d)", n)
	}
}

// (d) scope gate: schema_name outside {bug, suggestion, task} rejects
// with a clear scope error naming the requested schema. `chain` is the
// canonical out-of-scope case — chain+tasks is served by
// forge(chain, tasks=[…]) directly, not by batching forge(chain).
func TestBatch_ForgeCreate_OutOfScopeSchemaRejects(t *testing.T) {
	pool := openTestPool(t)
	deps := forgeBatchDeps(pool, loadForgeRegistry(t))
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "test: forge(chain) rejects (not batch-creatable)",
		"ops": []map[string]any{
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":      "chain",
					"slug":             "out-of-scope-chain",
					"output":           "o",
					"design_decisions": "d",
				},
				"rationale": "expected to reject",
			},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("expected 1 failed op, got %+v", res)
	}
	op := res.Ops[0]
	if op.OK {
		t.Errorf("op should not be ok")
	}
	if op.ErrorMessage == nil || (!strings.Contains(*op.ErrorMessage, "batch-creatable") && !strings.Contains(*op.ErrorMessage, "not allowlisted")) {
		t.Errorf("error should name the scope; got %v", op.ErrorMessage)
	}
}

// (f) forge(task) into an already-existing chain lands a fully-populated
// task row at position 1 with an open status.
func TestBatch_ForgeCreate_TaskIntoExistingChain(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "host-chain")
	deps := forgeBatchDeps(pool, loadForgeRegistry(t))
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "add one task to an existing chain",
		"ops": []map[string]any{
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":         "task",
					"chain_slug":          "host-chain",
					"slug":                "added-task",
					"problem_statement":   "the added task's problem",
					"acceptance_criteria": []string{"criterion one", "criterion two"},
				},
				"rationale": "add task",
			},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.OK || res.RolledBack {
		t.Fatalf("expected ok=true rolled_back=false, got %+v", res)
	}
	if res.Ops[0].EventID == nil || *res.Ops[0].EventID == "" {
		t.Errorf("task op missing event id")
	}
	var status, problem string
	var position int
	if err := pool.DB().QueryRow(
		`SELECT status, position, problem_statement FROM proj_current_tasks WHERE slug = 'added-task'`,
	).Scan(&status, &position, &problem); err != nil {
		t.Fatalf("added-task missing from proj_current_tasks: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if position != 1 {
		t.Errorf("position = %d, want 1", position)
	}
	if problem != "the added task's problem" {
		t.Errorf("problem_statement = %q", problem)
	}
}

// (g) A run of forge(task) ops into one existing chain gets SEQUENTIAL
// positions — each task's MAX(position) read sees its predecessor's
// fold within the outer tx (read-through-tx). This is the property the
// pool.DB()-based handlers lack.
func TestBatch_ForgeCreate_MultipleTasksSequentialPositions(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "seq-chain")
	// Pre-seed one task so the batch must continue from MAX=1, not 0.
	seedTask(t, pool, "seq-chain", "pre-existing", "pending")
	deps := forgeBatchDeps(pool, loadForgeRegistry(t))
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "add three tasks; positions must be 2,3,4",
		"ops": []map[string]any{
			{"op": "forge", "params": map[string]any{"schema_name": "task", "chain_slug": "seq-chain", "slug": "task-two", "problem_statement": "two"}, "rationale": "t2"},
			{"op": "forge", "params": map[string]any{"schema_name": "task", "chain_slug": "seq-chain", "slug": "task-three", "problem_statement": "three"}, "rationale": "t3"},
			{"op": "forge", "params": map[string]any{"schema_name": "task", "chain_slug": "seq-chain", "slug": "task-four", "problem_statement": "four"}, "rationale": "t4"},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.OK || res.Succeeded != 3 {
		t.Fatalf("expected ok with 3 succeeded, got %+v", res)
	}
	for slug, want := range map[string]int{"task-two": 2, "task-three": 3, "task-four": 4} {
		var pos int
		if err := pool.DB().QueryRow(
			`SELECT position FROM proj_current_tasks WHERE slug = ?`, slug,
		).Scan(&pos); err != nil {
			t.Fatalf("%s missing: %v", slug, err)
		}
		if pos != want {
			t.Errorf("%s position = %d, want %d", slug, pos, want)
		}
	}
}

// (h) Design boundary: you CANNOT bootstrap a chain inside a batch and
// then forge tasks into it — chain is not batch-creatable, so the chain
// op rejects and (abort-on-first-error default) the whole batch rolls
// back, leaving neither chain nor task. The supported path for chain +
// tasks is forge(chain, tasks=[{…}]) directly. This test pins that
// boundary so a future widening of the gate to `chain` is a deliberate,
// test-breaking choice rather than an accident.
func TestBatch_ForgeCreate_ChainPlusTaskInBatchRejectsAndRollsBack(t *testing.T) {
	pool := openTestPool(t)
	deps := forgeBatchDeps(pool, loadForgeRegistry(t))
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "attempt chain bootstrap + task in one batch (unsupported)",
		"ops": []map[string]any{
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":      "chain",
					"slug":             "born-in-batch",
					"output":           "the chain output",
					"design_decisions": "the design",
				},
				"rationale": "attempt to create chain in batch",
			},
			{
				"op": "forge",
				"params": map[string]any{
					"schema_name":       "task",
					"chain_slug":        "born-in-batch",
					"slug":              "child-task",
					"problem_statement": "child of a same-batch chain",
				},
				"rationale": "task into the same-batch chain",
			},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.RolledBack {
		t.Errorf("expected rolled_back=true (chain op rejects), got %+v", res)
	}
	// The first op (chain) fails the scope gate; abort-on-first-error
	// never reaches the task op, so nothing persists.
	var chainCount, taskCount int
	pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_chain_status WHERE slug = 'born-in-batch'`).Scan(&chainCount)
	pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_current_tasks WHERE slug = 'child-task'`).Scan(&taskCount)
	if chainCount != 0 {
		t.Errorf("chain born-in-batch persisted despite rollback (count=%d)", chainCount)
	}
	if taskCount != 0 {
		t.Errorf("child-task persisted despite rollback (count=%d)", taskCount)
	}
}

// (e) `forge` is in AllowedBatchOps.
func TestBatch_ForgeInAllowlist(t *testing.T) {
	allowed := work.AllowedBatchOps()
	hit := false
	for _, op := range allowed {
		if op == "forge" {
			hit = true
			break
		}
	}
	if !hit {
		t.Errorf("forge missing from AllowedBatchOps: %v", allowed)
	}
}
