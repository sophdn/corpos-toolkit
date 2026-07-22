package work_test

import (
	"context"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
	"toolkit/internal/work"
)

// openRoadmapTestPool returns a test pool with the roadmap_meta bookmark
// row seeded. The roadmap_meta table itself is created by migration 002
// via db.Open's embedded runner (bug 1326); only the bookmark row needs
// per-test setup.
func openRoadmapTestPool(t *testing.T) *db.Pool {
	t.Helper()
	pool := openTestPool(t)
	if _, err := pool.DB().Exec(
		`INSERT INTO roadmap_meta (key, value, updated_at)
		 VALUES ('last_reassessed_at', '1970-01-01 00:00:00', datetime('now'))
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
	); err != nil {
		t.Fatalf("seed roadmap_meta: %v", err)
	}
	return pool
}

func TestRoadmapList_Empty(t *testing.T) {
	pool := openRoadmapTestPool(t)
	resp, err := work.HandleRoadmapList(context.Background(), pool, "mcp-servers", nil)
	if err != nil {
		t.Fatalf("HandleRoadmapList: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("empty roadmap: %+v", resp)
	}
}

func TestRoadmapSet_ReplaceProjectScope(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")

	resp, err := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "c1"},
			{"ref_kind": "chain", "ref_slug": "c2", "note": "second"},
		},
	}))
	if err != nil {
		t.Fatalf("HandleRoadmapSet: %v", err)
	}
	if !resp.OK || resp.Count != 2 {
		t.Errorf("resp: %+v", resp)
	}

	listResp, _ := work.HandleRoadmapList(context.Background(), pool, "mcp-servers", nil)
	if len(listResp) != 2 || listResp[0].RefSlug != "c1" || listResp[1].RefSlug != "c2" {
		t.Errorf("post-set list: %+v", listResp)
	}
	if listResp[1].Note != "second" {
		t.Errorf("note: %q", listResp[1].Note)
	}
}

func TestRoadmapSet_RejectsTerminalChain(t *testing.T) {
	pool := openRoadmapTestPool(t)
	testutil.SeedChain(t, pool, "mcp-servers", "closed-chain", "closed", testutil.SeedChainOpts{})
	resp, _ := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{{"ref_kind": "chain", "ref_slug": "closed-chain"}},
	}))
	if resp.Error == "" || !contains(resp.Error, "only open chains") {
		t.Errorf("expected reject, got %q", resp.Error)
	}
}

func TestRoadmapSet_RejectsMixedProjects(t *testing.T) {
	pool := openRoadmapTestPool(t)
	pool.DB().Exec(`INSERT INTO projects (id, name) VALUES ('other', 'other')`)
	seedChain(t, pool, "mcp-servers", "a")
	seedChain(t, pool, "other", "b")

	resp, _ := work.HandleRoadmapSet(context.Background(), pool, "", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "a"},
			{"ref_kind": "chain", "ref_slug": "b"},
		},
	}))
	if !contains(resp.Error, "mixed projects") {
		t.Errorf("mixed-projects rejection: %q", resp.Error)
	}
}

func TestRoadmapPreviewSet_ReportsDelta(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")
	seedChain(t, pool, "mcp-servers", "c3")

	work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "c1"},
			{"ref_kind": "chain", "ref_slug": "c2"},
		},
	}))

	resp, _ := work.HandleRoadmapPreviewSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "c2"},
			{"ref_kind": "chain", "ref_slug": "c3"},
		},
	}))
	if resp.Preview == nil {
		t.Fatalf("expected preview, got %+v", resp)
	}
	preview := resp.Preview
	if len(preview.Removed) != 1 || preview.Removed[0].RefSlug != "c1" {
		t.Errorf("removed: %+v", preview.Removed)
	}
	if len(preview.Added) != 1 || preview.Added[0].RefSlug != "c3" {
		t.Errorf("added: %+v", preview.Added)
	}
	// c2 went from position 2 to position 1.
	if len(preview.Repositioned) != 1 || preview.Repositioned[0].Before != 2 || preview.Repositioned[0].After != 1 {
		t.Errorf("repositioned: %+v", preview.Repositioned)
	}
}

func TestRoadmapInsert_AppendsAtEnd(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")
	work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{{"ref_kind": "chain", "ref_slug": "c1"}},
	}))

	resp, err := work.HandleRoadmapInsert(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"ref_kind": "chain", "ref_slug": "c2",
	}))
	if err != nil {
		t.Fatalf("HandleRoadmapInsert: %v", err)
	}
	if !resp.OK || resp.Position != 2 {
		t.Errorf("resp: %+v", resp)
	}
}

func TestRoadmapInsert_AtSpecificPositionShiftsOthers(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")
	seedChain(t, pool, "mcp-servers", "c3")
	work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "c1"},
			{"ref_kind": "chain", "ref_slug": "c2"},
		},
	}))
	// Insert c3 at position 1 — c1 shifts to 2, c2 to 3.
	work.HandleRoadmapInsert(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"ref_kind": "chain", "ref_slug": "c3", "position": float64(1),
	}))
	listResp, _ := work.HandleRoadmapList(context.Background(), pool, "mcp-servers", nil)
	wantOrder := []string{"c3", "c1", "c2"}
	for i, want := range wantOrder {
		if listResp[i].RefSlug != want {
			t.Errorf("position %d: want %q, got %q", i+1, want, listResp[i].RefSlug)
		}
	}
}

func TestRoadmapInsert_RejectsDuplicate(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	work.HandleRoadmapInsert(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"ref_kind": "chain", "ref_slug": "c1",
	}))
	resp, _ := work.HandleRoadmapInsert(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"ref_kind": "chain", "ref_slug": "c1",
	}))
	if !contains(resp.Error, "already on the roadmap") {
		t.Errorf("expected already-on-roadmap: %q", resp.Error)
	}
}

// TestRoadmapSet_HonorsExplicitPositionWithGaps covers bug 1333:
// explicit Position values are honored (defaulting to 1-based array
// index only when nil) so callers can express gaps like positions 1,
// 5, 10 — used by cross-project priority bands and reserved-slot
// patterns.
func TestRoadmapSet_HonorsExplicitPositionWithGaps(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")
	seedChain(t, pool, "mcp-servers", "c3")

	resp, err := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "c1", "position": float64(1)},
			{"ref_kind": "chain", "ref_slug": "c2", "position": float64(5)},
			{"ref_kind": "chain", "ref_slug": "c3", "position": float64(10)},
		},
	}))
	if err != nil || !resp.OK {
		t.Fatalf("HandleRoadmapSet: err=%v resp=%+v", err, resp)
	}

	listResp, _ := work.HandleRoadmapList(context.Background(), pool, "mcp-servers", nil)
	if len(listResp) != 3 {
		t.Fatalf("expected 3 rows, got %d: %+v", len(listResp), listResp)
	}
	wantPositions := []int64{1, 5, 10}
	wantSlugs := []string{"c1", "c2", "c3"}
	for i, want := range wantPositions {
		if listResp[i].Position != want {
			t.Errorf("row %d: want position %d, got %d", i, want, listResp[i].Position)
		}
		if listResp[i].RefSlug != wantSlugs[i] {
			t.Errorf("row %d: want slug %q, got %q", i, wantSlugs[i], listResp[i].RefSlug)
		}
	}
}

// TestRoadmapSet_RejectsDuplicatePositions covers bug 1333: two items
// in the same call cannot share a position. Returned error names both
// colliding items so the caller can fix without guessing.
func TestRoadmapSet_RejectsDuplicatePositions(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")

	resp, _ := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "c1", "position": float64(3)},
			{"ref_kind": "chain", "ref_slug": "c2", "position": float64(3)},
		},
	}))
	if resp.Error == "" {
		t.Fatalf("expected duplicate-position rejection, got OK")
	}
	if !contains(resp.Error, "duplicate position 3") {
		t.Errorf("error should name duplicate position: %q", resp.Error)
	}
	if !contains(resp.Error, "c1") || !contains(resp.Error, "c2") {
		t.Errorf("error should name both colliding items: %q", resp.Error)
	}
}

// TestRoadmapSet_MixesExplicitAndImplicitPositions covers bug 1333:
// items without an explicit position default to 1-based array index;
// items with one use the explicit value. Mixed input is honored so
// long as the resulting positions don't collide.
func TestRoadmapSet_MixesExplicitAndImplicitPositions(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")
	seedChain(t, pool, "mcp-servers", "c3")

	// c1 defaults to position 1 (array index 0 + 1); c2 explicit 7;
	// c3 defaults to position 3 (array index 2 + 1). No collisions.
	resp, err := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "c1"},
			{"ref_kind": "chain", "ref_slug": "c2", "position": float64(7)},
			{"ref_kind": "chain", "ref_slug": "c3"},
		},
	}))
	if err != nil || !resp.OK {
		t.Fatalf("HandleRoadmapSet: err=%v resp=%+v", err, resp)
	}

	listResp, _ := work.HandleRoadmapList(context.Background(), pool, "mcp-servers", nil)
	wantByPos := map[int64]string{1: "c1", 3: "c3", 7: "c2"}
	for _, row := range listResp {
		want, ok := wantByPos[row.Position]
		if !ok {
			t.Errorf("unexpected position %d for slug %q", row.Position, row.RefSlug)
			continue
		}
		if row.RefSlug != want {
			t.Errorf("position %d: want %q, got %q", row.Position, want, row.RefSlug)
		}
	}
}

// TestRoadmapInsert_ErrorMessageNamesExplicitPositionField covers bug
// 1336: the error returned when re-inserting an already-roadmapped ref
// must point at the actual reposition mechanism (roadmap_set with an
// explicit position on the item), not the broken-historical-suggestion
// "use roadmap_set to reposition" — which used to imply position-by-
// array-order would honor the caller's intent.
func TestRoadmapInsert_ErrorMessageNamesExplicitPositionField(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	work.HandleRoadmapInsert(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"ref_kind": "chain", "ref_slug": "c1",
	}))
	resp, _ := work.HandleRoadmapInsert(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"ref_kind": "chain", "ref_slug": "c1",
	}))
	if !contains(resp.Error, "explicit `position` field") {
		t.Errorf("error should reference explicit position field: %q", resp.Error)
	}
}

// --- roadmap_update (PATCH) — bug roadmap-set-clobbers-notes-on-partial-item-submission ---

func TestRoadmapUpdate_PreservesUntouchedFields(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	seedChain(t, pool, "mcp-servers", "c2")
	seedChain(t, pool, "mcp-servers", "c3")
	work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "c1", "note": "first"},
			{"ref_kind": "chain", "ref_slug": "c2", "note": "second"},
			{"ref_kind": "chain", "ref_slug": "c3", "note": "third"},
		},
	}))

	// Update ONLY position 2's note. Positions 1 and 3 must keep
	// their notes — the bug is silent-clobber via roadmap_set, this
	// is the PATCH-shape fix.
	resp, err := work.HandleRoadmapUpdate(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"position": float64(2),
		"note":     "second-updated",
	}))
	if err != nil || !resp.OK {
		t.Fatalf("HandleRoadmapUpdate: err=%v resp=%+v", err, resp)
	}

	listResp, _ := work.HandleRoadmapList(context.Background(), pool, "mcp-servers", nil)
	wantByPos := map[int64]string{1: "first", 2: "second-updated", 3: "third"}
	for _, row := range listResp {
		want, ok := wantByPos[row.Position]
		if !ok {
			t.Errorf("unexpected position %d", row.Position)
			continue
		}
		if row.Note != want {
			t.Errorf("position %d note: want %q, got %q", row.Position, want, row.Note)
		}
	}
}

func TestRoadmapUpdate_ClearsNoteWithEmptyString(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{{"ref_kind": "chain", "ref_slug": "c1", "note": "has note"}},
	}))

	resp, _ := work.HandleRoadmapUpdate(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"position": float64(1),
		"note":     "",
	}))
	if !resp.OK {
		t.Fatalf("clear-note update failed: %+v", resp)
	}
	listResp, _ := work.HandleRoadmapList(context.Background(), pool, "mcp-servers", nil)
	if len(listResp) != 1 || listResp[0].Note != "" {
		t.Errorf("expected empty note, got %+v", listResp)
	}
}

func TestRoadmapUpdate_RejectsMissingPosition(t *testing.T) {
	pool := openRoadmapTestPool(t)
	resp, _ := work.HandleRoadmapUpdate(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"note": "no position",
	}))
	if !contains(resp.Error, "position") {
		t.Errorf("error should name position: %q", resp.Error)
	}
}

func TestRoadmapUpdate_RejectsNoFieldsToUpdate(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{{"ref_kind": "chain", "ref_slug": "c1"}},
	}))

	resp, _ := work.HandleRoadmapUpdate(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"position": float64(1),
	}))
	if !contains(resp.Error, "at least one of") {
		t.Errorf("error should name the required-at-least-one constraint: %q", resp.Error)
	}
}

func TestRoadmapUpdate_RejectsMissingRow(t *testing.T) {
	pool := openRoadmapTestPool(t)
	resp, _ := work.HandleRoadmapUpdate(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"position": float64(99),
		"note":     "x",
	}))
	if !contains(resp.Error, "no roadmap entry") {
		t.Errorf("error should say no entry: %q", resp.Error)
	}
}

func TestRoadmapUpdate_RejectsPartialRefChange(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "c1")
	work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{{"ref_kind": "chain", "ref_slug": "c1"}},
	}))

	resp, _ := work.HandleRoadmapUpdate(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"position": float64(1),
		"ref_kind": "task",
	}))
	if !contains(resp.Error, "together") {
		t.Errorf("error should require ref_kind+ref_slug together: %q", resp.Error)
	}
}

func TestRoadmapDiff_SurfacesUnplacedChainsAndTasks(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "new-chain")
	pool.DB().Exec(`UPDATE proj_chain_status SET created_at = datetime('now', '+1 day') WHERE slug = 'new-chain'`)

	resp, err := work.HandleRoadmapDiff(context.Background(), pool, "mcp-servers", nil)
	if err != nil {
		t.Fatalf("HandleRoadmapDiff: %v", err)
	}
	if len(resp.Chains) != 1 || resp.Chains[0].Slug != "new-chain" {
		t.Errorf("diff chains: %+v", resp.Chains)
	}
}

// Bug 684 regression: roadmap_set must honor explicit `position` values
// on each item, not silently coerce to dense 1-N by array order. The
// preview must report the explicit position as `after`, not the
// auto-normalized fallback.
func TestRoadmapSet_HonorsExplicitPosition(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "alpha")
	seedChain(t, pool, "mcp-servers", "beta")

	// Sparse positions: alpha at 7, beta at 16 (gap between).
	pos7 := int64(7)
	pos16 := int64(16)
	resp, err := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "alpha", "position": pos7},
			{"ref_kind": "chain", "ref_slug": "beta", "position": pos16},
		},
	}))
	if err != nil {
		t.Fatalf("HandleRoadmapSet: %v", err)
	}
	if !resp.OK {
		t.Fatalf("set failed: %+v", resp)
	}

	list, _ := work.HandleRoadmapList(context.Background(), pool, "mcp-servers", nil)
	if len(list) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(list))
	}
	// proj_roadmap_view is ordered by position ASC, so list[0] is alpha
	// at 7, list[1] is beta at 16. Auto-normalization would have
	// collapsed these to 1 and 2.
	if list[0].RefSlug != "alpha" || list[0].Position != 7 {
		t.Errorf("expected alpha@7, got %s@%d", list[0].RefSlug, list[0].Position)
	}
	if list[1].RefSlug != "beta" || list[1].Position != 16 {
		t.Errorf("expected beta@16, got %s@%d", list[1].RefSlug, list[1].Position)
	}
}

// Bug 684 regression: preview_set's `repositioned` field must report the
// requested position as `after`, not the auto-normalized fallback. The
// original bug surfaced as preview saying `repositioned: before=10,
// after=1` when the caller passed position=16.
func TestRoadmapPreviewSet_RepositionShowsRequestedAfter(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "gamma")
	// Seed roadmap with gamma at position 10.
	pos10 := int64(10)
	_, err := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "gamma", "position": pos10},
		},
	}))
	if err != nil {
		t.Fatalf("seed set: %v", err)
	}

	// Preview moving gamma from 10 → 16.
	pos16 := int64(16)
	prev, err := work.HandleRoadmapPreviewSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "gamma", "position": pos16},
		},
	}))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Preview == nil {
		t.Fatalf("nil preview, error=%q", prev.Error)
	}
	if len(prev.Preview.Repositioned) != 1 {
		t.Fatalf("expected 1 reposition, got %d (preview=%+v)", len(prev.Preview.Repositioned), prev.Preview)
	}
	move := prev.Preview.Repositioned[0]
	if move.Before != 10 || move.After != 16 {
		t.Errorf("expected before=10 after=16, got before=%d after=%d", move.Before, move.After)
	}
}

// Bug 684 regression: duplicate explicit positions within one items
// array must fail loudly. The error names which two items collided so
// the caller can correct without re-reading docs.
func TestRoadmapSet_RejectsDuplicateExplicitPositions(t *testing.T) {
	pool := openRoadmapTestPool(t)
	seedChain(t, pool, "mcp-servers", "delta")
	seedChain(t, pool, "mcp-servers", "epsilon")

	pos5 := int64(5)
	resp, _ := work.HandleRoadmapSet(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"items": []map[string]any{
			{"ref_kind": "chain", "ref_slug": "delta", "position": pos5},
			{"ref_kind": "chain", "ref_slug": "epsilon", "position": pos5},
		},
	}))
	if resp.Error == "" || !contains(resp.Error, "duplicate position 5") {
		t.Errorf("expected duplicate-position rejection, got %q", resp.Error)
	}
}

func TestRoadmapMarkReassessed_BumpsTimestamp(t *testing.T) {
	pool := openRoadmapTestPool(t)
	var before string
	pool.DB().QueryRow(`SELECT value FROM roadmap_meta WHERE key = 'last_reassessed_at'`).Scan(&before)

	work.HandleRoadmapMarkReassessed(context.Background(), pool, "mcp-servers", nil)

	var after string
	pool.DB().QueryRow(`SELECT value FROM roadmap_meta WHERE key = 'last_reassessed_at'`).Scan(&after)
	if after == "" || after == before {
		t.Errorf("mark_reassessed did not update last_reassessed_at: before=%q after=%q", before, after)
	}
}
