package observehttp

import (
	"fmt"
	"net/http"
	"sort"
	"testing"

	"toolkit/internal/testutil"
)

// Bug: audit-ledger-orders-by-event-id-buries-real-events-behind-
// synthetic-backfill.
//
// Pre-fix /events/list ordered by `event_id DESC`, which assumes every
// event_id is a ULID-shape string (`019xxx-…`) sortable by chronological
// embedding. Several backfill / migration programs author synthetic IDs
// — `started-<uuid>` from the benchmark backfill, `completed-<uuid>`
// from its sibling, and conceivably anything with a non-`0..9a..f`
// leading byte — which lex-sort AFTER every ULID and so dominate the
// top of an event_id DESC listing. User-visible symptom: the dashboard
// audit ledger appears frozen at the most-recent real (ULID-ID'd) event
// from BEFORE the backfill ran, while every fresh event since looks
// missing.
//
// These regression tests pin the (ts, event_id) ordering directly. If
// a future refactor reverts to event_id-only ordering, these tests fail
// loudly with a payload pointing at the bug-name above.

// TestEventsList_OrderedByTsDesc_SyntheticIdsDoNotBuryRecentEvents is
// the bug-shape regression: seed three event-id shapes
// (ULID-old, synthetic-prefix-MIDDLE-aged, ULID-NEWEST) with timestamps
// that contradict the lexicographic ordering of event_ids. Assert the
// /events/list response is in `ts DESC` order, NOT event_id DESC.
//
// Specifically:
//   - synth-1 ts=2026-05-15 (oldest), event_id=`started-zz...` (lex-greatest)
//   - ulid-2 ts=2026-05-17 (middle),  event_id=`019aaa...`     (lex-mid)
//   - ulid-3 ts=2026-05-23 (newest),  event_id=`019bbb...`     (lex-greater)
//
// Under the broken event_id DESC ordering, synth-1 would come first
// (its `started-zz` lex-dominates both ULIDs). The fix is `ORDER BY ts
// DESC, event_id DESC`; under that order ulid-3 (newest ts) must be
// first, and synth-1 (oldest ts) must be last.
func TestEventsList_OrderedByTsDesc_SyntheticIdsDoNotBuryRecentEvents(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")

	seedEvent(t, pool, seedEventInput{
		EventID:         "started-zzzzzzzz-4752-4f6d-ba3e-fbefd7ab4ad3",
		Ts:              "2026-05-15T08:00:00.000Z",
		Type:            "BenchmarkRunStarted",
		EntityKind:      "benchmark_run",
		EntitySlug:      "synth-1",
		EntityProjectID: "test",
	})
	seedEvent(t, pool, seedEventInput{
		EventID:         "019aaaaa-0000-7000-8000-000000000002",
		Ts:              "2026-05-17T12:00:00.000Z",
		Type:            "BugReported",
		EntityKind:      "bug",
		EntitySlug:      "ulid-2",
		EntityProjectID: "test",
	})
	seedEvent(t, pool, seedEventInput{
		EventID:         "019bbbbb-0000-7000-8000-000000000003",
		Ts:              "2026-05-23T15:34:50.183Z",
		Type:            "TaskCompleted",
		EntityKind:      "task",
		EntitySlug:      "ulid-3",
		EntityProjectID: "test",
	})

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	if code := getJSON(t, srv, "/events/list", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(resp.Items))
	}
	wantSlugs := []string{"ulid-3", "ulid-2", "synth-1"}
	for i, want := range wantSlugs {
		if got := resp.Items[i].Entity.Slug; got != want {
			t.Errorf("items[%d].entity.slug = %q, want %q (ts-DESC ordering must beat event_id-DESC; "+
				"bug: audit-ledger-orders-by-event-id-buries-real-events-behind-synthetic-backfill)",
				i, got, want)
		}
	}
	// Sanity: the broken ordering would have put synth-1 first (its
	// `started-zz` lex-dominates every ULID). Asserting the explicit
	// inverse is what makes this regression test useful — flipping the
	// SQL to event_id-only would fail this test on the very first row.
	if resp.Items[0].EventID == "started-zzzzzzzz-4752-4f6d-ba3e-fbefd7ab4ad3" {
		t.Errorf("synthetic-prefix event sorted to the top — bug regressed")
	}
}

// TestEntityEvents_OrderedByTsAsc_SyntheticIdsDoNotJumpToHead is the
// entity-timeline analog (ASC). Same ID/ts contradiction, asserting the
// fix carries to /entities/{kind}/{slug}/events. All three rows are
// scoped to the same entity so the endpoint returns them all.
func TestEntityEvents_OrderedByTsAsc_SyntheticIdsDoNotJumpToHead(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")

	for _, in := range []seedEventInput{
		{
			EventID:         "started-zzzzzzzz-4752-4f6d-ba3e-fbefd7ab4ad3",
			Ts:              "2026-05-15T08:00:00.000Z",
			Type:            "BenchmarkRunStarted",
			EntityKind:      "task",
			EntitySlug:      "shared",
			EntityProjectID: "test",
		},
		{
			EventID:         "019aaaaa-0000-7000-8000-000000000002",
			Ts:              "2026-05-17T12:00:00.000Z",
			Type:            "TaskCreated",
			EntityKind:      "task",
			EntitySlug:      "shared",
			EntityProjectID: "test",
		},
		{
			EventID:         "019bbbbb-0000-7000-8000-000000000003",
			Ts:              "2026-05-23T15:34:50.183Z",
			Type:            "TaskCompleted",
			EntityKind:      "task",
			EntitySlug:      "shared",
			EntityProjectID: "test",
		},
	} {
		seedEvent(t, pool, in)
	}

	srv := newAuditServer(t, pool)
	var resp eventListResponse
	getJSON(t, srv, "/entities/task/shared/events?project=test", &resp)
	if len(resp.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(resp.Items))
	}
	wantTypes := []string{"BenchmarkRunStarted", "TaskCreated", "TaskCompleted"}
	for i, want := range wantTypes {
		if got := resp.Items[i].Type; got != want {
			t.Errorf("items[%d].type = %q, want %q (ts-ASC ordering must beat event_id-ASC)",
				i, got, want)
		}
	}
}

// TestEventsList_CursorPagination_AcrossMixedIdShapes paginates a fully-
// mixed corpus (ULIDs interleaved with synthetic prefixes, timestamps
// scrambled so neither ID lex order nor seed order matches ts order)
// and asserts: every row is surfaced exactly once, the response order
// is the canonical ts-DESC order, and no row is skipped at a cursor
// boundary.
//
// The bug's failure mode under cursor pagination was twofold: the
// initial page already showed synthetic events at the top (covered by
// the test above), AND the cursor's `event_id < ?` predicate could
// either skip rows (when crossing into the ULID range from a
// synthetic-prefix page) or duplicate them (when the cursor's event_id
// happened to equal a row's). Walking the full corpus is the
// surface-level integration test that catches both classes.
func TestEventsList_CursorPagination_AcrossMixedIdShapes(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")

	// Seed 7 events. Slug embeds the desired ts-DESC rank for assertion;
	// id-shape alternates between ULID and synthetic so the pre-fix
	// event_id ordering disagrees with the ts ordering at every step.
	type seed struct {
		ts, eventID, slug string
	}
	seeds := []seed{
		// Newest → oldest by ts:
		{"2026-05-23T15:00:00.000Z", "019eeeee-0000-7000-8000-000000000007", "rank-1-ulid"},
		{"2026-05-22T15:00:00.000Z", "started-zzzz-0000-4000-8000-000000000006", "rank-2-synth"},
		{"2026-05-21T15:00:00.000Z", "019dddd0-0000-7000-8000-000000000005", "rank-3-ulid"},
		{"2026-05-20T15:00:00.000Z", "completed-aaaa-0000-4000-8000-00000004", "rank-4-synth"},
		{"2026-05-19T15:00:00.000Z", "019ccccc-0000-7000-8000-000000000003", "rank-5-ulid"},
		{"2026-05-18T15:00:00.000Z", "started-bbbb-0000-4000-8000-000000000002", "rank-6-synth"},
		{"2026-05-17T15:00:00.000Z", "019aaaaa-0000-7000-8000-000000000001", "rank-7-ulid"},
	}
	// Insert in a deliberately-shuffled order so DB row insertion order
	// doesn't accidentally produce the right answer.
	insertOrder := []int{3, 0, 5, 1, 6, 2, 4}
	for _, i := range insertOrder {
		s := seeds[i]
		seedEvent(t, pool, seedEventInput{
			EventID:         s.eventID,
			Ts:              s.ts,
			EntityKind:      "bug",
			EntitySlug:      s.slug,
			EntityProjectID: "test",
		})
	}

	srv := newAuditServer(t, pool)

	// Walk all pages with limit=2 → expect ceil(7/2) = 4 pages: 2, 2, 2, 1.
	var allSlugs []string
	cursor := ""
	for page := 0; page < 10; page++ {
		url := "/events/list?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		var resp eventListResponse
		getJSON(t, srv, url, &resp)
		for _, evt := range resp.Items {
			allSlugs = append(allSlugs, evt.Entity.Slug)
		}
		if resp.NextCursor == nil {
			break
		}
		cursor = *resp.NextCursor
	}

	wantOrder := []string{
		"rank-1-ulid", "rank-2-synth", "rank-3-ulid", "rank-4-synth",
		"rank-5-ulid", "rank-6-synth", "rank-7-ulid",
	}
	if len(allSlugs) != len(wantOrder) {
		t.Fatalf("paginated row count = %d, want %d. got: %v",
			len(allSlugs), len(wantOrder), allSlugs)
	}
	for i, want := range wantOrder {
		if allSlugs[i] != want {
			t.Errorf("position %d: got %q, want %q (full walk = %v)",
				i, allSlugs[i], want, allSlugs)
		}
	}
	// Defensive: no duplicates across pages (catches a cursor-equality
	// regression where the cursor's row leaks onto the next page).
	seen := make(map[string]int)
	for _, s := range allSlugs {
		seen[s]++
	}
	dups := []string{}
	for s, n := range seen {
		if n > 1 {
			dups = append(dups, fmt.Sprintf("%s=%d", s, n))
		}
	}
	sort.Strings(dups)
	if len(dups) > 0 {
		t.Errorf("cursor leaked rows across pages: %v", dups)
	}
}

// TestEventsList_CursorBoundaryAtSameTimestamp covers the tiebreaker
// path: two events sharing the same ts (typical for same-tx emits like
// task_complete + auto-fire ArcReviewListenerFired) must paginate
// cleanly across the cursor boundary that lands on their shared
// timestamp. The tuple-less predicate `(ts < ? OR (ts = ? AND event_id
// < ?))` does the work; this test pins it directly.
func TestEventsList_CursorBoundaryAtSameTimestamp(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")

	const sharedTs = "2026-05-23T15:34:50.183Z"
	seedEvent(t, pool, seedEventInput{
		EventID: "019bbbbb-0000-7000-8000-000000000001", Ts: sharedTs,
		EntitySlug: "same-ts-low-id", EntityProjectID: "test",
	})
	seedEvent(t, pool, seedEventInput{
		EventID: "019bbbbb-0000-7000-8000-000000000002", Ts: sharedTs,
		EntitySlug: "same-ts-high-id", EntityProjectID: "test",
	})
	seedEvent(t, pool, seedEventInput{
		EventID: "019aaaaa-0000-7000-8000-000000000003", Ts: "2026-05-22T15:34:50.183Z",
		EntitySlug: "earlier-ts", EntityProjectID: "test",
	})

	srv := newAuditServer(t, pool)

	// limit=1 forces a cursor between same-ts-high-id and
	// same-ts-low-id, then between same-ts-low-id and earlier-ts.
	var seenSlugs []string
	cursor := ""
	for page := 0; page < 5; page++ {
		url := "/events/list?limit=1"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		var resp eventListResponse
		getJSON(t, srv, url, &resp)
		for _, evt := range resp.Items {
			seenSlugs = append(seenSlugs, evt.Entity.Slug)
		}
		if resp.NextCursor == nil {
			break
		}
		cursor = *resp.NextCursor
	}
	want := []string{"same-ts-high-id", "same-ts-low-id", "earlier-ts"}
	if len(seenSlugs) != len(want) {
		t.Fatalf("paginated count = %d, want %d: got %v", len(seenSlugs), len(want), seenSlugs)
	}
	for i, w := range want {
		if seenSlugs[i] != w {
			t.Errorf("position %d: got %q, want %q (full walk = %v)", i, seenSlugs[i], w, seenSlugs)
		}
	}
}

// TestEventsList_InvalidCursor_Rejects400 pins the cursor decode-error
// path so a malformed deep-link doesn't silently degrade to "return
// page 1 ignoring cursor" (which would surface a different bug shape
// to the user — duplicated rows). The handler must reject with 400 so
// the frontend can fall back to refetching from the latest.
func TestEventsList_InvalidCursor_Rejects400(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	seedEvent(t, pool, seedEventInput{
		EventID: "019aaaaa-0000-7000-8000-000000000001", EntityProjectID: "test",
	})

	srv := newAuditServer(t, pool)
	// Cursors without a `|` separator (pre-fix format, or any garbage)
	// must reject with 400.
	resp, err := http.Get(srv.URL + "/events/list?cursor=just-an-event-id-no-delimiter")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for malformed cursor", resp.StatusCode)
	}
}
