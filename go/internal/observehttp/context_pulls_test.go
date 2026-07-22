package observehttp

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// reference-resolution-substrate-frontend RF2 tests.
//
// Coverage:
//   - list: filter narrowing across all four axes (query_source/shape/
//     confidence_tier/source_type), pagination cursor, ml score field
//     null vs populated, graceful empty
//   - detail: 200 with full envelope, 404 on missing id, 404 on a row
//     whose query_source != reference_resolution (scoping), 400 on
//     non-integer path id
//   - by-entity: prompt_id join correctness across multiple resolutions,
//     missing project_id → 400, empty-but-not-absent state
//   - stats + timeseries: zero-fill, segment validation, bucket shape

// --- seed helpers -----------------------------------------------------

type rreSeed struct {
	GroundingEventID           int64
	Shape                      string
	ConfidenceScore            float64
	DetectionMethod            string
	StartPos                   int
	EndPos                     int
	ConfidenceTier             string
	PresentationRecommendation string
	PresentedAs                string
	ResolverName               string
	RetrievalCostMs            int64
	MLConfidenceScore          *float64
}

func seedReferenceResolutionEmit(t *testing.T, pool *db.Pool, s rreSeed) {
	t.Helper()
	if s.Shape == "" {
		s.Shape = "chain_slug"
	}
	if s.DetectionMethod == "" {
		s.DetectionMethod = "regex+list_match"
	}
	if s.ConfidenceTier == "" {
		s.ConfidenceTier = "single_exact"
	}
	if s.PresentationRecommendation == "" {
		s.PresentationRecommendation = "use_directly"
	}
	if s.PresentedAs == "" {
		s.PresentedAs = "<presented-as-stub>"
	}
	if s.ResolverName == "" {
		s.ResolverName = "stubResolver"
	}
	var ml any
	if s.MLConfidenceScore != nil {
		ml = *s.MLConfidenceScore
	}
	if _, err := pool.DB().Exec(`
		INSERT INTO reference_resolution_emits
			(grounding_event_id, shape, confidence_score, detection_method,
			 start_pos, end_pos, confidence_tier, presentation_recommendation,
			 presented_as, resolver_name, retrieval_cost_ms, ml_confidence_score)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.GroundingEventID, s.Shape, s.ConfidenceScore, s.DetectionMethod,
		s.StartPos, s.EndPos, s.ConfidenceTier, s.PresentationRecommendation,
		s.PresentedAs, s.ResolverName, s.RetrievalCostMs, ml,
	); err != nil {
		t.Fatalf("seedReferenceResolutionEmit %d: %v", s.GroundingEventID, err)
	}
}

// seedRefResRow inserts a refresolve grounding_events row + side-table
// row pair, the canonical fixture for these tests. Defaults aim at the
// common case (single chain_slug detection with one candidate).
func seedRefResRow(t *testing.T, pool *db.Pool, id int64, project string, opts ...func(*geSeed, *rreSeed)) {
	t.Helper()
	ge := geSeed{
		ID:           id,
		ProjectID:    project,
		QuerySource:  "reference_resolution",
		Action:       "resolve_references",
		SourceRefs:   `["chain:cap-test"]`,
		ResultsCount: 1,
	}
	rre := rreSeed{
		GroundingEventID: id,
		Shape:            "chain_slug",
		ConfidenceScore:  1.0,
		DetectionMethod:  "regex+list_match",
		ConfidenceTier:   "single_exact",
		ResolverName:     "chainResolver",
		RetrievalCostMs:  5,
	}
	for _, opt := range opts {
		opt(&ge, &rre)
	}
	seedGroundingEvent(t, pool, ge)
	seedReferenceResolutionEmit(t, pool, rre)
}

// --- list -------------------------------------------------------------

func TestContextPullsList_ReturnsSeededRows(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedKnowledgePointer(t, pool, "p1", "chain", "chain:cap-test", "q", 0)
	seedRefResRow(t, pool, 100, "p1")

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	if code := getJSON(t, srv, "/context-pulls", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(resp.Items), resp.Items)
	}
	row := resp.Items[0]
	if row.GroundingEventID != 100 {
		t.Errorf("id = %d, want 100", row.GroundingEventID)
	}
	if row.Shape == nil || *row.Shape != "chain_slug" {
		t.Errorf("shape = %v, want chain_slug", row.Shape)
	}
	if row.FirstCandidate == nil || row.FirstCandidate.SourceRef != "chain:cap-test" {
		t.Errorf("first_candidate = %+v", row.FirstCandidate)
	}
	if row.FirstCandidate.SourceType != "chain" {
		t.Errorf("first_candidate.source_type = %q, want chain", row.FirstCandidate.SourceType)
	}
}

func TestContextPullsList_DefaultsToReferenceResolutionScope(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// One reference-resolution row, one agent-initiated row.
	seedRefResRow(t, pool, 100, "p1")
	seedGroundingEvent(t, pool, geSeed{ID: 200, ProjectID: "p1", QuerySource: "agent_initiated"})

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls", &resp)
	if len(resp.Items) != 1 || resp.Items[0].GroundingEventID != 100 {
		t.Errorf("default scope = %+v; want only the reference_resolution row 100", resp.Items)
	}
}

func TestContextPullsList_FiltersByShape(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedRefResRow(t, pool, 100, "p1", func(_ *geSeed, rre *rreSeed) { rre.Shape = "chain_slug" })
	seedRefResRow(t, pool, 200, "p1", func(ge *geSeed, rre *rreSeed) {
		ge.CallID = "c200"
		rre.Shape = "domain_term"
	})

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls?shape=domain_term", &resp)
	if len(resp.Items) != 1 || *resp.Items[0].Shape != "domain_term" {
		t.Errorf("shape filter wrong: %+v", resp.Items)
	}
}

func TestContextPullsList_FiltersByConfidenceTier(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedRefResRow(t, pool, 100, "p1", func(_ *geSeed, rre *rreSeed) { rre.ConfidenceTier = "single_exact" })
	seedRefResRow(t, pool, 200, "p1", func(ge *geSeed, rre *rreSeed) {
		ge.CallID = "c200"
		rre.ConfidenceTier = "weak_domain"
	})

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls?confidence_tier=weak_domain", &resp)
	if len(resp.Items) != 1 || *resp.Items[0].ConfidenceTier != "weak_domain" {
		t.Errorf("tier filter wrong: %+v", resp.Items)
	}
}

func TestContextPullsList_FiltersBySourceType(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedKnowledgePointer(t, pool, "p1", "chain", "chain:c1", "q", 0)
	seedKnowledgePointer(t, pool, "p1", "vault", "vault:v1", "q", 0)
	seedRefResRow(t, pool, 100, "p1", func(ge *geSeed, _ *rreSeed) { ge.SourceRefs = `["chain:c1"]` })
	seedRefResRow(t, pool, 200, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c200"
		ge.SourceRefs = `["vault:v1"]`
	})

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls?source_type=vault", &resp)
	if len(resp.Items) != 1 || resp.Items[0].GroundingEventID != 200 {
		t.Errorf("source_type filter wrong: %+v", resp.Items)
	}
}

func TestContextPullsList_InvalidQuerySourceReturns400(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/context-pulls?query_source=garbage", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(out["error"], "query_source") {
		t.Errorf("error = %q; want it to name the axis", out["error"])
	}
}

func TestContextPullsList_PaginationCursor(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	for i := int64(1); i <= 5; i++ {
		seedRefResRow(t, pool, i, "p1", func(ge *geSeed, _ *rreSeed) {
			ge.CallID = "c-" + string(rune('0'+i))
		})
	}

	srv := newAuditServer(t, pool)
	var page1 contextPullListResponse
	getJSON(t, srv, "/context-pulls?limit=2", &page1)
	if len(page1.Items) != 2 {
		t.Fatalf("page1 items = %d, want 2", len(page1.Items))
	}
	// DESC ordering: ids 5, 4 first.
	if page1.Items[0].GroundingEventID != 5 || page1.Items[1].GroundingEventID != 4 {
		t.Errorf("page1 ids = %d, %d; want 5, 4",
			page1.Items[0].GroundingEventID, page1.Items[1].GroundingEventID)
	}
	if page1.NextCursor == nil || *page1.NextCursor != 4 {
		t.Errorf("page1 next_cursor = %v, want 4", page1.NextCursor)
	}

	var page2 contextPullListResponse
	getJSON(t, srv, "/context-pulls?limit=2&cursor=4", &page2)
	if len(page2.Items) != 2 || page2.Items[0].GroundingEventID != 3 {
		t.Errorf("page2 = %+v", page2.Items)
	}
}

func TestContextPullsList_MLScoreSerializesNullByDefault(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedRefResRow(t, pool, 100, "p1")

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls", &resp)
	if resp.Items[0].MLConfidenceScore != nil {
		t.Errorf("ml_confidence_score should be nil pre-T7, got %v", *resp.Items[0].MLConfidenceScore)
	}
}

func TestContextPullsList_MLScoreSerializesWhenPopulated(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	score := 0.83
	seedRefResRow(t, pool, 100, "p1", func(_ *geSeed, rre *rreSeed) {
		rre.MLConfidenceScore = &score
	})

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls", &resp)
	if resp.Items[0].MLConfidenceScore == nil || *resp.Items[0].MLConfidenceScore != 0.83 {
		t.Errorf("ml_confidence_score = %v, want 0.83", resp.Items[0].MLConfidenceScore)
	}
}

// REGRESSION: scanContextPullRows used to store *contextPullRow pointers
// from a slice that grew under append, invalidating prior pointers when
// the underlying array reallocated. With < 8 rows the first append
// often stays in the initial allocation and the bug doesn't fire; at
// scale (84 rows in production) every row reads first_candidate=null.
// Seed enough rows here that the slice provably grows past at least one
// reallocation boundary (zero-cap [] grows in doublings).
func TestContextPullsList_FirstCandidatePopulatedAcrossSliceGrowths(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedKnowledgePointer(t, pool, "p1", "chain", "chain:cap-test", "q", 0)
	const N = 50
	for i := int64(1); i <= N; i++ {
		seedRefResRow(t, pool, i, "p1", func(ge *geSeed, _ *rreSeed) {
			ge.CallID = fmt.Sprintf("c-%d", i)
		})
	}

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, fmt.Sprintf("/context-pulls?limit=%d", N), &resp)
	if len(resp.Items) != N {
		t.Fatalf("got %d items, want %d", len(resp.Items), N)
	}
	for i, row := range resp.Items {
		if row.FirstCandidate == nil {
			t.Errorf("items[%d] (id=%d): first_candidate is nil; slice-pointer regression",
				i, row.GroundingEventID)
		}
	}
}

func TestContextPullsList_EmptyButNotAbsent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	code := getJSON(t, srv, "/context-pulls", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Items == nil {
		t.Errorf("items should be empty slice not nil")
	}
}

func TestContextPullsList_AvailableLegendsPopulated(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls", &resp)
	if len(resp.AvailableQuerySources) == 0 {
		t.Error("available_query_sources empty")
	}
	if len(resp.AvailableShapes) == 0 {
		t.Error("available_shapes empty")
	}
	if len(resp.AvailableConfidenceTrs) != 4 {
		t.Errorf("available_confidence_tiers len = %d, want 4", len(resp.AvailableConfidenceTrs))
	}
}

// --- detail -----------------------------------------------------------

func TestContextPullsDetail_ReturnsFullEnvelope(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedKnowledgePointer(t, pool, "p1", "chain", "chain:cap-test", "q", 0)
	seedRefResRow(t, pool, 100, "p1", func(ge *geSeed, rre *rreSeed) {
		ge.QueryText = "look at cap-test"
		rre.StartPos = 8
		rre.EndPos = 16
		rre.PresentedAs = "`cap-test` → chain in p1"
	})
	seedQueryInteraction(t, pool, qiSeed{
		GroundingEventID: 100, SourceRef: "chain:cap-test", Position: 1,
		ClickKind: "cited",
	})

	srv := newAuditServer(t, pool)
	var resp contextPullDetail
	code := getJSON(t, srv, "/context-pulls/100", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.GroundingEvent.ID != 100 {
		t.Errorf("grounding_event.id = %d", resp.GroundingEvent.ID)
	}
	if resp.Detection.Shape != "chain_slug" || resp.Detection.StartPos != 8 || resp.Detection.EndPos != 16 {
		t.Errorf("detection = %+v", resp.Detection)
	}
	if resp.Detection.Token != "look at cap-test" {
		t.Errorf("detection.token = %q, want %q", resp.Detection.Token, "look at cap-test")
	}
	if resp.Resolver.Name != "chainResolver" {
		t.Errorf("resolver.name = %q", resp.Resolver.Name)
	}
	if resp.Outcome.ConfidenceTier != "single_exact" {
		t.Errorf("outcome.confidence_tier = %q", resp.Outcome.ConfidenceTier)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].SourceRef != "chain:cap-test" {
		t.Errorf("candidates = %+v", resp.Candidates)
	}
	if len(resp.Interactions) != 1 || resp.Interactions[0].ClickKind != "cited" {
		t.Errorf("interactions = %+v", resp.Interactions)
	}
	// Cross-substrate reference: trajectory_deep_link is the QF3 path.
	if resp.TrajectoryDeepLink != "/telemetry/trajectories/100" {
		t.Errorf("trajectory_deep_link = %q", resp.TrajectoryDeepLink)
	}
}

func TestContextPullsDetail_404OnMissing(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/context-pulls/999999", &out)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestContextPullsDetail_404OnNonReferenceResolutionRow(t *testing.T) {
	// Scoping: a grounding_event with query_source='agent_initiated'
	// is out of scope for this endpoint; treat as 404 not 200.
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedGroundingEvent(t, pool, geSeed{ID: 300, ProjectID: "p1", QuerySource: "agent_initiated"})

	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/context-pulls/300", &out)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (scoping)", code)
	}
}

func TestContextPullsDetail_400OnInvalidID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/context-pulls/notanumber", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// --- by-entity --------------------------------------------------------

func TestContextPullsByEntity_JoinsByPromptID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	const prompt = "0190f8a3-bbbb-7000-8000-000000000001"

	// A resolve_references row stamped with the same prompt_id as the
	// resolving event of bug "my-bug" in project p1.
	seedRefResRow(t, pool, 100, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.PromptID = prompt
	})
	// A second resolve_references row in a different prompt — must NOT
	// show up in the entity-scoped result.
	seedRefResRow(t, pool, 200, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c200"
		ge.PromptID = "0190f8a3-bbbb-7000-8000-000000000099"
	})
	seedQueryResolution(t, pool, qrSeed{
		ResolutionID:    "reso-1",
		PromptID:        prompt,
		EntityKind:      "bug",
		EntitySlug:      "my-bug",
		EntityProjectID: "p1",
		OutcomeKind:     "resolved",
	})

	srv := newAuditServer(t, pool)
	var resp contextPullByEntityResponse
	code := getJSON(t, srv, "/context-pulls/by-entity/bug/my-bug?project=p1", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.MatchedPromptIDs) != 1 || resp.MatchedPromptIDs[0] != prompt {
		t.Errorf("matched_prompt_ids = %v; want [%s]", resp.MatchedPromptIDs, prompt)
	}
	if len(resp.Items) != 1 || resp.Items[0].GroundingEventID != 100 {
		t.Errorf("items = %+v; want only id=100", resp.Items)
	}
}

func TestContextPullsByEntity_400WithoutProject(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/context-pulls/by-entity/bug/my-bug", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(out["error"], "project") {
		t.Errorf("error = %q; want it to name project", out["error"])
	}
}

func TestContextPullsByEntity_EmptyWhenNoResolutions(t *testing.T) {
	// Entity exists but has no resolving query_resolutions yet —
	// matched_prompt_ids is empty, items is empty array, status 200.
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	srv := newAuditServer(t, pool)
	var resp contextPullByEntityResponse
	code := getJSON(t, srv, "/context-pulls/by-entity/task/no-resolutions?project=p1", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Items == nil || len(resp.Items) != 0 {
		t.Errorf("items = %+v, want empty array", resp.Items)
	}
	if resp.MatchedPromptIDs == nil || len(resp.MatchedPromptIDs) != 0 {
		t.Errorf("matched_prompt_ids = %v, want empty array", resp.MatchedPromptIDs)
	}
}

func TestContextPullsByEntity_400OnUnknownKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/context-pulls/by-entity/widget/x?project=p1", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// --- stats ------------------------------------------------------------

func TestContextPullsStats_ZeroFilledAndShaped(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedKnowledgePointer(t, pool, "p1", "chain", "chain:cap-test", "q", 0)
	seedRefResRow(t, pool, 100, "p1")
	seedRefResRow(t, pool, 200, "p1", func(ge *geSeed, rre *rreSeed) {
		ge.CallID = "c200"
		rre.Shape = "domain_term"
		rre.ConfidenceTier = "weak_domain"
	})

	srv := newAuditServer(t, pool)
	var resp contextPullStatsResponse
	code := getJSON(t, srv, "/context-pulls/stats", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.TotalReferences != 2 {
		t.Errorf("total = %d, want 2", resp.TotalReferences)
	}
	if resp.ByShape["chain_slug"] != 1 || resp.ByShape["domain_term"] != 1 {
		t.Errorf("by_shape = %+v", resp.ByShape)
	}
	if resp.ByConfidenceTier["single_exact"] != 1 || resp.ByConfidenceTier["weak_domain"] != 1 {
		t.Errorf("by_confidence_tier = %+v", resp.ByConfidenceTier)
	}
	if resp.ByConfidenceTier["fuzzy_multi"] != 0 || resp.ByConfidenceTier["no_hit"] != 0 {
		t.Errorf("zero-fill broken: %+v", resp.ByConfidenceTier)
	}
}

func TestContextPullsStatsTimeseries_BucketsAndSegments(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedRefResRow(t, pool, 1, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CreatedAt = "2026-05-15T08:00:00Z"
	})
	seedRefResRow(t, pool, 2, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c2"
		ge.CreatedAt = "2026-05-15T09:00:00Z"
	})
	seedRefResRow(t, pool, 3, "p1", func(ge *geSeed, rre *rreSeed) {
		ge.CallID = "c3"
		ge.CreatedAt = "2026-05-16T08:00:00Z"
		rre.Shape = "domain_term"
	})

	srv := newAuditServer(t, pool)
	var resp contextPullsTimeseriesResponse
	code := getJSON(t, srv, "/context-pulls/stats/timeseries?segment=shape", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Segment != "shape" {
		t.Errorf("segment echo = %q", resp.Segment)
	}
	if len(resp.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2: %+v", len(resp.Buckets), resp.Buckets)
	}
	if resp.Buckets[0].Segments["chain_slug"] != 2 {
		t.Errorf("day1 chain_slug = %d, want 2", resp.Buckets[0].Segments["chain_slug"])
	}
	if resp.Buckets[1].Segments["domain_term"] != 1 {
		t.Errorf("day2 domain_term = %d", resp.Buckets[1].Segments["domain_term"])
	}
}

func TestContextPullsStatsTimeseries_InvalidSegment(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/context-pulls/stats/timeseries?segment=garbage", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(out["error"], "shape") || !strings.Contains(out["error"], "confidence_tier") {
		t.Errorf("error = %q; want it to name valid segments", out["error"])
	}
}
