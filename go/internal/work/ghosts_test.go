package work_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"toolkit/internal/events"
	"toolkit/internal/projections"
	"toolkit/internal/work"
)

// badEvent is an unknown-type event — rejected by the thin-fast-local tier.
func badEvent(slug string) work.RecordEvent {
	return work.RecordEvent{Type: "NoSuchEventType", EntityKind: "bug", EntitySlug: slug, Payload: json.RawMessage(`{"original":"intent"}`)}
}

func ghostCount(t *testing.T, pool interface {
	DB() *sql.DB
}) int {
	t.Helper()
	var n int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM ghosts`).Scan(&n); err != nil {
		t.Fatalf("count ghosts: %v", err)
	}
	return n
}

func TestGhost_RejectedEvent_CreatesGhostAndFumbleCount(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	_, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, false,
		bugReportedEvent("real-bug", "valid"), badEvent("doomed")))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}
	if got := ghostCount(t, pool); got != 1 {
		t.Fatalf("expected 1 ghost, got %d", got)
	}

	// The rewrite-context (original payload) + reason are persisted.
	var reason, rewrite string
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT reason, rewrite_payload FROM ghosts WHERE attempted_type = ?`, "NoSuchEventType").
		Scan(&reason, &rewrite); err != nil {
		t.Fatalf("read ghost: %v", err)
	}
	if reason == "" {
		t.Fatal("ghost reason should be non-empty")
	}
	if rewrite != `{"original":"intent"}` {
		t.Fatalf("ghost should preserve the rewrite payload, got %q", rewrite)
	}

	// The fumble projection counts it.
	counts, err := work.GhostFumbleCounts(ctx, pool)
	if err != nil {
		t.Fatalf("GhostFumbleCounts: %v", err)
	}
	if len(counts) != 1 || counts[0].AttemptedType != "NoSuchEventType" || counts[0].Count != 1 {
		t.Fatalf("unexpected fumble counts: %+v", counts)
	}
}

// TestGhost_ExcludedFromEntityProjections_AndSurvivesRebuild is the load-
// bearing T4 invariant: a ghost never folds into an entity projection, and a
// from-empty projection rebuild leaves the entity tables byte-identical while
// the ghost record survives untouched.
func TestGhost_ExcludedFromEntityProjections_AndSurvivesRebuild(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}
	ctx := context.Background()

	_, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, false,
		bugReportedEvent("survivor-bug", "valid"), badEvent("rejected")))
	if err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}

	// The rejected event created NO entity projection row (only the valid one).
	var bugRows int
	if err := pool.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM proj_current_bugs`).Scan(&bugRows); err != nil {
		t.Fatalf("count bugs: %v", err)
	}
	if bugRows != 1 {
		t.Fatalf("expected exactly 1 entity bug row (the rejected event must not create one), got %d", bugRows)
	}

	ghostsBefore := ghostCount(t, pool)
	bugsBefore := dumpBugSlugs(t, pool)

	// A from-empty projection rebuild (truncate proj_* + refold from events).
	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, rErr := projections.RebuildAll(ctx, tx, nil)
		return rErr
	}); err != nil {
		t.Fatalf("RebuildAll: %v", err)
	}

	// Entity projection identical after rebuild (the valid bug re-folds from
	// its event); ghosts table untouched by the rebuild.
	if got := dumpBugSlugs(t, pool); got != bugsBefore {
		t.Fatalf("entity projection changed across rebuild: before=%v after=%v", bugsBefore, got)
	}
	if got := ghostCount(t, pool); got != ghostsBefore {
		t.Fatalf("ghosts must survive a projection rebuild untouched: before=%d after=%d", ghostsBefore, got)
	}
}

func TestGhost_SurfacedViaPendingDecisions_WhenSessionPresent(t *testing.T) {
	pool := openTestPool(t)
	deps := work.TableDeps{Pool: pool}

	// With a session: a pending_decisions ghost row is created (Stop-hook seam).
	ctx := events.WithMCPSessionID(context.Background(), "sess-ghost-123")
	if _, err := work.HandleRecord(ctx, deps, "mcp-servers", recordParams(t, false, badEvent("anchored"))); err != nil {
		t.Fatalf("HandleRecord (session): %v", err)
	}
	var pd int
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_decisions WHERE target_session_id = ? AND triggers_json LIKE '%record-rejection%'`,
		"sess-ghost-123").Scan(&pd); err != nil {
		t.Fatalf("count pending_decisions: %v", err)
	}
	if pd != 1 {
		t.Fatalf("expected 1 ghost pending_decision for the session, got %d", pd)
	}

	// Without a session: the ghost is still recorded, but no pending_decision
	// is pushed (nothing to anchor it to).
	if _, err := work.HandleRecord(context.Background(), deps, "mcp-servers", recordParams(t, false, badEvent("unanchored"))); err != nil {
		t.Fatalf("HandleRecord (no session): %v", err)
	}
	var pdEmpty int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM pending_decisions WHERE target_session_id = ''`).Scan(&pdEmpty); err != nil {
		t.Fatalf("count empty-session pending_decisions: %v", err)
	}
	if pdEmpty != 0 {
		t.Fatalf("an unanchored ghost must not create a session-less pending_decision, got %d", pdEmpty)
	}
	if ghostCount(t, pool) != 2 {
		t.Fatalf("expected 2 ghosts recorded (anchored + unanchored), got %d", ghostCount(t, pool))
	}
}

func dumpBugSlugs(t *testing.T, pool interface{ DB() *sql.DB }) string {
	t.Helper()
	rows, err := pool.DB().Query(`SELECT slug, status FROM proj_current_bugs ORDER BY slug`)
	if err != nil {
		t.Fatalf("dump bugs: %v", err)
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var slug, status string
		if err := rows.Scan(&slug, &status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out += slug + ":" + status + ";"
	}
	return out
}
