package work_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/work"
)

// Bug handler event-emit assertions (T2 of agent-first-substrate).
// Each test seeds a bug, runs the handler, then asserts that the events
// table holds exactly one new row of the expected type, entity, and
// payload shape. The dual-write atomicity invariant (Emit-first + CRUD
// under one tx) is structurally guaranteed by the handlers using
// pool.WithWrite; these tests verify the happy-path emit shape.

func TestBugResolve_EmitsBugResolved(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
	}{
		{
			name: "open_to_fixed_with_sha",
			params: map[string]any{
				"slug":            "ev-fixed",
				"resolution_kind": "fixed",
				"commit_sha":      "abc1234",
				"resolution_note": "patched the edge case",
			},
		},
		{
			name: "open_to_routed",
			params: map[string]any{
				"slug":              "ev-routed",
				"resolution_kind":   "routed",
				"routed_chain_slug": "some-chain",
				"routed_task_slug":  "t1",
			},
		},
		{
			name: "open_to_upstream",
			params: map[string]any{
				"slug":            "ev-upstream",
				"resolution_kind": "upstream",
				"resolution_note": "filed against jsonschema-go #23",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := openTestPool(t)
			slug := c.params["slug"].(string)
			seedBug(t, pool, "mcp-servers", slug, "open")

			resp, err := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, c.params))
			if err != nil {
				t.Fatalf("HandleBugResolve: %v", err)
			}
			if !resp.OK {
				t.Fatalf("resolve rejected: %+v", resp)
			}
			assertSingleEventForEntity(t, pool, "bug", slug, "BugResolved")
		})
	}
}

func TestBugReopen_EmitsBugReopenedWithPreviousResolution(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "ev-reopen", "open")
	// Resolve first so reopen has something to reverse. This also
	// emits BugResolved — total events after reopen should be 2.
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":            "ev-reopen",
		"resolution_kind": "fixed",
		"commit_sha":      "deadbeef",
		"resolution_note": "fixed it",
	}))
	// Reopen.
	resp, err := work.HandleBugReopen(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "ev-reopen"}))
	if err != nil {
		t.Fatalf("HandleBugReopen: %v", err)
	}
	if !resp.OK {
		t.Fatalf("reopen rejected: %+v", resp)
	}
	// Two events for this bug — BugResolved then BugReopened.
	if got := countEventsForEntity(t, pool, "bug", "ev-reopen"); got != 2 {
		t.Errorf("expected 2 events, got %d", got)
	}
	// The most recent event is the reopen.
	typ, payload := lastEventForEntity(t, pool, "bug", "ev-reopen")
	if typ != "BugReopened" {
		t.Errorf("most recent event = %q, want BugReopened", typ)
	}
	// PreviousResolution captures the kind that was reversed.
	var parsed struct {
		PreviousResolution struct {
			Kind      string `json:"kind"`
			CommitSHA string `json:"commit_sha"`
		} `json:"previous_resolution"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("parse payload: %v (raw=%s)", err, payload)
	}
	if parsed.PreviousResolution.Kind != "fixed" || parsed.PreviousResolution.CommitSHA != "deadbeef" {
		t.Errorf("previous_resolution snapshot wrong: %+v", parsed.PreviousResolution)
	}
}

func TestBugStampSHA_EmitsBugStamped(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "ev-stamp", "open")
	// Resolve without sha first (kind=wontfix takes no sha) — leaves room
	// for bug_stamp_sha to be the next emit. Total events = 2 after stamp.
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":            "ev-stamp",
		"resolution_kind": "wontfix",
	}))
	resp, err := work.HandleBugStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "ev-stamp",
		"commit_sha": "cafebabe",
	}))
	if err != nil {
		t.Fatalf("HandleBugStampSHA: %v", err)
	}
	if !resp.OK {
		t.Fatalf("stamp rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "bug", "ev-stamp")
	if typ != "BugStamped" {
		t.Errorf("most recent event = %q, want BugStamped", typ)
	}
	var parsed struct {
		CommitSHA string `json:"commit_sha"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("parse payload: %v (raw=%s)", err, payload)
	}
	if parsed.CommitSHA != "cafebabe" {
		t.Errorf("commit_sha payload mismatch: %q", parsed.CommitSHA)
	}
}

// ---------- shared assertion helpers ----------

// assertSingleEventForEntity checks the events table holds exactly one row
// for the entity tuple AND the row's type matches. Used by happy-path
// emit tests where the handler should emit exactly once.
func assertSingleEventForEntity(t *testing.T, pool *db.Pool, kind, slug, wantType string) {
	t.Helper()
	if got := countEventsForEntity(t, pool, kind, slug); got != 1 {
		t.Errorf("expected 1 event for %s '%s', got %d", kind, slug, got)
	}
	gotType, _ := lastEventForEntity(t, pool, kind, slug)
	if gotType != wantType {
		t.Errorf("event type for %s '%s': got %q, want %q", kind, slug, gotType, wantType)
	}
}

func countEventsForEntity(t *testing.T, pool *db.Pool, kind, slug string) int {
	t.Helper()
	var n int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE entity_kind = ? AND entity_slug = ?`,
		kind, slug,
	).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// lastEventForEntity returns the most recent event's type + payload JSON
// for the entity. Events are ordered by id (UUIDv7 monotonic) so the last
// inserted is the largest id; tests use this to inspect the freshest event.
func lastEventForEntity(t *testing.T, pool *db.Pool, kind, slug string) (string, []byte) {
	t.Helper()
	var typ, payload string
	if err := pool.DB().QueryRow(
		`SELECT type, payload FROM events
		 WHERE entity_kind = ? AND entity_slug = ?
		 ORDER BY ts DESC, event_id DESC LIMIT 1`,
		kind, slug,
	).Scan(&typ, &payload); err != nil {
		t.Fatalf("read latest event: %v", err)
	}
	return typ, []byte(payload)
}
