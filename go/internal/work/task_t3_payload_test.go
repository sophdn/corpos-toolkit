package work_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/work"
)

// T3 of agent-substrate-crud-retirement: additive payload bumps for
// substrate-rebuild reconstruction. These tests pin the new fields:
//
//   - TaskEdited.updated_values map (§9.4)
//   - TaskTransitioned.removed_blocker_slug (§9.1)
//   - HandleTaskBlock L1181 guard lift: 2nd+ blocker INSERT emits
//     (blocked → blocked) TaskTransitioned with blocker_slug=<new>
//     (§9.1)
//   - HandleTaskUnblock: one TaskTransitioned per removed edge

func TestTaskEdit_PayloadCarriesUpdatedValuesMap(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-edit-vals", "active")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":              "t-edit-vals",
		"chain_slug":        "c",
		"problem_statement": "new statement",
		"constraints":       "constrained",
	}))
	if !resp.OK {
		t.Fatalf("edit rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-edit-vals")
	if typ != "TaskEdited" {
		t.Fatalf("event type: got %q, want TaskEdited", typ)
	}
	var p struct {
		UpdatedFields []string       `json:"updated_fields"`
		UpdatedValues map[string]any `json:"updated_values"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v (raw=%s)", err, payload)
	}
	if got := p.UpdatedValues["problem_statement"]; got != "new statement" {
		t.Errorf("updated_values[problem_statement]: got %v, want 'new statement'", got)
	}
	if got := p.UpdatedValues["constraints"]; got != "constrained" {
		t.Errorf("updated_values[constraints]: got %v, want 'constrained'", got)
	}
}

func TestTaskBlock_SecondBlockerEmitsBlockedToBlockedTransition(t *testing.T) {
	// L1181 guard lift: a 2nd+ blocker INSERT on an already-blocked task
	// must emit a TaskTransitioned event so payload-only fold can
	// reconstruct proj_task_blockers.
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-multi-blocked", "active")
	seedTask(t, pool, "c", "blocker-1", "active")
	seedTask(t, pool, "c", "blocker-2", "active")

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-multi-blocked", "chain_slug": "c", "blocker_slug": "blocker-1",
	}))
	if !resp.OK {
		t.Fatalf("first block rejected: %+v", resp)
	}
	// Now add a SECOND blocker — pre-T3 this emitted no event.
	resp, _ = work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-multi-blocked", "chain_slug": "c", "blocker_slug": "blocker-2",
	}))
	if !resp.OK {
		t.Fatalf("second block rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-multi-blocked")
	if typ != "TaskTransitioned" {
		t.Errorf("event type: got %q, want TaskTransitioned (2nd-blocker emit)", typ)
	}
	var p struct {
		From        string  `json:"from_status"`
		To          string  `json:"to_status"`
		BlockerSlug *string `json:"blocker_slug"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v (raw=%s)", err, payload)
	}
	if p.From != "blocked" || p.To != "blocked" {
		t.Errorf("transition: %q → %q, want blocked → blocked", p.From, p.To)
	}
	if p.BlockerSlug == nil || *p.BlockerSlug != "blocker-2" {
		t.Errorf("blocker_slug: %+v, want blocker-2", p.BlockerSlug)
	}
}

func TestTaskUnblock_SingleEdgeCarriesRemovedBlockerSlug(t *testing.T) {
	// Single-edge unblock that drops the blocked flag emits ONE
	// TaskTransitioned with from=blocked, to=pending, and
	// removed_blocker_slug populated.
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-single-unblock", "active")
	seedTask(t, pool, "c", "the-only-blocker", "active")

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-single-unblock", "chain_slug": "c", "blocker_slug": "the-only-blocker",
	}))
	if !resp.OK {
		t.Fatalf("block rejected: %+v", resp)
	}
	resp, _ = work.HandleTaskUnblock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-single-unblock", "chain_slug": "c", "blocker_slug": "the-only-blocker",
	}))
	if !resp.OK {
		t.Fatalf("unblock rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-single-unblock")
	if typ != "TaskTransitioned" {
		t.Fatalf("event type: got %q, want TaskTransitioned", typ)
	}
	var p struct {
		From               string  `json:"from_status"`
		To                 string  `json:"to_status"`
		RemovedBlockerSlug *string `json:"removed_blocker_slug"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v (raw=%s)", err, payload)
	}
	if p.From != "blocked" || p.To != "pending" {
		t.Errorf("transition: %q → %q, want blocked → pending", p.From, p.To)
	}
	if p.RemovedBlockerSlug == nil || *p.RemovedBlockerSlug != "the-only-blocker" {
		t.Errorf("removed_blocker_slug: %+v, want the-only-blocker", p.RemovedBlockerSlug)
	}
}

func TestTaskUnblock_MultiEdgeUnblockAllEmitsOnePerRemovedEdge(t *testing.T) {
	// Multi-edge unblock-all (no blocker_slug supplied) emits one
	// TaskTransitioned per removed edge. The LAST emit also flips the
	// task back to pending; the prior emits are (blocked → blocked)
	// self-transitions carrying removed_blocker_slug.
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-multi-unblock", "active")
	seedTask(t, pool, "c", "blocker-a", "active")
	seedTask(t, pool, "c", "blocker-b", "active")

	_, _ = work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-multi-unblock", "chain_slug": "c", "blocker_slug": "blocker-a",
	}))
	_, _ = work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-multi-unblock", "chain_slug": "c", "blocker_slug": "blocker-b",
	}))
	// Unblock-all: no blocker_slug.
	resp, _ := work.HandleTaskUnblock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-multi-unblock", "chain_slug": "c",
	}))
	if !resp.OK {
		t.Fatalf("unblock-all rejected: %+v", resp)
	}
	// Read every TaskTransitioned for this task; expect at least 2 with
	// non-null removed_blocker_slug (one per dropped edge).
	rows, err := pool.DB().Query(
		`SELECT payload FROM events WHERE entity_kind='task' AND entity_slug='t-multi-unblock'
		   AND type='TaskTransitioned' ORDER BY ts ASC, event_id ASC`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var removedSlugs []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var p struct {
			RemovedBlockerSlug *string `json:"removed_blocker_slug"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.RemovedBlockerSlug != nil {
			removedSlugs = append(removedSlugs, *p.RemovedBlockerSlug)
		}
	}
	if len(removedSlugs) != 2 {
		t.Errorf("expected 2 removed_blocker_slug entries across the events ledger, got %d (%v)",
			len(removedSlugs), removedSlugs)
	}
}
