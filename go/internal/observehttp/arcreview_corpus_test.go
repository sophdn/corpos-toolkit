package observehttp

import (
	"encoding/json"
	"net/http"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// arcCorpusRow is a minimal arcreview_snapshot_corpus fixture. Caps are
// pinned to the production defaults (20 turns / 4000 tokens) so bucket
// boundaries match the live shape.
type arcCorpusRow struct {
	eventID   string
	sessionID string
	source    string
	msgCount  int
	estTokens int
	truncated int
}

func seedArcCorpusRow(t *testing.T, pool *db.Pool, r arcCorpusRow) {
	t.Helper()
	if _, err := pool.DB().Exec(`
		INSERT INTO arcreview_snapshot_corpus
			(event_id, session_id, fire_ts, messages_json, message_count,
			 estimated_tokens, truncated, max_turns, max_tokens, source, schema_version)
		VALUES (?, ?, '2026-05-20T00:00:00.000Z',
			'[{"role":"user","content":"hi"}]', ?, ?, ?, 20, 4000, ?, 1)`,
		r.eventID, r.sessionID, r.msgCount, r.estTokens, r.truncated, r.source,
	); err != nil {
		t.Fatalf("seedArcCorpusRow %s: %v", r.eventID, err)
	}
}

// arcFiringEvent seeds an ArcCloseFilingReviewed event with the given
// decisions array + arc_summary so the join-completeness count can be
// exercised.
func seedArcFiringEvent(t *testing.T, pool *db.Pool, eventID, decisionsJSON, arcSummary string) {
	t.Helper()
	payload := `{"decisions":` + decisionsJSON + `,"arc_summary":` + quote(arcSummary) + `}`
	seedEvent(t, pool, seedEventInput{
		EventID:    eventID,
		Type:       "ArcCloseFilingReviewed",
		EntityKind: "session",
		EntitySlug: "s",
		Payload:    payload,
	})
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestArcCorpusStats_AggregatesSourceTruncationCompletenessAndBuckets(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Four events: two carry decisions+arc_summary (tuple-complete), one is
	// nothing_to_file (empty decisions), one has decisions but no arc_summary.
	seedArcFiringEvent(t, pool, "e1", `[{"action":"forge_vault_note"}]`, "summary one")
	seedArcFiringEvent(t, pool, "e2", `[]`, "summary two")           // empty decisions
	seedArcFiringEvent(t, pool, "e3", `[{"action":"file_bug"}]`, "") // empty arc_summary
	seedArcFiringEvent(t, pool, "e4", `[{"action":"forge_suggestion"}]`, "summary four")

	// Rows: one live (s1), three recovered (s2 twice, s3). Truncated on A,B.
	seedArcCorpusRow(t, pool, arcCorpusRow{eventID: "e1", sessionID: "s1", source: "live", msgCount: 20, estTokens: 3500, truncated: 1})
	seedArcCorpusRow(t, pool, arcCorpusRow{eventID: "e2", sessionID: "s2", source: "recovered", msgCount: 12, estTokens: 2957, truncated: 1})
	seedArcCorpusRow(t, pool, arcCorpusRow{eventID: "e3", sessionID: "s2", source: "recovered", msgCount: 3, estTokens: 1278, truncated: 0})
	seedArcCorpusRow(t, pool, arcCorpusRow{eventID: "e4", sessionID: "s3", source: "recovered", msgCount: 5, estTokens: 900, truncated: 0})

	srv := newTestServer(t, pool)
	var resp ArcCorpusStatsResponse
	if code := getJSON(t, srv, "/telemetry/snapshot-corpus/stats", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if resp.TotalRows != 4 {
		t.Errorf("total_rows = %d, want 4", resp.TotalRows)
	}
	if resp.DistinctSessions != 3 {
		t.Errorf("distinct_sessions = %d, want 3 (s1,s2,s3)", resp.DistinctSessions)
	}
	if resp.BySource["live"] != 1 || resp.BySource["recovered"] != 3 {
		t.Errorf("by_source = %+v, want live=1 recovered=3", resp.BySource)
	}
	if resp.TruncatedRows != 2 {
		t.Errorf("truncated_rows = %d, want 2 (e1,e2)", resp.TruncatedRows)
	}
	// e1 + e4 carry both decisions and arc_summary; e2 (empty decisions) and
	// e3 (empty arc_summary) do not.
	if resp.TupleCompleteRows != 2 {
		t.Errorf("tuple_complete_rows = %d, want 2 (e1,e4)", resp.TupleCompleteRows)
	}

	// Message-count buckets: e3(3)+e4(5)→"1-5"; e2(12)→"11-15"; e1(20)→"20".
	wantMsg := map[string]int{"1-5": 2, "6-10": 0, "11-15": 1, "16-19": 0, "20": 1}
	assertBuckets(t, "message_count", resp.MessageCount, messageCountBucketOrder, wantMsg)

	// Token buckets: e4(900)→"<1000"; e3(1278)→"1000-1999"; e2(2957)→
	// "2000-2999"; e1(3500)→"3000-3999".
	wantTok := map[string]int{"<1000": 1, "1000-1999": 1, "2000-2999": 1, "3000-3999": 1, "4000+": 0}
	assertBuckets(t, "estimated_tokens", resp.EstimatedTokens, estimatedTokensBucketOrder, wantTok)
}

func TestArcCorpusStats_EmptyCorpusZeroFills(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)

	var resp ArcCorpusStatsResponse
	if code := getJSON(t, srv, "/telemetry/snapshot-corpus/stats", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.TotalRows != 0 {
		t.Errorf("total_rows = %d, want 0", resp.TotalRows)
	}
	// by_source must still carry both keys (zero-filled) so the chart
	// renders both cells on an empty corpus.
	if _, ok := resp.BySource["live"]; !ok {
		t.Error("by_source missing zero-filled 'live'")
	}
	if _, ok := resp.BySource["recovered"]; !ok {
		t.Error("by_source missing zero-filled 'recovered'")
	}
	// Both bucket sets render their full ordered length even with no data.
	if len(resp.MessageCount) != len(messageCountBucketOrder) {
		t.Errorf("message_count buckets len = %d, want %d", len(resp.MessageCount), len(messageCountBucketOrder))
	}
	if len(resp.EstimatedTokens) != len(estimatedTokensBucketOrder) {
		t.Errorf("estimated_tokens buckets len = %d, want %d", len(resp.EstimatedTokens), len(estimatedTokensBucketOrder))
	}
}

// assertBuckets pins both the bucket order and per-label counts so a
// reorder or a CASE/label drift in arcreview_corpus.go trips here.
func assertBuckets(t *testing.T, axis string, got []ArcCorpusBucket, order []string, want map[string]int) {
	t.Helper()
	if len(got) != len(order) {
		t.Fatalf("%s: got %d buckets, want %d", axis, len(got), len(order))
	}
	for i, label := range order {
		if got[i].Label != label {
			t.Errorf("%s bucket[%d].Label = %q, want %q (order drift)", axis, i, got[i].Label, label)
		}
		if got[i].Count != want[label] {
			t.Errorf("%s bucket %q count = %d, want %d", axis, label, got[i].Count, want[label])
		}
	}
}
