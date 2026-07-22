package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/projections"
)

// TestMigration070_RestoresTerminalStateAfterReopenLeavesLedgerOpen pins
// bug 886 (rebuild-projections-regresses-terminal-bugs-from-incomplete-
// events-ledger).
//
// The divergent state: a bug whose ledger ends at BugReopened (so a
// fold-from-empty leaves it `open`) but whose live projection is
// terminal — the 2026-05-22 recovery restored terminal projection state
// directly without emitting the final BugResolved. rebuild-projections
// then trips its own regression guard (open-bug count jumps).
//
// This test reproduces that state in-fixture, asserts the regression
// (rebuild-from-empty → open), applies the real migration 070 SQL, and
// asserts the rebuild now reconstructs the terminal state.
func TestMigration070_RestoresTerminalStateAfterReopenLeavesLedgerOpen(t *testing.T) {
	pool, _ := openFreshPool(t)
	defer pool.Close()
	ctx := context.Background()

	const (
		slug     = "fixture-reopened-then-restored-bug"
		project  = "mcp-servers"
		reopenTs = "2026-05-22T22:00:00.000Z"
		fixSHA   = "cafef00dcafef00dcafef00dcafef00dcafef00d"
	)

	// 1. Ledger: reported → resolved(fixed) → reopened. The reopen is the
	//    last event, so a fold-from-empty yields `open`.
	insertBugEvent(t, pool, "evt-070-1", "2026-05-20T10:00:00.000Z", "BugReported", slug, project,
		`{"title":"fixture bug","problem_statement":"reopened-then-restored fixture"}`)
	insertBugEvent(t, pool, "evt-070-2", "2026-05-20T11:00:00.000Z", "BugResolved", slug, project,
		`{"kind":"fixed","commit_sha":"`+fixSHA+`"}`)
	insertBugEvent(t, pool, "evt-070-3", reopenTs, "BugReopened", slug, project,
		`{"previous_resolution":{"kind":"fixed","commit_sha":"`+fixSHA+`"}}`)

	// 2. Reproduce the regression: rebuild current_bugs from empty and
	//    confirm the ledger-tail BugReopened leaves the bug open.
	rebuildCurrentBugs(t, pool, ctx)
	if got := bugStatus(t, pool, slug); got != "open" {
		t.Fatalf("precondition: ledger ending at BugReopened should fold to open, got %q", got)
	}

	// 3. Simulate the historical recovery: the live projection was
	//    restored to terminal directly (no terminal event emitted), while
	//    the ledger still ends at BugReopened. last_event_ts carries the
	//    reopen ts — migration 070 keys its synthetic-event ts off it.
	if _, err := pool.DB().ExecContext(ctx, `
		UPDATE proj_current_bugs
		SET status='fixed', resolution_kind='fixed', resolved_commit_sha=?,
		    resolved_at='2026-05-20T11:00:00.000Z', last_event_ts=?
		WHERE project_id=? AND slug=?`,
		fixSHA, reopenTs, project, slug); err != nil {
		t.Fatalf("simulate recovery update: %v", err)
	}

	// 4. Apply the real migration 070 SQL (it ran at db.Open before this
	//    fixture existed, so re-exec it against the now-divergent state).
	applyMigration070(t, pool, ctx)

	// 5. Rebuild from empty again — the synthetic terminal event must now
	//    reconstruct the fixed state.
	rebuildCurrentBugs(t, pool, ctx)
	if got := bugStatus(t, pool, slug); got != "fixed" {
		t.Errorf("after migration 070, rebuild-from-empty should reconstruct fixed, got %q", got)
	}
	if got := bugCommitSHA(t, pool, slug); got != fixSHA {
		t.Errorf("after migration 070, resolved_commit_sha should reconstruct as %q, got %q", fixSHA, got)
	}
}

// insertBugEvent writes one bug event into the events ledger, mirroring
// the column set migration 061 uses for synthetic events.
func insertBugEvent(t *testing.T, pool *db.Pool, eventID, ts, typ, slug, project, payload string) {
	t.Helper()
	if _, err := pool.DB().Exec(`
		INSERT INTO events (
			event_id, ts, actor_kind, actor_id, type,
			entity_kind, entity_slug, entity_project_id,
			payload, rationale, caused_by_event_id, related_entities,
			span_id, schema_version
		) VALUES (?, ?, 'system', 'test-fixture', ?, 'bug', ?, ?, ?, NULL, NULL, '[]', 'span-070', 1)`,
		eventID, ts, typ, slug, project, payload); err != nil {
		t.Fatalf("insert %s event: %v", typ, err)
	}
}

// rebuildCurrentBugs truncates + rebuilds proj_current_bugs from empty.
func rebuildCurrentBugs(t *testing.T, pool *db.Pool, ctx context.Context) {
	t.Helper()
	if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := projections.RebuildAll(ctx, tx, []string{"current_bugs"})
		return err
	}); err != nil {
		t.Fatalf("rebuild current_bugs: %v", err)
	}
}

// applyMigration070 reads the canonical migration 070 file and executes
// it against the pool — re-running the real fix SQL (it had already run
// inertly at db.Open, before the fixture's divergent state existed).
func applyMigration070(t *testing.T, pool *db.Pool, ctx context.Context) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migPath := filepath.Clean(filepath.Join(filepath.Dir(thisFile),
		"..", "..", "internal", "db", "migrations",
		"070_backfill_bug_terminal_restore_events.sql"))
	body, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatalf("read migration 070: %v", err)
	}
	if _, err := pool.DB().ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("exec migration 070: %v", err)
	}
}

func bugStatus(t *testing.T, pool *db.Pool, slug string) string {
	t.Helper()
	var s string
	if err := pool.DB().QueryRow(`SELECT status FROM proj_current_bugs WHERE slug=?`, slug).Scan(&s); err != nil {
		t.Fatalf("query bug status: %v", err)
	}
	return s
}

func bugCommitSHA(t *testing.T, pool *db.Pool, slug string) string {
	t.Helper()
	var s sql.NullString
	if err := pool.DB().QueryRow(`SELECT resolved_commit_sha FROM proj_current_bugs WHERE slug=?`, slug).Scan(&s); err != nil {
		t.Fatalf("query bug sha: %v", err)
	}
	return s.String
}
