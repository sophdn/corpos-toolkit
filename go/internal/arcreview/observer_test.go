package arcreview

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"toolkit/internal/db"
)

// seedSessionRegistry inserts one session_registry row. Caller supplies
// the timestamp string in SQLite datetime() compatible form so tests can
// order rows deterministically.
func seedSessionRegistry(t *testing.T, pool *db.Pool, sessionID, projectID, transcriptPath, lastActiveAt string) {
	t.Helper()
	_, err := pool.DB().Exec(`
		INSERT INTO session_registry (session_id, project_id, transcript_path, last_active_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, sessionID, projectID, transcriptPath, lastActiveAt, lastActiveAt)
	if err != nil {
		t.Fatalf("seed session_registry: %v", err)
	}
}

func TestLookupActiveSession_NoRow(t *testing.T) {
	pool := openTestPool(t)
	sess, err := lookupActiveSession(context.Background(), pool, "mcp-servers")
	if err != nil {
		t.Fatalf("lookupActiveSession err: %v", err)
	}
	if sess != nil {
		t.Fatalf("expected nil session for empty registry; got %+v", sess)
	}
}

func TestLookupActiveSession_SingleRow(t *testing.T) {
	pool := openTestPool(t)
	seedSessionRegistry(t, pool, "s-only", "mcp-servers", "/tmp/t.jsonl", "2026-05-20 12:00:00")

	sess, err := lookupActiveSession(context.Background(), pool, "mcp-servers")
	if err != nil {
		t.Fatalf("lookupActiveSession err: %v", err)
	}
	if sess == nil {
		t.Fatalf("expected a session row; got nil")
	}
	if sess.SessionID != "s-only" || sess.TranscriptPath != "/tmp/t.jsonl" {
		t.Fatalf("unexpected row: %+v", sess)
	}
}

func TestLookupActiveSession_PicksMostRecentlyActive(t *testing.T) {
	pool := openTestPool(t)
	seedSessionRegistry(t, pool, "s-older", "mcp-servers", "/tmp/older.jsonl", "2026-05-19 10:00:00")
	seedSessionRegistry(t, pool, "s-newer", "mcp-servers", "/tmp/newer.jsonl", "2026-05-20 15:00:00")
	seedSessionRegistry(t, pool, "s-different-project", "other", "/tmp/other.jsonl", "2026-05-20 23:59:59")

	sess, err := lookupActiveSession(context.Background(), pool, "mcp-servers")
	if err != nil {
		t.Fatalf("lookupActiveSession err: %v", err)
	}
	if sess == nil {
		t.Fatalf("expected a session row; got nil")
	}
	if sess.SessionID != "s-newer" {
		t.Fatalf("expected most-recently-active s-newer; got %+v", sess)
	}
}

func TestFireReview_NoSessionInRegistry_NoPendingDecisions(t *testing.T) {
	pool := openTestPool(t)
	obs := NewSubstrateReviewObserver(Deps{Pool: pool}) // Router nil; doesn't matter — we exit at lookupActiveSession

	projectID := "mcp-servers"
	obs.fireReview(context.Background(), SubstrateTriggerEvent{
		EventID:     "trig-1",
		EventType:   "BugResolved",
		TriggerSlug: "event_bug_resolved",
		EntityKind:  "bug",
		EntitySlug:  "no-session-bug",
		ProjectID:   &projectID,
	})

	var count int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM pending_decisions`).Scan(&count); err != nil {
		t.Fatalf("count pending_decisions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 pending_decisions rows when no session; got %d", count)
	}
}

func TestFireReview_NoProjectID_NoFire(t *testing.T) {
	pool := openTestPool(t)
	// Seed a session that COULD match if project_id were set — confirms
	// the early-return on nil project blocks the lookup.
	seedSessionRegistry(t, pool, "s-1", "mcp-servers", "/tmp/t.jsonl", "2026-05-20 12:00:00")
	o := NewSubstrateReviewObserver(Deps{Pool: pool})

	o.fireReview(context.Background(), SubstrateTriggerEvent{
		EventID:     "trig-2",
		EventType:   "BugResolved",
		TriggerSlug: "event_bug_resolved",
		ProjectID:   nil,
	})

	var count int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM pending_decisions`).Scan(&count); err != nil {
		t.Fatalf("count pending_decisions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 pending_decisions rows for nil project; got %d", count)
	}
}

func TestFireReview_RouterNil_NoPendingDecisions(t *testing.T) {
	pool := openTestPool(t)
	tpath := writeTranscriptFile(t, []string{`{"role":"user","content":"hi"}`})
	seedSessionRegistry(t, pool, "s-1", "mcp-servers", tpath, "2026-05-20 12:00:00")
	o := NewSubstrateReviewObserver(Deps{Pool: pool}) // Router nil → handler short-circuits to qwen_unreachable

	projectID := "mcp-servers"
	o.fireReview(context.Background(), SubstrateTriggerEvent{
		EventID:     "trig-3",
		EventType:   "BugResolved",
		TriggerSlug: "event_bug_resolved",
		ProjectID:   &projectID,
	})

	var count int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM pending_decisions`).Scan(&count); err != nil {
		t.Fatalf("count pending_decisions: %v", err)
	}
	if count != 0 {
		t.Fatalf("qwen_unreachable status must not enqueue pending_decisions; got %d", count)
	}
}

func TestFireReview_DebouncedBeforeFire_NoPendingDecisions(t *testing.T) {
	pool := openTestPool(t)
	tpath := writeTranscriptFile(t, []string{`{"role":"user","content":"hi"}`})
	seedSessionRegistry(t, pool, "s-1", "mcp-servers", tpath, "2026-05-20 12:00:00")

	// Seed a recent fire so the handler's debouncer suppresses the second
	// trigger. This is the "debouncer in front" assertion: a substrate
	// event landing within 60s of an earlier fire short-circuits before
	// reaching the snapshot/dispatch stages.
	deb := NewDebouncer(pool)
	if err := deb.RecordFire(context.Background(), "s-1"); err != nil {
		t.Fatalf("RecordFire seed: %v", err)
	}

	o := NewSubstrateReviewObserver(Deps{Pool: pool}) // Router nil; doesn't matter — debouncer fires first
	projectID := "mcp-servers"
	o.fireReview(context.Background(), SubstrateTriggerEvent{
		EventID:     "trig-debounced",
		EventType:   "BugResolved",
		TriggerSlug: "event_bug_resolved",
		ProjectID:   &projectID,
	})

	var count int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM pending_decisions`).Scan(&count); err != nil {
		t.Fatalf("count pending_decisions: %v", err)
	}
	if count != 0 {
		t.Fatalf("debounced status must not enqueue pending_decisions; got %d", count)
	}
}

func TestWritePendingDecisions_RoundTrip(t *testing.T) {
	pool := openTestPool(t)
	arcSummary := "did some work; resolved a bug; opened a vault note"
	res := ReviewArcForFilingResult{
		Status: "fired",
		Decisions: []FilingDecision{
			{
				Action:     ActionForgeBug,
				Confidence: 0.92,
				Reasoning:  "real friction observed",
				Payload:    json.RawMessage(`{"title":"x","problem_statement":"y","severity":"low"}`),
			},
		},
		Triggers:   []string{"event_bug_resolved"},
		ArcSummary: arcSummary,
		EventID:    "evt-corpus-1",
	}
	if err := writePendingDecisions(context.Background(), pool, "mcp-servers", "s-target", res); err != nil {
		t.Fatalf("writePendingDecisions: %v", err)
	}

	var (
		eventID, projectID, targetSessionID, decisionsJSON, triggersJSON string
		summary                                                          *string
		dispatchedAt                                                     *string
	)
	row := pool.DB().QueryRow(`
		SELECT event_id, project_id, target_session_id, decisions_json, triggers_json, arc_summary, dispatched_at
		FROM pending_decisions
		ORDER BY id DESC
		LIMIT 1
	`)
	if err := row.Scan(&eventID, &projectID, &targetSessionID, &decisionsJSON, &triggersJSON, &summary, &dispatchedAt); err != nil {
		t.Fatalf("scan inserted row: %v", err)
	}
	if eventID != "evt-corpus-1" || projectID != "mcp-servers" || targetSessionID != "s-target" {
		t.Fatalf("scalar fields mismatch: eventID=%q projectID=%q targetSessionID=%q", eventID, projectID, targetSessionID)
	}
	if summary == nil || *summary != arcSummary {
		t.Fatalf("arc_summary should be persisted verbatim; got %v", summary)
	}
	if dispatchedAt != nil {
		t.Fatalf("dispatched_at must be NULL until claim; got %v", *dispatchedAt)
	}
	// Triggers + decisions JSON round-trip.
	var triggers []string
	if err := json.Unmarshal([]byte(triggersJSON), &triggers); err != nil {
		t.Fatalf("unmarshal triggers_json: %v", err)
	}
	if len(triggers) != 1 || triggers[0] != "event_bug_resolved" {
		t.Fatalf("triggers round-trip failed: %+v", triggers)
	}
	var decisions []FilingDecision
	if err := json.Unmarshal([]byte(decisionsJSON), &decisions); err != nil {
		t.Fatalf("unmarshal decisions_json: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Action != ActionForgeBug || decisions[0].Confidence != 0.92 {
		t.Fatalf("decisions round-trip failed: %+v", decisions)
	}
}

func TestWritePendingDecisions_EmptyArcSummaryStoredAsNull(t *testing.T) {
	pool := openTestPool(t)
	res := ReviewArcForFilingResult{
		Status:   "fired",
		Triggers: []string{"event_task_completed"},
		EventID:  "evt-no-summary",
		// One actionable decision so the (bug 1471) filter doesn't
		// drop the entire row — this test asserts arc_summary
		// nullability, which presupposes a row landed.
		Decisions: []FilingDecision{
			{Action: ActionForgeBug, Confidence: 0.9, Reasoning: "needs row"},
		},
	}
	if err := writePendingDecisions(context.Background(), pool, "mcp-servers", "s-x", res); err != nil {
		t.Fatalf("writePendingDecisions: %v", err)
	}
	var summary *string
	if err := pool.DB().QueryRow(`SELECT arc_summary FROM pending_decisions WHERE event_id = ?`, "evt-no-summary").Scan(&summary); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if summary != nil {
		t.Fatalf("expected NULL arc_summary when empty; got %q", *summary)
	}
}

func TestWritePendingDecisions_FiltersNothingToFile(t *testing.T) {
	pool := openTestPool(t)
	res := ReviewArcForFilingResult{
		Status:   "fired",
		Triggers: []string{"event_bug_resolved"},
		EventID:  "evt-filter-1",
		Decisions: []FilingDecision{
			{Action: ActionForgeBug, Confidence: 0.92, Reasoning: "real friction"},
			{Action: ActionNothingToFile, Confidence: 1.0, Reasoning: "clean arc"},
			{Action: ActionForgeVaultNote, Confidence: 0.85, Reasoning: "cross-project framing"},
		},
	}
	if err := writePendingDecisions(context.Background(), pool, "mcp-servers", "s-target", res); err != nil {
		t.Fatalf("writePendingDecisions: %v", err)
	}
	var decisionsJSON string
	if err := pool.DB().QueryRow(`SELECT decisions_json FROM pending_decisions WHERE event_id = ?`, "evt-filter-1").Scan(&decisionsJSON); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var stored []FilingDecision
	if err := json.Unmarshal([]byte(decisionsJSON), &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("expected 2 actionable decisions; got %d (%+v)", len(stored), stored)
	}
	for _, d := range stored {
		if d.Action == ActionNothingToFile {
			t.Fatalf("nothing_to_file leaked into pending_decisions: %+v", d)
		}
	}
}

func TestWritePendingDecisions_AllNothingToFile_NoRowWritten(t *testing.T) {
	pool := openTestPool(t)
	res := ReviewArcForFilingResult{
		Status:   "fired",
		Triggers: []string{"event_commit_landed"},
		EventID:  "evt-allnone",
		Decisions: []FilingDecision{
			{Action: ActionNothingToFile, Confidence: 1.0, Reasoning: "clean arc"},
		},
	}
	if err := writePendingDecisions(context.Background(), pool, "mcp-servers", "s-x", res); err != nil {
		t.Fatalf("writePendingDecisions: %v", err)
	}
	var n int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM pending_decisions WHERE event_id = ?`, "evt-allnone").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows when every decision is nothing_to_file; got %d", n)
	}
}

func TestWritePendingDecisions_EmptyDecisions_NoRowWritten(t *testing.T) {
	pool := openTestPool(t)
	res := ReviewArcForFilingResult{
		Status:   "fired",
		Triggers: []string{"event_chain_closed"},
		EventID:  "evt-empty",
		// No decisions at all (defensive: shouldn't happen post-dispatch
		// but the substrate write path must not crash if it does).
	}
	if err := writePendingDecisions(context.Background(), pool, "mcp-servers", "s-y", res); err != nil {
		t.Fatalf("writePendingDecisions: %v", err)
	}
	var n int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM pending_decisions WHERE event_id = ?`, "evt-empty").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows for empty decision slice; got %d", n)
	}
}

// countListenerFiredEvents returns how many ArcReviewListenerFired
// events landed for the given trigger_event_id, plus the status and
// skip_reason of the most recent such row. Used by the regression
// tests below to assert one fire per Observe call and the right
// outcome at each branch (closes bug
// `stdio-process-observer-logs-not-captured-in-central-log-file`).
func countListenerFiredEvents(t *testing.T, pool *db.Pool, triggerEventID string) (count int, status string, skipReason *string) {
	t.Helper()
	rows, err := pool.DB().Query(`
		SELECT json_extract(payload, '$.status'), json_extract(payload, '$.skip_reason')
		FROM events
		WHERE type = 'ArcReviewListenerFired'
		  AND json_extract(payload, '$.trigger_event_id') = ?
		ORDER BY ts DESC
	`, triggerEventID)
	if err != nil {
		t.Fatalf("query ArcReviewListenerFired: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var r *string
		if err := rows.Scan(&s, &r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if count == 0 {
			status = s
			skipReason = r
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return count, status, skipReason
}

func TestFireReview_EmitsListenerFiredOnNoProjectID(t *testing.T) {
	pool := openTestPool(t)
	o := NewSubstrateReviewObserver(Deps{Pool: pool})
	o.fireReview(context.Background(), SubstrateTriggerEvent{
		EventID:     "0192f5b8-1111-7aaa-8111-000000000001",
		EventType:   "BugResolved",
		TriggerSlug: "event_bug_resolved",
		ProjectID:   nil,
	})
	n, status, reason := countListenerFiredEvents(t, pool, "0192f5b8-1111-7aaa-8111-000000000001")
	if n != 1 {
		t.Fatalf("expected exactly 1 ArcReviewListenerFired event; got %d", n)
	}
	if status != "skipped" {
		t.Fatalf("expected status=skipped; got %q", status)
	}
	if reason == nil || *reason != listenerSkipNoProjectID {
		t.Fatalf("expected skip_reason=%q; got %v", listenerSkipNoProjectID, reason)
	}
}

func TestFireReview_EmitsListenerFiredOnNoActiveSession(t *testing.T) {
	pool := openTestPool(t)
	o := NewSubstrateReviewObserver(Deps{Pool: pool})
	projectID := "mcp-servers"
	o.fireReview(context.Background(), SubstrateTriggerEvent{
		EventID:     "0192f5b8-1111-7aaa-8111-000000000002",
		EventType:   "TaskCompleted",
		TriggerSlug: "event_task_completed",
		EntityKind:  "task",
		EntitySlug:  "some-task",
		ProjectID:   &projectID,
	})
	n, status, reason := countListenerFiredEvents(t, pool, "0192f5b8-1111-7aaa-8111-000000000002")
	if n != 1 {
		t.Fatalf("expected exactly 1 ArcReviewListenerFired event; got %d", n)
	}
	if status != "skipped" {
		t.Fatalf("expected status=skipped; got %q", status)
	}
	if reason == nil || *reason != listenerSkipNoActiveSession {
		t.Fatalf("expected skip_reason=%q; got %v", listenerSkipNoActiveSession, reason)
	}
}

func TestFireReview_EmitsListenerFiredOnReviewNonFired(t *testing.T) {
	pool := openTestPool(t)
	tpath := writeTranscriptFile(t, []string{`{"role":"user","content":"hi"}`})
	seedSessionRegistry(t, pool, "s-1", "mcp-servers", tpath, "2026-05-20 12:00:00")
	o := NewSubstrateReviewObserver(Deps{Pool: pool}) // Router nil → qwen_unreachable

	projectID := "mcp-servers"
	o.fireReview(context.Background(), SubstrateTriggerEvent{
		EventID:     "0192f5b8-1111-7aaa-8111-000000000003",
		EventType:   "BugResolved",
		TriggerSlug: "event_bug_resolved",
		ProjectID:   &projectID,
	})
	n, status, reason := countListenerFiredEvents(t, pool, "0192f5b8-1111-7aaa-8111-000000000003")
	if n != 1 {
		t.Fatalf("expected exactly 1 ArcReviewListenerFired event; got %d", n)
	}
	if status != "skipped" {
		t.Fatalf("expected status=skipped; got %q", status)
	}
	if reason == nil || *reason != listenerSkipReviewNonFired {
		t.Fatalf("expected skip_reason=%q; got %v", listenerSkipReviewNonFired, reason)
	}
}

// TestObserverObserveDoesNotBlock ensures Observe returns quickly even
// when its goroutine cannot make progress (Router nil → hits the handler
// short-circuit immediately, but the assertion is on Observe itself
// returning before the goroutine completes — i.e. the fold tx is never
// blocked).
func TestObserverObserveDoesNotBlock(t *testing.T) {
	pool := openTestPool(t)
	o := NewSubstrateReviewObserver(Deps{Pool: pool})
	projectID := "mcp-servers"

	start := time.Now()
	o.Observe(context.Background(), SubstrateTriggerEvent{
		EventID:     "trig-fast",
		EventType:   "BugResolved",
		TriggerSlug: "event_bug_resolved",
		ProjectID:   &projectID,
	})
	elapsed := time.Since(start)
	// Observe should return in under 100ms (it just kicks a goroutine).
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Observe returned in %v; expected < 100ms — fold tx may be blocked", elapsed)
	}
}
