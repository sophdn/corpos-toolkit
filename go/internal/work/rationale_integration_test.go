package work_test

import (
	"context"
	"database/sql"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/work"
)

// TestRationaleFromContext_RecordedOnEventsRow is the integration test
// for T3's plumbing: when the dispatcher stamps rationale onto ctx
// (events.WithRationale), the existing handler emit calls (which do NOT
// set EmitArgs.Rationale) must still record the rationale on the events
// row. Verifies the full chain dispatch-ctx → events.Emit → SQL column.
func TestRationaleFromContext_RecordedOnEventsRow(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "rat-bug", "open")
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test-agent"})
	ctx = events.WithRationale(ctx, "closing as fixed after confirming patch in main")

	resp, err := work.HandleBugResolve(ctx, pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":            "rat-bug",
		"resolution_kind": "fixed",
		"commit_sha":      "abc1234",
	}))
	if err != nil {
		t.Fatalf("HandleBugResolve: %v", err)
	}
	if !resp.OK {
		t.Fatalf("resolve rejected: %+v", resp)
	}
	rationale := readRationaleForLatestEvent(t, pool, "bug", "rat-bug")
	if rationale == nil {
		t.Fatal("rationale column is NULL — ctx value was not picked up by Emit")
	}
	if got := *rationale; got != "closing as fixed after confirming patch in main" {
		t.Errorf("rationale mismatch: got %q", got)
	}
}

// TestRationaleAbsentFromContext_NullColumn pins that when the
// dispatcher does NOT stamp rationale onto ctx (e.g. degraded mode, or
// human actor with no rationale supplied), the events row's rationale
// column lands NULL — the existing T2 behaviour. Verifies the optional-
// ctx-fallback in events.Emit doesn't accidentally synthesize a value.
func TestRationaleAbsentFromContext_NullColumn(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "rat-bug-null", "open")
	// No WithRationale call — bare ctx with actor only.
	ctx := events.WithActor(context.Background(), events.Actor{Kind: "system", ID: "cli-test"})

	resp, err := work.HandleBugResolve(ctx, pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":            "rat-bug-null",
		"resolution_kind": "wontfix",
	}))
	if err != nil {
		t.Fatalf("HandleBugResolve: %v", err)
	}
	if !resp.OK {
		t.Fatalf("resolve rejected: %+v", resp)
	}
	rationale := readRationaleForLatestEvent(t, pool, "bug", "rat-bug-null")
	if rationale != nil {
		t.Errorf("rationale = %q, want NULL", *rationale)
	}
}

// readRationaleForLatestEvent returns the rationale column for the
// freshest event of an entity, or nil when NULL.
func readRationaleForLatestEvent(t *testing.T, pool *db.Pool, kind, slug string) *string {
	t.Helper()
	var rationale sql.NullString
	if err := pool.DB().QueryRow(
		`SELECT rationale FROM events
		 WHERE entity_kind = ? AND entity_slug = ?
		 ORDER BY ts DESC, event_id DESC LIMIT 1`,
		kind, slug,
	).Scan(&rationale); err != nil {
		t.Fatalf("read rationale: %v", err)
	}
	if !rationale.Valid {
		return nil
	}
	return &rationale.String
}
