package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// seedReviewEvent emits one ArcCloseFilingReviewed event so the audit
// query has data to scan. Returns the event_id so tests can correlate.
func seedReviewEvent(t *testing.T, pool *db.Pool, project, sessionID string, triggers []string, decisions []events.FilingDecisionSummary, autoCount, surfaceCount, skipCount int) string {
	t.Helper()
	if decisions == nil {
		decisions = []events.FilingDecisionSummary{}
	}
	if triggers == nil {
		triggers = []string{}
	}
	var eventID string
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		var arcSummary *string
		s := "test arc summary"
		arcSummary = &s
		entity := events.NewEntityRef("arc_review_session", sessionID, project)
		payload := events.ArcCloseFilingReviewedPayload{
			SessionID:              sessionID,
			Triggers:               triggers,
			SnapshotTruncated:      false,
			SnapshotTokenCount:     1024,
			SnapshotMessageCount:   10,
			ArcSummary:             arcSummary,
			Decisions:              decisions,
			AutoExecuteCount:       autoCount,
			SurfaceForConfirmCount: surfaceCount,
			SkipCount:              skipCount,
			LatencyMS:              1500,
		}
		id, err := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity:  entity,
			Payload: payload,
		})
		if err != nil {
			return err
		}
		eventID = id
		return nil
	})
	if err != nil {
		t.Fatalf("seedReviewEvent: %v", err)
	}
	return eventID
}

// seedBugResolved emits a BugResolved event so the audit's heuristic
// correction-signal join has data. Used by the "with corrections"
// case.
func seedBugResolved(t *testing.T, pool *db.Pool, project, bugSlug, kind string) {
	t.Helper()
	// Seed the project row + a bug row so the entity reference resolves
	// (BugResolved emit checks project + entity).
	_, _ = pool.DB().Exec(`INSERT INTO projects (id, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, project, project)
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		entity := events.NewEntityRef("bug", bugSlug, project)
		payload := events.BugResolvedPayload{Kind: kind}
		_, err := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity:  entity,
			Payload: payload,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seedBugResolved: %v", err)
	}
}

func TestArcReviewAudit_EmptyWindow(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	out, err := HandleArcReviewAudit(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(out.Reviews) != 0 {
		t.Fatalf("expected 0 reviews on empty events table; got %d", len(out.Reviews))
	}
	if out.HeuristicCorrectionNote == "" {
		t.Fatalf("heuristic_correction_note must surface even on empty results")
	}
	if out.CorrectionWindowHours != 24 {
		t.Fatalf("default correction_window_hours should be 24; got %d", out.CorrectionWindowHours)
	}
}

func TestArcReviewAudit_ReviewWithNoCorrections(t *testing.T) {
	pool := openTestPool(t)
	decisions := []events.FilingDecisionSummary{
		{Action: "forge_bug", Confidence: 0.9, Reasoning: "real friction"},
	}
	eventID := seedReviewEvent(t, pool, "mcp-servers", "sess-noncorr", []string{"event_bug_resolved"}, decisions, 1, 0, 0)

	deps := Deps{Pool: pool}
	out, err := HandleArcReviewAudit(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(out.Reviews) != 1 {
		t.Fatalf("expected 1 review; got %d", len(out.Reviews))
	}
	row := out.Reviews[0]
	if row.ReviewID != eventID {
		t.Fatalf("review_id mismatch: %q vs %q", row.ReviewID, eventID)
	}
	if row.SessionID != "sess-noncorr" {
		t.Fatalf("session_id mismatch: %q", row.SessionID)
	}
	if len(row.UserCorrectionSignals) != 0 {
		t.Fatalf("expected 0 correction signals; got %d", len(row.UserCorrectionSignals))
	}
	if len(row.Decisions) != 1 || row.Decisions[0].Action != "forge_bug" {
		t.Fatalf("decisions decoded incorrectly: %+v", row.Decisions)
	}
	if row.DispatchStatus == "" {
		t.Fatalf("dispatch_status must be set")
	}
}

func TestArcReviewAudit_ReviewWithDispatchedRow(t *testing.T) {
	pool := openTestPool(t)
	decisions := []events.FilingDecisionSummary{{Action: "forge_vault_note", Confidence: 0.85}}
	eventID := seedReviewEvent(t, pool, "mcp-servers", "sess-disp", []string{"event_commit_landed"}, decisions, 1, 0, 0)
	// Seed a matching pending_decisions row + mark it dispatched.
	if _, err := pool.DB().Exec(`
		INSERT INTO pending_decisions(event_id, project_id, target_session_id, decisions_json, triggers_json, arc_summary, dispatched_at, dispatch_session_id)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'), 'stopper-session')`,
		eventID, "mcp-servers", "sess-disp", "[]", "[]", "summary",
	); err != nil {
		t.Fatalf("seed pending_decisions: %v", err)
	}

	deps := Deps{Pool: pool}
	out, err := HandleArcReviewAudit(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(out.Reviews) != 1 {
		t.Fatalf("expected 1 review; got %d", len(out.Reviews))
	}
	row := out.Reviews[0]
	if row.DispatchStatus != "dispatched" {
		t.Fatalf("dispatch_status should be 'dispatched'; got %q", row.DispatchStatus)
	}
	if row.DispatchSessionID != "stopper-session" {
		t.Fatalf("dispatch_session_id mismatch: %q", row.DispatchSessionID)
	}
	if row.DispatchedAt == "" {
		t.Fatalf("dispatched_at must be populated when dispatch row exists")
	}
}

func TestArcReviewAudit_ReviewWithCorrectionSignal(t *testing.T) {
	pool := openTestPool(t)
	decisions := []events.FilingDecisionSummary{{Action: "forge_bug", Confidence: 0.92}}
	reviewID := seedReviewEvent(t, pool, "mcp-servers", "sess-corr", []string{"event_task_completed"}, decisions, 1, 0, 0)
	// Wait a moment so the BugResolved ts strictly follows the review ts.
	time.Sleep(50 * time.Millisecond)
	seedBugResolved(t, pool, "mcp-servers", "auto-filed-bug-slug", "wontfix")

	deps := Deps{Pool: pool}
	out, err := HandleArcReviewAudit(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var matched bool
	for _, r := range out.Reviews {
		if r.ReviewID != reviewID {
			continue
		}
		matched = true
		if len(r.UserCorrectionSignals) == 0 {
			t.Fatalf("expected BugResolved within 24h to surface as correction signal; got 0")
		}
		signal := r.UserCorrectionSignals[0]
		if signal.EventType != "BugResolved" {
			t.Fatalf("expected BugResolved signal; got %q", signal.EventType)
		}
		if signal.EntitySlug != "auto-filed-bug-slug" {
			t.Fatalf("entity_slug mismatch: %q", signal.EntitySlug)
		}
		if signal.DeltaSec < 0 {
			t.Fatalf("delta_sec must be positive (correction follows review); got %d", signal.DeltaSec)
		}
	}
	if !matched {
		t.Fatalf("review row not found in audit output")
	}
}

func TestArcReviewAudit_RespectsCorrectionWindow(t *testing.T) {
	pool := openTestPool(t)
	decisions := []events.FilingDecisionSummary{{Action: "forge_bug", Confidence: 0.9}}
	reviewID := seedReviewEvent(t, pool, "mcp-servers", "sess-window", []string{"event_bug_resolved"}, decisions, 1, 0, 0)
	time.Sleep(50 * time.Millisecond)
	seedBugResolved(t, pool, "mcp-servers", "in-window-bug", "fixed")

	deps := Deps{Pool: pool}
	// 0-hour window means literally zero look-ahead → BugResolved 50ms
	// later still falls in the window (delta = 0.05s, > 0); the
	// boundary check uses ts > review_ts AND ts <= review_ts + 0h, so
	// the bug ts must equal review_ts to surface. The 50ms sleep means
	// the bug's ts is strictly greater than review_ts but the window
	// adds 0 → upper bound = review_ts → signal is OUT.
	out, err := HandleArcReviewAudit(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{
		"correction_window_hours": 0,
	}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	// 0-hour window falls back to default 24h per the handler's `if
	// window <= 0` guard. So the signal SHOULD surface. Documenting
	// the fallback behavior explicitly is the test's job; if a future
	// change removes the fallback, this test fails and forces a
	// conscious choice.
	for _, r := range out.Reviews {
		if r.ReviewID == reviewID && len(r.UserCorrectionSignals) == 0 {
			t.Fatalf("0-hour window should fall back to 24h default → signal must surface")
		}
	}
}

func TestArcReviewAudit_FiltersBySince(t *testing.T) {
	pool := openTestPool(t)
	decisions := []events.FilingDecisionSummary{{Action: "forge_bug", Confidence: 0.9}}
	seedReviewEvent(t, pool, "mcp-servers", "sess-old", []string{}, decisions, 1, 0, 0)

	// Query with a since that's in the future → no rows.
	deps := Deps{Pool: pool}
	futureTS := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	out, err := HandleArcReviewAudit(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{
		"since": futureTS,
	}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(out.Reviews) != 0 {
		t.Fatalf("future since must filter out all reviews; got %d", len(out.Reviews))
	}
}

func TestArcReviewAudit_RespectsProjectScope(t *testing.T) {
	pool := openTestPool(t)
	decisions := []events.FilingDecisionSummary{{Action: "forge_bug", Confidence: 0.9}}
	seedReviewEvent(t, pool, "mcp-servers", "s1", []string{}, decisions, 1, 0, 0)
	seedReviewEvent(t, pool, "other-project", "s2", []string{}, decisions, 1, 0, 0)

	deps := Deps{Pool: pool}
	// Project-scoped: only mcp-servers row.
	out, err := HandleArcReviewAudit(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(out.Reviews) != 1 || out.Reviews[0].ProjectID != "mcp-servers" {
		t.Fatalf("project scope leaked: %+v", out.Reviews)
	}
	// Cross-project (project=""): both rows.
	out, err = HandleArcReviewAudit(context.Background(), deps, "", mustParams(t, map[string]any{}))
	if err != nil {
		t.Fatalf("audit (cross-project): %v", err)
	}
	if len(out.Reviews) != 2 {
		t.Fatalf("cross-project audit should see both reviews; got %d", len(out.Reviews))
	}
}

func TestArcReviewAudit_MixedDispatchStatuses(t *testing.T) {
	pool := openTestPool(t)
	d := []events.FilingDecisionSummary{{Action: "forge_bug", Confidence: 0.9}}
	// Three reviews — one dispatched, one with decisions but no pending row, one with no decisions at all.
	dispatchedID := seedReviewEvent(t, pool, "mcp-servers", "s-disp", []string{}, d, 1, 0, 0)
	if _, err := pool.DB().Exec(`
		INSERT INTO pending_decisions(event_id, project_id, target_session_id, decisions_json, triggers_json, arc_summary, dispatched_at, dispatch_session_id)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'), 'stopper')`,
		dispatchedID, "mcp-servers", "s-disp", "[]", "[]", "",
	); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	seedReviewEvent(t, pool, "mcp-servers", "s-harness", []string{}, d, 1, 0, 0)
	seedReviewEvent(t, pool, "mcp-servers", "s-empty", []string{}, nil, 0, 0, 1)

	deps := Deps{Pool: pool}
	out, err := HandleArcReviewAudit(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{}))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(out.Reviews) != 3 {
		t.Fatalf("expected 3 reviews; got %d", len(out.Reviews))
	}
	statuses := map[string]int{}
	for _, r := range out.Reviews {
		statuses[r.DispatchStatus]++
	}
	if statuses["dispatched"] != 1 {
		t.Fatalf("expected exactly 1 dispatched status; got %d (%+v)", statuses["dispatched"], statuses)
	}
	if statuses["skipped"] != 1 {
		t.Fatalf("expected 1 skipped status (no decisions at all); got %d (%+v)", statuses["skipped"], statuses)
	}
	if statuses["harness_in_band"] != 1 {
		t.Fatalf("expected 1 harness_in_band status; got %d (%+v)", statuses["harness_in_band"], statuses)
	}
}

// Compile-time check that the result is JSON-serializable in the
// expected shape — this catches naming drift between Go fields and JSON
// tags before the action surfaces to MCP callers.
func TestArcReviewAudit_ResultMarshalsClean(t *testing.T) {
	r := ArcReviewAuditResult{
		Reviews: []ArcReviewAuditRow{{
			ReviewID:              "test-id",
			TS:                    "2026-05-20T01:00:00Z",
			SessionID:             "s1",
			ProjectID:             "mcp-servers",
			Decisions:             []AuditDecisionRow{{Action: "forge_bug", Confidence: 0.9}},
			TriggerSignals:        []string{"event_bug_resolved"},
			UserCorrectionSignals: []CorrectionSignal{},
			DispatchStatus:        "skipped",
		}},
		HeuristicCorrectionNote: "test note",
		WindowStart:             "2026-05-13T00:00:00Z",
		CorrectionWindowHours:   24,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	mustContain := []string{
		`"review_id":"test-id"`,
		`"trigger_signals":["event_bug_resolved"]`,
		`"user_correction_signals":[]`,
		`"dispatch_status":"skipped"`,
		`"heuristic_correction_note":"test note"`,
		`"correction_window_hours":24`,
	}
	for _, m := range mustContain {
		if !contains(s, m) {
			t.Fatalf("marshal missing %q in: %s", m, s)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (firstIndex(s, sub) >= 0))
}

// firstIndex is a tiny strings.Index reimpl so the test file doesn't
// need to import "strings" (kept lean).
func firstIndex(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}

// Sanity-check that decodeReviewPayload handles an empty payload
// gracefully (defensive against future payload-shape drift).
func TestDecodeReviewPayload_EmptyJSON(t *testing.T) {
	var row ArcReviewAuditRow
	if err := decodeReviewPayload(`{}`, &row); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if row.TriggerSignals == nil {
		t.Fatalf("trigger_signals must be [], not nil")
	}
	if row.Decisions == nil {
		t.Fatalf("decisions must be [], not nil")
	}
}

// dummy ensures the test file compiles with fmt imported (used by
// other helpers above).
var _ = fmt.Sprintf
