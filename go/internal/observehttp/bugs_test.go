package observehttp

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"toolkit/internal/testutil"
)

func TestBugsList_FiltersByStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	// 'resolved' was the pre-vocab status name; canonical is 'fixed' (chain
	// rust-retirement-and-db-hardening T2 / migration 066 nailed the vocab).
	testutil.SeedBug(t, pool, "test", "a", "open", testutil.SeedBugOpts{Title: "A", Severity: "high"})
	testutil.SeedBug(t, pool, "test", "b", "fixed", testutil.SeedBugOpts{Title: "B", Severity: "low"})

	srv := newTestServer(t, pool)
	var got []BugRow
	getJSON(t, srv, "/bugs?status=open", &got)
	if len(got) != 1 || got[0].Slug != "a" {
		t.Fatalf("status filter wrong: %+v", got)
	}
}

func TestBugsList_FiltersBySeverityAndSurface(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	testutil.SeedBug(t, pool, "test", "a", "open", testutil.SeedBugOpts{Title: "A", Surface: "dashboard", Severity: "high"})
	testutil.SeedBug(t, pool, "test", "b", "open", testutil.SeedBugOpts{Title: "B", Surface: "cli", Severity: "high"})
	testutil.SeedBug(t, pool, "test", "c", "open", testutil.SeedBugOpts{Title: "C", Surface: "dashboard", Severity: "low"})

	srv := newTestServer(t, pool)
	var got []BugRow
	getJSON(t, srv, "/bugs?severity=high&surface=dashboard", &got)
	if len(got) != 1 || got[0].Slug != "a" {
		t.Fatalf("compound filter wrong: %+v", got)
	}
}

func TestBugsList_OrderedByFiledAtDesc(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	testutil.SeedBug(t, pool, "test", "old", "open", testutil.SeedBugOpts{Title: "O", Severity: "low", FiledAt: "2026-01-01T00:00:00Z"})
	testutil.SeedBug(t, pool, "test", "new", "open", testutil.SeedBugOpts{Title: "N", Severity: "low", FiledAt: "2026-05-01T00:00:00Z"})

	srv := newTestServer(t, pool)
	var got []BugRow
	getJSON(t, srv, "/bugs", &got)
	if len(got) != 2 || got[0].Slug != "new" {
		t.Fatalf("order wrong: %+v", got)
	}
}

func TestBugsList_LimitClampedTo1000(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	testutil.SeedBug(t, pool, "test", "a", "open", testutil.SeedBugOpts{Title: "A", Severity: "low"})

	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/bugs?limit=99999")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
}

// TestBugsList_SurfacesRoutedSuggestionSlug pins the additive wire-shape
// change from agent-suggestion-box T8: GET /bugs now includes
// routed_suggestion_slug on every row (empty string default, not null).
func TestBugsList_SurfacesRoutedSuggestionSlug(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	testutil.SeedBug(t, pool, "test", "b1", "open", testutil.SeedBugOpts{
		Title: "B1", Severity: "low", RoutedSuggestionSlug: "sug-followup",
	})
	testutil.SeedBug(t, pool, "test", "b2", "open", testutil.SeedBugOpts{
		Title: "B2", Severity: "low",
	})
	srv := newTestServer(t, pool)
	var got []BugRow
	getJSON(t, srv, "/bugs", &got)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	bySlug := map[string]string{}
	for _, r := range got {
		bySlug[r.Slug] = r.RoutedSuggestionSlug
	}
	if bySlug["b1"] != "sug-followup" || bySlug["b2"] != "" {
		t.Errorf("routed_suggestion_slug: %+v", bySlug)
	}
}

func TestBugsList_EmitsNullForResolvedAt(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	testutil.SeedBug(t, pool, "test", "open-bug", "open", testutil.SeedBugOpts{Title: "O", Severity: "low"})

	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/bugs")
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
}
