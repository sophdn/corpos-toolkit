package arcreview

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"toolkit/internal/db"
)

// seedPendingDecision inserts one pending_decisions row directly. Used
// by the claim tests to populate the queue without going through the
// observer's writePendingDecisions path. createdAt is bound so tests
// can assert claim ordering.
func seedPendingDecision(t *testing.T, pool *db.Pool, eventID, project, target string, decisions []FilingDecision, triggers []string, arcSummary, createdAt string) int64 {
	t.Helper()
	decJSON, err := json.Marshal(decisions)
	if err != nil {
		t.Fatalf("marshal decisions: %v", err)
	}
	trigJSON, err := json.Marshal(triggers)
	if err != nil {
		t.Fatalf("marshal triggers: %v", err)
	}
	res, err := pool.DB().Exec(`
		INSERT INTO pending_decisions
			(event_id, project_id, target_session_id, decisions_json, triggers_json, arc_summary, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, eventID, project, target, string(decJSON), string(trigJSON), arcSummary, createdAt)
	if err != nil {
		t.Fatalf("seed pending_decisions: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func TestPendingDecisionsClaim_EmptyQueueReturnsOK(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	out, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "stop-hook-s1"}))
	if err != nil {
		t.Fatalf("unexpected err on empty queue: %v", err)
	}
	if len(out.Claimed) != 0 {
		t.Fatalf("expected 0 claimed rows on empty queue; got %d", len(out.Claimed))
	}
}

func TestPendingDecisionsClaim_RequiresSessionID(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	_, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers", mustParams(t, map[string]any{}))
	if err == nil {
		t.Fatalf("expected error when session_id missing")
	}
}

func TestPendingDecisionsClaim_ClaimsAndMarksDispatched(t *testing.T) {
	pool := openTestPool(t)
	decisions := []FilingDecision{
		{Action: ActionForgeBug, Confidence: 0.92, Reasoning: "real friction", Payload: json.RawMessage(`{"title":"x"}`)},
	}
	id := seedPendingDecision(t, pool, "evt-claim-1", "mcp-servers", "s-target",
		decisions, []string{"event_bug_resolved"}, "summary text", "2026-05-20 10:00:00")

	deps := Deps{Pool: pool}
	out, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "s-target"}))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(out.Claimed) != 1 {
		t.Fatalf("expected 1 claimed row; got %d", len(out.Claimed))
	}
	row := out.Claimed[0]
	if row.ID != id || row.EventID != "evt-claim-1" || row.TargetSessionID != "s-target" {
		t.Fatalf("scalar mismatch: %+v", row)
	}
	if row.ArcSummary != "summary text" {
		t.Fatalf("arc_summary mismatch: %q", row.ArcSummary)
	}
	if len(row.Decisions) != 1 || row.Decisions[0].Action != ActionForgeBug {
		t.Fatalf("decisions round-trip failed: %+v", row.Decisions)
	}
	if len(row.Triggers) != 1 || row.Triggers[0] != "event_bug_resolved" {
		t.Fatalf("triggers round-trip failed: %+v", row.Triggers)
	}

	// Row must be marked dispatched in the same tx as the claim.
	var (
		dispatchedAt      *string
		dispatchSessionID *string
	)
	if err := pool.DB().QueryRow(`SELECT dispatched_at, dispatch_session_id FROM pending_decisions WHERE id = ?`, id).Scan(&dispatchedAt, &dispatchSessionID); err != nil {
		t.Fatalf("post-claim scan: %v", err)
	}
	if dispatchedAt == nil {
		t.Fatalf("dispatched_at must be set post-claim; got nil")
	}
	if dispatchSessionID == nil || *dispatchSessionID != "s-target" {
		t.Fatalf("dispatch_session_id mismatch: %v", dispatchSessionID)
	}
}

func TestPendingDecisionsClaim_SecondClaimSeesNoRows(t *testing.T) {
	pool := openTestPool(t)
	seedPendingDecision(t, pool, "evt-claim-2", "mcp-servers", "s-target",
		[]FilingDecision{{Action: ActionNothingToFile, Confidence: 0.95, Reasoning: "no signal"}},
		[]string{"event_task_completed"}, "", "2026-05-20 11:00:00")

	deps := Deps{Pool: pool}
	first, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "s-target"}))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first.Claimed) != 1 {
		t.Fatalf("first claim must return the row; got %d", len(first.Claimed))
	}
	second, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "s-target"}))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second.Claimed) != 0 {
		t.Fatalf("second claim must see no rows after the first claimed; got %d", len(second.Claimed))
	}
}

// TestPendingDecisionsClaim_RespectsSessionScope is the regression test for the
// cross-session bleed (bug 945): a session must claim ONLY pending decisions
// targeted at IT (target_session_id), never another session's, even within the
// same project. Pre-fix the claim filtered by project alone, so session B's
// UserPromptSubmit drain hook claimed session A's arc-close decisions and the
// drain hook injected A's arc (+ the >=0.85 auto-execute directive) into B.
func TestPendingDecisionsClaim_RespectsSessionScope(t *testing.T) {
	pool := openTestPool(t)
	// A decision targeted at session A (A's arc was the one reviewed).
	seedPendingDecision(t, pool, "evt-sessA", "mcp-servers", "session-A",
		[]FilingDecision{{Action: ActionForgeVaultNote, Confidence: 0.9}},
		[]string{"event_commit_landed"}, "A's arc summary", "2026-05-26 10:00:00")

	deps := Deps{Pool: pool}

	// Session B drains its prompt — must NOT receive A's decisions.
	bOut, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "session-B"}))
	if err != nil {
		t.Fatalf("claim as B: %v", err)
	}
	if len(bOut.Claimed) != 0 {
		t.Fatalf("session B claimed %d row(s) targeted at session A — cross-session bleed (bug 945)", len(bOut.Claimed))
	}
	// B's (non-)claim must NOT consume A's row.
	var dispatchedAt *string
	if err := pool.DB().QueryRow(`SELECT dispatched_at FROM pending_decisions WHERE event_id = ?`, "evt-sessA").Scan(&dispatchedAt); err != nil {
		t.Fatalf("scan after B claim: %v", err)
	}
	if dispatchedAt != nil {
		t.Fatalf("B's claim must not consume A's row; dispatched_at was set")
	}

	// Session A drains its prompt — receives its own decision.
	aOut, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "session-A"}))
	if err != nil {
		t.Fatalf("claim as A: %v", err)
	}
	if len(aOut.Claimed) != 1 || aOut.Claimed[0].EventID != "evt-sessA" {
		t.Fatalf("session A must claim its OWN decision; got %+v", aOut.Claimed)
	}
}

func TestPendingDecisionsClaim_OrderedByCreatedAtAscending(t *testing.T) {
	pool := openTestPool(t)
	seedPendingDecision(t, pool, "evt-late", "mcp-servers", "s-order",
		[]FilingDecision{{Action: ActionForgeBug, Confidence: 0.9}},
		[]string{"event_chain_closed"}, "", "2026-05-20 14:00:00")
	seedPendingDecision(t, pool, "evt-early", "mcp-servers", "s-order",
		[]FilingDecision{{Action: ActionForgeVaultNote, Confidence: 0.9}},
		[]string{"event_task_completed"}, "", "2026-05-20 10:00:00")
	seedPendingDecision(t, pool, "evt-mid", "mcp-servers", "s-order",
		[]FilingDecision{{Action: ActionMemoryWrite, Confidence: 0.9}},
		[]string{"event_bug_resolved"}, "", "2026-05-20 12:00:00")

	deps := Deps{Pool: pool}
	out, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "s-order"}))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(out.Claimed) != 3 {
		t.Fatalf("expected 3 claimed; got %d", len(out.Claimed))
	}
	if out.Claimed[0].EventID != "evt-early" || out.Claimed[1].EventID != "evt-mid" || out.Claimed[2].EventID != "evt-late" {
		t.Fatalf("expected created_at ascending; got %s, %s, %s",
			out.Claimed[0].EventID, out.Claimed[1].EventID, out.Claimed[2].EventID)
	}
}

func TestPendingDecisionsClaim_RespectsProjectScope(t *testing.T) {
	pool := openTestPool(t)
	seedPendingDecision(t, pool, "evt-mcp", "mcp-servers", "s-1",
		[]FilingDecision{{Action: ActionForgeBug, Confidence: 0.9}},
		[]string{"event_bug_resolved"}, "", "2026-05-20 10:00:00")
	seedPendingDecision(t, pool, "evt-other", "other-project", "s-1",
		[]FilingDecision{{Action: ActionForgeBug, Confidence: 0.9}},
		[]string{"event_bug_resolved"}, "", "2026-05-20 10:00:00")

	deps := Deps{Pool: pool}
	out, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "s-1"}))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(out.Claimed) != 1 || out.Claimed[0].EventID != "evt-mcp" {
		t.Fatalf("project scope leaked: %+v", out.Claimed)
	}
	// other-project row must still be undispatched.
	var dispatchedAt *string
	if err := pool.DB().QueryRow(`SELECT dispatched_at FROM pending_decisions WHERE event_id = ?`, "evt-other").Scan(&dispatchedAt); err != nil {
		t.Fatalf("scan other-project: %v", err)
	}
	if dispatchedAt != nil {
		t.Fatalf("other-project row must remain undispatched")
	}
}

// TestPendingDecisionsClaim_ConcurrentClaimsRaceSafe is the load-bearing
// race assertion required by T5 acceptance: two simultaneous claims for
// the same project must dispatch each row exactly once. SQLite's writer
// mutex (held by pool.WithWrite) serializes the SELECT+UPDATE tx;
// whichever claim grabs the lock first sees the row, marks it
// dispatched in the same tx, releases the lock — the second claim's
// SELECT sees the now-flipped dispatched_at and returns zero rows.
func TestPendingDecisionsClaim_ConcurrentClaimsRaceSafe(t *testing.T) {
	pool := openTestPool(t)
	// Seed 5 rows; with two concurrent claimers and no limit cap (default
	// is 10), the two claims must together return exactly 5 rows, none
	// duplicated.
	for i := 0; i < 5; i++ {
		seedPendingDecision(t, pool, "evt-race-"+string(rune('a'+i)), "mcp-servers", "s-target",
			[]FilingDecision{{Action: ActionForgeBug, Confidence: 0.9}},
			[]string{"event_bug_resolved"}, "", "2026-05-20 10:00:00")
	}

	deps := Deps{Pool: pool}
	var wg sync.WaitGroup
	results := make([][]PendingDecisionsRow, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Both claimers are the TARGET session: the realistic race is one
			// session's drain hook firing twice (rapid prompts) or the observer
			// + drain racing — all for the same target. Session scoping (bug 945)
			// means a non-target session would just see 0, which wouldn't
			// exercise the exactly-once writer-mutex serialization.
			out, err := HandlePendingDecisionsClaim(context.Background(), deps, "mcp-servers",
				mustParams(t, map[string]any{"session_id": "s-target"}))
			if err != nil {
				t.Errorf("concurrent claim %d: %v", idx, err)
				return
			}
			results[idx] = out.Claimed
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]bool)
	for _, rows := range results {
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("row %d dispatched twice across concurrent claims", r.ID)
			}
			seen[r.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct claims across both claimers; got %d", len(seen))
	}
	// Every row should be marked dispatched.
	var undispatched int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM pending_decisions WHERE dispatched_at IS NULL`).Scan(&undispatched); err != nil {
		t.Fatalf("count undispatched: %v", err)
	}
	if undispatched != 0 {
		t.Fatalf("post-race: %d rows still undispatched; expected 0", undispatched)
	}
}
