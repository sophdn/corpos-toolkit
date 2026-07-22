package knowledge

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/testutil"
)

func qiParams(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecordQueryInteraction_Records(t *testing.T) {
	pool := testutil.NewTestDB(t)
	geID := testutil.SeedGroundingEvent(t, pool,
		testutil.WithGroundingSpan("span-rec"),
		testutil.WithGroundingSourceRefs(`["vault/learnings/x.md"]`),
	)

	res, err := HandleRecordQueryInteraction(context.Background(), Deps{Pool: pool}, qiParams(t, map[string]any{
		"span_id":    "span-rec",
		"source_ref": "vault/learnings/x.md",
		"click_kind": "followed",
		"session_id": "sess-1",
		"position":   2,
	}))
	if err != nil {
		t.Fatalf("HandleRecordQueryInteraction: %v", err)
	}
	if !res.Recorded || res.Error != "" {
		t.Fatalf("expected recorded, got %+v", res)
	}
	if res.GroundingEventID != geID {
		t.Errorf("GroundingEventID = %d, want %d", res.GroundingEventID, geID)
	}
	if res.InteractionID == 0 {
		t.Errorf("expected a non-zero interaction id")
	}
	// The query_interactions row landed for this grounding event.
	var n int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM query_interactions WHERE grounding_event_id = ? AND click_kind = 'followed'`, geID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("want 1 query_interactions row, got %d", n)
	}
}

func TestRecordQueryInteraction_MissingParams(t *testing.T) {
	pool := testutil.NewTestDB(t)
	res, err := HandleRecordQueryInteraction(context.Background(), Deps{Pool: pool}, qiParams(t, map[string]any{
		"span_id": "x", // missing source_ref / click_kind / session_id
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == "" || res.Recorded {
		t.Errorf("expected a required-params error, got %+v", res)
	}
}

func TestRecordQueryInteraction_NoGroundingEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	res, err := HandleRecordQueryInteraction(context.Background(), Deps{Pool: pool}, qiParams(t, map[string]any{
		"span_id":    "no-such-span",
		"source_ref": "vault/x.md",
		"click_kind": "followed",
		"session_id": "sess-1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Recorded || res.Error == "" {
		t.Errorf("expected soft no-op for unknown span, got %+v", res)
	}
}

func TestRecordQueryInteraction_InvalidClickKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	testutil.SeedGroundingEvent(t, pool, testutil.WithGroundingSpan("span-bad"))
	res, err := HandleRecordQueryInteraction(context.Background(), Deps{Pool: pool}, qiParams(t, map[string]any{
		"span_id":    "span-bad",
		"source_ref": "vault/x.md",
		"click_kind": "teleported",
		"session_id": "sess-1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Recorded || res.Error == "" {
		t.Errorf("expected an emit error for invalid click_kind, got %+v", res)
	}
}
