package work_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/work"
)

// Task handler event-emit assertions (T2 of agent-first-substrate).
// One subtest per emitting handler.

func TestTaskStart_EmitsTaskTransitioned(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-start", "pending")

	resp, _ := work.HandleTaskStart(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-start", "chain_slug": "c",
	}))
	if !resp.OK {
		t.Fatalf("start rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-start")
	if typ != "TaskTransitioned" {
		t.Errorf("event type: got %q, want TaskTransitioned", typ)
	}
	var p struct {
		From string `json:"from_status"`
		To   string `json:"to_status"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if p.From != "pending" || p.To != "active" {
		t.Errorf("transition: %q → %q, want pending → active", p.From, p.To)
	}
}

func TestTaskComplete_EmitsTaskCompletedWithSHA(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-done", "active")

	resp, _ := work.HandleTaskComplete(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":           "t-done",
		"chain_slug":     "c",
		"commit_sha":     "abc1234",
		"handoff_output": "shipped X",
	}))
	if !resp.OK {
		t.Fatalf("complete rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-done")
	if typ != "TaskCompleted" {
		t.Errorf("event type: got %q, want TaskCompleted", typ)
	}
	var p struct {
		CommitSHA      *string `json:"commit_sha"`
		ClosureSummary *string `json:"closure_summary"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if p.CommitSHA == nil || *p.CommitSHA != "abc1234" {
		t.Errorf("commit_sha mismatch: %+v", p.CommitSHA)
	}
	if p.ClosureSummary == nil || *p.ClosureSummary != "shipped X" {
		t.Errorf("closure_summary mismatch: %+v", p.ClosureSummary)
	}
}

func TestTaskCancel_EmitsTaskCancelled(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-cancel", "pending")

	resp, _ := work.HandleTaskCancel(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-cancel", "chain_slug": "c",
	}))
	if !resp.OK {
		t.Fatalf("cancel rejected: %+v", resp)
	}
	typ, _ := lastEventForEntity(t, pool, "task", "t-cancel")
	if typ != "TaskCancelled" {
		t.Errorf("event type: got %q, want TaskCancelled", typ)
	}
}

func TestTaskReopen_EmitsTaskTransitioned(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-reopen", "closed")

	resp, _ := work.HandleTaskReopen(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-reopen", "chain_slug": "c",
	}))
	if !resp.OK {
		t.Fatalf("reopen rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-reopen")
	if typ != "TaskTransitioned" {
		t.Errorf("event type: got %q, want TaskTransitioned", typ)
	}
	var p struct {
		From string `json:"from_status"`
		To   string `json:"to_status"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if p.From != "closed" || p.To != "pending" {
		t.Errorf("transition: %q → %q, want closed → pending", p.From, p.To)
	}
}

func TestTaskBlock_EmitsTaskTransitionedWithBlocker(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-blocked", "active")
	seedTask(t, pool, "c", "t-blocker", "active")

	resp, _ := work.HandleTaskBlock(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":         "t-blocked",
		"chain_slug":   "c",
		"blocker_slug": "t-blocker",
		"reason":       "waiting on T-blocker",
	}))
	if !resp.OK {
		t.Fatalf("block rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-blocked")
	if typ != "TaskTransitioned" {
		t.Errorf("event type: got %q, want TaskTransitioned", typ)
	}
	var p struct {
		From        string  `json:"from_status"`
		To          string  `json:"to_status"`
		BlockerSlug *string `json:"blocker_slug"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if p.From != "active" || p.To != "blocked" {
		t.Errorf("transition: %q → %q, want active → blocked", p.From, p.To)
	}
	if p.BlockerSlug == nil || *p.BlockerSlug != "t-blocker" {
		t.Errorf("blocker_slug: %+v, want t-blocker", p.BlockerSlug)
	}
}

func TestTaskEdit_EmitsTaskEditedWithUpdatedFields(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-edit", "active")

	resp, _ := work.HandleTaskEdit(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":              "t-edit",
		"chain_slug":        "c",
		"problem_statement": "updated",
		"constraints":       "no new deps",
	}))
	if !resp.OK {
		t.Fatalf("edit rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-edit")
	if typ != "TaskEdited" {
		t.Errorf("event type: got %q, want TaskEdited", typ)
	}
	var p struct {
		UpdatedFields []string `json:"updated_fields"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if len(p.UpdatedFields) != 2 {
		t.Errorf("updated_fields: %+v, want 2 entries", p.UpdatedFields)
	}
}

func TestTaskStampSHA_EmitsTaskStamped(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "c")
	seedTask(t, pool, "c", "t-stamp", "closed")

	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "t-stamp", "chain_slug": "c", "commit_sha": "cafebabe",
	}))
	if !resp.OK {
		t.Fatalf("stamp rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "task", "t-stamp")
	if typ != "TaskStamped" {
		t.Errorf("event type: got %q, want TaskStamped", typ)
	}
	var p struct {
		CommitSHA string `json:"commit_sha"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if p.CommitSHA != "cafebabe" {
		t.Errorf("commit_sha: %q, want cafebabe", p.CommitSHA)
	}
}
