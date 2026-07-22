package observehttp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

func seedKnowledgePointer(t *testing.T, pool *db.Pool, project, sourceType, ref, question string, usage int64) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO knowledge_pointers (project_id, source_type, source_ref, question, invoke_when, usage_count, status)
		 VALUES (?, ?, ?, ?, 'when', ?, 'active')`,
		project, sourceType, ref, question, usage,
	); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeIndexCard_EmptyDBAllZeroes(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	var got knowledgeIndexCard
	if code := getJSON(t, srv, "/knowledge/index-card", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got.TotalActivePointers != 0 || got.GroundingSummary.UsedPct != 0 {
		t.Errorf("expected zeros, got %+v", got)
	}
}

func TestKnowledgeIndexCard_AggregatesPointers(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedKnowledgePointer(t, pool, "test", "vault", "decisions/a", "Q1", 5)
	seedKnowledgePointer(t, pool, "test", "vault", "decisions/b", "Q2", 3)
	seedKnowledgePointer(t, pool, "test", "library", "lib/c", "Q3", 10)
	// Mark one as retired — should not count in active totals.
	if _, err := pool.DB().Exec(
		`UPDATE knowledge_pointers SET status='retired' WHERE source_ref='decisions/b'`,
	); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, pool)
	var got knowledgeIndexCard
	getJSON(t, srv, "/knowledge/index-card", &got)

	if got.TotalActivePointers != 2 {
		t.Errorf("total = %d, want 2", got.TotalActivePointers)
	}
	if len(got.BySourceType) != 2 {
		t.Fatalf("source_type breakdown wrong: %+v", got.BySourceType)
	}
	// Top queried sorted by usage_count DESC; vault decisions/a is 5, library/c is 10.
	if got.TopQueried[0].SourceRef != "lib/c" || got.TopQueried[0].UsageCount != 10 {
		t.Errorf("top_queried order wrong: %+v", got.TopQueried)
	}
}

func TestKnowledgeIndexCard_GroundingSummary(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// 4 knowledge_search events; 2 used; 1 zero-result-with-output.
	if _, err := pool.DB().Exec(
		`INSERT INTO grounding_events
		 (project_id, session_id, call_id, action, results_count, next_turn_has_output, used) VALUES
		 ('test', 'sess-1', 'c1', 'knowledge_search', 3, 1, 1),
		 ('test', 'sess-1', 'c2', 'knowledge_search', 3, 1, 1),
		 ('test', 'sess-2', 'c3', 'knowledge_search', 0, 1, 0),
		 ('test', 'sess-2', 'c4', 'knowledge_search', 5, 0, 0),
		 ('test', 'sess-3', 'c5', 'vault_search',     1, 1, NULL)`,
	); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, pool)
	var got knowledgeIndexCard
	getJSON(t, srv, "/knowledge/index-card", &got)

	g := got.GroundingSummary
	if g.TotalSearchCalls != 4 {
		t.Errorf("total = %d, want 4", g.TotalSearchCalls)
	}
	if g.UsedCount != 2 {
		t.Errorf("used = %d, want 2", g.UsedCount)
	}
	if g.UsedPct != 50.0 {
		t.Errorf("used_pct = %v, want 50", g.UsedPct)
	}
	if g.ZeroResultGapCount != 1 {
		t.Errorf("zero_result_gap = %d, want 1", g.ZeroResultGapCount)
	}
	// sess-3 has next_turn_has_output=1 with no knowledge_search calls.
	if g.PureMemorySessions != 1 {
		t.Errorf("pure_memory = %d, want 1", g.PureMemorySessions)
	}
}

func TestKnowledgeIndexCard_ProjectFilterScopesPointers(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedKnowledgePointer(t, pool, "alpha", "vault", "a/1", "Q", 1)
	seedKnowledgePointer(t, pool, "beta", "vault", "b/1", "Q", 1)

	srv := newTestServer(t, pool)
	var got knowledgeIndexCard
	getJSON(t, srv, "/knowledge/index-card?project=alpha", &got)
	if got.TotalActivePointers != 1 {
		t.Errorf("project-scoped total = %d, want 1", got.TotalActivePointers)
	}
}

// TestKnowledgeIndexCard_ResponseShapeContract is the locked contract
// test added by chain curation-go-migration T2. Asserts the top-level
// keys + value types are stable, separate from any value-based test.
// Locks the API shape so a future refactor can't silently drop or
// rename a field without an explicit test update.
//
// Sibling fixture (used by the e2e verify task T14):
// apps/dashboard/tests/e2e/__fixtures__/knowledge-index-card-baseline.json
// captures the live response from 2026-05-17 (state after the manual
// bulk-reject of 502 noise candidates).
func TestKnowledgeIndexCard_ResponseShapeContract(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)

	var raw map[string]json.RawMessage
	if code := getJSON(t, srv, "/knowledge/index-card", &raw); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	// Top-level keys that must always be present, with the expected
	// JSON kind (object/array/number). Anything dropped here breaks
	// the dashboard's Knowledge index page (apps/dashboard/src/pages/
	// Knowledge/index.tsx + lib/knowledgeCard.ts).
	requiredKeys := map[string]string{
		"total_active_pointers":       "number",
		"by_source_type":              "array",
		"pending_curation_candidates": "number",
		"top_queried":                 "array",
		"recently_added":              "array",
		"grounding_summary":           "object",
	}
	for key, wantKind := range requiredKeys {
		val, ok := raw[key]
		if !ok {
			t.Errorf("response missing required key %q", key)
			continue
		}
		gotKind := jsonValueKind(val)
		if gotKind != wantKind {
			t.Errorf("key %q kind: want %s, got %s (value=%s)", key, wantKind, gotKind, val)
		}
	}

	// grounding_summary inner shape.
	var gs map[string]json.RawMessage
	if err := json.Unmarshal(raw["grounding_summary"], &gs); err != nil {
		t.Fatalf("grounding_summary unmarshal: %v", err)
	}
	for _, key := range []string{
		"total_search_calls", "used_count", "used_pct",
		"zero_result_gap_count", "pure_memory_sessions",
	} {
		if _, ok := gs[key]; !ok {
			t.Errorf("grounding_summary missing required key %q", key)
		}
	}
}

// jsonValueKind classifies a json.RawMessage into the JSON kinds the
// shape-contract test cares about. Coarse-grained (no string/bool
// today because the response doesn't surface either at top level).
func jsonValueKind(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if len(s) == 0 {
		return "empty"
	}
	switch s[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	default:
		// Numbers (positive, negative, decimal).
		return "number"
	}
}
