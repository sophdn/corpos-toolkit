package observehttp

import (
	"encoding/json"
	"net/http"
	"testing"

	"toolkit/internal/testutil"
)

func TestRoadmapList_JoinsLiveStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedChain(t, pool, "test", "chain-a", "open")
	// Post-T6: the retired roadmap_items CRUD table is gone; the
	// /roadmap handler reads from proj_roadmap_view directly. Seed
	// target_status / target_updated_at to mirror what the prod fold
	// would denormalise from the chain row.
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_roadmap_view
		   (project_id, position, ref_kind, ref_slug, chain_slug, note,
		    target_status, target_updated_at)
		 VALUES ('test', 1, 'chain', 'chain-a', NULL, 'first', 'open',
		         (SELECT updated_at FROM proj_chain_status WHERE project_id='test' AND slug='chain-a'))`,
	); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, pool)
	var got []roadmapEntry
	getJSON(t, srv, "/roadmap", &got)
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	e := got[0]
	if e.RefKind != "chain" || e.RefSlug != "chain-a" {
		t.Errorf("entry wrong: %+v", e)
	}
	if e.Status == nil || *e.Status != "open" {
		t.Errorf("status not joined: %+v", e.Status)
	}
	if e.ProjectID != "test" {
		t.Errorf("project_id not surfaced: %q", e.ProjectID)
	}
}

func TestRoadmapList_NullKeysSerializeAsNull(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	// Roadmap entry pointing at a ref that doesn't exist — the prod
	// fold path would denormalise target_status / target_updated_at as
	// NULL, exactly the dashboard's "missing ref" rendering case. Mirror
	// that by inserting a proj_roadmap_view row with the two columns
	// left NULL (column defaults handle this on omit).
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_roadmap_view (project_id, position, ref_kind, ref_slug)
		 VALUES ('test', 1, 'chain', 'ghost')`,
	); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/roadmap")
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
	for _, key := range []string{"chain_slug", "note", "status", "updated_at"} {
		v, present := raw[0][key]
		if !present {
			t.Errorf("%s key missing — should serialize as null", key)
			continue
		}
		if v != nil {
			t.Errorf("%s = %v, want null", key, v)
		}
	}
}

func TestRoadmapDiff_ReturnsUnplacedSinceReassess(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	// Mark reassessment timestamp at a moment before our seeded chains.
	if _, err := pool.DB().Exec(
		`INSERT OR REPLACE INTO roadmap_meta (key, value)
		 VALUES ('last_reassessed_at', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	// Post-T6: the retired chains CRUD table is gone; the /roadmap/diff
	// handler reads from proj_chain_status. Backdate created_at to
	// straddle the diff window — one chain inside, one outside.
	testutil.SeedChain(t, pool, "test", "new-chain", "open", testutil.SeedChainOpts{
		CreatedAt: "2026-03-01T00:00:00Z",
	})
	testutil.SeedChain(t, pool, "test", "old-chain", "open", testutil.SeedChainOpts{
		CreatedAt: "2025-12-01T00:00:00Z",
	})

	srv := newTestServer(t, pool)
	var got roadmapDiffResponse
	getJSON(t, srv, "/roadmap/diff", &got)
	if len(got.Chains) != 1 || got.Chains[0].Slug != "new-chain" {
		t.Fatalf("chains diff wrong: %+v", got.Chains)
	}
	if len(got.Tasks) != 0 {
		t.Errorf("tasks diff non-empty: %+v", got.Tasks)
	}
}

func TestRoadmapDiff_ExcludesAlreadyPlaced(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	if _, err := pool.DB().Exec(
		`INSERT OR REPLACE INTO roadmap_meta (key, value)
		 VALUES ('last_reassessed_at', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	testutil.SeedChain(t, pool, "test", "already-placed", "open", testutil.SeedChainOpts{
		CreatedAt: "2026-03-01T00:00:00Z",
	})
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_roadmap_view (project_id, position, ref_kind, ref_slug)
		 VALUES ('test', 1, 'chain', 'already-placed')`,
	); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, pool)
	var got roadmapDiffResponse
	getJSON(t, srv, "/roadmap/diff", &got)
	if len(got.Chains) != 0 {
		t.Errorf("already-placed chain leaked into diff: %+v", got.Chains)
	}
}
