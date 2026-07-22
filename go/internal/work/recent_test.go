package work_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/work"
)

// recent_activity reads the events ledger directly, so these tests seed
// the events table with raw INSERTs (full control over ts / type) rather
// than going through emit — the read path is what's under test.

// seedEvent inserts one row into the append-only events table. id must be
// unique; ts is an RFC-3339 string the handler compares lexically.
func seedEvent(t *testing.T, pool *db.Pool, id, project, typ, kind, slug, ts, payload, rationale string) {
	t.Helper()
	var proj any
	if project != "" {
		proj = project
	}
	var rat any
	if rationale != "" {
		rat = rationale
	}
	if payload == "" {
		payload = "{}"
	}
	if _, err := pool.DB().Exec(
		`INSERT INTO events (event_id, ts, actor_kind, actor_id, type,
			entity_kind, entity_slug, entity_project_id, payload, rationale,
			related_entities, span_id, schema_version)
		 VALUES (?, ?, 'agent', 'claude', ?, ?, ?, ?, ?, ?, '[]', 'span-test', 1)`,
		id, ts, typ, kind, slug, proj, payload, rat,
	); err != nil {
		t.Fatalf("seed event %q: %v", id, err)
	}
}

func callRecent(t *testing.T, pool *db.Pool, project string, params map[string]any) work.RecentActivityResult {
	t.Helper()
	res, err := work.HandleRecentActivity(context.Background(), pool, project, mustJSON(t, params))
	if err != nil {
		t.Fatalf("HandleRecentActivity: %v", err)
	}
	return res
}

const allTime = "2000-01-01T00:00:00.000Z"

func TestRecentActivity_PerProjectAndCrossProject(t *testing.T) {
	pool := openTestPool(t)
	if _, err := pool.DB().Exec(`INSERT INTO projects (id, name) VALUES ('other-proj','other-proj')`); err != nil {
		t.Fatalf("seed second project: %v", err)
	}
	seedEvent(t, pool, "e1", "mcp-servers", "TaskCompleted", "task", "task-a", "2026-06-10T10:00:00.000Z", "", "")
	seedEvent(t, pool, "e2", "other-proj", "ChainClosed", "chain", "chain-b", "2026-06-10T11:00:00.000Z", "", "")

	// Per-project scope returns only that project's events.
	perProject := callRecent(t, pool, "mcp-servers", map[string]any{"since": allTime})
	if perProject.Scope != "mcp-servers" {
		t.Errorf("scope: got %q, want mcp-servers", perProject.Scope)
	}
	if len(perProject.Timeline) != 1 || perProject.Timeline[0].EntitySlug != "task-a" {
		t.Fatalf("per-project timeline: want [task-a], got %+v", perProject.Timeline)
	}

	// Cross-project (no project) returns both, newest first.
	cross := callRecent(t, pool, "", map[string]any{"since": allTime})
	if cross.Scope != "cross-project" {
		t.Errorf("scope: got %q, want cross-project", cross.Scope)
	}
	if len(cross.Timeline) != 2 {
		t.Fatalf("cross-project timeline: want 2, got %d (%+v)", len(cross.Timeline), cross.Timeline)
	}
	if cross.Timeline[0].EntitySlug != "chain-b" {
		t.Errorf("newest-first ordering broken: want chain-b first, got %q", cross.Timeline[0].EntitySlug)
	}
}

func TestRecentActivity_SinceAndLimit(t *testing.T) {
	pool := openTestPool(t)
	for i := 0; i < 5; i++ {
		ts := fmt.Sprintf("2026-06-1%dT09:00:00.000Z", i) // 10th..14th
		seedEvent(t, pool, fmt.Sprintf("ev-%d", i), "mcp-servers", "TaskCreated", "task", fmt.Sprintf("t-%d", i), ts, "", "")
	}

	// since excludes everything strictly before the bound.
	sinceFiltered := callRecent(t, pool, "mcp-servers", map[string]any{"since": "2026-06-13T00:00:00.000Z"})
	if len(sinceFiltered.Timeline) != 2 {
		t.Fatalf("since filter: want 2 (13th,14th), got %d (%+v)", len(sinceFiltered.Timeline), sinceFiltered.Timeline)
	}

	// limit caps the timeline.
	limited := callRecent(t, pool, "mcp-servers", map[string]any{"since": allTime, "limit": 2})
	if len(limited.Timeline) != 2 {
		t.Fatalf("limit: want 2, got %d", len(limited.Timeline))
	}
	if limited.Timeline[0].EntitySlug != "t-4" {
		t.Errorf("limit should keep newest: want t-4 first, got %q", limited.Timeline[0].EntitySlug)
	}
}

func TestRecentActivity_InFlightAndClosures(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "live-chain")
	seedTask(t, pool, "live-chain", "active-task", "active")
	seedTask(t, pool, "live-chain", "pending-task", "pending")

	seedEvent(t, pool, "c1", "mcp-servers", "ChainClosed", "chain", "done-chain", "2026-06-10T10:00:00.000Z", "", "")
	seedEvent(t, pool, "c2", "mcp-servers", "TaskCompleted", "task", "done-task", "2026-06-10T11:00:00.000Z", "", "")
	seedEvent(t, pool, "c3", "mcp-servers", "BugReported", "bug", "some-bug", "2026-06-10T12:00:00.000Z", "", "")

	res := callRecent(t, pool, "mcp-servers", map[string]any{"since": allTime})

	// in_flight = active tasks only.
	if len(res.InFlight) != 1 || res.InFlight[0].Slug != "active-task" {
		t.Fatalf("in_flight: want [active-task], got %+v", res.InFlight)
	}
	if res.InFlight[0].ChainSlug != "live-chain" {
		t.Errorf("in_flight chain: got %q, want live-chain", res.InFlight[0].ChainSlug)
	}

	// recent_closures = ChainClosed / TaskCompleted only (not BugReported).
	if len(res.RecentClosures) != 2 {
		t.Fatalf("recent_closures: want 2, got %d (%+v)", len(res.RecentClosures), res.RecentClosures)
	}
	for _, c := range res.RecentClosures {
		if c.Type != "ChainClosed" && c.Type != "TaskCompleted" {
			t.Errorf("unexpected closure type %q", c.Type)
		}
	}
}

func TestRecentActivity_SummaryRendering(t *testing.T) {
	pool := openTestPool(t)
	// CommitLanded carries a subject + sha worth surfacing.
	commitPayload := `{"subject":"fix the thing","commit_sha":"abcdef1234567890"}`
	seedEvent(t, pool, "s1", "mcp-servers", "CommitLanded", "commit", "abcdef12", "2026-06-10T10:00:00.000Z", commitPayload, "")
	// An unfamiliar/future event type still renders via PascalCase humanizing.
	seedEvent(t, pool, "s2", "mcp-servers", "SomeFutureThingHappened", "widget", "wob", "2026-06-10T11:00:00.000Z", "", "because reasons")

	res := callRecent(t, pool, "mcp-servers", map[string]any{"since": allTime})
	byType := map[string]work.ActivityEvent{}
	for _, e := range res.Timeline {
		byType[e.Type] = e
	}

	commit := byType["CommitLanded"]
	if commit.Summary != "commit abcdef12 landed: fix the thing" {
		t.Errorf("CommitLanded summary: got %q", commit.Summary)
	}

	future := byType["SomeFutureThingHappened"]
	want := "some future thing happened widget 'wob' — because reasons"
	if future.Summary != want {
		t.Errorf("unknown-type summary: got %q, want %q", future.Summary, want)
	}
}

func TestRecentActivity_EmptyMarshalsSections(t *testing.T) {
	pool := openTestPool(t)
	res := callRecent(t, pool, "mcp-servers", map[string]any{"since": allTime})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// All three sections must serialize as [] (not null) so callers can
	// distinguish "nothing recent" from a tool error.
	for _, frag := range []string{`"in_flight":[]`, `"recent_closures":[]`, `"timeline":[]`} {
		if !contains(string(b), frag) {
			t.Errorf("expected %s in %s", frag, b)
		}
	}
}
