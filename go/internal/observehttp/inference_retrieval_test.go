package observehttp

import (
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// seedGroundingForRetrieval inserts one grounding_events row in the
// chain-T3c retrieval-health test fixture shape: a single search call
// for the given action with a fresh span_id. Returns the row's id so
// the caller can attach query_interactions.
func seedGroundingForRetrieval(t *testing.T, pool *db.Pool, action, spanID string, at time.Time) int64 {
	t.Helper()
	res, err := pool.DB().Exec(
		`INSERT INTO grounding_events
			(project_id, session_id, call_id, action, results_count, source_refs, span_id, created_at)
		 VALUES ('p', ?, ?, ?, 5, '["src"]', ?, ?)`,
		spanID, spanID, action, spanID, at.UTC().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		t.Fatalf("seed grounding: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedInteraction(t *testing.T, pool *db.Pool, gID int64, spanID, sourceRef, clickKind string, weight float64) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO query_interactions
			(grounding_event_id, source_ref, click_kind, click_weight, span_id, session_id, detected_at)
		 VALUES (?, ?, ?, ?, ?, ?, datetime('now'))`,
		gID, sourceRef, clickKind, weight, spanID, spanID,
	); err != nil {
		t.Fatalf("seed interaction (%s): %v", clickKind, err)
	}
}

func TestInferenceRetrievalHealth_EmptyWhenSubstrateBare(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	var got []RetrievalHealthAction
	getJSON(t, srv, "/inference/retrieval-health", &got)
	if len(got) != 0 {
		t.Errorf("expected empty array, got %d actions", len(got))
	}
}

func TestInferenceRetrievalHealth_VaultSearchTieredKinds(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// Seed 20 vault_search grounding rows, then 12 query_interactions:
	// 8 followed, 3 mentioned, 1 cited.
	for i := 0; i < 20; i++ {
		spanID := "span-vs-" + string(rune('a'+i))
		gID := seedGroundingForRetrieval(t, pool, "vault_search", spanID, now.Add(-time.Duration(i)*time.Minute))
		if i < 8 {
			seedInteraction(t, pool, gID, spanID, "ref-"+spanID, "followed", 1.0)
		}
		if i >= 8 && i < 11 {
			seedInteraction(t, pool, gID, spanID, "ref-"+spanID, "mentioned", 0.4)
		}
		if i == 11 {
			seedInteraction(t, pool, gID, spanID, "ref-"+spanID, "cited", 0.8)
		}
	}
	srv := newTestServer(t, pool)
	var got []RetrievalHealthAction
	getJSON(t, srv, "/inference/retrieval-health", &got)

	if len(got) != 1 {
		t.Fatalf("expected 1 action, got %d", len(got))
	}
	vs := got[0]
	if vs.Action != "vault_search" {
		t.Errorf("action: %s", vs.Action)
	}
	if vs.GroundingCount != 20 {
		t.Errorf("grounding_count = %d, want 20", vs.GroundingCount)
	}
	if vs.InteractionCount != 12 {
		t.Errorf("interaction_count = %d, want 12", vs.InteractionCount)
	}
	if vs.WarmingUp {
		t.Error("warming_up should be false at 20 grounding rows (≥ floor 10)")
	}

	// by_kind order: followed → cited → mentioned (resolved-from absent).
	if len(vs.ByKind) != 3 {
		t.Fatalf("expected 3 kinds, got %d", len(vs.ByKind))
	}
	wantOrder := []string{"followed", "cited", "mentioned"}
	for i, want := range wantOrder {
		if vs.ByKind[i].ClickKind != want {
			t.Errorf("by_kind[%d] = %q, want %q", i, vs.ByKind[i].ClickKind, want)
		}
	}

	followed := vs.ByKind[0]
	if followed.Count != 8 || followed.Rate != 0.4 || followed.Weight != 1.0 {
		t.Errorf("followed: count=%d rate=%v weight=%v", followed.Count, followed.Rate, followed.Weight)
	}
	cited := vs.ByKind[1]
	if cited.Count != 1 || cited.Weight != 0.8 {
		t.Errorf("cited: count=%d weight=%v", cited.Count, cited.Weight)
	}
	mentioned := vs.ByKind[2]
	if mentioned.Count != 3 || mentioned.Weight != 0.4 {
		t.Errorf("mentioned: count=%d weight=%v", mentioned.Count, mentioned.Weight)
	}

	// Weighted score = (8*1.0 + 1*0.8 + 3*0.4) / 20 = (8 + 0.8 + 1.2) / 20 = 10/20 = 0.5
	wantScore := (8*1.0 + 1*0.8 + 3*0.4) / 20.0
	if abs(vs.WeightedScore-wantScore) > 1e-9 {
		t.Errorf("weighted_score = %v, want %v", vs.WeightedScore, wantScore)
	}
}

func TestInferenceRetrievalHealth_WarmupBelowFloor(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// 5 grounding rows — below the 10-row floor.
	for i := 0; i < 5; i++ {
		spanID := "span-warm-" + string(rune('a'+i))
		gID := seedGroundingForRetrieval(t, pool, "vault_search", spanID, now.Add(-time.Duration(i)*time.Minute))
		seedInteraction(t, pool, gID, spanID, "ref-"+spanID, "followed", 1.0)
	}
	srv := newTestServer(t, pool)
	var got []RetrievalHealthAction
	getJSON(t, srv, "/inference/retrieval-health", &got)
	if len(got) != 1 {
		t.Fatalf("expected 1 action, got %d", len(got))
	}
	if !got[0].WarmingUp {
		t.Errorf("warming_up should be true at 5 < floor 10 grounding rows")
	}
}

func TestInferenceRetrievalHealth_ActionsWithoutGroundingSkipped(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// Seed vault_search only — kiwix_search + knowledge_search have no
	// rows and shouldn't appear in the response.
	for i := 0; i < 3; i++ {
		spanID := "span-only-" + string(rune('a'+i))
		seedGroundingForRetrieval(t, pool, "vault_search", spanID, now.Add(-time.Duration(i)*time.Minute))
	}
	srv := newTestServer(t, pool)
	var got []RetrievalHealthAction
	getJSON(t, srv, "/inference/retrieval-health", &got)
	if len(got) != 1 || got[0].Action != "vault_search" {
		t.Errorf("expected only vault_search, got: %+v", got)
	}
}

func TestInferenceRetrievalHealth_MostActiveFirst(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	// vault_search: 20 rows; kiwix_search: 30 rows. Kiwix should sort
	// first (higher grounding_count).
	for i := 0; i < 20; i++ {
		seedGroundingForRetrieval(t, pool, "vault_search", "span-vs-"+string(rune('a'+i)), now.Add(-time.Duration(i)*time.Minute))
	}
	for i := 0; i < 30; i++ {
		seedGroundingForRetrieval(t, pool, "kiwix_search", "span-ks-"+string(rune('a'+i)), now.Add(-time.Duration(i)*time.Minute))
	}
	srv := newTestServer(t, pool)
	var got []RetrievalHealthAction
	getJSON(t, srv, "/inference/retrieval-health", &got)
	if len(got) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(got))
	}
	if got[0].Action != "kiwix_search" {
		t.Errorf("expected kiwix_search first (30 > 20), got %s", got[0].Action)
	}
	if got[1].Action != "vault_search" {
		t.Errorf("expected vault_search second, got %s", got[1].Action)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
