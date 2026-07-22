package observehttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// query-telemetry-substrate-frontend QF2 tests.
//
// Coverage targets (task acceptance):
//   - per-filter subset correctness (label_kind, query_source, project, q)
//   - cursor pagination correctness on training-pairs
//   - chart endpoints bucket correctly + return ordered time-series
//   - trajectory endpoint composes query + results + interactions + resolutions
//     and carries per-result source_type from the knowledge_pointers join
//   - graceful no-data — non-empty body, not 204
//   - label_kind covers all 5 enum values
//   - segment param: missing OR invalid returns 400 naming both valid axes
//   - response shape pins (struct-tag field names; the closed enum field
//     set lives in this file's assertHas* helpers so accidental rename
//     in telemetry.go is caught here)

// --- seed helpers -----------------------------------------------------

// seedGroundingEvent inserts one row into grounding_events. Defaults
// in geFill keep callers focused on the columns their test exercises.
type geSeed struct {
	ID           int64
	ProjectID    string
	SessionID    string
	CallID       string
	Action       string
	QuerySource  string
	QueryText    string
	SpanID       string
	PromptID     string
	ParentSpanID string
	ResultsCount int
	SourceRefs   string // JSON; defaults to "[]"
	CreatedAt    string
}

func seedGroundingEvent(t *testing.T, pool *db.Pool, s geSeed) {
	t.Helper()
	if s.SessionID == "" {
		s.SessionID = "sess-default"
	}
	if s.CallID == "" {
		s.CallID = fmt.Sprintf("call-%d", s.ID)
	}
	if s.Action == "" {
		s.Action = "vault_search"
	}
	if s.QuerySource == "" {
		s.QuerySource = "agent_initiated"
	}
	if s.QueryText == "" {
		// Migration 071 + the query_training.go writer filter drop
		// NULL/empty-query grounding events from
		// proj_training_data_for_reranker (a reranker (query, candidate)
		// row with no query is untrainable). Default to a non-empty query
		// so callers that don't set one still land a trainable
		// training-pairs row.
		s.QueryText = "q:" + s.Action
	}
	if s.SpanID == "" {
		s.SpanID = "0190f8a3-0001-7000-8000-000000000001"
	}
	if s.SourceRefs == "" {
		s.SourceRefs = "[]"
	}
	if s.CreatedAt == "" {
		s.CreatedAt = "2026-05-17T12:00:00.000Z"
	}
	if s.ResultsCount == 0 {
		// Count the JSON array elements; cheap (no allocation if empty).
		s.ResultsCount = strings.Count(s.SourceRefs, ",") + 1
		if s.SourceRefs == "[]" {
			s.ResultsCount = 0
		}
	}
	var promptArg, parentArg, queryTextArg any
	if s.PromptID != "" {
		promptArg = s.PromptID
	}
	if s.ParentSpanID != "" {
		parentArg = s.ParentSpanID
	}
	if s.QueryText != "" {
		queryTextArg = s.QueryText
	}
	if _, err := pool.DB().Exec(`
		INSERT INTO grounding_events
		    (id, project_id, session_id, call_id, action, results_count,
		     source_refs, next_turn_has_output, span_id, query_source,
		     query_text, prompt_id, parent_span_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.ProjectID, s.SessionID, s.CallID, s.Action, s.ResultsCount,
		s.SourceRefs, s.SpanID, s.QuerySource, queryTextArg, promptArg, parentArg, s.CreatedAt,
	); err != nil {
		t.Fatalf("seedGroundingEvent %d: %v", s.ID, err)
	}
}

type qiSeed struct {
	GroundingEventID int64
	SourceRef        string
	Position         int
	ClickKind        string // followed|cited|mentioned|resolved-from
	ClickWeight      float64
	CitationKind     string
	DwellMs          int64
	WasInjected      int
	SpanID           string
	SessionID        string
	PromptID         string
	DetectedAt       string
}

func seedQueryInteraction(t *testing.T, pool *db.Pool, s qiSeed) {
	t.Helper()
	if s.SpanID == "" {
		s.SpanID = "0190f8a3-0001-7000-8000-000000000001"
	}
	if s.SessionID == "" {
		s.SessionID = "sess-default"
	}
	if s.DetectedAt == "" {
		s.DetectedAt = "2026-05-17T12:01:00.000Z"
	}
	if s.ClickKind == "" {
		s.ClickKind = "followed"
	}
	if s.ClickWeight == 0 {
		s.ClickWeight = 1.0
	}
	var citeArg, dwellArg, promptArg any
	if s.CitationKind != "" {
		citeArg = s.CitationKind
	}
	if s.DwellMs > 0 {
		dwellArg = s.DwellMs
	}
	if s.PromptID != "" {
		promptArg = s.PromptID
	}
	if _, err := pool.DB().Exec(`
		INSERT INTO query_interactions
		    (grounding_event_id, source_ref, position, click_kind, click_weight,
		     citation_kind, dwell_ms_estimate, was_injected, span_id, prompt_id,
		     session_id, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.GroundingEventID, s.SourceRef, s.Position, s.ClickKind, s.ClickWeight,
		citeArg, dwellArg, s.WasInjected, s.SpanID, promptArg, s.SessionID, s.DetectedAt,
	); err != nil {
		t.Fatalf("seedQueryInteraction: %v", err)
	}
}

type qrSeed struct {
	ResolutionID        string
	PromptID            string
	SessionID           string
	SpanID              string
	EntityKind          string
	EntitySlug          string
	EntityProjectID     string
	OutcomeKind         string
	WriteEventIDs       string // JSON array of event_ids
	GroundingEventIDs   string // JSON array of integers (grounding_events.id)
	QueryInteractionIDs string
	DetectedAt          string
}

func seedQueryResolution(t *testing.T, pool *db.Pool, s qrSeed) {
	t.Helper()
	if s.PromptID == "" {
		s.PromptID = "prompt-default"
	}
	if s.SessionID == "" {
		s.SessionID = "sess-default"
	}
	if s.SpanID == "" {
		s.SpanID = "0190f8a3-0001-7000-8000-000000000001"
	}
	if s.WriteEventIDs == "" {
		s.WriteEventIDs = "[]"
	}
	if s.GroundingEventIDs == "" {
		s.GroundingEventIDs = "[]"
	}
	if s.QueryInteractionIDs == "" {
		s.QueryInteractionIDs = "[]"
	}
	if s.DetectedAt == "" {
		s.DetectedAt = "2026-05-17T12:05:00.000Z"
	}
	if _, err := pool.DB().Exec(`
		INSERT INTO query_resolutions
		    (resolution_id, prompt_id, session_id, span_id,
		     entity_kind, entity_slug, entity_project_id, outcome_kind,
		     write_event_ids, grounding_event_ids, query_interaction_ids, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ResolutionID, s.PromptID, s.SessionID, s.SpanID,
		s.EntityKind, s.EntitySlug, s.EntityProjectID, s.OutcomeKind,
		s.WriteEventIDs, s.GroundingEventIDs, s.QueryInteractionIDs, s.DetectedAt,
	); err != nil {
		t.Fatalf("seedQueryResolution %s: %v", s.ResolutionID, err)
	}
}

// --- trajectory tests -------------------------------------------------

func TestTelemetryTrajectory_ComposesAllFourSections(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedKnowledgePointer(t, pool, "p1", "vault", "learnings/p1/foo.md", "q1", 0)
	seedKnowledgePointer(t, pool, "p1", "kiwix", "kiwix/devdocs/rust/iter", "q2", 0)
	seedGroundingEvent(t, pool, geSeed{
		ID: 100, ProjectID: "p1",
		QueryText:    "iteration semantics",
		SourceRefs:   `["learnings/p1/foo.md","kiwix/devdocs/rust/iter"]`,
		ResultsCount: 2,
	})
	seedQueryInteraction(t, pool, qiSeed{
		GroundingEventID: 100, SourceRef: "learnings/p1/foo.md", Position: 1,
		ClickKind: "followed", ClickWeight: 1.0, DwellMs: 4200,
	})
	seedQueryResolution(t, pool, qrSeed{
		ResolutionID: "reso-1", EntityKind: "task", EntitySlug: "t1",
		EntityProjectID: "p1", OutcomeKind: "completed",
		GroundingEventIDs: "[100]",
	})

	srv := newAuditServer(t, pool)
	var resp trajectoryResponse
	if code := getJSON(t, srv, "/telemetry/trajectories/100", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	if resp.Query.QueryID != 100 {
		t.Errorf("Query.QueryID = %d, want 100", resp.Query.QueryID)
	}
	if resp.Query.Action != "vault_search" {
		t.Errorf("Query.Action = %q, want vault_search", resp.Query.Action)
	}
	if resp.Query.QueryText == nil || *resp.Query.QueryText != "iteration semantics" {
		t.Errorf("Query.QueryText = %v, want pointer to %q", resp.Query.QueryText, "iteration semantics")
	}

	if len(resp.Results) != 2 {
		t.Fatalf("Results len = %d, want 2", len(resp.Results))
	}
	// Per-result source_type carried from the knowledge_pointers JOIN.
	r0 := resp.Results[0]
	if r0.Position != 1 || r0.SourceRef != "learnings/p1/foo.md" {
		t.Errorf("Results[0] = %+v", r0)
	}
	if r0.SourceType == nil || *r0.SourceType != "vault" {
		t.Errorf("Results[0].SourceType = %v, want pointer to vault", r0.SourceType)
	}
	r1 := resp.Results[1]
	if r1.SourceType == nil || *r1.SourceType != "kiwix" {
		t.Errorf("Results[1].SourceType = %v, want pointer to kiwix", r1.SourceType)
	}

	if len(resp.Interactions) != 1 || resp.Interactions[0].ClickKind != "followed" {
		t.Errorf("Interactions = %+v", resp.Interactions)
	}
	if len(resp.Resolutions) != 1 || resp.Resolutions[0].ResolutionID != "reso-1" {
		t.Errorf("Resolutions = %+v", resp.Resolutions)
	}
}

func TestTelemetryTrajectory_InvalidQueryID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/telemetry/trajectories/notanumber", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if out["error"] != "invalid query_id" {
		t.Errorf("error = %q, want %q", out["error"], "invalid query_id")
	}
}

func TestTelemetryTrajectory_NotFound(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/telemetry/trajectories/999999", &out)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if out["error"] != "query not found" {
		t.Errorf("error = %q", out["error"])
	}
}

func TestTelemetryTrajectory_EmptyButNotAbsent(t *testing.T) {
	// Query exists; no interactions, no resolutions. Expect 200 with
	// empty arrays (not 204; not error).
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedGroundingEvent(t, pool, geSeed{ID: 200, ProjectID: "p1", SourceRefs: "[]"})
	srv := newAuditServer(t, pool)

	var resp trajectoryResponse
	code := getJSON(t, srv, "/telemetry/trajectories/200", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Results == nil || resp.Interactions == nil || resp.Resolutions == nil {
		t.Errorf("expected non-nil empty arrays, got %+v", resp)
	}
	if len(resp.Results) != 0 || len(resp.Interactions) != 0 || len(resp.Resolutions) != 0 {
		t.Errorf("expected empty arrays, got %+v", resp)
	}
}

func TestTelemetryTrajectory_BySpan_MultipleEvents(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	const span = "0190f8a3-aaaa-7000-8000-000000000abc"
	seedGroundingEvent(t, pool, geSeed{ID: 301, ProjectID: "p1", SpanID: span, Action: "vault_search"})
	seedGroundingEvent(t, pool, geSeed{ID: 302, ProjectID: "p1", SpanID: span, Action: "kiwix_search", CallID: "c302"})

	srv := newAuditServer(t, pool)
	var resp trajectoryBySpanResponse
	code := getJSON(t, srv, "/telemetry/trajectories?span_id="+span, &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Trajectories) != 2 {
		t.Fatalf("trajectories len = %d, want 2: %+v", len(resp.Trajectories), resp.Trajectories)
	}
}

func TestTelemetryTrajectory_BySpan_MissingParam(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/telemetry/trajectories", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(out["error"], "query_id") || !strings.Contains(out["error"], "span_id") {
		t.Errorf("error = %q; want both axes named", out["error"])
	}
}

// --- analytics: volume-by-source --------------------------------------

func TestTelemetryVolumeBySource_BucketsAndTotals(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Three events across two days, two actions.
	seedGroundingEvent(t, pool, geSeed{ID: 1, ProjectID: "p1", Action: "vault_search",
		CreatedAt: "2026-05-15T08:00:00Z"})
	seedGroundingEvent(t, pool, geSeed{ID: 2, ProjectID: "p1", Action: "vault_search",
		CreatedAt: "2026-05-15T09:00:00Z", CallID: "c2"})
	seedGroundingEvent(t, pool, geSeed{ID: 3, ProjectID: "p1", Action: "kiwix_search",
		CreatedAt: "2026-05-16T08:00:00Z", CallID: "c3"})

	srv := newTestServer(t, pool)
	var resp analyticsVolumeResponse
	code := getJSON(t, srv, "/telemetry/analytics/volume-by-source?segment=action&since=2026-05-15&until=2026-05-16&project=p1", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Segment != "action" {
		t.Errorf("segment echo = %q", resp.Segment)
	}
	if len(resp.Buckets) != 2 {
		t.Fatalf("buckets len = %d, want 2: %+v", len(resp.Buckets), resp.Buckets)
	}
	if resp.Buckets[0].Day != "2026-05-15" || resp.Buckets[1].Day != "2026-05-16" {
		t.Errorf("day order wrong: %+v", resp.Buckets)
	}
	if resp.Buckets[0].Segments["vault_search"] != 2 {
		t.Errorf("day 15 vault_search = %d, want 2", resp.Buckets[0].Segments["vault_search"])
	}
	if resp.TotalsBySegment["vault_search"] != 2 || resp.TotalsBySegment["kiwix_search"] != 1 {
		t.Errorf("totals = %+v", resp.TotalsBySegment)
	}
}

func TestTelemetryVolumeBySource_InvalidSegment(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/telemetry/analytics/volume-by-source?segment=source_type", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	// Must name BOTH valid axes in the error so the operator doesn't
	// have to dig through docs.
	if !strings.Contains(out["error"], "action") || !strings.Contains(out["error"], "query_source") {
		t.Errorf("error = %q; want both axes named", out["error"])
	}
}

func TestTelemetryVolumeBySource_MissingSegment(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/telemetry/analytics/volume-by-source", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(out["error"], "action") || !strings.Contains(out["error"], "query_source") {
		t.Errorf("error = %q; want both axes named", out["error"])
	}
}

func TestTelemetryVolumeBySource_NoDataNonEmptyBody(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var resp analyticsVolumeResponse
	code := getJSON(t, srv, "/telemetry/analytics/volume-by-source?segment=action", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Segment != "action" {
		t.Errorf("segment = %q", resp.Segment)
	}
	if resp.Buckets == nil || resp.TotalsBySegment == nil {
		t.Errorf("expected non-nil collections, got %+v", resp)
	}
}

// --- analytics: success-rate ------------------------------------------

func TestTelemetrySuccessRate_BucketsAndRate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Two queries on day 15: one followed (success), one ignored.
	// One query on day 16: cited (success).
	seedGroundingEvent(t, pool, geSeed{ID: 11, ProjectID: "p1", Action: "vault_search",
		SourceRefs: `["a","b"]`, ResultsCount: 2, CreatedAt: "2026-05-15T08:00:00Z"})
	seedGroundingEvent(t, pool, geSeed{ID: 12, ProjectID: "p1", Action: "vault_search",
		SourceRefs: `["c","d"]`, ResultsCount: 2, CreatedAt: "2026-05-15T09:00:00Z", CallID: "c12"})
	seedGroundingEvent(t, pool, geSeed{ID: 13, ProjectID: "p1", Action: "vault_search",
		SourceRefs: `["e"]`, ResultsCount: 1, CreatedAt: "2026-05-16T09:00:00Z", CallID: "c13"})
	seedQueryInteraction(t, pool, qiSeed{
		GroundingEventID: 11, SourceRef: "a", ClickKind: "followed", ClickWeight: 1.0, Position: 1,
	})
	seedQueryInteraction(t, pool, qiSeed{
		GroundingEventID: 13, SourceRef: "e", ClickKind: "cited", ClickWeight: 0.8, Position: 1,
	})

	srv := newTestServer(t, pool) // refreshes proj_retrieval_success_per_query
	var resp analyticsSuccessResponse
	code := getJSON(t, srv, "/telemetry/analytics/success-rate?segment=action&since=2026-05-15&until=2026-05-16&project=p1", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Buckets) != 2 {
		t.Fatalf("buckets len = %d, want 2", len(resp.Buckets))
	}
	day15 := resp.Buckets[0].Segments["vault_search"]
	if day15.QueryCount != 2 || day15.SuccessCount != 1 || day15.SuccessRate != 0.5 {
		t.Errorf("day 15 vault_search = %+v, want {2,1,0.5}", day15)
	}
	day16 := resp.Buckets[1].Segments["vault_search"]
	if day16.QueryCount != 1 || day16.SuccessCount != 1 || day16.SuccessRate != 1.0 {
		t.Errorf("day 16 vault_search = %+v, want {1,1,1.0}", day16)
	}
	tot := resp.TotalsBySegment["vault_search"]
	if tot.QueryCount != 3 || tot.SuccessCount != 2 {
		t.Errorf("totals = %+v", tot)
	}
}

// --- training-pairs: list ---------------------------------------------

func TestTelemetryTrainingPairs_FiltersAndPagination(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Six events with controlled refs to drive the training-data
	// projection. The proj_training_data_for_reranker rule emits one
	// row per (event_id, ref). We seed enough variety to assert
	// label_kind covers all 5 buckets across the corpus.
	for i := 1; i <= 6; i++ {
		seedGroundingEvent(t, pool, geSeed{
			ID: int64(i * 10), ProjectID: "p1",
			QueryText:    fmt.Sprintf("query number %d about telemetry", i),
			SourceRefs:   fmt.Sprintf(`["ref-%d-1","ref-%d-2","ref-%d-3","ref-%d-4","ref-%d-5"]`, i, i, i, i, i),
			ResultsCount: 5,
			CallID:       fmt.Sprintf("c-%d", i),
		})
	}
	// ID 10: followed on ref 1 → positive
	seedQueryInteraction(t, pool, qiSeed{
		GroundingEventID: 10, SourceRef: "ref-1-1", ClickKind: "followed", ClickWeight: 1.0, Position: 1,
	})
	// ID 20: mentioned on ref 1 (weight 0.2) → weakly_positive
	seedQueryInteraction(t, pool, qiSeed{
		GroundingEventID: 20, SourceRef: "ref-2-1", ClickKind: "mentioned", ClickWeight: 0.2, Position: 1,
	})
	// IDs 30-60: no interactions; refs at positions 1-3 in a 5-result
	// list become hard_negative, positions 4-5 become negative.
	// (Per the label classifier: max_weight=0 AND position<=3 AND
	// results_count>=5 → hard_negative; else position<=10 → negative.)

	srv := newTestServer(t, pool) // rebuilds proj_training_data_for_reranker

	// All rows.
	var all trainingPairsResponse
	getJSON(t, srv, "/telemetry/training-pairs?limit=100", &all)
	if all.PageSize != 100 {
		t.Errorf("page_size = %d", all.PageSize)
	}
	if len(all.Items) != 30 {
		t.Fatalf("all-items len = %d, want 30 (6 events × 5 refs each): %+v", len(all.Items), all.Items)
	}

	// Cover the full 5-value label_kind enum across the corpus.
	got := map[string]struct{}{}
	for _, item := range all.Items {
		got[item.LabelKind] = struct{}{}
	}
	for kind := range telemetryLabelKinds {
		if _, ok := got[kind]; !ok && kind != "unlabeled" {
			// unlabeled is the in-flight bucket; our seed has terminal
			// states only. The other 4 must appear at least once.
			t.Errorf("missing label_kind %q in corpus; got %v", kind, keys(got))
		}
	}

	// label_kind=positive filter narrows to the one positive pair.
	var positives trainingPairsResponse
	getJSON(t, srv, "/telemetry/training-pairs?label_kind=positive", &positives)
	if len(positives.Items) != 1 || positives.Items[0].LabelKind != "positive" {
		t.Errorf("positive filter = %+v", positives.Items)
	}

	// Repeated label_kind filter (OR within name).
	var positivesAndWeak trainingPairsResponse
	getJSON(t, srv, "/telemetry/training-pairs?label_kind=positive&label_kind=weakly_positive", &positivesAndWeak)
	if len(positivesAndWeak.Items) != 2 {
		t.Errorf("positive+weakly_positive filter len = %d, want 2: %+v", len(positivesAndWeak.Items), positivesAndWeak.Items)
	}

	// Free-text q filter on query_text.
	var qFiltered trainingPairsResponse
	getJSON(t, srv, "/telemetry/training-pairs?q=number+3", &qFiltered)
	if len(qFiltered.Items) != 5 { // event 30 has 5 refs
		t.Errorf("q filter len = %d, want 5", len(qFiltered.Items))
	}

	// Cursor pagination: page size 4 → first page items[3] is the
	// next_cursor.
	var page1 trainingPairsResponse
	getJSON(t, srv, "/telemetry/training-pairs?limit=4", &page1)
	if len(page1.Items) != 4 || page1.NextCursor == nil {
		t.Fatalf("page1 = %+v", page1)
	}
	cursor := *page1.NextCursor
	var page2 trainingPairsResponse
	getJSON(t, srv, fmt.Sprintf("/telemetry/training-pairs?limit=4&cursor=%d", cursor), &page2)
	if len(page2.Items) != 4 {
		t.Errorf("page2 len = %d, want 4", len(page2.Items))
	}
	// Pages disjoint (DESC order, exclusive cursor).
	for _, p1 := range page1.Items {
		for _, p2 := range page2.Items {
			if p1.TrainingID == p2.TrainingID {
				t.Errorf("page1 and page2 share training_id %d", p1.TrainingID)
			}
		}
	}
}

func TestTelemetryTrainingPairs_InvalidLabelKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	var out map[string]string
	code := getJSON(t, srv, "/telemetry/training-pairs?label_kind=clicked", &out)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(out["error"], "clicked") {
		t.Errorf("error = %q; want to name the bad value", out["error"])
	}
}

// --- training-pairs: stats --------------------------------------------

func TestTelemetryTrainingPairsStats_ZeroFilledDistributions(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedGroundingEvent(t, pool, geSeed{
		ID: 100, ProjectID: "p1",
		SourceRefs: `["r1","r2"]`, ResultsCount: 2,
	})
	seedQueryInteraction(t, pool, qiSeed{
		GroundingEventID: 100, SourceRef: "r1", ClickKind: "followed", ClickWeight: 1.0, Position: 1,
	})

	srv := newTestServer(t, pool)
	var resp trainingPairsStatsResponse
	code := getJSON(t, srv, "/telemetry/training-pairs/stats", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.TotalPairs != 2 {
		t.Errorf("total_pairs = %d, want 2", resp.TotalPairs)
	}
	// All 5 label_kind buckets present, zero-filled where absent.
	for kind := range telemetryLabelKinds {
		if _, ok := resp.ByLabelKind[kind]; !ok {
			t.Errorf("ByLabelKind missing key %q (zero-fill broken)", kind)
		}
	}
	// All 4 query_source buckets present, zero-filled where absent.
	for src := range telemetryQuerySources {
		if _, ok := resp.ByQuerySource[src]; !ok {
			t.Errorf("ByQuerySource missing key %q", src)
		}
	}
	if resp.ByLabelKind["positive"] != 1 {
		t.Errorf("positive count = %d, want 1", resp.ByLabelKind["positive"])
	}
	if resp.ByAction["vault_search"] != 2 {
		t.Errorf("vault_search action count = %d, want 2", resp.ByAction["vault_search"])
	}
}

// --- response shape pins ----------------------------------------------

// TestTelemetryTrajectory_ResponseFieldNames asserts the trajectory
// payload's JSON keys against the canonical names. The check is here
// rather than in events_test.go because this surface's wire shape is
// the cross-substrate seam the dashboard's <QueryTrajectoryView>
// binds to, and a silent rename breaks the consumer.
func TestTelemetryTrajectory_ResponseFieldNames(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedGroundingEvent(t, pool, geSeed{ID: 42, ProjectID: "p1", SourceRefs: "[]"})
	srv := newAuditServer(t, pool)

	resp, err := http.Get(srv.URL + "/telemetry/trajectories/42")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"query", "results", "interactions", "resolutions"} {
		if _, ok := raw[want]; !ok {
			t.Errorf("trajectory response missing key %q", want)
		}
	}

	var queryRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["query"], &queryRaw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"query_id", "span_id", "prompt_id", "session_id", "parent_span_id",
		"project_id", "action", "query_source", "query_text",
		"results_count", "created_at",
	} {
		if _, ok := queryRaw[want]; !ok {
			t.Errorf("query block missing key %q", want)
		}
	}
}

// TestTelemetryStats_ResponseFieldNames pins the stats envelope's
// three named axes. The browser banner reads these literally; rename
// here breaks the banner.
func TestTelemetryStats_ResponseFieldNames(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	resp, err := http.Get(srv.URL + "/telemetry/training-pairs/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"total_pairs", "by_label_kind", "by_query_source", "by_action"} {
		if _, ok := raw[want]; !ok {
			t.Errorf("stats response missing key %q", want)
		}
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
