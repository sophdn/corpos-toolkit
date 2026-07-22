package observehttp

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/projections"
	"toolkit/internal/testutil"
)

// seedProject is a thin alias kept for call-site brevity; consolidated
// implementation lives in [testutil.SeedProject].
func seedProject(t *testing.T, pool *db.Pool, id string) {
	t.Helper()
	testutil.SeedProject(t, pool, id)
}

// seedChain wraps [testutil.SeedChain] returning the assigned chain id —
// observehttp's chains-list tests need the id to seed downstream tasks
// scoped to it. The wrapper survives because the typed-return adds
// meaningful value beyond pure forwarding.
func seedChain(t *testing.T, pool *db.Pool, project, slug, status string) int64 {
	t.Helper()
	return testutil.SeedChain(t, pool, project, slug, status, testutil.SeedChainOpts{})
}

// seedTask wraps [testutil.SeedTask] AND refreshes the parent chain's
// counter columns so /chains list-counts assertions hold without a
// separate recompute step. The counter refresh is the package-specific
// value-add; without it, observehttp's chain-row assertions see stale
// zeros.
func seedTask(t *testing.T, pool *db.Pool, chainID int64, slug, status string) {
	t.Helper()
	testutil.SeedTask(t, pool, chainID, slug, status, testutil.SeedTaskOpts{})
	testutil.RefreshChainCounters(t, pool, chainID)
}

func newTestServer(t *testing.T, pool *db.Pool) *httptest.Server {
	t.Helper()
	// T6 (agent-substrate-crud-retirement): the retired CRUD tables are
	// gone. Write-side projection rows are direct-INSERTed by test
	// fixtures, so no CRUD→projection refresh is needed for them.
	//
	// Read-side telemetry projections (query_volume_by_source /
	// retrieval_success_per_query / training_data_for_reranker) still
	// snapshot from non-retired sources (grounding_events,
	// query_interactions, query_resolutions). Tests that seed those
	// tables and then exercise a handler reading the projection need a
	// rebuild step; otherwise the projection is empty and the assertion
	// trips.
	refreshReadSideProjections(t, pool)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool}))
	t.Cleanup(srv.Close)
	return srv
}

// refreshReadSideProjections rebuilds the telemetry-side projections
// from grounding_events / query_interactions / query_resolutions. The
// write-side projections aren't rebuilt here — those rows come from
// direct INSERTs in test fixtures and a rebuild would clobber them.
func refreshReadSideProjections(t *testing.T, pool *db.Pool) {
	t.Helper()
	readSideRebuilds := []string{
		"query_volume_by_source",
		"retrieval_success_per_query",
		"training_data_for_reranker",
		"inference_tool_model_performance",
		"inference_call_success",
	}
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, err := projections.RebuildAll(context.Background(), tx, readSideRebuilds)
		return err
	})
	if err != nil {
		t.Fatalf("refresh read-side projections: %v", err)
	}
}

func getJSON(t *testing.T, srv *httptest.Server, path string, out any) int {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("decode %s body=%q: %v", path, body, err)
		}
	}
	return resp.StatusCode
}

func TestChainsList_ExcludesClosedByDefault(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedChain(t, pool, "test", "open-one", "open")
	seedChain(t, pool, "test", "closed-one", "closed")

	srv := newTestServer(t, pool)
	var got []ChainRow
	if code := getJSON(t, srv, "/chains?project=test", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got) != 1 || got[0].Slug != "open-one" {
		t.Fatalf("got = %+v, want [open-one]", got)
	}
}

func TestChainsList_IncludeClosed(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedChain(t, pool, "test", "a", "open")
	seedChain(t, pool, "test", "b", "closed")

	srv := newTestServer(t, pool)
	var got []ChainRow
	getJSON(t, srv, "/chains?project=test&include_closed=true", &got)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
}

func TestChainsList_TaskCounts(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	cid := seedChain(t, pool, "test", "c", "open")
	seedTask(t, pool, cid, "t1", "pending")
	seedTask(t, pool, cid, "t2", "active")
	seedTask(t, pool, cid, "t3", "closed")

	srv := newTestServer(t, pool)
	var got []ChainRow
	getJSON(t, srv, "/chains?project=test", &got)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	r := got[0]
	if r.TotalTasks != 3 || r.Pending != 1 || r.Active != 1 || r.Closed != 1 {
		t.Errorf("counts = total=%d pending=%d active=%d closed=%d, want 3/1/1/1",
			r.TotalTasks, r.Pending, r.Active, r.Closed)
	}
}

func TestChainsList_ProjectFilter(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "alpha")
	seedProject(t, pool, "beta")
	seedChain(t, pool, "alpha", "x", "open")
	seedChain(t, pool, "beta", "y", "open")

	srv := newTestServer(t, pool)
	var got []ChainRow
	getJSON(t, srv, "/chains?project=alpha", &got)
	if len(got) != 1 || got[0].ProjectID != "alpha" {
		t.Fatalf("project filter failed: got %+v", got)
	}
}

func TestChainsDetail_Returns404WhenMissing(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	srv := newTestServer(t, pool)
	code := getJSON(t, srv, "/chains/nope?project=test", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestChainsDetail_ReturnsProseFields(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	// design_decisions retired in migration 065 (Phase 4 F2); the
	// observe-http detail endpoint no longer surfaces it. The
	// EventTimeline reads it from ChainCreated event payloads.
	testutil.SeedChain(t, pool, "test", "detail-me", "open", testutil.SeedChainOpts{
		Output:              "OUTPUT",
		CompletionCondition: "DONE-WHEN",
	})

	srv := newTestServer(t, pool)
	var got ChainDetail
	if code := getJSON(t, srv, "/chains/detail-me?project=test", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Output != "OUTPUT" || got.CompletionCondition != "DONE-WHEN" {
		t.Errorf("prose fields wrong: %+v", got)
	}
}
