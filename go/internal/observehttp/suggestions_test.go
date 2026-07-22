package observehttp

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// suggestionIDSeq hands out sequential ids for proj_current_suggestions.
// proj_current_suggestions has no AUTOINCREMENT so tests supply id.
var suggestionIDSeq int64

func nextSuggestionID() int64 {
	suggestionIDSeq++
	return suggestionIDSeq
}

// seedSuggestion direct-inserts into the projection table the
// suggestions handler reads. Post-migration 060 the CRUD suggestions
// table is gone; every test fixture seeds proj_current_suggestions.
func seedSuggestion(t *testing.T, pool *db.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestSuggestionsList_FiltersByStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedSuggestion(t, pool,
		`INSERT INTO proj_current_suggestions (id, project_id, slug, title, priority, status, filed_at, updated_at)
		 VALUES (?, 'test', 'a', 'A', 'high', 'open',    datetime('now'), datetime('now')),
		        (?, 'test', 'b', 'B', 'low',  'adopted', datetime('now'), datetime('now'))`,
		nextSuggestionID(), nextSuggestionID())

	srv := newTestServer(t, pool)
	var got []SuggestionRow
	getJSON(t, srv, "/suggestions?status=open", &got)
	if len(got) != 1 || got[0].Slug != "a" {
		t.Fatalf("status filter wrong: %+v", got)
	}
}

func TestSuggestionsList_FiltersByPriorityAndSurface(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedSuggestion(t, pool,
		`INSERT INTO proj_current_suggestions (id, project_id, slug, title, surface, priority, status, filed_at, updated_at) VALUES
		 (?, 'test', 'a', 'A', 'dashboard', 'high', 'open', datetime('now'), datetime('now')),
		 (?, 'test', 'b', 'B', 'cli',       'high', 'open', datetime('now'), datetime('now')),
		 (?, 'test', 'c', 'C', 'dashboard', 'low',  'open', datetime('now'), datetime('now'))`,
		nextSuggestionID(), nextSuggestionID(), nextSuggestionID())

	srv := newTestServer(t, pool)
	var got []SuggestionRow
	getJSON(t, srv, "/suggestions?priority=high&surface=dashboard", &got)
	if len(got) != 1 || got[0].Slug != "a" {
		t.Fatalf("compound filter wrong: %+v", got)
	}
}

func TestSuggestionsList_OrderedByFiledAtDesc(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedSuggestion(t, pool,
		`INSERT INTO proj_current_suggestions (id, project_id, slug, title, priority, status, filed_at, updated_at) VALUES
		 (?, 'test', 'old', 'O', 'low', 'open', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
		 (?, 'test', 'new', 'N', 'low', 'open', '2026-05-01T00:00:00Z', '2026-05-01T00:00:00Z')`,
		nextSuggestionID(), nextSuggestionID())

	srv := newTestServer(t, pool)
	var got []SuggestionRow
	getJSON(t, srv, "/suggestions", &got)
	if len(got) != 2 || got[0].Slug != "new" {
		t.Fatalf("order wrong: %+v", got)
	}
}

func TestSuggestionsList_RejectsInvalidStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")

	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/suggestions?status=fixed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s; want 400 (bug-side vocab should be rejected on suggestions)", resp.StatusCode, body)
	}
}

func TestSuggestionsList_RejectsInvalidPriority(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")

	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/suggestions?priority=critical")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s; want 400 for unknown priority", resp.StatusCode, body)
	}
}

func TestSuggestionsList_EmitsNullForResolvedAt(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedSuggestion(t, pool,
		`INSERT INTO proj_current_suggestions (id, project_id, slug, title, priority, status, filed_at, updated_at) VALUES
		 (?, 'test', 'open-sug', 'O', 'low', 'open', datetime('now'), datetime('now'))`,
		nextSuggestionID())

	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/suggestions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d rows", len(raw))
	}
	if v, present := raw[0]["resolved_at"]; !present || v != nil {
		t.Errorf("resolved_at should serialize as null, got present=%v val=%v", present, v)
	}
	if v, present := raw[0]["routed_bug_slug"]; !present || v != "" {
		t.Errorf("routed_bug_slug default should be empty string, got present=%v val=%v", present, v)
	}
}
