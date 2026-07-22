package observehttp

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// seedManyBugs inserts n bugs alternating between two statuses (open / fixed)
// and three severities (high / medium / low) so grouped-count assertions
// can verify both totals and per-bucket counts deterministically.
func seedManyBugs(t *testing.T, pool *db.Pool, project string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		status := "open"
		if i%2 == 1 {
			status = "fixed"
		}
		sev := []string{"high", "medium", "low"}[i%3]
		slug := fmt.Sprintf("bug-%d", i)
		testutil.SeedBug(t, pool, project, slug, status, testutil.SeedBugOpts{
			Title: "T", Severity: sev,
		})
	}
}

// TestBugsCounts_TotalSurvivesAboveListCap is the bug-1573 regression:
// the /bugs list endpoint caps at 1000 rows, so a frontend that counts
// rows.length silently undercaps for any corpus >1000. /bugs/counts
// runs an aggregate query and must return the true total regardless.
func TestBugsCounts_TotalSurvivesAboveListCap(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	const n = 1250 // > /bugs?limit max of 1000
	seedManyBugs(t, pool, "test", n)

	srv := newTestServer(t, pool)
	var got countResponse
	getJSON(t, srv, "/bugs/counts", &got)
	if got.Total != int64(n) {
		t.Fatalf("Total: got %d, want %d (list-cap leakage)", got.Total, n)
	}
	if got.Buckets != nil {
		t.Errorf("Buckets should be nil when group_by absent, got %+v", got.Buckets)
	}
}

func TestBugsCounts_GroupByStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedManyBugs(t, pool, "test", 10) // 5 open, 5 fixed

	srv := newTestServer(t, pool)
	var got countResponse
	getJSON(t, srv, "/bugs/counts?group_by=status", &got)
	if got.GroupBy != "status" {
		t.Errorf("GroupBy: got %q, want %q", got.GroupBy, "status")
	}
	if got.Buckets["open"] != 5 || got.Buckets["fixed"] != 5 {
		t.Errorf("Buckets: got %+v, want open=5 fixed=5", got.Buckets)
	}
	if got.Total != 10 {
		t.Errorf("Total: got %d, want 10", got.Total)
	}
}

func TestBugsCounts_HonorsFilters(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedManyBugs(t, pool, "test", 12) // 6 open, 6 fixed; 4 each severity

	srv := newTestServer(t, pool)
	var got countResponse
	getJSON(t, srv, "/bugs/counts?status=open", &got)
	if got.Total != 6 {
		t.Errorf("status=open Total: got %d, want 6", got.Total)
	}
}

func TestBugsCounts_RejectsInvalidGroupBy(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/bugs/counts?group_by=problem_statement")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestSuggestionsCounts_TotalAndGroupBy(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	// Seed 6 suggestions: 3 open, 3 adopted, mixed priority.
	for i, st := range []string{"open", "open", "open", "adopted", "adopted", "adopted"} {
		pri := []string{"high", "medium", "low"}[i%3]
		slug := fmt.Sprintf("sug-%d", i)
		if _, err := pool.DB().Exec(
			`INSERT INTO proj_current_suggestions
			    (id, project_id, slug, title, surface, priority, status,
			     routed_chain_slug, routed_task_slug, routed_bug_slug,
			     filed_at, updated_at)
			 VALUES (?, 'test', ?, 'T', '', ?, ?, '', '', '', datetime('now'), datetime('now'))`,
			i+1, slug, pri, st); err != nil {
			t.Fatalf("seed suggestion %d: %v", i, err)
		}
	}

	srv := newTestServer(t, pool)
	var ungrouped countResponse
	getJSON(t, srv, "/suggestions/counts", &ungrouped)
	if ungrouped.Total != 6 {
		t.Errorf("ungrouped Total: got %d, want 6", ungrouped.Total)
	}

	var grouped countResponse
	getJSON(t, srv, "/suggestions/counts?group_by=status", &grouped)
	if grouped.Buckets["open"] != 3 || grouped.Buckets["adopted"] != 3 {
		t.Errorf("status buckets: got %+v, want open=3 adopted=3", grouped.Buckets)
	}
}

func TestSuggestionsCounts_RejectsInvalidStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/suggestions/counts?status=mystery")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid status: got %d, want 400", resp.StatusCode)
	}
}

func TestTasksCounts_GroupByStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	c := seedChain(t, pool, "test", "c1", "open")
	seedTask(t, pool, c, "t1", "pending")
	seedTask(t, pool, c, "t2", "pending")
	seedTask(t, pool, c, "t3", "closed")

	srv := newTestServer(t, pool)
	var got countResponse
	getJSON(t, srv, "/tasks/counts?group_by=status", &got)
	if got.Buckets["pending"] != 2 || got.Buckets["closed"] != 1 {
		t.Errorf("status buckets: got %+v, want pending=2 closed=1", got.Buckets)
	}
	if got.Total != 3 {
		t.Errorf("Total: got %d, want 3", got.Total)
	}
}

func TestTasksCounts_FiltersByChainSlug(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	c1 := seedChain(t, pool, "test", "c1", "open")
	c2 := seedChain(t, pool, "test", "c2", "open")
	seedTask(t, pool, c1, "t1", "pending")
	seedTask(t, pool, c1, "t2", "pending")
	seedTask(t, pool, c2, "t3", "pending")

	srv := newTestServer(t, pool)
	var got countResponse
	getJSON(t, srv, "/tasks/counts?chain_slug=c1", &got)
	if got.Total != 2 {
		t.Errorf("chain_slug=c1 Total: got %d, want 2", got.Total)
	}
}

func TestChainsCounts_GroupByStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedChain(t, pool, "test", "c-open-1", "open")
	seedChain(t, pool, "test", "c-open-2", "open")
	seedChain(t, pool, "test", "c-closed", "closed")
	// Chain status vocab is just {open, closed} (migration 066 CHECK).
	// The earlier 'cancelled' bucket was test-fiction; the dashboard
	// will never see it from a real corpus.

	srv := newTestServer(t, pool)
	var got countResponse
	getJSON(t, srv, "/chains/counts?group_by=status", &got)
	if got.Buckets["open"] != 2 || got.Buckets["closed"] != 1 {
		t.Errorf("status buckets: got %+v, want open=2 closed=1", got.Buckets)
	}
	if got.Total != 3 {
		t.Errorf("Total: got %d, want 3", got.Total)
	}
}

func TestChainsCounts_FilterByStatusUngrouped(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedChain(t, pool, "test", "c-open", "open")
	seedChain(t, pool, "test", "c-closed", "closed")

	srv := newTestServer(t, pool)
	var got countResponse
	getJSON(t, srv, "/chains/counts?status=open", &got)
	if got.Total != 1 {
		t.Errorf("status=open Total: got %d, want 1", got.Total)
	}
}

func TestCountsResponse_GroupByValueShape(t *testing.T) {
	// Belt-and-suspenders: every grouped response carries GroupBy
	// (string) and Buckets (map) keys; every ungrouped response omits
	// them (omitempty). The frontend module switches on this shape, so
	// regressing the shape would silently break the consumer.
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedManyBugs(t, pool, "test", 3)

	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/bugs/counts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"total":3`) {
		t.Errorf("ungrouped body missing total:3 — got %s", body)
	}
	if strings.Contains(body, `"group_by"`) || strings.Contains(body, `"buckets"`) {
		t.Errorf("ungrouped body should omit group_by/buckets — got %s", body)
	}
}
