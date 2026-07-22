package testutil_test

import (
	"testing"

	"toolkit/internal/testutil"
)

func TestSeedGroundingEvent_DefaultsLand(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := testutil.SeedGroundingEvent(t, pool)
	if id <= 0 {
		t.Fatalf("expected positive id; got %d", id)
	}
	var project, session, action, querySource string
	if err := pool.DB().QueryRow(
		`SELECT project_id, session_id, action, query_source FROM grounding_events WHERE id = ?`,
		id,
	).Scan(&project, &session, &action, &querySource); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if project != "mcp-servers" {
		t.Errorf("project default mismatch: %q", project)
	}
	if session != "test-session" {
		t.Errorf("session default mismatch: %q", session)
	}
	if action != "test_action" {
		t.Errorf("action default mismatch: %q", action)
	}
	if querySource != "agent_initiated" {
		t.Errorf("query_source default mismatch: %q", querySource)
	}
}

func TestSeedGroundingEvent_OverridesApply(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := testutil.SeedGroundingEvent(t, pool,
		testutil.WithGroundingProject("other-project"),
		testutil.WithGroundingSession("custom-session"),
		testutil.WithGroundingAction("vault_search"),
		testutil.WithGroundingSpan("custom-span"),
		testutil.WithGroundingQuerySource("reference_resolution"),
		testutil.WithGroundingQueryText("look at chain X"),
		testutil.WithGroundingResultsCount(3),
	)
	var (
		project, session, action, spanID, querySource string
		queryText                                     *string
		resultsCount                                  int
	)
	if err := pool.DB().QueryRow(
		`SELECT project_id, session_id, action, span_id, query_source, query_text, results_count FROM grounding_events WHERE id = ?`,
		id,
	).Scan(&project, &session, &action, &spanID, &querySource, &queryText, &resultsCount); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if project != "other-project" || session != "custom-session" || action != "vault_search" || spanID != "custom-span" || querySource != "reference_resolution" || resultsCount != 3 {
		t.Errorf("override mismatch: %+v / %+v / %+v / %+v / %+v / %d", project, session, action, spanID, querySource, resultsCount)
	}
	if queryText == nil || *queryText != "look at chain X" {
		t.Errorf("query_text override didn't land: %v", queryText)
	}
}

func TestSeedQueryInteraction_DefaultsLandAndFK(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := testutil.SeedGroundingEvent(t, pool)
	qiID := testutil.SeedQueryInteraction(t, pool, geID)
	if qiID <= 0 {
		t.Fatalf("expected positive qi id; got %d", qiID)
	}
	var (
		parent      int64
		sourceRef   string
		clickKind   string
		clickWeight float64
		spanID      string
		sessionID   string
		wasInjected int
	)
	if err := pool.DB().QueryRow(
		`SELECT grounding_event_id, source_ref, click_kind, click_weight, span_id, session_id, was_injected FROM query_interactions WHERE id = ?`,
		qiID,
	).Scan(&parent, &sourceRef, &clickKind, &clickWeight, &spanID, &sessionID, &wasInjected); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if parent != geID {
		t.Errorf("FK mismatch: %d vs %d", parent, geID)
	}
	if sourceRef != "test/source-ref" || clickKind != "cited" || clickWeight != 1.0 || sessionID != "test-session" || wasInjected != 0 {
		t.Errorf("defaults mismatch: %s / %s / %v / %s / %d", sourceRef, clickKind, clickWeight, sessionID, wasInjected)
	}
	if spanID == "" {
		t.Errorf("span_id default must auto-generate; got empty")
	}
}

func TestSeedQueryInteraction_OverridesApply(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := testutil.SeedGroundingEvent(t, pool)
	qiID := testutil.SeedQueryInteraction(t, pool, geID,
		testutil.WithQISourceRef("vault:reference/foo.md"),
		testutil.WithQIClickKind("followed"),
		testutil.WithQIClickWeight(0.5),
		testutil.WithQISpan("explicit-span"),
		testutil.WithQISession("custom-session"),
		testutil.WithQIPosition(7),
		testutil.WithQIWasInjected(true),
	)
	var (
		sourceRef   string
		clickKind   string
		clickWeight float64
		spanID      string
		sessionID   string
		position    *int64
		wasInjected int
	)
	if err := pool.DB().QueryRow(
		`SELECT source_ref, click_kind, click_weight, span_id, session_id, position, was_injected FROM query_interactions WHERE id = ?`,
		qiID,
	).Scan(&sourceRef, &clickKind, &clickWeight, &spanID, &sessionID, &position, &wasInjected); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sourceRef != "vault:reference/foo.md" || clickKind != "followed" || clickWeight != 0.5 || spanID != "explicit-span" || sessionID != "custom-session" || wasInjected != 1 {
		t.Errorf("override mismatch: %+v / %+v / %+v / %+v / %+v / %d", sourceRef, clickKind, clickWeight, spanID, sessionID, wasInjected)
	}
	if position == nil || *position != 7 {
		t.Errorf("position override didn't land: %v", position)
	}
}

// Two seeded interactions against the same span must NOT collide on the
// (span_id, source_ref, click_kind) unique index when one of the three
// varies — proves the auto-vary defaults work for back-to-back seeding.
func TestSeedQueryInteraction_BackToBackDoesNotCollide(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := testutil.SeedGroundingEvent(t, pool)
	_ = testutil.SeedQueryInteraction(t, pool, geID)
	_ = testutil.SeedQueryInteraction(t, pool, geID) // would collide if span_id were a fixed default
	var n int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM query_interactions WHERE grounding_event_id = ?`, geID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 distinct rows; got %d", n)
	}
}
