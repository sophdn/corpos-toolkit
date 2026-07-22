package observehttp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// --- test fixture helpers --------------------------------------------

// seedEventInput is the per-event seed shape. Defaults are filled in
// seedEvent so most callers only set the dimensions their test cares
// about.
type seedEventInput struct {
	EventID         string
	Ts              string
	ActorKind       string
	ActorID         string
	Type            string
	EntityKind      string
	EntitySlug      string
	EntityProjectID string // empty string = NULL
	Payload         string // JSON; defaults to "{}"
	Rationale       string // empty string = NULL
	CausedByEventID string // empty = NULL
	RelatedEntities string // JSON array; defaults to "[]"
	SpanID          string
}

// seedEvent inserts one event row directly via INSERT. The events
// table's append-only trigger blocks UPDATE/DELETE; INSERT is the
// allowed write. We bypass the events.Emit helper because these tests
// exercise the HTTP read layer in isolation; coupling them to the emit
// path would re-test the substrate's emit discipline (covered by
// go/internal/events/events_test.go).
func seedEvent(t *testing.T, pool *db.Pool, in seedEventInput) {
	t.Helper()
	if in.EventID == "" {
		t.Fatal("seedEvent: EventID is required")
	}
	if in.Ts == "" {
		in.Ts = "2026-05-17T12:00:00.000Z"
	}
	if in.ActorKind == "" {
		in.ActorKind = "agent"
	}
	if in.ActorID == "" {
		in.ActorID = "claude-opus-4-7"
	}
	if in.Type == "" {
		in.Type = "BugReported"
	}
	if in.EntityKind == "" {
		in.EntityKind = "bug"
	}
	if in.EntitySlug == "" {
		in.EntitySlug = "test-slug"
	}
	if in.Payload == "" {
		in.Payload = "{}"
	}
	if in.RelatedEntities == "" {
		in.RelatedEntities = "[]"
	}
	if in.SpanID == "" {
		in.SpanID = "00000000-0000-4000-8000-000000000000"
	}

	var (
		projectIDArg any
		rationaleArg any
		causedByArg  any
	)
	if in.EntityProjectID == "" {
		projectIDArg = nil
	} else {
		projectIDArg = in.EntityProjectID
	}
	if in.Rationale == "" {
		rationaleArg = nil
	} else {
		rationaleArg = in.Rationale
	}
	if in.CausedByEventID == "" {
		causedByArg = nil
	} else {
		causedByArg = in.CausedByEventID
	}

	if _, err := pool.DB().Exec(`
		INSERT INTO events (
			event_id, ts, actor_kind, actor_id, type,
			entity_kind, entity_slug, entity_project_id,
			payload, rationale, caused_by_event_id, related_entities,
			span_id, schema_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		in.EventID, in.Ts, in.ActorKind, in.ActorID, in.Type,
		in.EntityKind, in.EntitySlug, projectIDArg,
		in.Payload, rationaleArg, causedByArg, in.RelatedEntities,
		in.SpanID,
	); err != nil {
		t.Fatalf("seedEvent %s: %v", in.EventID, err)
	}
}

// mkUUIDv7 mints a deterministic UUIDv7-shaped string with a synthetic
// monotonic suffix. Real UUIDv7s carry a 48-bit Unix-ms prefix; our
// test fixtures need lexicographic ordering only, not real wall-clock
// timing. The suffix becomes the sort key.
func mkUUIDv7(seq int) string {
	return fmt.Sprintf("0190f8a3-%04x-7000-8000-%012d", seq, seq)
}

// newAuditServer is a thin wrapper that builds the test server without
// the projection-refresh dance — events table reads don't need
// projections refreshed.
func newAuditServer(t *testing.T, pool *db.Pool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool}))
	t.Cleanup(srv.Close)
	return srv
}

// --- /events/list shape tests ----------------------------------------

func TestEventsList_ReturnsSeededRows(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	if code := getJSON(t, srv, "/events/list", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(resp.Items), resp.Items)
	}
	if resp.Items[0].EventID != mkUUIDv7(1) {
		t.Errorf("event_id = %q, want %q", resp.Items[0].EventID, mkUUIDv7(1))
	}
	if resp.Items[0].Actor.Kind != "agent" {
		t.Errorf("actor.kind = %q, want agent", resp.Items[0].Actor.Kind)
	}
	if resp.Items[0].SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", resp.Items[0].SchemaVersion)
	}
}

func TestEventsList_OrderedByEventIDDesc(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	for i := 1; i <= 5; i++ {
		seedEvent(t, pool, seedEventInput{
			EventID:         mkUUIDv7(i),
			EntityProjectID: "test",
			EntitySlug:      fmt.Sprintf("bug-%d", i),
		})
	}

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list", &resp)
	if len(resp.Items) != 5 {
		t.Fatalf("got %d items, want 5", len(resp.Items))
	}
	for i, evt := range resp.Items {
		expected := mkUUIDv7(5 - i)
		if evt.EventID != expected {
			t.Errorf("items[%d].event_id = %q, want %q", i, evt.EventID, expected)
		}
	}
}

func TestEventsList_FiltersByEntityKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), EntityKind: "bug", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(2), EntityKind: "task", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(3), EntityKind: "chain", EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list?entity_kind=task", &resp)
	if len(resp.Items) != 1 || resp.Items[0].Entity.Kind != "task" {
		t.Fatalf("entity_kind filter wrong: %+v", resp.Items)
	}
}

func TestEventsList_FiltersByType(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), Type: "BugReported", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(2), Type: "BugResolved", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(3), Type: "TaskCreated", EntityKind: "task", EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list?type=BugResolved", &resp)
	if len(resp.Items) != 1 || resp.Items[0].Type != "BugResolved" {
		t.Fatalf("type filter wrong: %+v", resp.Items)
	}
}

func TestEventsList_FiltersByMultipleTypes(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), Type: "BugReported", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(2), Type: "BugResolved", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(3), Type: "TaskCreated", EntityKind: "task", EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list?type=BugReported&type=BugResolved", &resp)
	if len(resp.Items) != 2 {
		t.Fatalf("multi-type filter got %d, want 2: %+v", len(resp.Items), resp.Items)
	}
	for _, evt := range resp.Items {
		if evt.Type != "BugReported" && evt.Type != "BugResolved" {
			t.Errorf("unexpected type %q in result", evt.Type)
		}
	}
}

func TestEventsList_FiltersByProject(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "alpha")
	seedProject(t, pool, "beta")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), EntityProjectID: "alpha"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(2), EntityProjectID: "beta"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list?project=alpha", &resp)
	if len(resp.Items) != 1 || resp.Items[0].Entity.ProjectID == nil || *resp.Items[0].Entity.ProjectID != "alpha" {
		t.Fatalf("project filter wrong: %+v", resp.Items)
	}
}

func TestEventsList_FiltersBySpanID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	const targetSpan = "11111111-1111-4111-8111-111111111111"
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), SpanID: targetSpan, EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(2), SpanID: "22222222-2222-4222-8222-222222222222", EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list?span_id="+targetSpan, &resp)
	if len(resp.Items) != 1 || resp.Items[0].SpanID != targetSpan {
		t.Fatalf("span_id filter wrong: %+v", resp.Items)
	}
}

func TestEventsList_FiltersByActor(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), ActorKind: "agent", ActorID: "claude-opus-4-7", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(2), ActorKind: "human", ActorID: "portal-anonymous-x", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(3), ActorKind: "system", ActorID: "cli-rebuild-projections", EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list?actor_kind=agent", &resp)
	if len(resp.Items) != 1 || resp.Items[0].Actor.Kind != "agent" {
		t.Fatalf("actor_kind filter wrong: %+v", resp.Items)
	}

	var resp2 eventListResponse
	getJSON(t, srv, "/events/list?actor_id=cli-rebuild-projections", &resp2)
	if len(resp2.Items) != 1 || resp2.Items[0].Actor.ID != "cli-rebuild-projections" {
		t.Fatalf("actor_id filter wrong: %+v", resp2.Items)
	}
}

func TestEventsList_FiltersBySinceUntil(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), Ts: "2026-05-15T00:00:00.000Z", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(2), Ts: "2026-05-16T00:00:00.000Z", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(3), Ts: "2026-05-17T00:00:00.000Z", EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list?since=2026-05-16T00:00:00.000Z&until=2026-05-17T00:00:00.000Z", &resp)
	if len(resp.Items) != 1 || resp.Items[0].Ts != "2026-05-16T00:00:00.000Z" {
		t.Fatalf("since/until filter wrong: %+v", resp.Items)
	}
}

func TestEventsList_FiltersByRationaleQ_CaseInsensitive(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{
		EventID:         mkUUIDv7(1),
		Rationale:       "Fixing the FORGE bug-title-omitted regression in the writer path.",
		EntityProjectID: "test",
	})
	seedEvent(t, pool, seedEventInput{
		EventID:         mkUUIDv7(2),
		Rationale:       "Closing a duplicate of an earlier task.",
		EntityProjectID: "test",
	})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list?q=regression", &resp)
	if len(resp.Items) != 1 || resp.Items[0].EventID != mkUUIDv7(1) {
		t.Fatalf("q filter wrong: %+v", resp.Items)
	}

	var resp2 eventListResponse
	getJSON(t, srv, "/events/list?q=DUPLICATE", &resp2) // case-insensitive
	if len(resp2.Items) != 1 || resp2.Items[0].EventID != mkUUIDv7(2) {
		t.Fatalf("case-insensitive q filter wrong: %+v", resp2.Items)
	}
}

func TestEventsList_RationaleQEscapesLikeMetachars(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{
		EventID:         mkUUIDv7(1),
		Rationale:       "Fixing the literal percent-sign 100% boundary.",
		EntityProjectID: "test",
	})
	seedEvent(t, pool, seedEventInput{
		EventID:         mkUUIDv7(2),
		Rationale:       "Unrelated text.",
		EntityProjectID: "test",
	})

	srv := newAuditServer(t, pool)
	// "100%" as a search term must NOT match because the % is escaped;
	// it should match only the literal "100%" substring.
	var resp eventListResponse
	getJSON(t, srv, "/events/list?q=100%25", &resp) // URL-encoded %
	if len(resp.Items) != 1 || resp.Items[0].EventID != mkUUIDv7(1) {
		t.Fatalf("escaped-LIKE q filter wrong: %+v", resp.Items)
	}

	// A search for the bare "0" character SHOULD match (no % wildcard
	// abuse possible because of the escape).
	var resp2 eventListResponse
	getJSON(t, srv, "/events/list?q=Unrelated", &resp2)
	if len(resp2.Items) != 1 || resp2.Items[0].EventID != mkUUIDv7(2) {
		t.Fatalf("plain q filter wrong: %+v", resp2.Items)
	}
}

func TestEventsList_LimitClamped(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	// limit=99999 should NOT error; the handler clamps to eventListLimitMax.
	resp, err := http.Get(srv.URL + "/events/list?limit=99999")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var parsed eventListResponse
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	if parsed.PageSize != eventListLimitMax {
		t.Errorf("page_size = %d, want %d", parsed.PageSize, eventListLimitMax)
	}
}

func TestEventsList_LimitDefaultsTo50(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/events/list", &resp)
	if resp.PageSize != eventListLimitDefault {
		t.Errorf("page_size = %d, want %d", resp.PageSize, eventListLimitDefault)
	}
}

// --- cursor pagination ----------------------------------------------

func TestEventsList_CursorPaginationAdvances(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	for i := 1; i <= 10; i++ {
		seedEvent(t, pool, seedEventInput{
			EventID:         mkUUIDv7(i),
			EntityProjectID: "test",
		})
	}

	srv := newAuditServer(t, pool)

	// First page: limit=3 → 3 items, next_cursor non-nil
	var page1 eventListResponse
	getJSON(t, srv, "/events/list?limit=3", &page1)
	if len(page1.Items) != 3 {
		t.Fatalf("page1 len = %d, want 3", len(page1.Items))
	}
	if page1.NextCursor == nil {
		t.Fatal("page1.next_cursor should not be nil")
	}
	// DESC order: page1 has events 10, 9, 8.
	if page1.Items[0].EventID != mkUUIDv7(10) || page1.Items[2].EventID != mkUUIDv7(8) {
		t.Fatalf("page1 order wrong: %v", eventIDs(page1.Items))
	}

	// Second page using next_cursor. Cursor is the visible tail of
	// page1 (event 8), so page2's filter `event_id < 8` includes the
	// lookahead row (event 7) first — no skip between pages.
	var page2 eventListResponse
	getJSON(t, srv, "/events/list?limit=3&cursor="+*page1.NextCursor, &page2)
	if len(page2.Items) != 3 {
		t.Fatalf("page2 len = %d, want 3", len(page2.Items))
	}
	// page2 = events 7, 6, 5
	if page2.Items[0].EventID != mkUUIDv7(7) || page2.Items[2].EventID != mkUUIDv7(5) {
		t.Fatalf("page2 order wrong: %v", eventIDs(page2.Items))
	}

	// page3 = events 4, 3, 2 (limit+1=4 read returns event 1 as the
	// lookahead, so next_cursor is non-nil and points at event 2).
	var page3 eventListResponse
	getJSON(t, srv, "/events/list?limit=3&cursor="+*page2.NextCursor, &page3)
	if len(page3.Items) != 3 || page3.NextCursor == nil {
		t.Fatalf("page3 should still have a cursor: %+v", page3)
	}
	if page3.Items[0].EventID != mkUUIDv7(4) || page3.Items[2].EventID != mkUUIDv7(2) {
		t.Fatalf("page3 order wrong: %v", eventIDs(page3.Items))
	}

	// page4 = just event 1, cursor exhausted.
	var page4 eventListResponse
	getJSON(t, srv, "/events/list?limit=3&cursor="+*page3.NextCursor, &page4)
	if len(page4.Items) != 1 || page4.NextCursor != nil {
		t.Fatalf("page4 should be the single remaining event with nil cursor: %+v", page4)
	}
	if page4.Items[0].EventID != mkUUIDv7(1) {
		t.Fatalf("page4 = %q, want %q", page4.Items[0].EventID, mkUUIDv7(1))
	}
}

func TestEventsList_CursorSurvivesMidPaginateInsert(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	for i := 1; i <= 5; i++ {
		seedEvent(t, pool, seedEventInput{
			EventID:         mkUUIDv7(i),
			EntityProjectID: "test",
		})
	}

	srv := newAuditServer(t, pool)

	var page1 eventListResponse
	getJSON(t, srv, "/events/list?limit=2", &page1)
	if len(page1.Items) != 2 || page1.NextCursor == nil {
		t.Fatalf("page1 setup wrong: %+v", page1)
	}

	// Insert a new event AFTER page1 was minted. Its event_id is
	// strictly greater than every existing one (UUIDv7 monotonicity).
	// The cursor pin should still exclude it from page2 because the
	// cursor's "event_id < ?" predicate uses the cursor value, which is
	// the tail of page1 (i.e. mkUUIDv7(4)). The new event (mkUUIDv7(99))
	// is greater than the cursor, so it's excluded from descending
	// pages until the operator restarts the paginate from the latest.
	seedEvent(t, pool, seedEventInput{
		EventID:         mkUUIDv7(99),
		EntityProjectID: "test",
	})

	var page2 eventListResponse
	getJSON(t, srv, "/events/list?limit=2&cursor="+*page1.NextCursor, &page2)
	// page2 should be events 3, 2 — NOT the new event 99.
	if len(page2.Items) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2.Items))
	}
	for _, evt := range page2.Items {
		if evt.EventID == mkUUIDv7(99) {
			t.Errorf("mid-paginate insert leaked into cursor page: %v", eventIDs(page2.Items))
		}
	}
}

// --- /events/{event_id} -----------------------------------------------

func TestEventsDetail_ReturnsFullEnvelope(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	const id = "0190f8a3-7b21-7c64-9d83-1f44a2b18cde"
	seedEvent(t, pool, seedEventInput{
		EventID:         id,
		Type:            "BugResolved",
		EntitySlug:      "forge-bug-title-omitted",
		EntityProjectID: "test",
		Payload:         `{"kind":"fixed","commit_sha":"abc1234"}`,
		Rationale:       "Root cause was the bug-schema title field defaulting to empty.",
		RelatedEntities: `[{"kind":"task","slug":"fix-it","project_id":"test"}]`,
		SpanID:          "9f8e7d6c-5b4a-3c2d-1e0f-aabbccddeeff",
	})

	srv := newAuditServer(t, pool)
	var resp eventDetailResponse
	if code := getJSON(t, srv, "/events/"+id, &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.EventID != id {
		t.Errorf("event_id = %q, want %q", resp.EventID, id)
	}
	if resp.Type != "BugResolved" {
		t.Errorf("type = %q, want BugResolved", resp.Type)
	}
	if resp.Rationale == nil || !strings.Contains(*resp.Rationale, "Root cause") {
		t.Errorf("rationale not preserved: %v", resp.Rationale)
	}
	// payload arrives as json.RawMessage; decode and check
	var payload map[string]any
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["kind"] != "fixed" {
		t.Errorf("payload kind = %v, want fixed", payload["kind"])
	}
	// related_entities arrives as json.RawMessage; decode and check
	var related []map[string]any
	if err := json.Unmarshal(resp.RelatedEntities, &related); err != nil {
		t.Fatalf("decode related_entities: %v", err)
	}
	if len(related) != 1 || related[0]["slug"] != "fix-it" {
		t.Errorf("related_entities = %v", related)
	}
	// related_queries: after query-telemetry-substrate TT2 (migration 037)
	// the table is always present, so the empty-table case returns []
	// (empty slice), not nil. The nil/[] distinction the dashboard
	// originally tracked collapses with TT2's landing.
	if resp.RelatedQueries == nil {
		t.Errorf("related_queries should be empty array (not nil) when no matching rows")
	}
	if len(resp.RelatedQueries) != 0 {
		t.Errorf("related_queries = %v, want empty", resp.RelatedQueries)
	}
}

func TestEventsDetail_NotFoundOnMissing(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	// Well-formed UUIDv7 but no such row.
	code := getJSON(t, srv, "/events/0190f8a3-7b21-7c64-9d83-1f44a2b18cde", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestEventsDetail_BadRequestOnGarbageID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	code := getJSON(t, srv, "/events/not-a-uuid", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// TestEventsDetail_EmitsEmptyArrayForRelatedQueriesWhenNoMatch
// (formerly _EmitsExplicitNullForRelatedQueriesWhenSiblingAbsent — renamed
// post query-telemetry-substrate TT2 because migration 037 makes the
// table always present, collapsing the nil/[] distinction to always-[]
// when no matching rows). The renamed test now pins the
// table-present-but-no-rows behavior to JSON [], not null.
func TestEventsDetail_EmitsEmptyArrayForRelatedQueriesWhenNoMatch(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	const id = "0190f8a3-7b21-7c64-9d83-1f44a2b18cde"
	seedEvent(t, pool, seedEventInput{EventID: id, EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	resp, err := http.Get(srv.URL + "/events/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	v, present := raw["related_queries"]
	if !present {
		t.Fatal("related_queries field missing")
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("related_queries = %v (%T), want []any (JSON empty array)", v, v)
	}
	if len(arr) != 0 {
		t.Errorf("related_queries = %v, want empty array", arr)
	}
}

// --- /entities/{kind}/{slug}/events ----------------------------------

func TestEntityEvents_ChronologicalOrder(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	// Same entity (bug/the-bug), three events in sequence
	for i := 1; i <= 3; i++ {
		seedEvent(t, pool, seedEventInput{
			EventID:         mkUUIDv7(i),
			EntityKind:      "bug",
			EntitySlug:      "the-bug",
			EntityProjectID: "test",
		})
	}
	// One unrelated event
	seedEvent(t, pool, seedEventInput{
		EventID:         mkUUIDv7(4),
		EntityKind:      "bug",
		EntitySlug:      "other-bug",
		EntityProjectID: "test",
	})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/entities/bug/the-bug/events?project=test", &resp)
	if len(resp.Items) != 3 {
		t.Fatalf("got %d items, want 3: %v", len(resp.Items), eventIDs(resp.Items))
	}
	// Ascending: 1, 2, 3
	for i, want := range []int{1, 2, 3} {
		if resp.Items[i].EventID != mkUUIDv7(want) {
			t.Errorf("items[%d].event_id = %q, want %q", i, resp.Items[i].EventID, mkUUIDv7(want))
		}
	}
}

func TestEntityEvents_RejectsUnknownKind(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newAuditServer(t, pool)
	code := getJSON(t, srv, "/entities/unicorn/foo/events", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestEntityEvents_CursorAscendingPagination(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	for i := 1; i <= 5; i++ {
		seedEvent(t, pool, seedEventInput{
			EventID:         mkUUIDv7(i),
			EntityKind:      "chain",
			EntitySlug:      "the-chain",
			EntityProjectID: "test",
		})
	}

	srv := newAuditServer(t, pool)

	var page1 eventListResponse
	getJSON(t, srv, "/entities/chain/the-chain/events?project=test&limit=2", &page1)
	if len(page1.Items) != 2 || page1.NextCursor == nil {
		t.Fatalf("page1: %+v", page1)
	}
	// Ascending: page1 = [1, 2]
	if page1.Items[0].EventID != mkUUIDv7(1) || page1.Items[1].EventID != mkUUIDv7(2) {
		t.Fatalf("page1 order wrong: %v", eventIDs(page1.Items))
	}

	// Cursor is the last visible row of page1 (event 2). Filter
	// `event_id > 2` includes event 3 first — no skip.
	var page2 eventListResponse
	getJSON(t, srv, "/entities/chain/the-chain/events?project=test&limit=2&cursor="+*page1.NextCursor, &page2)
	if len(page2.Items) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2.Items))
	}
	// page2 = [3, 4]
	if page2.Items[0].EventID != mkUUIDv7(3) || page2.Items[1].EventID != mkUUIDv7(4) {
		t.Fatalf("page2 order wrong: %v", eventIDs(page2.Items))
	}
}

func TestEntityEvents_FiltersHonoredOnTopOfEntityPin(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	// Same entity, different types
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(1), Type: "BugReported", EntityKind: "bug", EntitySlug: "the-bug", EntityProjectID: "test"})
	seedEvent(t, pool, seedEventInput{EventID: mkUUIDv7(2), Type: "BugResolved", EntityKind: "bug", EntitySlug: "the-bug", EntityProjectID: "test"})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/entities/bug/the-bug/events?project=test&type=BugResolved", &resp)
	if len(resp.Items) != 1 || resp.Items[0].Type != "BugResolved" {
		t.Fatalf("filter on top of entity pin wrong: %+v", resp.Items)
	}
}

// --- cross-substrate join (related_queries) --------------------------

func TestEventsDetail_RelatedQueriesPresentWhenSiblingTableExists(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	const id = "0190f8a3-7b21-7c64-9d83-1f44a2b18cde"
	const otherEventID = "0190f8a3-7b21-7c64-9d83-2222222222ff"
	seedEvent(t, pool, seedEventInput{EventID: id, EntityProjectID: "test"})
	// Second event so the multi-element write_event_ids array passes
	// migration 037's FK integrity trigger.
	seedEvent(t, pool, seedEventInput{EventID: otherEventID, EntityProjectID: "test"})

	// query_resolutions table now lives at migration 037
	// (query-telemetry-substrate TT2). Insert a canonical-schema row
	// referencing the seeded event_id; the handler's LIKE-based scan
	// against write_event_ids must find it.
	if _, err := pool.DB().Exec(`
		INSERT INTO query_resolutions (
			resolution_id, prompt_id, session_id, span_id,
			entity_kind, entity_slug, entity_project_id, outcome_kind,
			write_event_ids, grounding_event_ids, query_interaction_ids,
			detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', '[]', ?)`,
		"res-uuid-1", "prompt-1", "sess-1", "span-1",
		"bug", "forge-bug-title-omitted", "test", "resolved",
		`["`+id+`","`+otherEventID+`"]`,
		"2026-05-17T20:00:00Z",
	); err != nil {
		t.Fatal(err)
	}

	srv := newAuditServer(t, pool)
	var resp eventDetailResponse
	getJSON(t, srv, "/events/"+id, &resp)
	if resp.RelatedQueries == nil {
		t.Fatal("related_queries should be non-nil when sibling table has a matching row")
	}
	if len(resp.RelatedQueries) != 1 || resp.RelatedQueries[0].ResolutionID != "res-uuid-1" {
		t.Errorf("related_queries = %+v", resp.RelatedQueries)
	}
	if resp.RelatedQueries[0].EntityKind != "bug" || resp.RelatedQueries[0].OutcomeKind != "resolved" {
		t.Errorf("entity/outcome mismatch: %+v", resp.RelatedQueries[0])
	}
}

// Bug 1386: the related_queries wire shape is the cross-substrate seam
// the dashboard's AuditLedger drawer binds to via lib/auditEvents.ts.
// A silent rename on the Go side ({resolution_id, entity_kind,
// entity_slug, outcome_kind, prompt_id}) breaks the drawer with empty
// cells. This test pins the JSON keys raw — a renamed field fails here
// loudly instead of silently in the browser.
func TestEventsDetail_RelatedQueriesWireKeyPin(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	const id = "0190f8a3-7b21-7c64-9d83-1f44a2b18cde"
	seedEvent(t, pool, seedEventInput{EventID: id, EntityProjectID: "test"})

	if _, err := pool.DB().Exec(`
		INSERT INTO query_resolutions (
			resolution_id, prompt_id, session_id, span_id,
			entity_kind, entity_slug, entity_project_id, outcome_kind,
			write_event_ids, grounding_event_ids, query_interaction_ids,
			detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', '[]', ?)`,
		"res-uuid-pin", "prompt-1", "sess-1", "span-1",
		"bug", "drift-pin", "test", "resolved",
		`["`+id+`"]`,
		"2026-05-17T20:00:00Z",
	); err != nil {
		t.Fatal(err)
	}

	srv := newAuditServer(t, pool)
	httpResp, err := http.Get(srv.URL + "/events/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp.Body.Close()
	body, _ := io.ReadAll(httpResp.Body)

	var raw struct {
		RelatedQueries []map[string]any `json:"related_queries"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(raw.RelatedQueries) != 1 {
		t.Fatalf("related_queries len = %d, want 1; body=%s", len(raw.RelatedQueries), body)
	}
	row := raw.RelatedQueries[0]
	for _, want := range []string{
		"resolution_id", "entity_kind", "entity_slug", "outcome_kind", "prompt_id",
	} {
		if _, ok := row[want]; !ok {
			t.Errorf("related_queries[0] missing key %q (canonical Go shape drift); got keys %v",
				want, mapKeys(row))
		}
	}
	// Negative pin: the obsolete sketch shape MUST NOT appear. If the Go
	// side ever flips back to interaction_id / query / source_type, the
	// dashboard's vestigial sketch becomes live again and the drawer
	// breaks silently.
	for _, banned := range []string{"interaction_id", "query", "source_type"} {
		if _, ok := row[banned]; ok {
			t.Errorf("related_queries[0] has banned key %q (pre-bug-1386 sketch shape)", banned)
		}
	}
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEventsDetail_RelatedQueriesEmptyArrayWhenSiblingTablePresentButNoMatch(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	const id = "0190f8a3-7b21-7c64-9d83-1f44a2b18cde"
	seedEvent(t, pool, seedEventInput{EventID: id, EntityProjectID: "test"})

	// Table exists from migration 037; no row references our event_id.
	srv := newAuditServer(t, pool)
	resp, err := http.Get(srv.URL + "/events/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	// related_queries should be [] (empty array). Post-TT2 the never-null
	// invariant holds — the table is always present.
	v, present := raw["related_queries"]
	if !present {
		t.Fatal("related_queries field missing")
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("related_queries = %v (%T), want []any", v, v)
	}
	if len(arr) != 0 {
		t.Errorf("related_queries = %v, want empty array", arr)
	}
}

// --- shape pins ------------------------------------------------------

func TestEventsList_NullsRenderAsExplicitNull(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{
		EventID: mkUUIDv7(1),
		// No project, no rationale, no caused_by, no related_entities.
	})

	srv := newAuditServer(t, pool)
	resp, err := http.Get(srv.URL + "/events/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(raw.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(raw.Items))
	}
	item := raw.Items[0]
	entity, ok := item["entity"].(map[string]any)
	if !ok {
		t.Fatalf("entity = %v", item["entity"])
	}
	if v, present := entity["project_id"]; !present || v != nil {
		t.Errorf("entity.project_id should be JSON null, got present=%v val=%v", present, v)
	}
	if v, present := item["rationale"]; !present || v != nil {
		t.Errorf("rationale should be JSON null, got present=%v val=%v", present, v)
	}
	if v, present := item["caused_by_event_id"]; !present || v != nil {
		t.Errorf("caused_by_event_id should be JSON null, got present=%v val=%v", present, v)
	}
}

func TestEventsList_CacheHeadersByCursorState(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	for i := 1; i <= 3; i++ {
		seedEvent(t, pool, seedEventInput{
			EventID:         mkUUIDv7(i),
			EntityProjectID: "test",
		})
	}

	srv := newAuditServer(t, pool)

	// First page (no cursor) — must be no-cache (latest page may grow).
	resp1, err := http.Get(srv.URL + "/events/list?limit=2")
	if err != nil {
		t.Fatalf("GET page1: %v", err)
	}
	defer resp1.Body.Close()
	if got := resp1.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("first page Cache-Control = %q, want no-cache", got)
	}

	var page1 eventListResponse
	if err := json.NewDecoder(resp1.Body).Decode(&page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if page1.NextCursor == nil {
		t.Fatal("page1 cursor nil")
	}

	// Cursored page — content-stable, may cache.
	resp2, err := http.Get(srv.URL + "/events/list?limit=2&cursor=" + *page1.NextCursor)
	if err != nil {
		t.Fatalf("GET page2: %v", err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get("Cache-Control"); !strings.HasPrefix(got, "public") {
		t.Errorf("cursored page Cache-Control = %q, want public, max-age=300", got)
	}
}

// --- helpers ---------------------------------------------------------

func eventIDs(items []eventRow) []string {
	out := make([]string, len(items))
	for i, e := range items {
		out[i] = e.EventID
	}
	return out
}
