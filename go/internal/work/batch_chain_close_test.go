package work_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/work"
)

// Smoke tests for T1 of batch-allowlist-widening: chain_close added to
// the work.batch allowlist. Canonical use case is batch([task_complete,
// chain_close]) — the chain-finalize pattern.

// (a) Happy path: batch([task_complete on the chain's last task,
// chain_close]). Both ops land in the outer tx; the chain ends up
// closed with closure_summary set; both cascade events emit. The
// non-terminal-tasks count inside chain_close sees the just-completed
// task because closeChainInTx reads via tx.QueryRowContext.
func TestBatch_ChainCloseChainFinalize_HappyPath(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "finalize-chain")
	seedTask(t, pool, "finalize-chain", "last-task", "active")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "chain-finalize: close last task + close chain in one round-trip",
		"ops": []map[string]any{
			{
				"op": "task_complete",
				"params": map[string]any{
					"slug":       "last-task",
					"chain_slug": "finalize-chain",
					"commit_sha": "cafebabe",
				},
				"rationale": "T-last landed in cafebabe",
			},
			{
				"op": "chain_close",
				"params": map[string]any{
					"slug":            "finalize-chain",
					"closure_summary": "Closed via batch([task_complete, chain_close]) — dog-foods T1 of batch-allowlist-widening.",
				},
				"rationale": "chain done; close it",
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
	if res.OpCount != 2 || res.Succeeded != 2 || res.Failed != 0 {
		t.Errorf("counts: %+v", res)
	}
	for i, op := range res.Ops {
		if !op.OK {
			t.Errorf("op[%d] (%s) not ok: %+v", i, op.Action, op)
		}
		if op.EventID == nil || *op.EventID == "" {
			t.Errorf("op[%d] (%s) missing cascade event id", i, op.Action)
		}
	}

	// Verify the projection state.
	var taskStatus, chainStatus, closureSummary string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 'last-task'`).Scan(&taskStatus)
	pool.DB().QueryRow(`SELECT status, closure_summary FROM proj_chain_status WHERE slug = 'finalize-chain'`).Scan(&chainStatus, &closureSummary)
	if taskStatus != "closed" {
		t.Errorf("last task status = %q, want closed", taskStatus)
	}
	if chainStatus != "closed" {
		t.Errorf("chain status = %q, want closed", chainStatus)
	}
	if !strings.Contains(closureSummary, "batch-allowlist-widening") {
		t.Errorf("closure_summary doesn't carry the batched value: %q", closureSummary)
	}
}

// (b) chain_close fails mid-batch (chain has a non-terminal task that
// wasn't included in the batch). Outer tx rolls back; the prior
// task_complete is UNDONE. The chain stays open.
func TestBatch_ChainCloseFailure_DefaultRollsBack(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "rollback-chain")
	seedTask(t, pool, "rollback-chain", "t-one", "active")
	seedTask(t, pool, "rollback-chain", "t-blocking", "pending") // NOT in the batch — will fail the chain_close

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "test: chain_close fails because t-blocking remains pending",
		"ops": []map[string]any{
			{
				"op": "task_complete",
				"params": map[string]any{
					"slug":       "t-one",
					"chain_slug": "rollback-chain",
					"commit_sha": "deadbeef",
				},
				"rationale": "complete t-one",
			},
			{
				"op": "chain_close",
				"params": map[string]any{
					"slug":            "rollback-chain",
					"closure_summary": "Should not land — t-blocking is still pending.",
				},
				"rationale": "attempt chain_close; expected to fail",
			},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if !res.RolledBack {
		t.Errorf("expected rolled_back=true on chain_close failure, got %+v", res)
	}
	if res.Failed != 1 {
		t.Errorf("failed count = %d, want 1", res.Failed)
	}

	// t-one's complete must have been undone.
	var taskStatus string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't-one'`).Scan(&taskStatus)
	if taskStatus != "active" {
		t.Errorf("task t-one status after rollback = %q, want active (rolled back)", taskStatus)
	}
	// Chain stays open.
	var chainStatus string
	pool.DB().QueryRow(`SELECT status FROM proj_chain_status WHERE slug = 'rollback-chain'`).Scan(&chainStatus)
	if chainStatus != "open" {
		t.Errorf("chain status = %q, want open", chainStatus)
	}
}

// (c) continue_on_error=true variant: chain_close failure leaves the
// preceding task_complete intact. The chain stays open, the task ends
// up closed. Asymmetric to (b)'s rollback semantics — the caller
// chose at-most-once semantics per op via the flag.
func TestBatch_ChainCloseFailure_ContinueOnErrorKeepsTaskComplete(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "continue-chain")
	seedTask(t, pool, "continue-chain", "t-one", "active")
	seedTask(t, pool, "continue-chain", "t-blocking", "pending")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale":   "test: continue_on_error keeps task_complete after chain_close failure",
		"continue_on_error": true,
		"ops": []map[string]any{
			{
				"op": "task_complete",
				"params": map[string]any{
					"slug":       "t-one",
					"chain_slug": "continue-chain",
					"commit_sha": "deadbeef",
				},
				"rationale": "complete t-one",
			},
			{
				"op": "chain_close",
				"params": map[string]any{
					"slug": "continue-chain",
				},
				"rationale": "attempt chain_close; expected to fail; tx should NOT roll back",
			},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if res.RolledBack {
		t.Errorf("continue_on_error=true should NOT roll back, got %+v", res)
	}

	// t-one IS closed (continue_on_error preserved its write).
	var taskStatus string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't-one'`).Scan(&taskStatus)
	if taskStatus != "closed" {
		t.Errorf("task t-one status = %q, want closed (continue_on_error preserved the prior op)", taskStatus)
	}
	// Chain stays open because chain_close failed.
	var chainStatus string
	pool.DB().QueryRow(`SELECT status FROM proj_chain_status WHERE slug = 'continue-chain'`).Scan(&chainStatus)
	if chainStatus != "open" {
		t.Errorf("chain status = %q, want open", chainStatus)
	}
}

// (d) Sanity: chain_close on an unknown chain rejects with chain_not_found
// — verifies the InTx variant's error mapping matches the non-tx-aware
// HandleChainClose's error envelope.
func TestBatch_ChainClose_UnknownChainRejects(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	raw, _ := json.Marshal(map[string]any{
		"batch_rationale": "test: chain_close on missing chain",
		"ops": []map[string]any{
			{
				"op": "chain_close",
				"params": map[string]any{
					"slug": "does-not-exist",
				},
				"rationale": "should reject with chain_not_found",
			},
		},
	})
	res, err := work.HandleBatch(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleBatch: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("failed count = %d, want 1; res=%+v", res.Failed, res)
	}
	op := res.Ops[0]
	if op.OK {
		t.Errorf("op should not be ok")
	}
	if !strings.Contains(*op.ErrorMessage, "chain_not_found") {
		t.Errorf("error should name chain_not_found; got %v", op.ErrorMessage)
	}
}

// (e) Allowlist parity: AllowedBatchOps lists chain_close so callers
// hitting the action-discovery surface see it.
func TestBatch_ChainCloseInAllowlist(t *testing.T) {
	allowed := work.AllowedBatchOps()
	hit := false
	for _, op := range allowed {
		if op == "chain_close" {
			hit = true
			break
		}
	}
	if !hit {
		t.Errorf("chain_close missing from AllowedBatchOps: %v", allowed)
	}
}
