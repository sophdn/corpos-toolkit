package arcreview

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"toolkit/internal/arcreview/arcparams"
)

// TestExtractSnapshotAsOf_FiltersToFireTime: the point-in-time path keeps
// only rows at/before the cutoff, then truncates identically to
// ExtractSnapshot — so a recovered historical fire reproduces what its
// review saw, not the session's later turns (chain T4).
func TestExtractSnapshotAsOf_FiltersToFireTime(t *testing.T) {
	lines := []string{
		`{"role":"user","content":"first","timestamp":"2026-05-24T00:00:01Z"}`,
		`{"role":"assistant","content":"second","timestamp":"2026-05-24T00:00:02Z"}`,
		`{"role":"user","content":"third-after-fire","timestamp":"2026-05-24T00:00:03Z"}`,
	}
	path := writeTranscriptFile(t, lines)

	full, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}
	if len(full.Messages) != 3 {
		t.Fatalf("full snapshot = %d messages, want 3", len(full.Messages))
	}

	asOf, _ := time.Parse(time.RFC3339, "2026-05-24T00:00:02Z")
	snap, err := ExtractSnapshotAsOf(path, 20, 4000, asOf)
	if err != nil {
		t.Fatalf("ExtractSnapshotAsOf: %v", err)
	}
	if len(snap.Messages) != 2 {
		t.Fatalf("as-of snapshot = %d messages, want 2 (third row is after the cutoff)", len(snap.Messages))
	}
	if snap.Messages[0].Content != "first" || snap.Messages[1].Content != "second" {
		t.Errorf("as-of content = %+v, want [first, second]", snap.Messages)
	}
}

// Chain arc-close-snapshot-corpus-capture T2: the live capture writer must
// persist the EXACT snapshot fed to Qwen into arcreview_snapshot_corpus,
// ATOMICALLY with the ArcCloseFilingReviewed emit (no fire-with-snapshot
// without its corpus row; a corpus-write failure rolls the whole fire back).

// TestEmitFilingReviewed_CapturesSnapshotCorpus: a fire lands both the
// ArcCloseFilingReviewed event AND a matching corpus row whose content
// equals the snapshot fed to Qwen.
func TestEmitFilingReviewed_CapturesSnapshotCorpus(t *testing.T) {
	pool := openTestPool(t)
	if _, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('mcp-servers','mcp-servers') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	snap := Snapshot{
		Messages: []Message{
			{Role: "user", Content: "why is the cli exiting 0 on failure?"},
			{Role: "assistant", Content: "the wrapper swallows the exit code; fix it to propagate $?."},
		},
		EstimatedTokens: 42,
		Truncated:       true,
	}
	result := ArcReviewResult{ArcSummary: "cli exit-code bug + fix", LatencyMS: 123}
	params := arcparams.ReviewArcForFilingParams{SessionID: "corpus-happy-session", Triggers: []string{"user_shape_done"}}

	eventID, err := emitFilingReviewedEvent(context.Background(), pool, "mcp-servers",
		params, snap, result, PartitionedDecisions{}, 20, 4000)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if eventID == "" {
		t.Fatal("empty event_id")
	}

	var evCount int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE type='ArcCloseFilingReviewed' AND event_id=?`, eventID,
	).Scan(&evCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if evCount != 1 {
		t.Errorf("ArcCloseFilingReviewed events = %d, want 1", evCount)
	}

	var (
		sid, fireTS, msgsJSON, source        string
		msgCount, estTokens, trunc, mt, mtok int
	)
	if err := pool.DB().QueryRow(`
		SELECT session_id, fire_ts, messages_json, message_count,
		       estimated_tokens, truncated, max_turns, max_tokens, source
		FROM arcreview_snapshot_corpus WHERE event_id=?`, eventID,
	).Scan(&sid, &fireTS, &msgsJSON, &msgCount, &estTokens, &trunc, &mt, &mtok, &source); err != nil {
		t.Fatalf("corpus row not found for event %s: %v", eventID, err)
	}
	if sid != "corpus-happy-session" {
		t.Errorf("session_id = %q, want corpus-happy-session", sid)
	}
	if source != "live" {
		t.Errorf("source = %q, want live", source)
	}
	if msgCount != 2 {
		t.Errorf("message_count = %d, want 2", msgCount)
	}
	if mt != 20 || mtok != 4000 {
		t.Errorf("caps = %d/%d, want 20/4000", mt, mtok)
	}
	if estTokens != 42 {
		t.Errorf("estimated_tokens = %d, want 42", estTokens)
	}
	if trunc != 1 {
		t.Errorf("truncated = %d, want 1", trunc)
	}

	// fire_ts agrees with the event row (read-your-write within the tx).
	var evTS string
	if err := pool.DB().QueryRow(`SELECT ts FROM events WHERE event_id=?`, eventID).Scan(&evTS); err != nil {
		t.Fatalf("read event ts: %v", err)
	}
	if fireTS != evTS {
		t.Errorf("fire_ts = %q != event ts %q", fireTS, evTS)
	}

	// messages_json round-trips to the EXACT snapshot content + order.
	var got []Message
	if err := json.Unmarshal([]byte(msgsJSON), &got); err != nil {
		t.Fatalf("messages_json unmarshal: %v", err)
	}
	if len(got) != 2 ||
		got[0].Role != "user" || got[0].Content != snap.Messages[0].Content ||
		got[1].Role != "assistant" || got[1].Content != snap.Messages[1].Content {
		t.Errorf("messages_json content mismatch: got %+v", got)
	}
}

// TestEmitFilingReviewed_CorpusFailureRollsBackFire: when the corpus insert
// fails, the whole fire rolls back — NO ArcCloseFilingReviewed event persists.
// The injected failure is a dropped corpus table (deterministic).
func TestEmitFilingReviewed_CorpusFailureRollsBackFire(t *testing.T) {
	pool := openTestPool(t)
	if _, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('mcp-servers','mcp-servers') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Inject a corpus-write failure: drop the table so the in-tx INSERT errors.
	if _, err := pool.DB().Exec(`DROP TABLE arcreview_snapshot_corpus`); err != nil {
		t.Fatalf("drop corpus table: %v", err)
	}

	snap := Snapshot{Messages: []Message{{Role: "user", Content: "x"}}, EstimatedTokens: 1}
	params := arcparams.ReviewArcForFilingParams{SessionID: "corpus-rollback-session"}

	_, err := emitFilingReviewedEvent(context.Background(), pool, "mcp-servers",
		params, snap, ArcReviewResult{}, PartitionedDecisions{}, 20, 4000)
	if err == nil {
		t.Fatal("expected emit to fail when the corpus insert fails")
	}

	// The whole fire rolled back: no event for this session.
	var evCount int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE type='ArcCloseFilingReviewed' AND entity_slug=?`,
		"corpus-rollback-session",
	).Scan(&evCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if evCount != 0 {
		t.Errorf("ArcCloseFilingReviewed events = %d, want 0 (fire must roll back with the failed corpus insert)", evCount)
	}
}
