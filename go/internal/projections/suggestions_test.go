package projections_test

import (
	"testing"

	"toolkit/internal/testutil"
)

// TestCurrentSuggestions_RebuildFromEmpty seeds three SuggestionReported
// events directly into the events table (post-T5-suggestions, rebuild
// replays events; the CRUD `suggestions` table is no longer the snapshot
// source). Asserts that a fresh rebuild produces three projection rows
// with deterministic content, and that running rebuild twice produces
// the same checksum (idempotency).
func TestCurrentSuggestions_RebuildFromEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Seed three SuggestionReported events. Fold replays these into
	// proj_current_suggestions during RebuildFromEmpty.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7a00-0001-7000-8000-000000000001', '2026-05-21 00:00:01', 'system', 'test', 'SuggestionReported', 'suggestion', 's-1', 'p1',
		 '{"title":"first suggestion","problem_statement":"expand the docs","priority":"high"}', '019e7a00-0001-7000-8000-000000000001', 1),
		('019e7a00-0002-7000-8000-000000000002', '2026-05-21 00:00:02', 'system', 'test', 'SuggestionReported', 'suggestion', 's-2', 'p1',
		 '{"title":"second suggestion","problem_statement":"wire the toggle","priority":"medium"}', '019e7a00-0002-7000-8000-000000000002', 1),
		('019e7a00-0003-7000-8000-000000000003', '2026-05-21 00:00:03', 'system', 'test', 'SuggestionReported', 'suggestion', 's-3', 'p1',
		 '{"title":"third suggestion","problem_statement":"add the test","priority":"low"}', '019e7a00-0003-7000-8000-000000000003', 1)`)

	mustExec(t, pool, `DELETE FROM proj_current_suggestions`)
	mustRebuild(t, pool, []string{"current_suggestions"})
	reference := tableChecksum(t, pool, "proj_current_suggestions")

	mustExec(t, pool, `DELETE FROM proj_current_suggestions`)
	mustRebuild(t, pool, []string{"current_suggestions"})
	after := tableChecksum(t, pool, "proj_current_suggestions")
	if reference != after {
		t.Fatalf("proj_current_suggestions checksum drift: reference=%s after=%s", reference, after)
	}

	if got := tableCount(t, pool, "proj_current_suggestions"); got != 3 {
		t.Errorf("proj_current_suggestions rows = %d, want 3", got)
	}
}
