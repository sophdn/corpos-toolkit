package work_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/work"
)

// Smoke (a): happy path. Close T_n with sha + handoff, start T_n+1
// in the same chain via one lifecycle_step call. Both projection rows
// land in expected state; TaskHandoff event id is non-empty alongside
// the cascade close/start event ids.
func TestLifecycleStep_HappyPath(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "tn", "active")
	seedTask(t, pool, "c", "tn-plus-1", "pending")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"close_task_slug":      "tn",
		"close_commit_sha":     "deadbeef",
		"close_handoff_output": "T_n deliverable landed at deadbeef",
		"close_rationale":      "T_n acceptance criteria all met",
		"next_task_slug":       "tn-plus-1",
		"next_rationale":       "T_n+1 picked up",
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleLifecycleStep(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleLifecycleStep: %v", err)
	}
	if !res.OK || res.Error != "" {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if res.ChainSlug != "c" {
		t.Errorf("inferred chain_slug: got %q, want %q", res.ChainSlug, "c")
	}
	if res.CloseEventID == "" || res.StartEventID == "" || res.HandoffEventID == "" {
		t.Errorf("expected non-empty event ids, got close=%q start=%q handoff=%q",
			res.CloseEventID, res.StartEventID, res.HandoffEventID)
	}
	// Both projection rows reflect the transitions.
	var closedStatus, startedStatus string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 'tn'`).Scan(&closedStatus)
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 'tn-plus-1'`).Scan(&startedStatus)
	if closedStatus != "closed" {
		t.Errorf("close projection: status=%q", closedStatus)
	}
	if startedStatus != "active" {
		t.Errorf("start projection: status=%q", startedStatus)
	}
}

// Smoke (b): next-task-doesn't-exist. The same-chain lookup rejects
// pre-dispatch (the lookup returns "task not found in any chain")
// before any DB write. Close projection stays unchanged.
func TestLifecycleStep_NextTaskMissing_RejectsPreDispatch(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "tn", "active")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"close_task_slug": "tn",
		"close_rationale": "ready to close",
		"next_task_slug":  "does-not-exist",
		"next_rationale":  "next op references missing task",
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleLifecycleStep(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleLifecycleStep: %v", err)
	}
	if res.Error == "" || !strings.Contains(res.Error, "next_task_slug") {
		t.Errorf("expected next_task_slug rejection, got %+v", res)
	}
	// Close projection unchanged.
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 'tn'`).Scan(&status)
	if status != "active" {
		t.Errorf("pre-dispatch rejection leaked: tn status=%q", status)
	}
}

// Smoke (c): close-already-closed. The underlying task_complete on a
// closed task hits checkTaskTransition's gate; the batch fails and
// rolls back; lifecycle_step surfaces the rolled-back error. The
// already-closed task stays closed (no regression).
func TestLifecycleStep_CloseAlreadyClosed_RolledBackBatch(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "tn", "closed")
	seedTask(t, pool, "c", "tn-plus-1", "pending")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"close_task_slug": "tn",
		"close_rationale": "trying to close an already-closed task",
		"next_task_slug":  "tn-plus-1",
		"next_rationale":  "next op should not run",
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleLifecycleStep(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleLifecycleStep: %v", err)
	}
	if res.OK || res.Error == "" {
		t.Errorf("expected ok=false with error, got %+v", res)
	}
	if !res.Batch.RolledBack {
		t.Errorf("expected batch.rolled_back=true, got %+v", res.Batch)
	}
	// Next task stays pending (rollback worked).
	var nextStatus string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 'tn-plus-1'`).Scan(&nextStatus)
	if nextStatus != "pending" {
		t.Errorf("rollback failed: tn-plus-1 status=%q", nextStatus)
	}
}

// Smoke (d): cross-chain rejection. close_task_slug is in chain A;
// next_task_slug is in chain B. The same-chain invariant fires and
// rejects pre-dispatch, naming both chain slugs in the error.
func TestLifecycleStep_CrossChainHandoff_Rejects(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "chain-a")
	seedChain(t, pool, "mcp-servers", "chain-b")
	seedTask(t, pool, "chain-a", "task-in-a", "active")
	seedTask(t, pool, "chain-b", "task-in-b", "pending")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}
	params := map[string]any{
		"close_task_slug": "task-in-a",
		"close_rationale": "ready to close in chain A",
		"next_task_slug":  "task-in-b",
		"next_rationale":  "next op points at chain B — cross-chain seam",
	}
	raw, _ := json.Marshal(params)
	res, err := work.HandleLifecycleStep(context.Background(), deps, "mcp-servers", raw)
	if err != nil {
		t.Fatalf("HandleLifecycleStep: %v", err)
	}
	if res.Error == "" || !strings.Contains(res.Error, "cross-chain") {
		t.Errorf("expected cross-chain rejection, got %+v", res)
	}
	if !strings.Contains(res.Error, "chain-a") || !strings.Contains(res.Error, "chain-b") {
		t.Errorf("rejection should name both chain slugs, got %q", res.Error)
	}
	// Close projection unchanged.
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 'task-in-a'`).Scan(&status)
	if status != "active" {
		t.Errorf("pre-dispatch rejection leaked: task-in-a status=%q", status)
	}
}

// Smoke (e): missing rationale on either half rejects pre-execution.
// The close_rationale / next_rationale gates fire before the batch
// envelope is even built.
func TestLifecycleStep_MissingRationale_RejectsPreExecution(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "tn", "active")
	seedTask(t, pool, "c", "tn-plus-1", "pending")

	deps := work.TableDeps{Pool: pool, Schemas: loadForgeRegistry(t)}

	t.Run("missing close_rationale", func(t *testing.T) {
		params := map[string]any{
			"close_task_slug": "tn",
			"next_task_slug":  "tn-plus-1",
			"next_rationale":  "ok",
		}
		raw, _ := json.Marshal(params)
		res, _ := work.HandleLifecycleStep(context.Background(), deps, "mcp-servers", raw)
		if res.Error == "" || !strings.Contains(res.Error, "close_rationale") {
			t.Errorf("expected close_rationale rejection, got %+v", res)
		}
	})

	t.Run("missing next_rationale", func(t *testing.T) {
		params := map[string]any{
			"close_task_slug": "tn",
			"close_rationale": "ok",
			"next_task_slug":  "tn-plus-1",
		}
		raw, _ := json.Marshal(params)
		res, _ := work.HandleLifecycleStep(context.Background(), deps, "mcp-servers", raw)
		if res.Error == "" || !strings.Contains(res.Error, "next_rationale") {
			t.Errorf("expected next_rationale rejection, got %+v", res)
		}
	})
}
